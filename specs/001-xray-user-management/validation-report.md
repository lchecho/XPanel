# Validation Report: Xray 多用户管理 MVP

**Feature**: `001-xray-user-management` | **Branch**: `feat/20260904-xpanel/main` | **Date**: 2026-09-04

本报告记录 T125（自动化证据）、T126（真实 Xray 人工验收）与 T127（发布门禁）的状态。
**当前状态：自动化部分已完成；需要真实 Xray v26.3.27 二进制的部分待执行。**

## 1. 自动化证据（fake Xray，`go test ./...`）

| 成功标准 | 证据 | 结果 |
|---|---|---|
| SC-003 流量展示 ≤10s | `tests/e2e/success_criteria_test.go`：20 个分配采集提交后下一次 fragment 即可见；结构上界 5s 采集 + 5s 轮询 | 通过 |
| SC-004 越界 10s 内阻止新连接 / 失败 30s 内显示 | 同上：越界在同一采集 + 同步轮次内移除；Xray 不可达时详情页显示“移除待同步”与故障原因 | 通过 |
| SC-005 新周期 60s 内恢复、误恢复 0 | 同上 + `tests/e2e/quota_test.go`、`internal/worker/collector_test.go`：仅超限用户恢复，手动禁用/删除恢复数为 0 | 通过 |
| SC-006 重启后 60s 内一致 | 同上 + `tests/e2e/recovery_test.go`、`internal/worker/reconciler_test.go`：一次协调（15s 周期）+ 同步后 0 处不一致 | 通过 |
| SC-007 负流量/重复计量 = 0 | 同上 + `internal/domain/traffic_test.go`（delta 永不为负）、`tests/integration/failure_matrix_test.go`（42 个故障单元格） | 通过 |
| SC-008 审计覆盖 100%、泄露 0 | 同上 + `tests/integration/secrets_leak_test.go`（页面/fragment/审计/日志无密码、密钥、令牌、`ss://`） | 通过 |
| SC-011 重置后旧会话失效 | 同上 + `tests/integration/cli_test.go`（真实二进制 `admin reset-password --password-stdin`） | 通过 |

宪章要求的六类故障 × 七类变更矩阵：`tests/integration/failure_matrix_test.go`，42/42 收敛。

## 2. 发布门禁（T127）

| 步骤 | 命令 | 结果 |
|---|---|---|
| 格式 | `make fmt` | 通过（2026-09-04） |
| 静态检查 | `make vet` | 通过（2026-09-04） |
| 全量测试 | `make test` | 通过（2026-09-04，11 个包，含 42 格故障矩阵与 CLI 二进制测试） |
| 竞态检测 | `make test-race` | 通过（2026-09-04） |
| 固定 Xray 契约套件 | `XRAY_BIN=<path> XPANEL_REQUIRE_CONTRACT=1 make contract` | **待执行：本机无 xray v26.3.27 二进制** |

## 3. 真实 Xray 人工验收（T126，待执行）

按 `quickstart.md` §2–§9 在具备 Xray v26.3.27 的环境执行，并把结果填入下表：

| 项目 | 方法 | 结果 |
|---|---|---|
| SC-001 5 分钟内创建首个用户并复制连接信息 | 从首次登录开始计时至复制出 `ss://` | 待执行 |
| SC-002 95% 操作 2s 内反馈 | 登录/搜索/创建/编辑/状态切换各 ≥20 次，记录 P95（计时自请求发出至 HTML 完整返回） | 待执行 |
| SC-010 键盘与移动宽度 100% | 纯键盘以及 360×640、390×844 视口各完成登录/创建/编辑/禁用/删除 | 待执行（结构性规则已由 `internal/web/handlers/accessibility_test.go` 自动校验） |
| 契约门禁 1–9 | `tests/contract/xray/*_test.go` 全部通过 | 待执行 |
| 真实 TCP/UDP 流量与两方向计数 | `tests/contract/xray/traffic_test.go` | 待执行 |
| 重启与超时收敛 | `tests/contract/xray/recovery_test.go` | 待执行 |
| 备份/恢复 | `docs/operations.md` §2–§3 在真实环境演练 | 待执行（自动化演练见 `tests/integration/backup_restore_test.go`） |

SC-009（首次使用管理员可用性研究）按 spec 定义为发布后研究，不作为实现门禁。
