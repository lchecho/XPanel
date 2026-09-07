package xray

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"

	"github.com/sagernet/sing-shadowsocks/shadowaead_2022"
	"github.com/sagernet/sing/common/buf"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	statscommand "github.com/xtls/xray-core/app/stats/command"

	"xpanel/internal/domain"
	"xpanel/internal/ports"
	"xpanel/internal/security"
)

// 下游调用：探针入站 via Shadowsocks 2022（进程内客户端）。
//
// 职责：在一次性探针入站上产生经过 SS2022 身份认证的最小流量，用来证明该节点确实开启了
// 用户级流量统计；不负责判定其它能力，也不参与任何用户的计量。
// 约束：只连接面板自己刚创建的探针入站，目标是面板自己起的本地回显端口，不产生任何外部流量；
// 使用的密钥是探针专用、随机生成、用完即弃。
//
// 契约：specs/002-per-user-inbound/contracts/xray-adapter.md 兼容性门禁第 5 条
// AI-LOCK：这是 FR-005「独立用户流量统计」的唯一可验证手段——实测表明在产生流量之前，
// 无论 policy.levels."0".statsUserUplink/statsUserDownlink 是否开启，用户计数器一律返回
// NotFound，两种配置不可区分（research.md C-005/C-007）。

// probeTrafficBudget 是「产生流量 + 等待计数器出现」的总预算。
const probeTrafficBudget = 5 * time.Second

// probeTrafficPayload 是探针发送的载荷；固定内容便于在诊断里辨认，且长度即预期的最小计数。
var probeTrafficPayload = []byte("xpanel-capability-probe")

// verifyUserTrafficAccounting 经探针入站按模板的网络能力发送回显流量，并确认该探针身份的
// 上行与下行计数器都可读；结束时把这两个计数器清零，避免残留值影响后续判断。
//
// 返回 (true, "") 表示用户级统计可用；(false, 原因) 表示节点缺少 policy 或计数器不可读；
// 返回 error 只用于「无法完成这次探测」的意外情况（例如探针入站根本连不上）。
func (c *Client) verifyUserTrafficAccounting(ctx context.Context, probe ports.TemplateProbe,
	serverKey, userKey security.RedactedString, statisticsID string) (bool, string, error) {
	// 计数器按统计标识注册，Xray 没有删除计数器的 API；探针身份每次都是新的，
	// 因此不会有陈旧数据让后续验证误通过，退出前再清零一次以免留下非零值。
	// 用独立的有界上下文清理：原请求可能已被取消或超时，但清理不能因此被跳过（T093）。
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), probeTrafficBudget)
		defer cancel()
		c.resetCounters(cleanup, statisticsID)
	}()

	host := dialTarget(probe.ListenAddress)
	// 网络能力必须明确：探针要按模板实际会用的网络发送流量。取值异常时宁可报错，
	// 也不能「什么都不发」——那会让统计门禁静默退化为永远不通过。
	if probe.Network != domain.NetworkTCP && probe.Network != domain.NetworkUDP && probe.Network != domain.NetworkTCPUDP {
		return false, "", &ports.AdapterError{Kind: ports.ErrorInvalidArgument, Operation: "validate_template",
			SafeSummary: "template probe carries an unsupported network selection"}
	}
	if probe.Network == domain.NetworkTCP || probe.Network == domain.NetworkTCPUDP {
		echo, err := startEchoListener()
		if err != nil {
			return false, "", err
		}
		err = exchangeThroughInbound(host, probe.ProbePort, probe.Method, serverKey, userKey, echo.Addr().String())
		echo.Close()
		if err != nil {
			return false, "", fmt.Errorf("probe inbound did not carry authenticated TCP traffic: %w", err)
		}
	}
	if probe.Network == domain.NetworkUDP || probe.Network == domain.NetworkTCPUDP {
		echo, err := startPacketEcho()
		if err != nil {
			return false, "", err
		}
		err = exchangePacketThroughInbound(host, probe.ProbePort, probe.Method, serverKey, userKey, echo.LocalAddr().String())
		echo.Close()
		if err != nil {
			return false, "", fmt.Errorf("probe inbound did not carry authenticated UDP traffic: %w", err)
		}
	}
	deadline := time.Now().Add(probeTrafficBudget)
	for {
		round, err := c.ReadTraffic(ctx, ports.TrafficQuery{StatisticsIDs: []string{statisticsID}})
		if err != nil {
			return false, "", err
		}
		uplink, downlink := false, false
		for _, snapshot := range round.Snapshots {
			if !snapshot.Found || snapshot.Bytes == 0 {
				continue
			}
			if snapshot.Direction == ports.Uplink {
				uplink = true
			} else {
				downlink = true
			}
		}
		if uplink && downlink {
			return true, "", nil
		}
		if time.Now().After(deadline) {
			return false, "node does not report per-user traffic counters; enable statsUserUplink and statsUserDownlink", nil
		}
		select {
		case <-ctx.Done():
			return false, "", ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// dialTarget 把监听地址翻译为可拨号的地址：未指定地址（0.0.0.0 / ::）时走回环。
func dialTarget(listenAddress string) string {
	ip := net.ParseIP(listenAddress)
	if ip == nil || ip.IsUnspecified() {
		return "127.0.0.1"
	}
	return listenAddress
}

// startEchoListener 起一个只回显的本地监听，作为探针流量的目的地。
func startEchoListener() (net.Listener, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() { defer conn.Close(); _, _ = io.Copy(conn, conn) }()
		}
	}()
	return listener, nil
}

// resetCounters 把探针身份的两个方向计数器清零（Xray 只能清零，不能删除计数器）。
func (c *Client) resetCounters(ctx context.Context, statisticsID string) {
	for _, direction := range []ports.Direction{ports.Uplink, ports.Downlink} {
		name, err := CounterName(statisticsID, direction)
		if err != nil {
			continue
		}
		callCtx, cancel := c.deadline(ctx)
		_, _ = c.stats.GetStats(callCtx, &statscommand.GetStatsRequest{Name: name, Reset_: true})
		cancel()
	}
}

// startPacketEcho 起一个只回显的本地 UDP 监听，作为 UDP 探针流量的目的地。
func startPacketEcho() (net.PacketConn, error) {
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	go func() {
		buffer := make([]byte, 2048)
		for {
			n, addr, err := conn.ReadFrom(buffer)
			if err != nil {
				return
			}
			_, _ = conn.WriteTo(buffer[:n], addr)
		}
	}()
	return conn, nil
}

// exchangePacketThroughInbound 以 SS2022 客户端经探针入站发一个 UDP 包并读回。
func exchangePacketThroughInbound(host string, port int, method string, serverKey, userKey security.RedactedString, echo string) error {
	client, err := shadowaead_2022.NewWithPassword(method, serverKey.Reveal()+":"+userKey.Reveal(), nil)
	if err != nil {
		return err
	}
	raw, err := net.DialTimeout("udp", net.JoinHostPort(host, strconv.Itoa(port)), probeTrafficBudget)
	if err != nil {
		return err
	}
	defer raw.Close()
	if err := raw.SetDeadline(time.Now().Add(probeTrafficBudget)); err != nil {
		return err
	}
	packets := client.DialPacketConn(raw)
	// SS2022 的 UDP 封装要在载荷前后加头尾，必须按 sing 的约定预留 headroom，
	// 否则写入时会因为缓冲区没有前置空间而 panic。
	buffer := buf.NewPacket()
	defer buffer.Release()
	buffer.Resize(N.CalculateFrontHeadroom(packets), 0)
	buffer.Reserve(N.CalculateRearHeadroom(packets))
	if _, err := buffer.Write(probeTrafficPayload); err != nil {
		return err
	}
	if err := packets.WritePacket(buffer, M.ParseSocksaddr(echo)); err != nil {
		return err
	}
	reply := buf.NewPacket()
	defer reply.Release()
	if _, err := packets.ReadPacket(reply); err != nil {
		return err
	}
	if string(reply.Bytes()) != string(probeTrafficPayload) {
		return errors.New("probe payload was not echoed back over UDP")
	}
	return nil
}

// exchangeThroughInbound 以 SS2022 客户端经探针入站把载荷发到回显端口并读回，确认身份认证成功。
func exchangeThroughInbound(host string, port int, method string, serverKey, userKey security.RedactedString, echo string) error {
	client, err := shadowaead_2022.NewWithPassword(method, serverKey.Reveal()+":"+userKey.Reveal(), nil)
	if err != nil {
		return err
	}
	raw, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), probeTrafficBudget)
	if err != nil {
		return err
	}
	defer raw.Close()
	if err := raw.SetDeadline(time.Now().Add(probeTrafficBudget)); err != nil {
		return err
	}
	conn := client.DialEarlyConn(raw, M.ParseSocksaddr(echo))
	if _, err := conn.Write(probeTrafficPayload); err != nil {
		return err
	}
	echoed := make([]byte, len(probeTrafficPayload))
	if _, err := io.ReadFull(conn, echoed); err != nil {
		return err
	}
	if string(echoed) != string(probeTrafficPayload) {
		return errors.New("probe payload was not echoed back through the inbound")
	}
	return nil
}
