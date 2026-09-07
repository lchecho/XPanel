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
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	xrayadapter "xpanel/internal/adapter/xray"
	"xpanel/internal/domain"
	"xpanel/internal/ports"
	"xpanel/internal/security"
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

// 契约门禁 1：固定二进制的运行时版本与其构建信息中的 xray-core 模块版本必须同时匹配（contracts/xray-adapter.md）。
func TestPinnedRuntimeAndModule(t *testing.T) {
	bin := contractBinary(t)
	out, err := exec.Command(bin, "version").CombinedOutput()
	if err != nil {
		t.Fatalf("xray version failed: %s", sanitize(out))
	}
	if !bytes.Contains(out, []byte("26.3.27")) {
		t.Fatalf("Xray runtime does not match %s: %s", wantRuntime, sanitize(out))
	}
	module, ok := binaryModuleVersion(t, bin)
	if !ok {
		t.Logf("WARNING: Go toolchain unavailable; xray-core module version of %s not verified from build info", wantModule)
		return
	}
	if module != wantModule {
		t.Fatalf("Xray binary was built from xray-core %q, want %s", module, wantModule)
	}
}

// binaryModuleVersion 用 `go version -m` 读取 XRAY_BIN 的主模块版本；没有 go 工具链时返回 ok=false（尽力而为）。
func binaryModuleVersion(t *testing.T, bin string) (string, bool) {
	t.Helper()
	goTool, err := exec.LookPath("go")
	if err != nil {
		return "", false
	}
	out, err := exec.Command(goTool, "version", "-m", bin).Output()
	if err != nil {
		t.Fatalf("go version -m failed: %s", sanitize(out))
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == "mod" && fields[1] == "github.com/xtls/xray-core" {
			return fields[2], true
		}
	}
	t.Fatalf("go version -m did not report the xray-core module: %s", sanitize(out))
	return "", false
}

// sanitize 去掉进程输出中的密钥材料（32 字节 base64 = 44 字符）与多余空白，只保留可安全打印的最后几行。
func sanitize(output []byte) string {
	redacted := base64Key.ReplaceAllString(string(output), "[redacted-key]")
	lines := strings.Split(strings.TrimSpace(redacted), "\n")
	if len(lines) > 12 {
		lines = lines[len(lines)-12:]
	}
	return strings.Join(lines, "\n")
}

var base64Key = regexp.MustCompile(`[A-Za-z0-9+/]{43}=`)

// liveRuntime 是一个受控的真实 Xray 进程。
//
// 配置里没有任何面板入站：面板入站全部由被测代码在运行期通过 HandlerService 创建（002 模型）。
// 配置里保留一条运维自有入站 operator-inbound（面板命名空间之外），用于证明面板的只读边界。
type liveRuntime struct {
	client       *xrayadapter.Client
	cancel       context.CancelFunc
	command      *exec.Cmd
	output       *bytes.Buffer
	operator     string
	operatorPort int
	api          string
	config       string
}

const (
	operatorInboundTag = "operator-inbound"
	listenAddress      = "127.0.0.1"
)

// diagnostics 返回脱敏后的进程输出尾部，用于失败时定位（不含密钥）。
func (r *liveRuntime) diagnostics() string { return sanitize(r.output.Bytes()) }

func freeAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", listenAddress+":0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

// freePort 返回一个当前空闲的端口号；面板入站将在其上创建。
func freePort(t *testing.T) int {
	t.Helper()
	return portOf(t, freeAddress(t))
}

func portOf(t *testing.T, address string) int {
	t.Helper()
	_, portText, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

// panelTag 生成一个面板命名空间内的入站标签。
func panelTag(suffix string) string { return domain.NamespacePrefix + suffix }

func testKey(fill byte) string {
	return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{fill}, 32))
}

// createInbound 在运行期创建一条专属入站（一个端口、一个受管客户端），返回其服务端密钥。
func createInbound(t *testing.T, runtime *liveRuntime, tag string, port int, userKey string) string {
	t.Helper()
	serverKey := testKey('s')
	command := ports.CreateInboundCommand{InboundTag: tag, ListenAddress: listenAddress, Port: port,
		Method: security.MethodAES256, Network: domain.NetworkTCPUDP, ServerKey: security.NewRedactedString(serverKey),
		Client: ports.InboundClient{StatisticsID: panelTag(tag[len(domain.NamespacePrefix):] + "-client"), CredentialVersion: 1,
			UserKey: security.NewRedactedString(userKey)}}
	if _, err := runtime.client.CreateInbound(context.Background(), command); err != nil {
		t.Fatalf("create inbound %s on port %d: %v\n%s", tag, port, err, runtime.diagnostics())
	}
	return serverKey
}

// listening 判定某端口当前是否可建立 TCP 连接。
func listening(port int) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(listenAddress, strconv.Itoa(port)), 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// baseConfig 只包含 API、统计、策略与运维自有入站；面板入站在运行期创建。
func baseConfig(apiAddress, operatorAddress string) map[string]any {
	host, portText, _ := net.SplitHostPort(operatorAddress)
	var port int
	_, _ = fmt.Sscanf(portText, "%d", &port)
	return map[string]any{
		// Xray 要求 api.tag 非空（"API tag can't be empty"）；与 deploy/xray-v26.3.27.example.json 保持一致。
		"api":   map[string]any{"tag": "api", "listen": apiAddress, "services": []string{"HandlerService", "StatsService"}},
		"stats": map[string]any{},
		// 用户级计数器依赖该策略段；面板无法经 API 设置，故为部署前置条件（research.md R-007）。
		"policy": map[string]any{"levels": map[string]any{"0": map[string]any{"statsUserUplink": true, "statsUserDownlink": true}}},
		"inbounds": []any{map[string]any{"tag": operatorInboundTag, "listen": host, "port": port, "protocol": "shadowsocks",
			"settings": map[string]any{"method": "2022-blake3-aes-256-gcm", "password": testKey('o'), "network": "tcp,udp",
				"clients": []map[string]string{{"email": "operator", "password": testKey('O')}}}}},
		"outbounds": []any{map[string]any{"protocol": "freedom", "tag": "direct"}},
	}
}

// configWithInbound 在基础配置上追加一条配置文件入站，用于配置检查阶段的负例。
func configWithInbound(config map[string]any, tag, address, method, serverKey string, clients []map[string]string) map[string]any {
	host, portText, _ := net.SplitHostPort(address)
	var port int
	_, _ = fmt.Sscanf(portText, "%d", &port)
	inbounds, _ := config["inbounds"].([]any)
	config["inbounds"] = append(inbounds, map[string]any{"tag": tag, "listen": host, "port": port, "protocol": "shadowsocks",
		"settings": map[string]any{"method": method, "password": serverKey, "network": "tcp,udp", "clients": clients}})
	return config
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

// startRuntime 启动受控的真实 Xray；地址被别的进程抢走时换一组地址重试。
//
// freeAddress 只能证明「刚才那一刻端口是空的」，内核随后可能把同一个临时端口分给别人
// （契约套件同时在起停多个进程，撞车并不罕见）。因此就绪失败不是断言失败，而是重来一次。
func startRuntime(t *testing.T) *liveRuntime {
	t.Helper()
	for attempt := 0; ; attempt++ {
		apiAddress, operatorAddress := freeAddress(t), freeAddress(t)
		runtime, err := launchRuntime(t, apiAddress, baseConfig(apiAddress, operatorAddress))
		if err == nil {
			runtime.operator, runtime.operatorPort = operatorAddress, portOf(t, operatorAddress)
			return runtime
		}
		if attempt == 2 {
			t.Fatalf("Xray runtime did not start after %d attempts: %v", attempt+1, err)
		}
		t.Logf("retrying Xray runtime start on fresh addresses: %v", err)
	}
}

// launchRuntime 写入配置、启动进程并等待 API 就绪；就绪失败返回错误（含脱敏诊断）供调用方重试。
func launchRuntime(t *testing.T, apiAddress string, config map[string]any) (*liveRuntime, error) {
	t.Helper()
	bin := contractBinary(t)
	path := writeRuntimeConfig(t, config)
	ctx, cancel := context.WithCancel(context.Background())
	command := exec.CommandContext(ctx, bin, "run", "-config", path)
	output := &bytes.Buffer{}
	command.Stdout, command.Stderr = output, output
	if err := command.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start Xray runtime: %w", err)
	}
	target := ports.InstanceTarget{APIEndpoint: apiAddress, ExpectedVersion: wantRuntime, RPCTimeout: 500 * time.Millisecond}
	client, err := xrayadapter.New(target)
	if err != nil {
		cancel()
		_ = command.Wait()
		return nil, err
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err = client.Probe(context.Background(), target); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = client.Close()
			cancel()
			_ = command.Wait()
			return nil, fmt.Errorf("Xray API did not become ready: %w\n%s", err, sanitize(output.Bytes()))
		}
		time.Sleep(25 * time.Millisecond)
	}
	runtime := &liveRuntime{client: client, cancel: cancel, command: command, output: output, api: apiAddress, config: path}
	// 通过结构体字段停止进程：restart 会替换 cancel/command，捕获启动时的局部变量会漏掉重启后的进程。
	t.Cleanup(func() {
		_ = client.Close()
		runtime.cancel()
		_ = runtime.command.Wait()
	})
	return runtime, nil
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

// restart 停止并用相同配置重新启动 Xray 进程，模拟运行时重启（动态用户丢失、boot epoch 变化）。
func (r *liveRuntime) restart(t *testing.T, bin string) {
	t.Helper()
	r.restartWith(t, bin, nil)
}

// restartWith 用新配置重启同一个管理端点上的 Xray：config 为 nil 时沿用原配置。
// 用于「节点重启后能力真的变了」这类场景（例如 policy 被去掉）。
func (r *liveRuntime) restartWith(t *testing.T, bin string, config map[string]any) {
	t.Helper()
	if config != nil {
		r.config = writeRuntimeConfig(t, config)
	}
	r.cancel()
	_ = r.command.Wait()
	ctx, cancel := context.WithCancel(context.Background())
	command := exec.CommandContext(ctx, bin, "run", "-config", r.config)
	command.Stdout, command.Stderr = r.output, r.output
	if err := command.Start(); err != nil {
		cancel()
		t.Fatalf("restart Xray runtime: %v", err)
	}
	r.cancel, r.command = cancel, command
	target := ports.InstanceTarget{APIEndpoint: r.api, ExpectedVersion: wantRuntime, RPCTimeout: 500 * time.Millisecond}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := r.client.Probe(context.Background(), target); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Xray API did not come back after restart\n%s", r.diagnostics())
		}
		time.Sleep(25 * time.Millisecond)
	}
}
