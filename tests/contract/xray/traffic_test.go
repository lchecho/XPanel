package xray_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"

	"xpanel/internal/ports"
	"xpanel/internal/security"
)

// 契约门禁 5–6：真实 AES-256 客户端经 server-key:user-key 通过 TCP 与 UDP，两个精确计数器递增；
// 移除用户后新握手被拒，而已建立的连接可以继续。客户端由第二个 xray 进程（socks 入站 + shadowsocks 出站）扮演。
func TestLiveTrafficCountersAndRemovalSemantics(t *testing.T) {
	runtime := startRuntime(t)
	bin := contractBinary(t)
	userKey := testKey('u')
	statisticsID := "xpanel-11111111-2222-4333-8444-555555555555"
	if _, err := runtime.client.AddUser(context.Background(), ports.AddUserCommand{ProfileTag: "managed", StatisticsID: statisticsID,
		CredentialVersion: 1, UserKey: security.NewRedactedString(userKey)}); err != nil {
		t.Fatal(err)
	}
	tcpEcho := startTCPEcho(t)
	udpEcho := startUDPEcho(t)
	socks := startClientRuntime(t, bin, runtime.inbound, runtime.serverKey+":"+userKey)

	payload := make([]byte, 64*1024)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	established := socksConnect(t, socks, tcpEcho)
	defer established.Close()
	echoThrough(t, established, payload)
	if reply := socksUDPEcho(t, socks, udpEcho, payload[:1024]); !bytes.Equal(reply, payload[:1024]) {
		t.Fatal("UDP payload was not echoed through the Shadowsocks 2022 tunnel")
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		round, err := runtime.client.ReadTraffic(context.Background(), ports.TrafficQuery{StatisticsIDs: []string{statisticsID}})
		if err != nil {
			t.Fatal(err)
		}
		uplink, downlink := uint64(0), uint64(0)
		for _, snapshot := range round.Snapshots {
			if snapshot.Found && snapshot.Direction == ports.Uplink {
				uplink = snapshot.Bytes
			}
			if snapshot.Found && snapshot.Direction == ports.Downlink {
				downlink = snapshot.Bytes
			}
		}
		if uplink >= uint64(len(payload)) && downlink >= uint64(len(payload)) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("counters did not reflect traffic: uplink=%d downlink=%d", uplink, downlink)
		}
		time.Sleep(50 * time.Millisecond)
	}
	bootstrapRound, err := runtime.client.ReadTraffic(context.Background(), ports.TrafficQuery{StatisticsIDs: []string{"bootstrap"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, snapshot := range bootstrapRound.Snapshots {
		if snapshot.Found && snapshot.Bytes != 0 {
			t.Fatalf("bootstrap identity accumulated traffic: %#v", snapshot)
		}
	}

	// 门禁 6：移除后新握手被拒，已建立连接继续。
	if _, err := runtime.client.RemoveUser(context.Background(), ports.RemoveUserCommand{ProfileTag: "managed", StatisticsID: statisticsID}); err != nil {
		t.Fatal(err)
	}
	echoThrough(t, established, payload[:4096])
	rejected, err := socksDial(socks, tcpEcho)
	if err == nil {
		_ = rejected.SetDeadline(time.Now().Add(3 * time.Second))
		_, writeErr := rejected.Write(payload[:1024])
		buffer := make([]byte, 1024)
		_, readErr := io.ReadFull(rejected, buffer)
		rejected.Close()
		if writeErr == nil && readErr == nil {
			t.Fatal("new connection succeeded after the user was removed")
		}
	}
}

func startTCPEcho(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() { defer conn.Close(); _, _ = io.Copy(conn, conn) }()
		}
	}()
	return listener.Addr().String()
}

func startUDPEcho(t *testing.T) string {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		buffer := make([]byte, 65535)
		for {
			n, addr, err := conn.ReadFrom(buffer)
			if err != nil {
				return
			}
			_, _ = conn.WriteTo(buffer[:n], addr)
		}
	}()
	return conn.LocalAddr().String()
}

func startClientRuntime(t *testing.T, bin, serverAddress, password string) string {
	t.Helper()
	socks := freeAddress(t)
	socksHost, socksPort, _ := net.SplitHostPort(socks)
	serverHost, serverPort, _ := net.SplitHostPort(serverAddress)
	var socksPortNumber, serverPortNumber int
	_, _ = fmt.Sscanf(socksPort, "%d", &socksPortNumber)
	_, _ = fmt.Sscanf(serverPort, "%d", &serverPortNumber)
	config := map[string]any{
		"log":      map[string]any{"loglevel": "warning"},
		"inbounds": []any{map[string]any{"tag": "socks", "listen": socksHost, "port": socksPortNumber, "protocol": "socks", "settings": map[string]any{"udp": true}}},
		"outbounds": []any{map[string]any{"protocol": "shadowsocks", "settings": map[string]any{"servers": []any{map[string]any{
			"address": serverHost, "port": serverPortNumber, "method": "2022-blake3-aes-256-gcm", "password": password}}}}},
	}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	path := t.TempDir() + "/client.json"
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	command := exec.CommandContext(ctx, bin, "run", "-config", path)
	command.Stdout, command.Stderr = io.Discard, io.Discard
	if err := command.Start(); err != nil {
		cancel()
		t.Fatal("start Xray client runtime")
	}
	t.Cleanup(func() { cancel(); _ = command.Wait() })
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", socks, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			return socks
		}
		if time.Now().After(deadline) {
			t.Fatal("Xray client SOCKS inbound did not become ready")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func socksAddress(target string) ([]byte, error) {
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		return nil, err
	}
	ip := net.ParseIP(host).To4()
	if ip == nil {
		return nil, errors.New("only IPv4 targets are supported by the test helper")
	}
	var portNumber int
	_, _ = fmt.Sscanf(port, "%d", &portNumber)
	address := append([]byte{0x01}, ip...)
	return binary.BigEndian.AppendUint16(address, uint16(portNumber)), nil
}

func socksDial(socks, target string) (net.Conn, error) {
	conn, err := net.DialTimeout("tcp", socks, 2*time.Second)
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		conn.Close()
		return nil, err
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil || reply[1] != 0x00 {
		conn.Close()
		return nil, errors.New("SOCKS5 method negotiation failed")
	}
	address, err := socksAddress(target)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if _, err := conn.Write(append([]byte{0x05, 0x01, 0x00}, address...)); err != nil {
		conn.Close()
		return nil, err
	}
	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil || header[1] != 0x00 {
		conn.Close()
		return nil, errors.New("SOCKS5 connect was refused")
	}
	if _, err := io.ReadFull(conn, make([]byte, 6)); err != nil {
		conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

func socksConnect(t *testing.T, socks, target string) net.Conn {
	t.Helper()
	conn, err := socksDial(socks, target)
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

func echoThrough(t *testing.T, conn net.Conn, payload []byte) {
	t.Helper()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write(payload); err != nil {
		t.Fatal(err)
	}
	echoed := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, echoed); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(echoed, payload) {
		t.Fatal("TCP payload was not echoed through the Shadowsocks 2022 tunnel")
	}
	_ = conn.SetDeadline(time.Time{})
}

func socksUDPEcho(t *testing.T, socks, target string, payload []byte) []byte {
	t.Helper()
	control, err := net.DialTimeout("tcp", socks, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	_ = control.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := control.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(control, reply); err != nil || reply[1] != 0x00 {
		t.Fatal("SOCKS5 UDP method negotiation failed")
	}
	if _, err := control.Write([]byte{0x05, 0x03, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	header := make([]byte, 4)
	if _, err := io.ReadFull(control, header); err != nil || header[1] != 0x00 || header[3] != 0x01 {
		t.Fatal("SOCKS5 UDP associate was refused")
	}
	bound := make([]byte, 6)
	if _, err := io.ReadFull(control, bound); err != nil {
		t.Fatal(err)
	}
	relayIP := net.IP(bound[:4])
	if relayIP.IsUnspecified() {
		relayIP = net.ParseIP("127.0.0.1")
	}
	relay := &net.UDPAddr{IP: relayIP, Port: int(binary.BigEndian.Uint16(bound[4:]))}
	udp, err := net.DialUDP("udp", nil, relay)
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	address, err := socksAddress(target)
	if err != nil {
		t.Fatal(err)
	}
	datagram := append(append([]byte{0x00, 0x00, 0x00}, address...), payload...)
	_ = udp.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := udp.Write(datagram); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 65535)
	n, err := udp.Read(buffer)
	if err != nil {
		t.Fatal(err)
	}
	if n < 10 {
		t.Fatal("UDP reply too short")
	}
	return buffer[10:n]
}
