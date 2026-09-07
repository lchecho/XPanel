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

// liveRuntime 是一个受控的真实 Xray 进程：主 inbound `managed`（bootstrap `bootstrap`）、
// 第二 inbound `managed2`（bootstrap `bootstrap2`）用于多 profile 门禁，
// 第三 inbound `managed3` 故意复用 bootstrap 邮箱 `bootstrap`（Xray 允许，但统计计数器按邮箱全局共享）。
type liveRuntime struct {
	client     *xrayadapter.Client
	cancel     context.CancelFunc
	command    *exec.Cmd
	output     *bytes.Buffer
	inbound    string
	inbound2   string
	inbound3   string
	serverKey  string
	serverKey2 string
	serverKey3 string
	api        string
	config     string
}

const (
	secondInboundTag = "managed2"
	secondBootstrap  = "bootstrap2"
	thirdInboundTag  = "managed3"
)

// diagnostics 返回脱敏后的进程输出尾部，用于失败时定位（不含密钥）。
func (r *liveRuntime) diagnostics() string { return sanitize(r.output.Bytes()) }

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
		// Xray 要求 api.tag 非空（"API tag can't be empty"）；与 deploy/xray-v26.3.27.example.json 保持一致。
		"api":    map[string]any{"tag": "api", "listen": apiAddress, "services": []string{"HandlerService", "StatsService"}},
		"stats":  map[string]any{},
		"policy": map[string]any{"levels": map[string]any{"0": map[string]any{"statsUserUplink": true, "statsUserDownlink": true}}},
		"inbounds": []any{map[string]any{"tag": "managed", "listen": host, "port": port, "protocol": "shadowsocks",
			"settings": map[string]any{"method": method, "password": serverKey, "network": "tcp,udp", "clients": clients}}},
		"outbounds": []any{map[string]any{"protocol": "freedom", "tag": "direct"}},
	}
}

// withInbound 追加一个 SS2022 多用户 inbound，用于多 profile 场景。
func withInbound(config map[string]any, tag, address, method, serverKey string, clients []map[string]string) map[string]any {
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

func startRuntime(t *testing.T) *liveRuntime {
	t.Helper()
	apiAddress, inboundAddress, secondAddress, thirdAddress := freeAddress(t), freeAddress(t), freeAddress(t), freeAddress(t)
	config := runtimeConfig(apiAddress, inboundAddress, "2022-blake3-aes-256-gcm", testKey('s'),
		[]map[string]string{{"email": "bootstrap", "password": testKey('b')}})
	config = withInbound(config, secondInboundTag, secondAddress, "2022-blake3-aes-256-gcm", testKey('S'),
		[]map[string]string{{"email": secondBootstrap, "password": testKey('B')}})
	config = withInbound(config, thirdInboundTag, thirdAddress, "2022-blake3-aes-256-gcm", testKey('T'),
		[]map[string]string{{"email": "bootstrap", "password": testKey('C')}})
	runtime := launchRuntime(t, apiAddress, config)
	runtime.inbound, runtime.inbound2, runtime.inbound3 = inboundAddress, secondAddress, thirdAddress
	runtime.serverKey, runtime.serverKey2, runtime.serverKey3 = testKey('s'), testKey('S'), testKey('T')
	return runtime
}

// startCustomRuntime 只启动主 inbound `managed`，clients 由调用方指定（用于运行期负例，如空 clients 的单用户模式）。
func startCustomRuntime(t *testing.T, clients []map[string]string) *liveRuntime {
	t.Helper()
	apiAddress, inboundAddress := freeAddress(t), freeAddress(t)
	config := runtimeConfig(apiAddress, inboundAddress, "2022-blake3-aes-256-gcm", testKey('s'), clients)
	runtime := launchRuntime(t, apiAddress, config)
	runtime.inbound, runtime.serverKey = inboundAddress, testKey('s')
	return runtime
}

// launchRuntime 写入配置、启动进程并等待 API 就绪；失败时输出脱敏诊断。
func launchRuntime(t *testing.T, apiAddress string, config map[string]any) *liveRuntime {
	t.Helper()
	bin := contractBinary(t)
	path := writeRuntimeConfig(t, config)
	ctx, cancel := context.WithCancel(context.Background())
	command := exec.CommandContext(ctx, bin, "run", "-config", path)
	output := &bytes.Buffer{}
	command.Stdout, command.Stderr = output, output
	if err := command.Start(); err != nil {
		cancel()
		t.Fatalf("start Xray runtime: %v", err)
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
			t.Fatalf("Xray API did not become ready: %v\n%s", err, sanitize(output.Bytes()))
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

// restart 停止并用相同配置重新启动 Xray 进程，模拟运行时重启（动态用户丢失、boot epoch 变化）。
func (r *liveRuntime) restart(t *testing.T, bin string) {
	t.Helper()
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
