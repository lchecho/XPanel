package xray_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sagernet/sing-shadowsocks/shadowaead_2022"
	M "github.com/sagernet/sing/common/metadata"

	"xpanel/internal/application"
	"xpanel/internal/domain"
	"xpanel/internal/ports"
	"xpanel/internal/security"
	"xpanel/internal/testsupport"
)

// crashingAdapter 在第 n 次变更类 RPC 之后模拟进程崩溃（panic），用于在真实 Xray 上
// 逐个边界中断轮换；崩溃点计数在每次调用后推进，因此覆盖「RPC 成功之后、下一步之前」。
type crashingAdapter struct {
	ports.Adapter
	t        *testing.T
	after    int
	calls    int
	crashed  bool
	observe  func(step string)
	disabled bool
}

func (a *crashingAdapter) step(name string) {
	if a.disabled {
		return
	}
	a.calls++
	if a.observe != nil {
		a.observe(fmt.Sprintf("after %s #%d", name, a.calls))
	}
	if a.calls == a.after {
		a.crashed = true
		panic("crash after " + name)
	}
}

func (a *crashingAdapter) AddUser(ctx context.Context, command ports.AddUserCommand) (ports.MutationReceipt, error) {
	receipt, err := a.Adapter.AddUser(ctx, command)
	if err == nil {
		a.step("add_user")
	}
	return receipt, err
}

func (a *crashingAdapter) RemoveUser(ctx context.Context, command ports.RemoveUserCommand) (ports.MutationReceipt, error) {
	receipt, err := a.Adapter.RemoveUser(ctx, command)
	if err == nil {
		a.step("remove_user")
	}
	return receipt, err
}

// handshake 用给定的连接口令经该端口发起一次真实 SS2022 握手并回显，成功返回 nil。
func handshake(method, password, host string, port int, echo string) error {
	client, err := shadowaead_2022.NewWithPassword(method, password, nil)
	if err != nil {
		return err
	}
	raw, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 3*time.Second)
	if err != nil {
		return err
	}
	defer raw.Close()
	if err := raw.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		return err
	}
	conn := client.DialEarlyConn(raw, M.ParseSocksaddr(echo))
	payload := []byte("rotation-handshake")
	if _, err := conn.Write(payload); err != nil {
		return err
	}
	echoed := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, echoed); err != nil {
		return err
	}
	return nil
}

// T087：在真实 Xray 上用真实的 service + synchronizer 执行轮换，并在四次变更 RPC 成功后的
// 每个边界中断、回收租约、重放。断言全程端口监听、客户端数不为 0，恢复后旧凭证被拒、新凭证可用、
// 只剩原不可变统计身份、过渡身份不进入连接信息、历史流量连续。
func TestLiveAppRotationSurvivesCrashAtEveryBoundary(t *testing.T) {
	for boundary := 1; boundary <= 4; boundary++ {
		t.Run(fmt.Sprintf("crash_after_rpc_%d", boundary), func(t *testing.T) {
			runtime := startRuntime(t)
			target := ports.InstanceTarget{APIEndpoint: runtime.api, ExpectedVersion: wantRuntime, RPCTimeout: 500 * time.Millisecond}
			crasher := &crashingAdapter{Adapter: runtime.client, t: t, after: boundary}
			app := testsupport.NewWith(t, testsupport.Options{Adapter: crasher, Target: &target})
			app.Clock.Set(time.Now().UTC())
			templateID := registerLiveTemplate(t, app, "Primary", 4)
			record := app.CreateUser("Rotating", templateID, nil)
			convergeAll(t, app, []ports.UserRecord{record})

			port := app.User(record.User.ID).Inbound.Inbound.Port
			tag := app.User(record.User.ID).Inbound.Inbound.InboundTag
			safety := domain.RotationSafetyID(record.Identity.StatisticsID)
			echo := startTCPEcho(t)
			method := security.MethodAES256

			// 轮换前：旧凭证可用，并产生一段流量作为历史基线。
			before, err := app.Connections.BuildConnectionInfo(context.Background(), record.User.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := handshake(method, before.Password.Reveal(), listenAddress, port, echo); err != nil {
				t.Fatalf("baseline handshake failed: %v\n%s", err, runtime.diagnostics())
			}
			baseline := readCounter(t, runtime, record.Identity.StatisticsID)
			if baseline == 0 {
				t.Fatal("baseline traffic was not accounted")
			}

			// 每次 RPC 之后都检查不变量：端口在听、入站里至少有一个客户端。
			crasher.observe = func(step string) {
				if !listening(port) {
					t.Errorf("%s: port %d stopped listening", step, port)
				}
				users, err := runtime.client.ListUsers(context.Background(),
					ports.RuntimeInbound{InboundTag: tag, Method: method})
				if err != nil {
					t.Errorf("%s: %v", step, err)
					return
				}
				if len(users) == 0 {
					t.Errorf("%s: inbound has no client", step)
				}
			}

			// 轮换并在指定边界崩溃。
			current := app.User(record.User.ID)
			if _, err := app.Users.RotateCredential(context.Background(), application.LifecycleInput{ID: record.User.ID,
				ExpectedRevision: current.User.Revision, RequestID: testsupport.NewID(t), ActorID: app.AdminID}); err != nil {
				t.Fatal(err)
			}
			func() {
				defer func() { _ = recover() }()
				_, _ = app.Sync.Drain(context.Background())
			}()
			if !crasher.crashed {
				t.Fatalf("boundary %d was never reached: only %d mutation RPCs ran", boundary, crasher.calls)
			}
			if !listening(port) {
				t.Fatalf("port %d stopped listening after the crash", port)
			}

			// 租约到期后恢复：同一操作续上。
			crasher.disabled = true
			app.Clock.Set(app.Clock.Now().Add(11 * time.Second))
			convergeAll(t, app, []ports.UserRecord{record})

			// 最终状态：恰好一个受管客户端，就是原来的统计身份。
			users, err := runtime.client.ListUsers(context.Background(), ports.RuntimeInbound{InboundTag: tag, Method: method})
			if err != nil {
				t.Fatal(err)
			}
			if len(users) != 1 || users[0].StatisticsID != record.Identity.StatisticsID {
				t.Fatalf("final clients = %#v, want exactly the original identity", users)
			}
			after := app.User(record.User.ID)
			if after.Identity.StatisticsID != record.Identity.StatisticsID || after.Inbound.Inbound.Port != port {
				t.Fatalf("rotation moved the identity or port: %#v", after.Inbound.Inbound)
			}

			// 新凭证可用，旧凭证被拒；过渡身份不在连接信息里。
			updated, err := app.Connections.BuildConnectionInfo(context.Background(), record.User.ID)
			if err != nil {
				t.Fatal(err)
			}
			if updated.Password.Reveal() == before.Password.Reveal() {
				t.Fatal("the rotation did not change the credential")
			}
			if strings.Contains(updated.URI.Reveal(), domain.RotationSuffix) {
				t.Fatal("the transition identity leaked into the connection information")
			}
			if err := handshake(method, updated.Password.Reveal(), listenAddress, port, echo); err != nil {
				t.Fatalf("new credential rejected after rotation: %v\n%s", err, runtime.diagnostics())
			}
			if err := handshake(method, before.Password.Reveal(), listenAddress, port, echo); err == nil {
				t.Fatal("the old credential still completed a handshake after rotation")
			}

			// 历史流量连续：计数按原统计身份继续累计，没有回退。
			if final := readCounter(t, runtime, record.Identity.StatisticsID); final < baseline {
				t.Fatalf("traffic history regressed: %d < %d", final, baseline)
			}
			// 过渡身份没有残留。
			for _, user := range users {
				if user.StatisticsID == safety {
					t.Fatal("the transition identity survived the rotation")
				}
			}
		})
	}
}

// readCounter 读取某统计身份的上行计数（缺失按 0）。
func readCounter(t *testing.T, runtime *liveRuntime, statisticsID string) uint64 {
	t.Helper()
	round, err := runtime.client.ReadTraffic(context.Background(), ports.TrafficQuery{StatisticsIDs: []string{statisticsID}})
	if err != nil {
		t.Fatal(err)
	}
	for _, snapshot := range round.Snapshots {
		if snapshot.Found && snapshot.Direction == ports.Uplink {
			return snapshot.Bytes
		}
	}
	return 0
}
