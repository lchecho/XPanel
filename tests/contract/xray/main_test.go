package xray_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	xrayadapter "xpanel/internal/adapter/xray"
	"xpanel/internal/ports"
)

const (
	wantRuntime = "v26.3.27"
	wantModule  = "v1.260327.0"
)

func contractBinary(t *testing.T) string {
	t.Helper()
	path := os.Getenv("XRAY_BIN")
	if path == "" {
		if os.Getenv("XPANEL_REQUIRE_CONTRACT") == "1" {
			t.Fatal("XRAY_BIN is required because XPANEL_REQUIRE_CONTRACT=1")
		}
		t.Log("WARNING: XRAY_BIN is unset; fixed-runtime Xray contract test skipped")
		t.SkipNow()
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		t.Fatalf("XRAY_BIN is not an executable regular file")
	}
	return path
}

func TestPinnedRuntimeAndModule(t *testing.T) {
	bin := contractBinary(t)
	out, err := exec.Command(bin, "version").CombinedOutput()
	if err != nil {
		t.Fatalf("xray version failed")
	}
	if !bytes.Contains(out, []byte("26.3.27")) {
		t.Fatalf("Xray runtime does not match %s", wantRuntime)
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		t.Fatal("Go build information unavailable")
	}
	found := false
	for _, dependency := range info.Deps {
		if dependency.Path == "github.com/xtls/xray-core" {
			found = dependency.Version == wantModule
		}
	}
	if !found {
		t.Fatalf("Xray Go module does not match %s", wantModule)
	}
}

type liveRuntime struct {
	client  *xrayadapter.Client
	cancel  context.CancelFunc
	command *exec.Cmd
	output  *bytes.Buffer
}

func freeAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

func testKey(fill byte) string {
	return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{fill}, 32))
}

func runtimeConfig(apiAddress, inboundAddress, method, serverKey string, clients []map[string]string) map[string]any {
	host, portText, _ := net.SplitHostPort(inboundAddress)
	var port int
	_, _ = fmt.Sscanf(portText, "%d", &port)
	return map[string]any{
		"api":    map[string]any{"listen": apiAddress, "services": []string{"HandlerService", "StatsService"}},
		"stats":  map[string]any{},
		"policy": map[string]any{"levels": map[string]any{"0": map[string]any{"statsUserUplink": true, "statsUserDownlink": true}}},
		"inbounds": []any{map[string]any{"tag": "managed", "listen": host, "port": port, "protocol": "shadowsocks",
			"settings": map[string]any{"method": method, "password": serverKey, "network": "tcp,udp", "clients": clients}}},
		"outbounds": []any{map[string]any{"protocol": "freedom", "tag": "direct"}},
	}
}

func writeRuntimeConfig(t *testing.T, config map[string]any) string {
	t.Helper()
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	path := t.TempDir() + "/xray.json"
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func startRuntime(t *testing.T) *liveRuntime {
	t.Helper()
	bin := contractBinary(t)
	apiAddress, inboundAddress := freeAddress(t), freeAddress(t)
	config := runtimeConfig(apiAddress, inboundAddress, "2022-blake3-aes-256-gcm", testKey('s'),
		[]map[string]string{{"email": "bootstrap", "password": testKey('b')}})
	path := writeRuntimeConfig(t, config)
	ctx, cancel := context.WithCancel(context.Background())
	command := exec.CommandContext(ctx, bin, "run", "-config", path)
	output := &bytes.Buffer{}
	command.Stdout, command.Stderr = output, output
	if err := command.Start(); err != nil {
		cancel()
		t.Fatalf("start Xray runtime")
	}
	target := ports.InstanceTarget{APIEndpoint: apiAddress, ExpectedVersion: wantRuntime, RPCTimeout: 500 * time.Millisecond}
	client, err := xrayadapter.New(target)
	if err != nil {
		cancel()
		_ = command.Wait()
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err = client.Probe(context.Background(), target); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = client.Close()
			cancel()
			_ = command.Wait()
			t.Fatalf("Xray API did not become ready")
		}
		time.Sleep(25 * time.Millisecond)
	}
	runtime := &liveRuntime{client: client, cancel: cancel, command: command, output: output}
	t.Cleanup(func() {
		_ = client.Close()
		cancel()
		_ = command.Wait()
	})
	return runtime
}

func runConfigCheck(t *testing.T, bin string, config map[string]any) error {
	t.Helper()
	path := writeRuntimeConfig(t, config)
	command := exec.Command(bin, "run", "-test", "-config", path)
	command.Stdout, command.Stderr = &bytes.Buffer{}, &bytes.Buffer{}
	return command.Run()
}

func safeContains(output []byte, value string) bool {
	return strings.Contains(strings.ToLower(string(output)), strings.ToLower(value))
}
