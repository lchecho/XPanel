# Validation Report: 每用户专属入站

**Feature**: `002-per-user-inbound` | **Branch**: `feat/20260904-xpanel/main` | **Date**: 2026-09-07

本报告记录 T073（本地门禁）、T074（发布门禁）与 T075（自动化证据）的状态。
**当前状态：自动化证据与真实 Xray v26.3.27 的 10 项兼容性门禁全部通过；
SC-001、SC-002、SC-010 的人工验收（T076）待执行。**

真实 Xray 二进制来源：`GOBIN=<dir> go install github.com/xtls/xray-core/main@v1.260327.0`
（本机 darwin/arm64，`xray version` = `Xray 26.3.27`，`go version -m` 报告
`mod github.com/xtls/xray-core v1.260327.0`）。

## 1. 自动化证据（fake Xray，`go test ./...`）

19 个包中 12 个有测试，全部通过。

| 成功标准 | 证据 | 结果 |
|---|---|---|
| SC-003 端口与用量展示 ≤10s | `tests/e2e/success_criteria_test.go`、`tests/e2e/dashboard_test.go`：采集提交后下一次 fragment 即含端口、监听状态与用量 | 通过 |
| SC-004 越界后端口停止监听 | `tests/integration/quota_inbound_test.go`、`tests/e2e/quota_test.go`：越界在同一采集 + 同步轮次内移除整条入站，端口停止监听 | 通过 |
| SC-005 新周期以原端口恢复、误恢复 0 | 同上：恢复复用原端口分配（`dedicated_inbounds` 仍是同一行），手动禁用与已删除用户不被恢复 | 通过 |
| SC-006 重启后 60s 内按原端口一致 | `tests/e2e/recovery_test.go`：重启窗口内全部端口不监听、界面给出说明，一次对账 + 一次同步内按原端口收敛，用时远小于 60s 上界 | 通过 |
| SC-007 负流量 / 重复计量 = 0 | `tests/integration/failure_matrix_test.go`（7 变更 × 8 故障 = 56 格，每格断言精确记入字节数与恰好一个成功操作）、`internal/application/batch_consistency_test.go` | 通过 |
| SC-008 审计覆盖、泄露 0 | `tests/integration/secrets_leak_test.go`、`tests/integration/inbound_drift_test.go`；入站创建/移除、端口分配/释放均留痕，摘要含端口与入站标签但不含密钥 | 通过 |
| SC-012 单用户操作不影响他人 | `tests/integration/isolation_test.go`：对一个用户跑完创建→禁用→启用→轮换→配额封禁→恢复→删除→端口再分配，另一个用户的端口、入站标签、客户端与累计计量零偏差 | 通过 |
| SC-013 重复端口分配 = 0 | `tests/integration/port_allocation_test.go`：自动分配升序且填补空洞、指定端口、已占用 409、池外 422、池耗尽 409、16 路并发创建；唯一性由迁移 00004 的部分唯一索引保证（`migrations_test.go` 直接验证该索引） | 通过 |
| SC-014 孤儿入站清理 100%、命名空间外 0 改动 | `tests/integration/inbound_drift_test.go`：命名空间内两条孤儿入站被移除并释放端口且留审计，运维自有入站与在用用户不受影响，二次对账不产生新意图 | 通过 |

宪章 v1.2.0 要求的故障矩阵在 001 的六类之外新增两列（`external_port_occupied`、`port_conflict`），
覆盖全部七类变更，56/56 收敛。

## 2. 发布门禁（T073 / T074）

| 步骤 | 命令 | 结果 |
|---|---|---|
| 格式 | `make fmt` | 通过（2026-09-07） |
| 静态检查 | `make vet` | 通过（2026-09-07） |
| 全量测试 | `make test` | 通过（2026-09-07，12 个有测试的包） |
| 竞态检测 | `make test-race` | 通过（2026-09-07，契约套件在 `-race` 下同样通过） |
| 完整门禁 | `XRAY_BIN=<v26.3.27> XPANEL_REQUIRE_CONTRACT=1 make check` | 通过（2026-09-07，`exit=0`） |

## 3. 真实 Xray 兼容性门禁（contracts/xray-adapter.md 十项）

`tests/contract/xray` 共 15 个测试全部通过。

| 门禁 | 证据 | 结果 |
|---|---|---|
| 1 版本双向一致、管理端点仅回环 | `TestPinnedRuntimeAndModule` | 通过 |
| 2 运行时创建 SS2022 入站、返回后立即监听 | `TestLiveInboundLifecycleContract`（AddInbound 返回后即可 TCP 连接） | 通过 |
| 3 `GetInboundUsersCount > 0` 且可增删用户 | `TestLiveInboundLifecycleContract`、`TestLiveUserMutationContractOnADedicatedInbound` | 通过 |
| 3b 用户级统计能力前置门禁 | `TestLiveTemplateValidationUsesADisposableProbe`（开启 policy 时四项能力全部证实）、`TestLiveTemplateValidationRejectsNodesWithoutUserTrafficStats`（未配置 policy 时判为不兼容并给出中文原因，探针照常清理） | 通过 |
| 4 空 `Users` 在构建期被拒 | `TestKnownSS2022ConfigurationFailuresAndEmptyClientRejection`（请求不到达 Xray） | 通过 |
| 5 真实 TCP/UDP 流量与两方向计数 | `TestLiveTrafficCountersAndInboundRemovalSemantics`（第二个 xray 进程扮演客户端，64KiB TCP 回显 + UDP 回显） | 通过 |
| 6 移除后新握手被拒、端口释放、既有连接可继续 | 同上 | 通过 |
| 7 重启清空并按原端口重建 | `TestLiveRestartDropsPanelInboundsAndRebuildsOnTheSamePorts`、`TestLiveAppRestartReconcilesActiveUsersOnly`（应用层按库中端口只重建应监听用户） | 通过 |
| 8 端口被外部占用时的补偿与收敛 | `TestLiveCreateInboundOnOccupiedPortCompensatesAndConverges`、`TestLiveCreateInboundBindFailureIsRecoverable`、`TestLiveUncertainCreateInboundConvergesThroughReadAfterWrite` | 通过 |
| 9 两个用户的端口相互隔离 | `TestLiveInboundsAreIsolatedFromEachOther`、`TestLiveAppPerUserInboundsAreIsolated`（禁用/启用/轮换/删除全过程中另一端口持续监听） | 通过 |
| 10 命名空间之外的入站保持不变 | 同上 + `TestLiveUserMutationContractOnADedicatedInbound`（AddUser/RemoveUser/RemoveInbound 三个入口都拒绝越界） | 通过 |

进程清理（T077）：契约套件的清理闭包引用 `liveRuntime` 的当前字段而非启动时的局部变量
（重启会替换 `cancel`/`command`）；整套运行结束后连续三次 `pgrep` 均为 0 个残留 Xray 进程。

## 4. 迁移与文档

| 项目 | 证据 | 结果 |
|---|---|---|
| 迁移 00004 up/down/up 往返 | `internal/persistence/sqlite/migrations_test.go`：回滚恢复 001 结构、再次 up 恢复 002 结构、`pragma_foreign_key_check` 为 0 | 通过 |
| 端口唯一性索引 | 同上 `TestDedicatedInboundPortUniqueness`：重复 `(listen_address, port)` 被拒，`released_at` 非空后同端口可再分配 | 通过 |
| 运维文档 | `docs/operations.md` §6（入站与端口运维预期）、§6.1（端口池扩容流程）、§7（00004 破坏性说明与备份演练顺序） | 已更新 |
| 上手文档 | `README.md`：入站模板 → 兼容性验证 → 创建用户 → 交付连接信息的最小路径 | 已更新 |

## 5. 收敛轮次

### Phase 9（2026-09-07）

| 任务 | 证据 | 结果 |
|---|---|---|
| T078 轮换状态机 | 轮换改为单阶段意图（先读实际状态再决策）。**该结论已被 Phase 10 的 T084 推翻并替换**：单阶段仍会在两次 RPC 之间留下空客户端入站，见 research.md C-006 | 已被 T084 取代 |
| T079 能力门禁 | `ValidateTemplate` 不再忽略探针移除结果，新增 InboundRemovable 硬门禁；不兼容原因经 `views.CompatibilityReasonSentence` 中文化。**其中「用户级统计无法在校验期证实」的判断已被 Phase 10 的 T085 推翻**（research.md C-007），统计现在也是硬门禁；InboundRemovable 与中文化部分继续有效 | 部分被 T085 取代 |
| T080 更换端口 | 迁移 00005（新增 port_change 原因，补回 00004 丢失的 idempotency_key 唯一与 UNIQUE(allocation_id, desired_revision)）；`Store.ChangeInboundPort` 单事务完成校验、改端口、写意图与审计；`tests/integration/port_change_test.go` 覆盖成功、三类拒绝无部分状态、重复提交只产生一条意图、并发抢同一端口只有一个成功；handler 测试覆盖 422/409/成功；故障矩阵新增 port_change 列（8×8=64 格全绿） | 通过 |
| T081 无归属漂移 | 迁移 00006（template_id 可空 + COALESCE 归组的唯一索引）；协调器优先挂模板、无模板时以「无归属」持久化；LeaseDueDriftRemoval 改 LEFT JOIN 且兼容性只约束 identity 类；新增 OrphanStaleDriftRemovals 重排队。`tests/integration/inbound_drift_test.go` 覆盖「零模板」「全归档」两种场景与永久失败后的重排队 | 通过 |
| T087 轮换故障契约 | `internal/worker/synchronizer_rotation_test.go` 覆盖 5 个边界：四次变更 RPC 之前各一次崩溃，外加「四次 RPC 全部成功、ConfirmSync 之前崩溃」（用 store 包装器注入），每个边界都断言确实被命中。`tests/contract/xray/rotation_app_test.go` 用真实 service + synchronizer 驱动真实 Xray，在四次变更 RPC **成功之后**的每个边界崩溃、回收租约、重放：每次 RPC 后校验端口在听且入站有客户端，恢复后以真实 SS2022 握手证明新凭证可用、旧凭证被拒，最终只剩原不可变统计身份、过渡身份不进连接信息、上行计数不回退 | 通过 |
| T083 契约稳定性 | 端口池基址改到临时端口范围之下并逐个绑定校验，采集断言前用 `convergeAll` 等待全部分配收敛；`XRAY_BIN=<v26.3.27> XPANEL_REQUIRE_CONTRACT=1 go test ./tests/contract/xray -count=1` 连续 5 次全绿，随后整条 `make check` `exit=0`，运行后连续三次 `pgrep` 均无残留 Xray 进程 | 通过 |

### Phase 11（2026-09-07）

| 任务 | 证据 | 结果 |
|---|---|---|
| T086 未知客户端对账 | 协调器把专属入站里除期望身份之外的**任何**客户端都按漂移处理（不再看 `xpanel-` 前缀），轮换过渡身份仅在意图开放期间豁免；适配器与 fake 的最后客户端守卫改为「移除后是否还留有受管客户端」，外部身份不算「还有人」。`tests/integration/unknown_identity_test.go`（两类未知身份被清理、只剩未知身份时期望身份被恢复、过渡身份豁免与到期清理）、`tests/contract/xray/inbound_test.go`（真实节点上注入外部身份后可安全清理，且面板无法把入站变成「只剩未知身份」）、`internal/adapter/xray/handler_test.go` | 通过 |
| T087 轮换故障契约 | 崩溃边界补齐到 5 个（含四次 RPC 全部成功、ConfirmSync 之前）；`tests/contract/xray/rotation_app_test.go` 用真实 service + synchronizer 驱动真实 Xray，在四次变更 RPC 成功后的每个边界崩溃并重放，以真实 SS2022 握手证明新凭证可用、旧凭证被拒 | 通过 |
| T088 探针方法与网络 | 探针按模板的 Method 与 Network 发送 TCP/UDP 流量，网络取值异常直接报错；每次验证使用全新探针身份并在结束时清零计数。AES-128/AES-256 × tcp/udp/tcp_udp 六组矩阵各断言首次通过、重复通过、探针不残留；三组缺 policy 的回归确保没有组合因未发流量而误通过 | 通过 |
| T089 能力世代绑定 | 迁移 00007 记录 `validated_boot_epoch`；协调器每轮把纪元不符的兼容模板置回待验证并立即重跑门禁（计入 `Revalidated`）；`CreateUser` 只接受对当前世代验证通过的模板。`tests/integration/capability_generation_test.go` 与 `tests/contract/xray/template_test.go`（同一管理端点重启到缺少 policy 的配置后旧缓存立即失效、新建被拒，恢复后才允许创建） | 通过 |
| T091 依赖决策 | 统计探针直接引用 `sing-shadowsocks`/`sing`：这两个模块本就在 xray-core 依赖图内，改动只是把 `go.mod` 的 `// indirect` 提升为直接依赖，**`go.sum` 零变化**、`go mod tidy -diff` 无差异、`CGO_ENABLED=0 go build ./cmd/xpanel` 通过（单二进制内容不变）。已在 `plan.md` 的 Primary Dependencies、Constitution Check 与 Complexity Tracking 记录必要性、安全边界与版本固定要求 | 通过 |

### Phase 10（2026-09-07）

| 任务 | 证据 | 结果 |
|---|---|---|
| T084 轮换过渡客户端 | 轮换改为四步过渡（加过渡 → 删旧 → 加新 → 删过渡），全程客户端数为 1 或 2；适配器新增「不移除最后一个受管客户端」守卫（`last_managed_client`）作为与租约无关的最终防线。证据在 T087 补齐后完整（见下） | 通过 |
| T085 统计能力前置门禁 | `ValidateTemplate` 在探针入站上用进程内 SS2022 客户端产生一次认证回显流量并回读两个方向的计数器；漏配 policy 的节点在创建任何用户之前即判为不兼容，中文原因直接指向要改的配置键。实测证据见 research.md C-007（开启 policy 时两个计数器均为 23 字节，未配置时始终 NotFound） | 通过 |

实测更正记录见 `research.md` C-004（移除唯一客户端不会让入站退化为服务端密钥可直连）、
C-006（轮换必须用过渡客户端，「未持久化的空窗」不算故障安全，修正 T078 的判断）与
C-007（产生一次认证流量后用户级统计能力可以被证实，修正 C-005 的过窄结论）。

## 6. 人工验收（T076 / T082，待执行）

按 `quickstart.md` §3 在具备 Xray v26.3.27 的环境执行，并把结果填入下表：

| 项目 | 方法 | 结果 |
|---|---|---|
| SC-001 5 分钟内登记模板并创建首个用户 | 从首次登录开始计时至复制出含专属端口的 `ss://` | 待执行 |
| SC-002 95% 操作 2s 内反馈 | 登录/搜索/创建/编辑/状态切换各 ≥20 次，记录 P95 | 待执行 |
| SC-010 键盘与移动宽度 100% | 纯键盘以及 360×640、390×844 视口完成登记模板/创建/编辑/禁用/删除 | 待执行（结构性规则已由 `internal/web/handlers/accessibility_test.go` 自动校验） |
| 端口池扩容与防火墙放行 | `docs/operations.md` §6.1 在真实环境演练 | 待执行 |
| 备份/恢复 | `docs/operations.md` §2–§3 在真实环境演练 | 待执行（自动化演练见 `tests/integration/backup_restore_test.go`） |

T082 与 T076 是同一件事：按 quickstart §3 完成人工验收后，把计时、P95 与成功率回填上表。

SC-009（首次使用管理员可用性研究）按 spec 定义为发布后研究，不作为实现门禁。
