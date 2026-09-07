# Validation Report: Xray 多用户管理 MVP

**Feature**: `001-xray-user-management` | **Branch**: `feat/20260904-xpanel/main` | **Date**: 2026-09-04（自动化） / 2026-09-05（真实 Xray 契约）

本报告记录 T125（自动化证据）、T126（真实 Xray 人工验收）与 T127（发布门禁）的状态。
**当前状态：自动化部分与真实 Xray v26.3.27 契约门禁 1–9 已通过；SC-001/SC-002/SC-010 的人工验收待执行。**

真实 Xray 二进制来源：`GOBIN=<dir> go install github.com/xtls/xray-core/main@v1.260327.0`（本机 darwin/arm64，`xray version` = `Xray 26.3.27`，`go version -m` 报告 `mod github.com/xtls/xray-core v1.260327.0`）。

## 1. 自动化证据（fake Xray，`go test ./...`）

| 成功标准 | 证据 | 结果 |
|---|---|---|
| SC-003 流量展示 ≤10s | `tests/e2e/success_criteria_test.go`：20 个分配采集提交后下一次 fragment 即可见；结构上界 5s 采集 + 5s 轮询 | 通过 |
| SC-004 越界 10s 内阻止新连接 / 失败 30s 内显示 | 同上：越界在同一采集 + 同步轮次内移除；Xray 不可达时详情页显示“移除待同步”与故障原因 | 通过 |
| SC-005 新周期 60s 内恢复、误恢复 0 | 同上 + `tests/e2e/quota_test.go`、`internal/worker/collector_test.go`：仅超限用户恢复，手动禁用/删除恢复数为 0 | 通过 |
| SC-006 重启后 60s 内一致 | 同上 + `tests/e2e/recovery_test.go`、`internal/worker/reconciler_test.go`：一次协调（15s 周期）+ 同步后 0 处不一致 | 通过 |
| SC-007 负流量/重复计量 = 0 | 同上 + `internal/domain/traffic_test.go`（delta 永不为负）、`tests/integration/failure_matrix_test.go`（42 个故障单元格；每格断言精确记入字节数、恰好一个成功操作、无未终结操作） | 通过 |
| SC-008 审计覆盖 100%、泄露 0 | 同上 + `tests/integration/secrets_leak_test.go`（页面/fragment/审计/日志无密码、密钥、令牌、`ss://`）；故障矩阵每格断言变更审计与同步成功审计；SC 测试以真实用户密码与连接 URI 扫描审计摘要，泄露数非零即失败 | 通过 |
| SC-011 重置后旧会话失效 | 同上 + `tests/integration/cli_test.go`（真实二进制 `admin reset-password --password-stdin`） | 通过 |

宪章要求的六类故障 × 七类变更矩阵：`tests/integration/failure_matrix_test.go`，42/42 收敛。

Phase 10（2026-09-05，第二轮 converge）补充的收敛证据：

| 任务 | 证据 | 结果 |
|---|---|---|
| T143 租约 fencing | `internal/worker/synchronizer_fencing_test.go`：慢 RPC 超过租期 + 第二 worker 回收 + 并发新意图，旧 worker 不再调用/确认 Xray；过期租约只能从旧 owner 回收；漂移移除不重复执行 | 通过 |
| T144 契约字段冻结 | `tests/integration/profile_guard_test.go`：删除已提交未 Drain、漂移移除已排队、pending create 时改 tag 均 409，确认 absent 后允许且旧入站无 `xpanel-` 身份 | 通过 |
| T145 周期边界 | `tests/integration/cycle_boundary_test.go`：先切换后采集 / 先采集后切换两种顺序、reset_day 与时区变更、禁用用户、边界后重置；唯一 open 周期、恢复操作恰好一次、流量只记一次 | 通过 |
| T146 分批一致性 | `internal/application/batch_consistency_test.go`：25 分配两批之间重启整轮丢弃（无状态变化/事件/封禁），下一轮确认重启后越界全部封禁；计数缺失期间保留 boot epoch | 通过 |
| T147 认证语义 | `tests/integration/auth_consistency_test.go`、`internal/persistence/sqlite/sessions_test.go`：审计写入、会话提交与撤销故障注入下 HTTP 不返回成功，会话可用性与 succeeded/failed 审计一致，已撤销令牌不被迟到提交复活 | 通过 |

Phase 11（2026-09-05，第三轮 converge）补充的收敛证据：

| 任务 | 证据 | 结果 |
|---|---|---|
| T148 会话/审计单一协议 | `internal/web/middleware/session.go`（自有 LoadAndSave：每请求至多提交一次）、`tests/integration/auth_consistency_test.go`：中间件提交失败无部分状态、成功审计失败叠加会话撤销失败仍撤销全部会话、登录/登出协议可判定 | 通过 |
| T149 永久失败漂移移除 | `tests/integration/profile_guard_test.go`：未知身份移除永久失败 → 改 tag 被拒 → Xray 恢复重新移除 → 改 tag 成功，旧入站无遗留 `xpanel-` 身份 | 通过 |
| T150 uptime 量化抖动 | `internal/application/batch_consistency_test.go`（一秒抖动不判重启、>1s 或 uptime 下降判重启）、`tests/contract/xray/app_test.go`：固定 Xray 下 21 分配分两批三轮可提交，两批之间真实重启整轮丢弃且游标不变 | 通过（真实 Xray） |
| T151 漂移移除外部去重 | `internal/worker/synchronizer_fencing_test.go`：RPC 成功后崩溃、租约到期回收重放，外部移除/完成/审计各恰好一次；所有构造路径租约 ≥ 3×RPC 超时 | 通过 |

Phase 12（2026-09-06，第四轮 converge）补充的收敛证据：

| 任务 | 证据 | 结果 |
|---|---|---|
| T152 会话/审计原子事务 | `internal/persistence/sqlite/sessions.go`（scs CtxStore，ctx 携带写事务时同一事务写入）、`internal/application/auth_service.go`（EstablishSession/RevokeSession）、`tests/integration/auth_consistency_test.go`：InsertSession/RevokeSession/AppendAudit 语句分别失败，登录失败无 live session 与 succeeded 审计，登出失败原会话可用且只有 failed 审计，重试后恰好一个 succeeded | 通过 |
| T153 辅助会话刷新语义 | `internal/web/middleware/session.go`：既有会话的 idle/flash 刷新失败保留原会话与真实业务响应；`TestMiddlewareSessionCommitFailureLeavesNoPartialState`：设置只提交一次、303、无新 cookie、同 request ID 重放幂等 | 通过 |
| T154 永久失败意图收敛 | `Store.StaleDriftRemovals` + 协调器重新排队 + synchronizer 确认 absent 审计；`tests/integration/profile_guard_test.go`：移除永久失败 → Xray 重启 → reconcile/drain → 改 inbound tag 成功，旧入站无身份、失败意图已被成功确认 | 通过 |

## 2. 发布门禁（T127）

| 步骤 | 命令 | 结果 |
|---|---|---|
| 格式 | `make fmt` | 通过（2026-09-04；2026-09-05 复跑通过） |
| 静态检查 | `make vet` | 通过（2026-09-04；2026-09-05 复跑通过） |
| 全量测试 | `make test` | 通过（2026-09-04；2026-09-05 复跑通过，12 个包，含 42 格故障矩阵、CLI 二进制测试与真实 Xray 契约套件） |
| 竞态检测 | `make test-race` | 通过（2026-09-04；2026-09-05 复跑通过，契约套件在 `-race` 下同样通过） |
| 固定 Xray 契约套件 | `XRAY_BIN=<path> XPANEL_REQUIRE_CONTRACT=1 make check` | 通过（2026-09-05，`XRAY_BIN` 指向从 `github.com/xtls/xray-core@v1.260327.0` 构建的 `Xray 26.3.27`；`tests/contract/xray` 9 个测试全部通过，`exit=0`；Phase 10、Phase 11、Phase 12 完成后各复跑整条门禁均 `exit=0`） |

## 3. 真实 Xray 人工验收（T126，待执行）

按 `quickstart.md` §2–§9 在具备 Xray v26.3.27 的环境执行，并把结果填入下表：

| 项目 | 方法 | 结果 |
|---|---|---|
| SC-001 5 分钟内创建首个用户并复制连接信息 | 从首次登录开始计时至复制出 `ss://` | 待执行 |
| SC-002 95% 操作 2s 内反馈 | 登录/搜索/创建/编辑/状态切换各 ≥20 次，记录 P95（计时自请求发出至 HTML 完整返回） | 待执行 |
| SC-010 键盘与移动宽度 100% | 纯键盘以及 360×640、390×844 视口各完成登录/创建/编辑/禁用/删除 | 待执行（结构性规则已由 `internal/web/handlers/accessibility_test.go` 自动校验） |
| 契约门禁 1–9 | `tests/contract/xray/*_test.go` 全部通过 | 通过（2026-09-05，`XRAY_BIN` 指向上述二进制；门禁 7–9 由 `app_test.go` 以真实 SQLite + 真实客户端驱动应用 reconciler/synchronizer 验证） |
| 真实 TCP/UDP 流量与两方向计数 | `tests/contract/xray/traffic_test.go` | 通过（2026-09-05：真实 SS2022 AES-256 客户端经 SOCKS 完成 TCP 64KiB 回显与 UDP 回显，两方向计数器增长；移除后新握手被拒） |
| 重启与超时收敛 | `tests/contract/xray/recovery_test.go`、`app_test.go` | 通过（2026-09-05：重启后 boot epoch 前进、动态用户丢失，应用协调仅恢复 active 用户；200µs 超时的不确定新增经持久操作读后写收敛，Xray 中恰好一份身份） |
| 备份/恢复 | `docs/operations.md` §2–§3 在真实环境演练 | 待执行（自动化演练见 `tests/integration/backup_restore_test.go`） |

SC-009（首次使用管理员可用性研究）按 spec 定义为发布后研究，不作为实现门禁。
