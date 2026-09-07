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
	M "github.com/sagernet/sing/common/metadata"

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

// verifyUserTrafficAccounting 经探针入站发送一次回显流量，并确认该探针身份的上行与下行计数器都可读。
//
// 返回 (true, "") 表示用户级统计可用；(false, 原因) 表示节点缺少 policy 或计数器不可读；
// 返回 error 只用于「无法完成这次探测」的意外情况（例如探针入站根本连不上）。
func (c *Client) verifyUserTrafficAccounting(ctx context.Context, listenAddress string, port int,
	serverKey, userKey security.RedactedString, statisticsID string) (bool, string, error) {
	echo, err := startEchoListener()
	if err != nil {
		return false, "", err
	}
	defer echo.Close()

	if err := exchangeThroughInbound(dialTarget(listenAddress), port, serverKey, userKey, echo.Addr().String()); err != nil {
		return false, "", fmt.Errorf("probe inbound did not carry authenticated traffic: %w", err)
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

// exchangeThroughInbound 以 SS2022 客户端经探针入站把载荷发到回显端口并读回，确认身份认证成功。
func exchangeThroughInbound(host string, port int, serverKey, userKey security.RedactedString, echo string) error {
	method, err := shadowaead_2022.NewWithPassword(security.MethodAES256, serverKey.Reveal()+":"+userKey.Reveal(), nil)
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
	conn := method.DialEarlyConn(raw, M.ParseSocksaddr(echo))
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
