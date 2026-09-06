---

description: "Xray 多用户管理 MVP 的实现任务清单"
---

# Tasks: Xray 多用户管理 MVP

**Input**: Design documents from `/specs/001-xray-user-management/`

**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/（http.md、cli.md、config.md、xray-adapter.md）, quickstart.md

**Tests**: 项目宪章「开发流程与质量门禁」强制要求单元、集成、Handler、Xray 契约与端到端测试，因此本清单包含测试任务；它们作为各故事的必交付物，与实现任务同阶段列出，不要求严格 TDD 先行。

**Organization**: 任务按用户故事分组。US1 是后续所有故事的基础层（用户、凭证、同步 worker、Xray Adapter 变更操作），US2/US3 在 US1 之后可并行，US4/US5 依赖 US2 的流量数据。

**宪章标注约定**: 任务描述中的 【迁移】【Xray 契约】【重试/协调】【可观测性】 标签对应宪章要求显式标注的工作项。

## Format: `[ID] [P?] [Story] Description`

- **[P]**: 可并行（不同文件、不依赖未完成任务）
- **[Story]**: 所属用户故事（US1–US5）
- 每个任务都包含精确文件路径

## Path Conventions

单 Go module 的模块化单体（见 plan.md「Project Structure」）：

- 入口：`cmd/xpanel/`
- 业务与适配：`internal/{config,domain,application,ports,persistence/sqlite,adapter/xray,security,logging,worker,web}`
- 测试：`tests/{contract/xray,integration,e2e}` 与各包 `*_test.go`
- 部署样例：`deploy/`

> 相对 plan.md 的两处小补充：`internal/adapter/xray/fake/` 存放手写 fake Adapter；`internal/logging/` 存放 slog JSON handler 与集中脱敏。

---

## Phase 1: Setup（项目初始化）

**Purpose**: 建立可编译的 Go 工程骨架、固定依赖版本与本地嵌入的前端资源

- [X] T001 初始化 `go.mod`（module `xpanel`，`go 1.26.8`），固定 `github.com/xtls/xray-core@v1.260327.0`、`modernc.org/sqlite@v1.58.0`、`github.com/pressly/goose/v3@v3.28.0`、`golang.org/x/crypto@v0.56.0`、`github.com/alexedwards/scs/v2@v2.9.0`、`github.com/gorilla/csrf@v1.7.3`，生成 `go.sum`
- [X] T002 按 plan.md 创建目录骨架并放置 `doc.go` 包说明：`cmd/xpanel/`、`internal/{config,domain,application,ports,persistence/sqlite/migrations,adapter/xray/fake,security,logging,worker,web/{middleware,handlers,views,templates/{layouts,pages,fragments},static}}`、`tests/{contract/xray,integration,e2e}`、`deploy/`
- [X] T003 [P] 下载 HTMX `2.0.10` 到 `internal/web/static/htmx-2.0.10.min.js`，并在 `internal/web/static/THIRD_PARTY.md` 记录来源 URL、版本与 SHA-256
- [X] T004 [P] 创建 `Makefile`：`fmt`（gofmt 检查）、`vet`、`test`（`go test ./...`）、`test-race`、`build`（`CGO_ENABLED=0 go build -trimpath -o bin/xpanel ./cmd/xpanel`）、`contract`（设置 `XRAY_BIN` 后运行 `tests/contract/xray`；`XPANEL_REQUIRE_CONTRACT=1` 时缺少 `XRAY_BIN` 直接失败）、`check`（合并门禁：依次执行 `fmt`、`vet`、`test`、`test-race`、`contract`，并以 `XPANEL_REQUIRE_CONTRACT=1` 运行）
- [X] T005 [P] 按 contracts/config.md 与 quickstart.md §2 编写 `deploy/xpanel.example.json`、`deploy/xray-v26.3.27.example.json`（含 api/stats/policy 与非空 `clients` bootstrap 用户）和 `deploy/xpanel.service`（systemd，专用账号、`0700` 数据目录）

---

## Phase 2: Foundational（阻塞性基础设施）

**Purpose**: 配置、安全原语、SQLite 与迁移、认证会话、Web 骨架、Xray 客户端基础和测试支撑；所有用户故事都依赖本阶段

**⚠️ CRITICAL**: 本阶段完成前不得开始任何用户故事

### 配置与安全原语

- [X] T006 实现 `internal/config/config.go`：严格 JSON 解析（`DisallowUnknownFields`）、Go duration 正值校验、`server.listen`/`xray.api_endpoint` 单 socket 地址与回环校验、`public_url` 绝对 HTTPS 校验、`insecure_development` 仅回环允许并产生启动警告、`database_path` 本地目录权限与文件系统类型校验（拒绝 NFS/SMB/网络 FUSE）、`traffic_interval` 默认 5s 且 ≤60s（>5s 警告）、`reconcile_interval` ≤30s、`initial.quota_timezone` IANA 校验
- [X] T007 [P] 编写 `internal/config/config_test.go` 表驱动测试覆盖每条校验规则与错误信息不含敏感值
- [X] T008 [P] 实现 `internal/security/password.go`：Argon2id（64 MiB、3 iterations、4 lanes、16 字节盐、32 字节输出）PHC 编码/校验、常量时间比较、密码策略（12–256 字节、不得等于用户名，见 config.md §Administrator Credential Policy）与用户名 NFKC 规范化、大小写不敏感
- [X] T009 [P] 实现 `internal/security/secrets.go`：读取 root key 文件（标准 Base64 的 32 字节 + 换行、`0600` 权限校验）、HKDF-SHA256 派生 `xpanel-field-aead-v1` 与 `xpanel-csrf-v1` 子密钥、XChaCha20-Poly1305 字段加解密（24 字节随机 nonce，AAD 绑定实体/行 ID/字段名/版本）、`panel_settings.key_verifier` 校验值生成与启动解密校验
- [X] T010 [P] 实现 `internal/security/tokens.go`：`crypto/rand` 令牌、SS2022 用户密钥生成（AES-256 为 32 字节、AES-128 为 16 字节，标准 Base64）与校验、统计身份 `xpanel-<allocation-uuid>` 生成与校验（非空、不含 `>>>`）、默认脱敏的 `RedactedString` 类型
- [X] T011 [P] 编写 `internal/security/password_test.go`、`secrets_test.go`、`tokens_test.go`（含 AEAD AAD 不匹配失败、错误密钥长度拒绝、脱敏类型 `%v`/`%s` 输出为占位符）
- [X] T012 [P] 实现 `internal/logging/logging.go` 与 `internal/logging/redact.go` 【可观测性】：`log/slog` JSONHandler、固定字段（component、request_id、operation_id、allocation_id、node_id、target_state、result、duration_ms、error_kind）、集中脱敏（密码、密钥、token、完整连接 URI）

### 端口与领域基础

- [X] T013 实现 `internal/ports/clock.go`（可注入时钟）、`internal/ports/xray.go`（按 contracts/xray-adapter.md 定义 `Adapter` 接口、`InstanceTarget`/`RuntimeProfile`/`RemoteUser`/`AddUserCommand`/`RemoveUserCommand`/`CounterSnapshot`/`ProfileCapabilities`/`MutationReceipt` 与稳定错误类型 `AdapterError{Kind, Operation, Retryable, SafeSummary}`）、`internal/ports/store.go`（`Store`、`WriteTx`/`ReadTx` 事务边界与基础仓储接口）
- [X] T014 [P] 实现 `internal/domain/identity.go`：UUID 生成、`Revision`、`ActorType`、`DomainCommand`（id、fingerprint、state、result_reference）、`AuditEvent`、稳定动作名常量（`login`、`logout`、`administrator_initialized`、`password_reset`、`user_created`、`user_updated`、`user_enabled`、`user_disabled`、`credential_rotated`、`user_deleted`、`quota_exceeded`、`traffic_reset`、`cycle_restored`、`sync_failed` 等）
- [X] T015 [P] 实现 `internal/domain/errors.go`：`ValidationError`（字段级）、`ConflictError`（版本/请求 ID）、`NotFoundError`、`InvalidStateError`，均不携带敏感值

### SQLite 持久化

- [X] T016 编写 `internal/persistence/sqlite/migrations/00001_initial.sql` 【迁移】：按 data-model.md 创建全部 STRICT 表（panel_settings、administrators、admin_sessions、managed_xray_instances、access_profiles、xray_user_identities、managed_users、access_allocations、access_credentials、quota_policies、quota_cycles、allocation_traffic_totals、traffic_cursors、daily_traffic_aggregates、traffic_continuity_events、quota_reset_events、domain_commands、synchronization_operations、audit_events），含 CHECK 枚举/布尔、外键、`managed_users` 未删除名称部分唯一索引、`access_allocations.user_id` 唯一、`quota_cycles` 单 open 周期部分唯一索引、`UNIQUE(instance_id, statistics_id)`
- [X] T017 实现 `internal/persistence/sqlite/db.go`：modernc 打开数据库、设置并回读验证 `journal_mode=WAL`、`foreign_keys=ON`、`busy_timeout=5000`、`synchronous=FULL`，写句柄 `MaxOpenConns(1)`、读句柄小连接池，数据库/WAL/SHM 文件最小权限、`<database_path>.lock` 排他 flock 保证单写入实例（锁被占用则退出码 3）
- [X] T018 实现 `internal/persistence/sqlite/migrations.go` 【迁移】：goose 嵌入式顺序迁移，在 HTTP 监听与 worker 之前执行；失败返回错误使 `/readyz` 不就绪
- [X] T019 实现 `internal/persistence/sqlite/store.go`：`Store` 基础（`WithWriteTx`/`WithReadTx`）、`DomainCommand` 幂等记录（同 id 同 fingerprint 返回原结果；同 id 不同 fingerprint 返回冲突）、`AuditEvent` append-only 写入、`PanelSettings` 读取/首次创建/按 revision 更新、`Administrator` 读取/创建/更新密码与 `password_version`、`ManagedXrayInstance` 单例读写、`EnsureSingletons`（`panel_settings` 不存在时按 `initial.quota_timezone` 创建；`managed_xray_instances` 按配置 upsert `api_endpoint`/`supported_runtime_version`，重复执行幂等）
- [X] T020 实现 `internal/persistence/sqlite/sessions.go`：基于 `admin_sessions` 的 `scs.Store`（token digest、`password_version` 校验、idle/absolute 过期、`Revoke`、`RevokeAll`）
- [X] T021 [P] 编写 `internal/persistence/sqlite/db_test.go` 与 `migrations_test.go`：PRAGMA 回读、迁移幂等重跑、外键约束生效、busy timeout 下并发读写、单 open 周期索引
- [X] T022 [P] 编写 `internal/persistence/sqlite/store_test.go` 与 `sessions_test.go`：命令幂等/冲突、审计仅追加、会话过期与撤销、`EnsureSingletons` 重复执行幂等且不覆盖已被管理员修改的时区

### 认证应用服务

- [X] T023 实现 `internal/application/auth_service.go`：`InitializeAdministrator`（仅当不存在，同事务写 `administrator_initialized` 审计）、`Login`（用户名规范化、Argon2id 校验、非枚举错误、同用户名+来源地址 5 次/15 分钟与全局 20 次/15 分钟限流，忽略 `X-Forwarded-For`）、`Logout`、会话校验（`password_version` 一致）、登录/登出审计
- [X] T024 [P] 编写 `internal/application/auth_service_test.go`：初始化只允许一次、错误密码非枚举、限流触发与恢复、密码版本不一致会话失效

### Web 骨架与安全中间件

- [X] T025 实现 `internal/web/server.go` 与 `internal/web/routes.go`：`net/http.ServeMux` 方法/路径模式、`go:embed` 模板与静态资源、来自配置的 `ReadHeaderTimeout`/请求超时/`shutdown_timeout`、`GET /healthz`（仅进程存活）、`GET /readyz`（配置校验与迁移完成后才 200）、优雅关闭钩子
- [X] T026 [P] 实现 `internal/web/middleware/security_headers.go`：认证页面 `Cache-Control: no-store`、`Referrer-Policy: no-referrer`、`X-Content-Type-Options: nosniff`、CSP `default-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'`
- [X] T027 [P] 实现 `internal/web/middleware/session.go`：scs `LoadAndSave`、`__Host-xpanel_session` Cookie（`Secure`/`HttpOnly`/`SameSite=Strict`/`Path=/`，`insecure_development` 时允许非 Secure）、`RequireAuth`（未认证 303 到 `/login`，fragment 请求返回 403）
- [X] T028 [P] 实现 `internal/web/middleware/csrf.go`：gorilla/csrf 使用 HKDF `xpanel-csrf-v1` 子密钥 + Go `http.CrossOriginProtection`；CSRF 失败返回 403 且不进入业务写路径
- [X] T029 [P] 实现 `internal/web/handlers/forms.go`：解析 `application/x-www-form-urlencoded`、提取 `_csrf`/`_request_id`/`_version`、`_request_id` 绑定 session+action+target 的 fingerprint 计算、通用 422 字段错误渲染辅助、409 冲突页回填非敏感输入并附当前值与新 `_version`
- [X] T030 实现 `internal/web/handlers/errors.go` 与 `internal/web/templates/pages/error.html`：400/403/404/409/422/429/500/503 不泄露信息的错误页，500 仅展示安全错误 ID，429 带有界 `Retry-After`
- [X] T031 编写 `internal/web/templates/layouts/base.html`、`internal/web/templates/pages/login.html` 与 `internal/web/views/flash.go`：基础布局（导航链接、`aria-live="polite"` 消息区、可聚焦错误摘要、`aria-describedby` 字段错误）、登录表单、PRG flash 消息（不含敏感值）
- [X] T032 实现 `internal/web/handlers/auth.go`：`GET /login`（已登录重定向 `/`）、`POST /login`（成功轮换 token 并 303 到 `/`；失败 401 非枚举文案；限流 429）、`POST /logout`（撤销会话，303 到 `/login`）
- [X] T033 [P] 编写 `internal/web/static/app.css`：基础样式、可见焦点样式、状态以文字而非仅颜色表达、常见移动宽度下关键操作不依赖横向滚动
- [X] T034 实现 `cmd/xpanel/main.go`：子命令 `serve`/`admin init`；`admin init` 无回显 TTY 两次输入或 `--password-stdin`、已存在管理员时拒绝并提示 `reset-password`；退出码 0/2/3/4；`serve` 启动顺序：配置 → 数据库与 PRAGMA → 迁移 → `EnsureSingletons`（panel_settings 含 key_verifier、managed_xray_instances）→ 主密钥校验值解密（失败则不就绪）→ Store/security/服务/Adapter/路由 → worker → readiness；slog 初始化

### Xray 客户端基础与测试支撑

- [X] T035 实现 `internal/adapter/xray/client.go` 与 `internal/adapter/xray/errors.go` 【Xray 契约】：仅在证明目标为回环后建立明文 gRPC 连接、连接复用、每次调用显式 deadline（`rpc_timeout`）、gRPC status → 稳定错误类型映射（`invalid_argument`、`unsupported_protocol`、`incompatible_profile`、`instance_unavailable`、`deadline_exceeded`、`profile_not_found`、`user_already_exists`、`user_not_found`、`stats_not_found`、`version_mismatch`、`upstream_rejected`、`internal`）、`retryable` 判定、原始错误脱敏
- [X] T036 [P] 实现 `internal/adapter/xray/fake/fake.go`：内存 fake `ports.Adapter`，支持可编程失败/超时/“已生效但返回超时”的不确定结果、用户集合、上下行计数器、模拟重启（清空动态用户并更换 boot epoch）、调用记录
- [X] T037 [P] 编写 `tests/e2e/harness_test.go`：端到端测试夹具（临时 SQLite、fake Adapter、fake 时钟、`httptest` 服务、带 Cookie jar 的客户端、CSRF token 提取、表单 POST 与 303 跟随辅助、等待同步收敛辅助）
- [X] T038 [P] 编写 `tests/integration/auth_test.go`：登录/登出、idle 与 absolute 过期、CSRF 失败 403 无写入、安全响应头、限流 429、未认证访问受保护路由 303 且响应不含敏感内容
- [X] T039 [P] 编写 `internal/web/handlers/auth_test.go`：登录页渲染、错误摘要可聚焦、敏感输入不回显

**Checkpoint**: 可初始化管理员、登录登出、访问空面板；数据库、迁移、安全头与 CSRF 全部生效

---

## Phase 3: User Story 1 - 创建并分享用户访问 (Priority: P1) 🎯 MVP

**Goal**: 管理员登记并验证 SS2022 访问配置，创建具有独立凭证的受管用户，系统通过持久同步 worker 投影到 Xray，并提供仅认证后可见的连接信息；Xray 不可达时显示待同步并自动重试

**Independent Test**: 在健康的兼容访问配置上创建用户，复制连接信息并用真实/fake Xray 完成一次新连接认证；停止 Xray 后创建用户应显示待同步并在恢复后收敛；不兼容配置创建被拒绝

### 领域模型

- [X] T040 [P] [US1] 实现 `internal/domain/profile.go`：`AccessProfile`（host/port/tag/method/network 校验，method 仅 `2022-blake3-aes-128-gcm`/`2022-blake3-aes-256-gcm`）、`CompatibilityState` 状态机（unverified/compatible/incompatible/unreachable）、`XrayUserIdentity`（kind bootstrap/managed）
- [X] T041 [P] [US1] 实现 `internal/domain/user.go`：`ManagedUser`（显示名称去空白后 1–64 字符、NFKC 小写规范化、生命周期 active/deleted）、`AccessAllocation`（`admin_enabled`/`quota_state`/`projection_state` 正交状态、期望存在判定、页面状态按 data-model §AccessAllocation 派生表（deleted/disabling/disabled/quota_disabling/quota_exceeded/enabling/active）派生并附加同步错误标记）、`AccessCredential`（版本、pending/active/retired/destroyed 约束：至多一个 pending 与一个 active）
- [X] T042 [P] [US1] 实现 `internal/domain/sync.go` 【重试/协调】：`SynchronizationOperation`（reason/phase/state）、`idempotency_key` 规则、`NextBackoff(attempt, rng)` 有界指数退避（上限 `max_retry_interval`，full jitter）、`Supersede` 规则（旧 `desired_revision` 标记 superseded）、租约模型
- [X] T043 [P] [US1] 编写 `internal/domain/profile_test.go`、`user_test.go`、`sync_test.go`：状态派生优先级、凭证约束、退避序列确定性（固定随机源）、supersede

### 持久化

- [X] T044 [US1] 扩展 `internal/ports/store.go`：profile 仓储、identity 注册、user/allocation/credential 仓储、同步操作队列接口（`Enqueue`、`LeaseDue`、`ConfirmIfRevisionCurrent`、`Reschedule`、`Supersede`、`RecordError`）
- [X] T045 [US1] 实现 `internal/persistence/sqlite/store_profiles.go`：profile 创建/读取/按 revision 更新（服务端密钥 AEAD 密文与 nonce，空输入不改密钥）、归档、兼容状态与原因更新、bootstrap identity 注册（`UNIQUE(instance_id, statistics_id)`）
- [X] T046 [US1] 实现 `internal/persistence/sqlite/store_users.go`：创建用户事务（command、user、identity、allocation、credential v1 pending、policy、open cycle、cursor、sync operation、audit 一次提交）、按 ID 读取用户及其 allocation/credential/profile、凭证密文读取
- [X] T047 [US1] 实现 `internal/persistence/sqlite/store_sync.go`：领取到期操作（有限租约、单实例串行）、按 revision 条件确认、重排 `next_attempt_at` 与 `attempt_count`、supersede 旧 revision、更新 allocation `projection_state`/`synced_revision`/`synced_credential_version`/`last_sync_*`
- [X] T048 [P] [US1] 编写 `internal/persistence/sqlite/store_profiles_test.go`、`store_users_test.go`、`store_sync_test.go`：创建原子性（中途失败无残留）、未删除名称唯一、每用户一个 allocation、租约过期回收、旧 revision 确认被拒绝

### Xray Adapter（变更与探测）

- [X] T049 [US1] 实现 `internal/adapter/xray/handler.go` 【Xray 契约】：`Probe`（经 `GetSysStats` 判定可达并由 uptime 推导 boot epoch，返回 `InstanceObservation`）、`ValidateProfile`（经 `ListInbounds`/`GetInboundUsersCount`/`GetInboundUsers` 与 bootstrap 计数器 `GetStats` 判定 inbound 存在、协议/method 支持、UserManager 多用户能力、bootstrap 可见、独立上下行统计，网络失败→`unreachable`、契约不符→`incompatible`）、`ListUsers`（`GetInboundUsers`，区分 `xpanel-` 命名空间、bootstrap 与未知用户）、`AddUser`（`AlterInbound` + `AddUserOperation`，`protocol.User{Level:0, Email:statisticsID}` + `shadowsocks_2022.Account{Key}`，不记录密钥）、`RemoveUser`（`RemoveUserOperation{Email}`，not-found 映射为可收敛）
- [X] T050 [P] [US1] 编写 `internal/adapter/xray/handler_test.go`：bufconn 上的 HandlerService stub，验证 TypedMessage 类型、email/level 编码、already-exists/not-found/deadline 映射、错误字符串不含密钥
- [X] T051 [US1] 编写 `tests/contract/xray/main_test.go`、`profile_test.go`、`users_test.go` 【Xray 契约】：`XRAY_BIN` 未设置时跳过并打印醒目警告，但 `XPANEL_REQUIRE_CONTRACT=1` 时缺少二进制即失败（T075/T108 沿用同一规则）；启动固定 v26.3.27 真实进程验证门禁 1–4 与 9（版本/module 一致且回环 API、非空 `clients` 进入多用户模式、空 `clients`/错误 account/无效密钥为已知失败、add/list/remove 与重复/缺失稳定错误、多 profile 统计 ID 全局唯一且 bootstrap 排除）

### 应用服务

- [X] T052 [US1] 实现 `internal/application/profile_service.go`：`RegisterProfile`（校验后以 unverified 落库，事务外调度探测）、`UpdateProfile`（revision、空密钥保留；修改 inbound_tag/method/密钥后回到 unverified 并重新排队验证）、`Revalidate`、`RunValidation`（调用 Adapter 更新 compatibility_state/reason/last_validated_at，注册 bootstrap identity，更新实例健康与 `last_success_at`/`last_error_*`）、审计
- [X] T053 [US1] 实现 `internal/application/user_service.go`（创建路径）：`CreateUser`（名称唯一、目标 profile 必须 compatible、配额为无限或正整数、重置日 1–28、按 method 生成用户密钥并 AEAD 加密、统计身份 `xpanel-<uuid>`、按全局时区开启首个周期、按数据模型「原子事务边界 1」提交、`DomainCommand` 幂等、`user_created` 审计）；事务提交后通过进程内通知立即唤醒 synchronizer（见 T056），创建成功页在确认前显示“启用中（待同步）”
- [X] T054 [US1] 实现 `internal/application/connection_service.go`：`BuildConnectionInfo`（仅当凭证已确认；组合 `server-key:user-key` 客户端密码、host/port/method/label 与 `ss://` URI；返回脱敏类型，绝不持久化或记录）、禁用/超限时的“不活跃”标记、轮换中返回“待确认”
- [X] T055 [P] [US1] 编写 `internal/application/profile_service_test.go`、`user_service_test.go`、`connection_service_test.go`：创建成功、不兼容 profile 拒绝且原因安全、重名拒绝、零/负配额拒绝、同 `_request_id` 重放返回原结果、fake Xray 离线时创建成功并产生 pending 操作、连接信息在确认前不可用

### 同步 Worker

- [X] T056 [US1] 实现 `internal/worker/synchronizer.go` 【重试/协调】：单实例串行循环，由提交后通知立即唤醒并以 1 秒兜底轮询；短事务领取到期操作 → 事务外调用 Adapter → 独立短事务按 revision 确认；`deadline_exceeded`/未知结果先 `ListUsers` 读后写再决定确认或重放；`user_already_exists` 比对期望 revision 与凭证版本后选择成功或受控 remove/add 修复；有界退避重排；不可重试错误标记 `permanent_failed`；所属 profile 为 unverified/incompatible 时不领取操作，unreachable 时退避照常但不标记 permanent_failed，并按 data-model §Profile compatibility 触发漂移状态；更新 `projection_state` 与 `last_sync_*`；`sync_failed` 审计；slog 字段（allocation_id、operation_id、target_state、result、duration_ms）【可观测性】
- [X] T057 [P] [US1] 编写 `internal/worker/synchronizer_test.go`：成功确认、超时后读到已存在→确认、超时后不存在→重放、already-exists 修复、更高 revision supersede、退避时间表、进程重启后从租约/revision 恢复

### Web 页面

- [X] T058 [US1] 实现 `internal/web/views/format.go`、`internal/web/views/profile.go`、`internal/web/views/user.go`：字节格式化（IEC 1024 进制，≥1 MiB 两位小数四舍五入）、时间按面板时区 `YYYY-MM-DD HH:MM:SS` 格式化、状态标签文案与派生（data-model §AccessAllocation 表）、稳定错误/事件→固定中文句子映射表（http.md §Response Semantics）、`pending_sync` 标记
- [X] T059 [US1] 实现 `internal/web/templates/pages/profiles_list.html`、`profile_form.html`、`profile_detail.html` 与 `internal/web/handlers/profiles.go`：`GET /profiles`、`GET /profiles/new`、`POST /profiles`（303）、`GET /profiles/{profile_id}`、`GET /profiles/{profile_id}/edit`（密钥字段恒为空）、`POST /profiles/{profile_id}`（`_version`）、`POST /profiles/{profile_id}/revalidate`；422 字段错误、409 版本冲突、兼容状态与安全原因展示
- [X] T060 [US1] 实现 `internal/web/templates/pages/user_form.html` 与 `internal/web/handlers/users.go`（创建部分）：`GET /users/new`（仅 compatible profile 可选，无兼容 profile 时显示零状态引导）、`POST /users`（校验、幂等、303 到 `/users/{user_id}`）
- [X] T061 [US1] 实现 `internal/web/templates/pages/user_detail.html`（US1 部分）与 `GET /users/{user_id}`：显示名称、profile、派生状态、`pending_sync`/同步错误摘要、最后同步时间、连接信息页链接
- [X] T062 [US1] 实现 `internal/web/templates/pages/user_connection.html` 与 `GET /users/{user_id}/connection`：`no-store`；仅凭证确认后展示；禁用/超限附“不活跃”警示；轮换中标记不可用/待确认；无脚本可用的只读文本块供复制；密钥不作为独立字段记录；客户端密码加 print-hidden 类
- [X] T063 [P] [US1] 编写 `internal/web/handlers/profiles_test.go` 与 `users_create_test.go`：模板渲染、422/409、未认证 303 且正文不含连接信息、服务端密钥永不回显、连接页 `Cache-Control: no-store`
- [X] T064 [US1] 编写 `tests/e2e/create_user_test.go`：登录 → 登记 profile → 验证 compatible → 创建用户 → 等待 fake Xray 出现用户 → 打开连接信息；不兼容 profile 创建被拒；fake Xray 离线时创建显示待同步并在恢复后收敛；未登录访问被拒
- [X] T065 [US1] 在 `cmd/xpanel/main.go` 接线 synchronizer 与 profile 验证任务；启动时记录节点标识、Xray 端点与受支持版本【可观测性】

**Checkpoint**: US1 可独立演示：登记配置、创建用户、投影到 Xray、分享连接信息、离线待同步与恢复

---

## Phase 4: User Story 2 - 自动执行流量配额 (Priority: P1)

**Goal**: 每 5 秒非破坏性采集用户上下行流量，同事务更新游标/累计/日聚合/周期，达到配额立即封禁新连接，新周期自动恢复符合条件的用户，支持调额即时生效与手动重置本周期流量

**Independent Test**: 为用户设置小额配额并产生流量触发超限（fake Xray 或真实流量），验证新连接被拒；推进到下一周期验证自动恢复；手动禁用用户在周期切换后不恢复；调低/调高配额与手动重置的即时效果

### 领域模型

- [X] T066 [P] [US2] 实现 `internal/domain/quota.go`：`QuotaPolicy` 校验（null 无限或 >0，reset_day 1–28）、`QuotaCycle`（gross/accounted 双计数）、周期边界计算（IANA 时区、reset day、`time.Date` 在 Location 中构造以正确处理夏令时）、配额判定（accounted 总和 ≥ limit → exceeded；无限永不超限）、调额/手动重置/新周期的状态转换规则
- [X] T067 [P] [US2] 实现 `internal/domain/traffic.go`：`TrafficCursor` 方向级 delta 规则（同 epoch 且不下降→差值；已确认新 boot epoch→当前值并记 `node_restart`；未确认下降→0、方向 epoch+1、记 `counter_decrease`；缺失→无 delta 并维护 `missing_since`；首个样本→当前值并记 `baseline`；单方向缺失只影响该方向；`observed_at` 早于游标的迟到样本整体丢弃；溢出按未确认下降处理并记 `overflow`）、`uint64`→有符号范围与求和溢出检查、按样本完成时间归属日期/周期、超过一个采集间隔的 `boundary_gap` 事件
- [X] T068 [P] [US2] 编写 `internal/domain/quota_test.go` 与 `traffic_test.go`：月份天数与 reset day 28、夏令时切换、恰好等于配额判超限、无限不封禁、四条 delta 规则、溢出拒绝、永不产生负增量

### 持久化

- [X] T069 [US2] 扩展 `internal/ports/store.go`：采集批次提交、周期开启/关闭、策略更新、重置事件、活跃 allocation 与游标读取接口
- [X] T070 [US2] 实现 `internal/persistence/sqlite/store_traffic.go`：一轮采集结果的单个短事务（所有返回用户的 cursor、totals、daily upsert、open cycle gross/accounted，首次越界写 `quota_block` remove 操作与 `quota_exceeded` 审计，continuity events），采集失败时仅更新 `missing_since` 不覆盖历史
- [X] T071 [US2] 实现 `internal/persistence/sqlite/store_quota.go`：按 revision 更新策略并重算 `quota_state`；手动重置事务（`QuotaResetEvent` 唯一 `command_id`、accounted 清零、状态、恢复操作、审计）；周期切换事务（关闭旧周期、创建新周期 `UNIQUE(allocation_id, starts_at_utc)`、清除配额阻断、恢复操作、审计）；历史周期与日聚合查询
- [X] T072 [P] [US2] 编写 `internal/persistence/sqlite/store_traffic_test.go` 与 `store_quota_test.go`：批次原子性（提交失败无部分写入）、周期切换重复执行幂等、手动重置保留 gross/lifetime/daily/边界

### Xray Adapter（统计）

- [X] T073 [US2] 实现 `internal/adapter/xray/stats.go` 【Xray 契约】：`ReadTraffic` 对最多 20 个身份逐个精确 `GetStats(reset=false)` 读取 `user>>><id>>>traffic>>>uplink|downlink`，`found=false` 与零值区分，格式错误映射稳定错误，从 `Probe` 获取 boot epoch
- [X] T074 [P] [US2] 编写 `internal/adapter/xray/stats_test.go`：bufconn StatsService stub，验证精确名称、`reset=false`、缺失/零/格式错误三种结果
- [X] T075 [US2] 编写 `tests/contract/xray/traffic_test.go` 【Xray 契约】：门禁 5–6（真实 AES-256 客户端使用 `server-key:user-key` 发送 TCP 与 UDP 流量，两方向精确计数器递增；移除用户后新握手被拒而已建立连接可继续）

### 应用服务

- [X] T076 [US2] 实现 `internal/application/traffic_service.go`：`CollectOnce`（读取活跃 allocation 与游标 → 事务外 RPC → 领域 delta 计算 → 单事务提交批次；RPC 失败标记陈旧并记录实例 `last_error_*`；首次越界产生 `quota_block`）
- [X] T077 [US2] 实现 `internal/application/quota_service.go`：`UpdateQuotaPolicy`（调低至不高于当前用量→立即 exceeded 并入队 remove；调高/无限→重算 within_limit，若 admin_enabled 且 active 则入队 add）、`ResetCurrentCycleTraffic`（需确认，原子边界 4，配额为唯一阻断时入队恢复）、`RolloverCycle`（原子边界 5，仅“仅因超限”者恢复）、审计 `quota_exceeded`/`traffic_reset`/`cycle_restored`
- [X] T078 [P] [US2] 编写 `internal/application/traffic_service_test.go` 与 `quota_service_test.go`：等于配额即封禁、调低即时封禁、调高即时恢复、手动禁用+超限不恢复、重置不改周期结束时间、DB 失败下一轮安全重读

### Worker

- [X] T079 [US2] 实现 `internal/worker/collector.go`：按 `traffic_interval` 定时（默认 5s，>5s 启动警告）调用 `CollectOnce`，RPC 不在写事务持锁期间执行，记录每轮耗时/用户数/陈旧状态【可观测性】
- [X] T080 [US2] 实现 `internal/worker/scheduler.go` 【重试/协调】：按周期 timezone_name 快照计算每个 allocation 的下一日/周期边界（边界定时器 + 每 60 秒兜底扫描），幂等关闭旧周期并打开新周期，停机后追赶多个漏执行边界并收敛到唯一 open 周期，触发恢复操作
- [X] T081 [P] [US2] 编写 `internal/worker/collector_test.go` 与 `scheduler_test.go`：fake Adapter + fake 时钟；越界在一轮内产生 remove 操作；周期切换仅恢复“仅超限”用户；停机追赶

### Web 页面

- [X] T082 [US2] 实现 `internal/web/templates/pages/user_edit.html` 并扩展 `internal/web/handlers/users.go`：`GET /users/{user_id}/edit`、`POST /users/{user_id}`（`_version`；配额为整数 + 单位选择（MiB/GiB/TiB）精确换算为字节或勾选“无限制”，空值未勾选、零/负值、超过 2^62 均 422；reset day 1–28 默认 1；启用意图）
- [X] T083 [US2] 实现 `internal/web/templates/pages/user_reset_traffic.html` 与 `GET/POST /users/{user_id}/reset-traffic`：说明仅清零本周期 accounted、保留 gross/lifetime/日趋势、不改变周期结束时间，POST 后 303
- [X] T084 [US2] 扩展 `internal/web/templates/pages/user_detail.html` 与 `internal/web/views/quota.go`：上行/下行/总量/剩余、无限配额与超过 100% 的显示规则、当前周期边界（面板时区）、数据最后更新时间、配额超限说明与“轮询检测、主要阻止新连接、已有连接可能少量超额”提示（FR-019）
- [X] T085 [US2] 实现 `internal/web/templates/pages/settings.html` 与 `internal/web/handlers/settings.go`：`GET /settings`、`POST /settings`（IANA 时区校验、`_version`、仅影响未来周期的说明、审计）
- [X] T086 [P] [US2] 编写 `internal/web/handlers/users_edit_test.go` 与 `settings_test.go`：字段级 422、409、重置确认页文案、时区非法拒绝
- [X] T087 [US2] 编写 `tests/e2e/quota_test.go`：小额配额 → fake 计数增长 → exceeded → fake Xray 中用户被移除；推进时钟到新周期 → 自动恢复；手动禁用者不恢复；调低再调高；手动重置后恢复且周期结束时间不变
- [X] T088 [US2] 在 `cmd/xpanel/main.go` 接线 collector 与 scheduler，并在 `traffic_interval>5s` 时输出配额超额风险启动警告

**Checkpoint**: US1 + US2 构成完整 P1 交付：创建、分享、计量、封禁、恢复、调额与手动重置

---

## Phase 5: User Story 3 - 管理用户生命周期 (Priority: P2)

**Goal**: 搜索/筛选用户，修改显示信息，手动启停，轮换凭证，软删除并保留历史；所有变更幂等且不覆盖更新的管理员意图

**Independent Test**: 对已有用户依次执行编辑、禁用、启用、轮换、删除，每个 POST 用相同 `_request_id` 提交两次并提交一次过期 `_version`，验证页面状态、fake Xray 中的用户集合与审计记录

- [X] T089 [US3] 扩展 `internal/persistence/sqlite/store_users.go`：按规范化名称搜索与按派生状态筛选（默认排除已删除，显式包含选项）、按 revision 更新显示名称、启用/禁用意图、软删除（`deleted_at`、`admin_enabled=false`）、凭证下一版本插入/激活/销毁（清空密文）
- [X] T090 [US3] 扩展 `internal/application/user_service.go`：`UpdateUser`（重名校验、revision）、`EnableUser`（先检查当前用量是否符合配额）、`DisableUser`、`RotateCredential`（生成下一版本密钥、`desired_credential_version`、phase=`remove_old` 操作、旧信息标为即将失效；轮换进行中再次轮换返回 409，禁用/删除 supersede 轮换）、`DeleteUser`（需确认、软删除、入队 remove、确认后销毁密钥；已删除用户的任何 POST 返回 409）；均经 `DomainCommand` 幂等与 revision 保护并写审计
- [X] T091 [US3] 扩展 `internal/worker/synchronizer.go` 【重试/协调】：轮换阶段 `remove_old → add_desired → confirm`（每阶段持久化后再执行，超时读后写，确认后新凭证 active、旧凭证 destroyed）；删除确认后销毁密钥；任一阶段失败保持 pending/error 重试，绝不重新发布旧密钥
- [X] T092 [P] [US3] 扩展 `internal/application/user_service_test.go` 与 `internal/worker/synchronizer_test.go`：轮换各阶段超时/重启/新意图 supersede、重复提交、过期版本、删除后重名创建获得新身份
- [X] T093 [US3] 实现 `internal/web/templates/pages/users_list.html` 并扩展 `internal/web/handlers/users.go`：`GET /users`（名称搜索、状态筛选 启用/手动禁用/配额超限/待同步/已删除、包含已删除选项、零状态、状态文字标签、表格 caption 与 scope）
- [X] T094 [US3] 实现 `internal/web/templates/pages/user_rotate.html`、`user_delete.html` 并扩展 `internal/web/handlers/users.go`：`GET/POST /users/{user_id}/rotate`、`GET/POST /users/{user_id}/delete`（无脚本确认页，说明旧凭证失效、已有连接可能继续、历史保留）、`POST /users/{user_id}/enable`、`POST /users/{user_id}/disable`
- [X] T095 [P] [US3] 编写 `internal/web/handlers/users_list_test.go` 与 `users_lifecycle_test.go`：筛选组合、409 过期版本显示当前状态、同 `_request_id` 重放返回原 303、同 id 不同载荷 409、已删除用户不在默认列表
- [X] T096 [US3] 编写 `tests/e2e/lifecycle_test.go`：编辑/禁用/启用/轮换/删除全流程，重复提交与过期版本，旧凭证在 fake Xray 中消失、新凭证出现，流量历史归属不变，删除后重名创建为新身份

**Checkpoint**: 日常维护流程可替代手工编辑配置；幂等与并发保护可验证

---

## Phase 6: User Story 4 - 查看用量与运行状态 (Priority: P2)

**Goal**: 仪表盘展示节点健康、最后同步、各状态用户数、当期总流量与最近故障；HTMX 每 5 秒局部刷新，失败时保留最后确认数据并标记陈旧；用户详情展示当前周期每日趋势

**Independent Test**: 使用多种状态与用量的用户数据打开仪表盘，核对汇总、筛选、趋势；停止 fake Xray 采集后验证陈旧标记与最后成功时间；禁用脚本后页面仍可用

- [X] T097 [US4] 实现 `internal/persistence/sqlite/store_dashboard.go`：各派生状态计数、当期 accounted 总流量、实例健康/最后成功/最近错误、最近失败同步操作摘要、最后采集成功时间、指定 allocation 当前周期每日聚合
- [X] T098 [US4] 实现 `internal/application/dashboard_service.go`：汇总视图、陈旧判定（最后采集成功时间超过 2 个 `traffic_interval` 即陈旧，阈值来自配置）、用户表行数据、详情每日趋势
- [X] T099 [US4] 实现 `internal/web/templates/pages/dashboard.html`、`internal/web/templates/fragments/dashboard_summary.html`、`internal/web/templates/fragments/users_table.html`、`internal/web/handlers/dashboard.go` 与 `internal/web/handlers/fragments.go`：`GET /`、`GET /fragments/dashboard-summary`、`GET /fragments/users-table`（非敏感查询参数；fragment 无布局、只读 SQLite、不发 Xray RPC；`Vary: HX-Request`；503 时带可见陈旧文案；包含最后确认时间与 fresh/stale 状态（陈旧阈值 2×traffic_interval 或最近探测失败）、零状态引导、活跃分配 >20 的容量提示）
- [X] T100 [US4] 扩展 `internal/web/templates/pages/user_detail.html`：当前周期每日趋势表（本地日期、上行、下行、总量）、统计连续性事件标记、FR-003 的节点健康与最近故障摘要引用
- [X] T101 [US4] 编写 `internal/web/static/app.js`（原生 JS，无 CDN）：HTMX 轮询配置（不快于 5 秒）、fragment 替换时保留焦点或移动到稳定摘要、保护搜索框输入不被清空、失败时保留旧内容并显示陈旧摘要
- [X] T102 [P] [US4] 编写 `internal/web/handlers/dashboard_test.go` 与 `fragments_test.go`：fragment 不含 `<html>`、陈旧标记、503 文案、筛选参数、未认证 fragment 403、无 Adapter 调用
- [X] T103 [US4] 编写 `tests/e2e/dashboard_test.go`：多状态用户汇总正确；采集失败后最后确认值保留且标记陈旧；按名称/状态筛选；无脚本路径下整页数据完整

**Checkpoint**: 管理员可通过仪表盘判断配额控制与同步是否正常工作

---

## Phase 7: User Story 5 - 故障后自动恢复并可审计 (Priority: P3)

**Goal**: 面板或 Xray 重启、连接中断或操作超时后，协调器按期望状态恢复应启用用户、保持禁用/超限/删除用户不可用，统计连续性事件可见，审计记录可筛选且不含敏感凭证

**Independent Test**: 构造启用、手动禁用、配额超限、待同步和已删除用户后重启 fake/真实 Xray，恢复连接后 60 秒内状态收敛；模拟 RPC 超时与计数下降验证无重复凭证、无负流量；审计页可按用户与动作筛选

- [X] T104 [US5] 实现 `internal/application/reconciliation_service.go` 【重试/协调】：按 profile 对比 SQLite 期望存在集合与 `ListUsers` 实际集合，`xpanel-` 命名空间内 SQLite 无记录或已删除的身份按漂移移除并写 `reconcile_removed_unknown` 审计，非该命名空间身份（含 bootstrap）一律保留；仅在漂移时创建 `reconcile` 操作；更新实例健康与 boot epoch；检测新 boot epoch 时标记游标进入新纪元
- [X] T105 [US5] 实现 `internal/worker/reconciler.go` 【重试/协调】：在启动、Xray 由 unreachable 转 healthy 时以及每 `reconcile_interval`（≤30s）运行；与 synchronizer 共用串行执行通道避免并发变更同一节点；记录漂移数量与耗时；任一分配 `desired_revision ≠ synced_revision` 持续超过 3×`reconcile_interval` 时输出 warn 并在仪表盘标记“持续不同步”【可观测性】
- [X] T106 [P] [US5] 编写 `internal/application/reconciliation_service_test.go` 与 `internal/worker/reconciler_test.go`：fake 重启后 active 恢复、disabled/exceeded/deleted 保持缺席、外部用户不动、不确定变更与更高 revision 意图的优先级
- [X] T107 [US5] 扩展 `internal/application/traffic_service.go`：将 `Probe` 的 boot epoch 与游标 `boot_epoch` 对比，重启后首个绝对值按 `node_restart` 计入，未知下降按 `counter_decrease` 建新基线，缺失后重现记 `reappeared`；持续不同步阈值与告警见 T105【可观测性】
- [X] T108 [US5] 编写 `tests/contract/xray/recovery_test.go` 【Xray 契约】：门禁 7–8（重启改变 boot/统计纪元、丢弃动态用户并仅恢复 active 用户；可能已生效的变更超时后通过读后写收敛且无重复）
- [X] T109 [US5] 实现 `internal/persistence/sqlite/store_audit.go`：按用户/动作/结果筛选的游标分页审计查询；不提供 UPDATE/DELETE 方法
- [X] T110 [US5] 实现 `internal/web/templates/pages/audit.html` 与 `internal/web/handlers/audit.go`：`GET /audit`（筛选、游标分页、空结果文案；展示时间、操作者、目标、动作、结果与安全摘要；已删除用户仍可按目标筛选）
- [X] T111 [US5] 扩展 `internal/web/templates/pages/user_detail.html`：统计连续性事件列表与同步操作历史（pending/retry_wait/permanent_failed 及安全错误摘要、下一次重试时间）
- [X] T112 [P] [US5] 编写 `internal/web/handlers/audit_test.go`：筛选与分页、正文不含密码/密钥/token/完整 URI
- [X] T113 [US5] 编写 `tests/e2e/recovery_test.go`：四类用户 + fake 重启 → 状态收敛；不确定 RPC 结果无重复用户；计数下降无负流量且事件可见；在提交后确认前重启应用 → 操作从租约恢复到相同结果；审计筛选结果无敏感值
- [X] T114 [US5] 在 `cmd/xpanel/main.go` 接线 reconciler（启动顺序：synchronizer → reconciler → collector → scheduler → readiness）

**Checkpoint**: 全部五个用户故事可独立验证；重启与超时场景可自动收敛

---

## Phase 8: 横切能力与收尾

**Purpose**: 本机密码重置、优雅关闭、安全加固、备份恢复、文档与发布门禁

- [X] T115 扩展 `cmd/xpanel/main.go` 与 `internal/application/auth_service.go`：`admin reset-password`（不需旧密码；TTY 无回显两次输入或 `--password-stdin`；单事务替换 Argon2id 哈希、递增 `password_version`、撤销全部会话、写 `actor_type=local_cli` 审计；不触碰用户/profile/配额/流量；退出码 0/2/3/4；stdout 无任何秘密）
- [X] T116 [P] 编写 `tests/integration/cli_test.go`：`--password-stdin` 模式下 init 与 reset；管理员已存在时 init 拒绝；reset 后旧密码与既有会话立即失效；输出不含哈希/密钥
- [X] T117 实现优雅关闭于 `internal/web/server.go` 与 `cmd/xpanel/main.go`：停止接收写请求、取消后台 RPC、完成或取消短事务、释放租约与连接；编写 `tests/integration/shutdown_test.go` 验证关闭期间无部分提交
- [X] T118 [P] 编写 `tests/integration/secrets_leak_test.go`：遍历所有认证页面、fragment、审计与日志输出，断言不含密码、服务端/用户密钥、session/CSRF token、完整 `ss://` URI；编写 `internal/logging/redact_test.go`
- [X] T119 [P] 编写 `tests/integration/failure_matrix_test.go` 与故障注入 Store 包装器 `tests/integration/faultstore_test.go`：按 变更类型 {创建, 启用, 禁用, 轮换, 删除, 配额封禁, 周期恢复} × 故障 {数据库提交失败, RPC 超时, RPC 成功后进程崩溃, Xray 重启, 重复请求, 协调器重放} 生成表驱动用例（42 个单元格，缺失单元格视为失败）；每个单元格断言 SQLite 期望状态与审计一致、fake Xray 最终用户集合与期望一致、无重复用户/重复流量/负流量（宪章「开发流程与质量门禁」）
- [X] T120 [P] 编写 fuzz 测试 `internal/security/tokens_fuzz_test.go`（统计身份、SS2022 密钥校验）与 `internal/adapter/xray/stats_fuzz_test.go`（计数器名称解析）
- [X] T121 [P] 无障碍与渐进增强复核：检查 `internal/web/templates/**` 所有输入有程序化标签、错误 `aria-describedby`、表格 caption/scope、导航为链接/动作为按钮；`internal/web/static/app.css` 移动宽度下关键操作可见；编写 `internal/web/handlers/accessibility_test.go` 断言上述结构与 CSP 兼容（无内联脚本）
- [X] T122 [P] 编写 `docs/operations.md`：SQLite 备份（WAL checkpoint）、root key 分离存储与丢失后果、恢复步骤、恢复后协调验证；编写 `tests/integration/backup_restore_test.go` 验证恢复数据库后 active 用户被协调而 blocked/deleted 不复活
- [X] T123 [P] 编写 `README.md`：构建、配置、`admin init`/`serve`/`reset-password`、quickstart 指引、软配额语义声明
- [X] T124 编写 `tests/integration/perf_test.go`：20 个活跃 allocation 下管理操作反馈 <2s、一轮采集提交耗时记录（SC-002/SC-003 基线）
- [X] T125 编写 `tests/e2e/success_criteria_test.go`：用 fake 时钟与 fake Adapter 为可自动化的成功标准产生证据并以结构化 `t.Log` 输出测量摘要：SC-003（20 个分配下数据从采集到页面 ≤10s）、SC-004（越界后 ≤10s 完成移除投影；失败路径 ≤30s 显示待同步）、SC-005（周期切换 ≤60s 恢复，手动禁用/已删除恢复数为 0）、SC-006（重连 ≤60s 收敛）、SC-007（负流量与重复计量为 0）、SC-008（受审计操作覆盖 100% 且无明文凭证）、SC-011（重置后旧会话可用数为 0）
- [ ] T126 【2026-09-05：已用 `go install github.com/xtls/xray-core/main@v1.260327.0` 构建固定二进制并通过契约门禁 1–9（见 validation-report.md）；剩余 SC-001/SC-002/SC-010 需人工在浏览器中计时与操作，待用户执行】按 `specs/001-xray-user-management/quickstart.md` §2–§9 使用真实 Xray v26.3.27 完成人工验收并记录到 `specs/001-xray-user-management/validation-report.md`：SC-001（从首次登录到复制出连接信息的计时）、SC-002（登录/搜索/创建/编辑/状态切换各执行 ≥20 次，记录 P95 反馈时间）、SC-010（纯键盘以及 360×640、390×844 视口各完成登录/创建/编辑/禁用/删除，记录成功率）；引用 T125 的自动化证据；SC-009 属可用性研究，需另行组织，不由本任务验证
- [X] T127 【2026-09-05：`XRAY_BIN=<v26.3.27> XPANEL_REQUIRE_CONTRACT=1 make check` 全部通过，证据已写入 validation-report.md】运行发布门禁 `make check`（`XRAY_BIN` 指向固定二进制并设置 `XPANEL_REQUIRE_CONTRACT=1`），全部通过后更新 `validation-report.md` 的门禁证据

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup (Phase 1)**: 无依赖，可立即开始
- **Foundational (Phase 2)**: 依赖 Phase 1；阻塞所有用户故事
- **US1 (Phase 3)**: 依赖 Phase 2；是其余故事的基础层
- **US2 (Phase 4)**: 依赖 US1（用户/allocation/凭证、同步 worker、Adapter 变更操作）
- **US3 (Phase 5)**: 依赖 US1；与 US2 无相互依赖，可并行
- **US4 (Phase 6)**: 依赖 US1 与 US2（每日趋势与当期流量）
- **US5 (Phase 7)**: 依赖 US1（synchronizer/Adapter）与 US2（游标与 boot epoch）；审计页部分仅依赖 Phase 2
- **Polish (Phase 8)**: 依赖所有目标故事完成；T115/T116（密码重置）仅依赖 Phase 2，可提前

### User Story Dependencies

- **US1 (P1)**: Phase 2 完成后开始；无其他故事依赖
- **US2 (P1)**: US1 完成后开始；扩展 `user_service.go`/`users.go`/`user_detail.html`
- **US3 (P2)**: US1 完成后开始；与 US2 并行时注意二者都修改 `internal/web/handlers/users.go` 与 `internal/worker/synchronizer.go`，需按文件协调合并
- **US4 (P2)**: US2 完成后开始
- **US5 (P3)**: US2 完成后开始；T109/T110/T112（审计页）可在 Phase 2 后提前

### Within Each User Story

- 领域模型 → 持久化 → Adapter → 应用服务 → Worker → Web 页面 → E2E
- 同一文件的扩展任务（如 `users.go`、`synchronizer.go`、`user_detail.html`）必须串行
- 各故事的 `*_test.go` 在对应实现完成后可并行运行

### Parallel Opportunities

- Phase 1：T003、T004、T005 并行
- Phase 2：T007–T012 并行；T014/T015 并行；T021/T022 并行；T024；T026–T029、T033 并行；T036–T039 并行
- US1：T040–T043 并行；T048、T050、T055、T057、T063 在对应实现后并行
- US2：T066–T068 并行；T072、T074、T078、T081、T086 并行
- US3：T092、T095 并行
- US4：T102
- US5：T106、T112 并行
- Phase 8：T116、T118–T123 并行

---

## Parallel Example: User Story 1

```bash
# 领域模型（不同文件，无相互依赖）：
Task: "实现 internal/domain/profile.go"
Task: "实现 internal/domain/user.go"
Task: "实现 internal/domain/sync.go"
Task: "编写 internal/domain/profile_test.go、user_test.go、sync_test.go"

# 实现完成后并行测试：
Task: "编写 internal/persistence/sqlite/store_profiles_test.go、store_users_test.go、store_sync_test.go"
Task: "编写 internal/adapter/xray/handler_test.go"
Task: "编写 internal/application/profile_service_test.go、user_service_test.go、connection_service_test.go"
Task: "编写 internal/worker/synchronizer_test.go"
Task: "编写 internal/web/handlers/profiles_test.go 与 users_create_test.go"
```

## Parallel Example: User Story 2

```bash
Task: "实现 internal/domain/quota.go"
Task: "实现 internal/domain/traffic.go"
Task: "编写 internal/domain/quota_test.go 与 traffic_test.go"
```

---

## Implementation Strategy

### MVP First (User Story 1 Only)

1. 完成 Phase 1 Setup
2. 完成 Phase 2 Foundational（阻塞项）
3. 完成 Phase 3 US1
4. **STOP and VALIDATE**：运行 `tests/e2e/create_user_test.go` 与 `make contract`，用 quickstart §4–§5 演示
5. 可作为“创建并分享用户”MVP 演示

### Incremental Delivery

1. Setup + Foundational → 可登录的空面板
2. US1 → 创建/分享/待同步（MVP）
3. US2 → 计量、封禁、周期恢复、调额、手动重置（完整 P1 交付）
4. US3 → 生命周期维护与幂等
5. US4 → 仪表盘与趋势
6. US5 → 协调恢复与审计
7. Phase 8 → 密码重置、关闭、故障矩阵、加固、备份、文档、成功标准证据、发布门禁

### Parallel Team Strategy

1. 团队共同完成 Setup + Foundational
2. 一人完成 US1（基础层）
3. US1 完成后：开发者 A 做 US2，开发者 B 做 US3（协调 `users.go`/`synchronizer.go` 合并），开发者 C 提前做审计页（T109/T110/T112）与密码重置（T115/T116）
4. US2 完成后：US4 与 US5 并行

---

## Notes

- [P] = 不同文件且无未完成依赖
- [Story] 标签用于追溯到 spec.md 的用户故事
- 【迁移】【Xray 契约】【重试/协调】【可观测性】 标签满足宪章对任务显式标注的要求
- 每个影响访问权的变更（创建、启停、轮换、删除、封禁、恢复）× 六类故障（数据库提交失败、RPC 超时、RPC 成功后进程崩溃、Xray 重启、重复请求、协调器重放）由 T119 的故障矩阵系统性覆盖；各故事的 worker/E2E 测试为补充，不替代矩阵
- 每个任务或逻辑组完成后提交；在任一 Checkpoint 停下独立验证该故事
- 避免：模糊任务、同文件并行冲突、破坏故事独立性的跨故事依赖

---

## Phase 9: Convergence

- [X] T128 CRITICAL 为 `internal/persistence/sqlite/store_sync.go` 的阶段推进、重试重排和永久失败写入增加 operation state、租约 owner 与 allocation desired revision 的条件更新；旧操作被 supersede 或新意图已提交时只结束旧操作，不得把当前 allocation 改回 pending/error，并增加 worker 与 SQLite 并发回归测试 per Constitution IV / FR-021 (contradicts)
- [X] T129 CRITICAL 重构采集越界、手动流量重置、调额和周期切换事务，使其在写事务内重读最新 policy、admin_enabled、lifecycle、open cycle 与 allocation revision 后重算最终状态，并以 revision/CAS 防止事务外旧快照覆盖较新的管理员意图；覆盖边界时刻并发调额、禁用、重置、采集与 rollover 的确定性测试 per Constitution IV / FR-021 (contradicts)
- [X] T130 CRITICAL 将 `internal/application/reconciliation_service.go` 对 SQLite 无记录的 `xpanel-` 漂移身份移除改为 RPC 前持久化、可租约和可重试的漂移移除意图及关联审计结果，由串行 synchronizer 执行，并覆盖 RPC 超时、进程崩溃和重放 per Constitution I / Constitution IV / FR-020 (contradicts)
- [X] T131 为 profile 验证结果增加 profile revision 条件提交，把 compatibility、实例健康和验证审计按一致的事务边界写入；让 revalidate 接收 `_version`/`_request_id` 并幂等排队，验证期间发生编辑时丢弃旧结果并验证最新 revision per FR-021 (partial)
- [X] T132 将 handler 生成的 session+action+target+canonical-payload fingerprint 传入所有状态变更命令并持久比较；修复 profile 创建 fingerprint 对随机新 profile ID 的依赖，使同 session 同请求重放返回原 canonical result、跨 session 或不同载荷复用返回 409，并补 profile/setting/user HTTP 回归测试 per FR-021 / FR-027 (partial)
- [X] T133 为已有 allocation 的 profile 限制或实现可恢复的 inbound_tag/method/bootstrap identity 变更：不得在旧入站遗留可用用户，method 变化必须校验或轮换现有 user/server keys，所有受影响 projection 必须经持久操作收敛，并增加在线用户迁移/拒绝测试 per FR-006 / FR-007 / FR-020 / FR-030 (partial)
- [X] T134 修改 bootstrap identity 注册以区分同 profile 幂等重放与跨 profile 全局冲突，冲突时不得把 profile 标为 compatible；增加多 profile bootstrap/managed statistics ID 全实例唯一测试 per data-model: XrayUserIdentity / contract gate 9 (partial)
- [X] T135 修正 `deploy/xray-v26.3.27.example.json` 和 `tests/contract/xray` 运行时配置以满足固定 Xray API `tag` 契约，改为从 `XRAY_BIN` 构建信息验证 `github.com/xtls/xray-core@v1.260327.0`，失败时输出安全的进程诊断，并让固定二进制的真实契约套件能够启动和通过 per plan: Xray contract and deployment config (partial)
- [X] T136 扩展真实 Xray 契约门禁 7–9：用应用 reconciler/synchronizer 验证重启后仅恢复 active 用户，用持久操作验证可能已生效的超时经读后写收敛，并用多个 profile 验证统计 ID 全局唯一与 bootstrap 排除，而非在测试中手工重加用户 per plan: Xray compatibility gates 7-9 (partial)
- [X] T137 将流量采集目标按最多 20 个 statistics ID 分批执行并合并为同一安全提交语义，使超过 20 个活跃 allocation 时继续计量、标记陈旧和执行配额封禁，同时只撤销性能承诺而不停止功能，并增加 21+ 用户回归测试 per FR-013 / FR-017 (contradicts)
- [X] T138 加强 `tests/integration/failure_matrix_test.go` 与 `tests/e2e/success_criteria_test.go`：每个故障单元格断言对应动作/结果的审计、operation 唯一性、流量不重复且不为负；对 SC-008 的 leak count 显式断言为零，确保缺少审计或发生泄露时门禁失败 per SC-007 / SC-008 (partial)
- [X] T139 调整用户状态筛选，使 admin enabled 且 within-limit 的 enabling/pending/error 用户仍命中“启用”业务筛选，同时也命中“待同步”筛选，并增加正交筛选组合测试 per FR-008 (partial)
- [X] T140 为无效及限流登录记录不枚举账号且不含密码的 failed 审计，并确保 logout 的会话撤销与审计失败不会被静默报告为完整成功；补齐成功/失败/限流认证路径的审计覆盖率断言 per FR-025 / FR-026 / SC-008 (partial)
- [X] T141 为嵌入式 CSS/JS 生成内容哈希资源名并仅对静态资源返回 `Cache-Control: public, max-age=31536000, immutable`，继续对认证 HTML 返回 `no-store`，增加缓存头和模板引用测试 per plan: performance and static delivery decision (contradicts)
- [X] T142 在创建和编辑用户时保存去除首尾空白的 display_name，并保持 NFKC/大小写不敏感 normalized_name 唯一语义，增加展示值与名称复用测试 per T041 / FR-005 (partial)

---

## Phase 10: Convergence

- [X] T143 CRITICAL 为 `internal/persistence/sqlite/store_sync.go`、`store_drift.go` 与 `internal/worker/synchronizer.go` 补齐租约 fencing：领取必须以到期状态/旧 owner 做 CAS，worker 获得 node 锁后在 RPC 前重新确认租约与最新 desired revision，成功确认也必须携带并校验 lease owner（不能只校验 revision），且 lease 时长必须覆盖 RPC 或可续租；用“慢 RPC 超过租期 + 第二 worker 回收 + 并发新管理员意图”测试断言旧 worker 不再调用/确认 Xray、不会短暂恢复旧意图，漂移移除也不重复执行 per Constitution IV / FR-020 / FR-021 / FR-022 (partial: T128/T130)
- [X] T144 CRITICAL 收紧 `internal/persistence/sqlite/store_profiles.go` 的契约字段变更条件并稳定漂移目标：修改 inbound_tag/method/bootstrap 前必须处理所有 allocation（包括已 soft-delete 但 remove 尚未确认者）、未终结 synchronization operation 与 drift removal；选择拒绝直到旧投影确认 absent/意图终结，或为操作持久化原 inbound tag 并完成可恢复迁移。增加“删除事务已提交但尚未 Drain 即编辑 profile”及“未知身份移除已排队即改 tag”的回归测试，断言旧入站不遗留可用 `xpanel-` 身份 per FR-006 / FR-012 / FR-020 / T130 / T133 (partial)
- [X] T145 CRITICAL 重构 `internal/persistence/sqlite/store_quota.go` 与 `store_traffic.go` 的周期边界提交：rollover 在同一写事务中重读当前 open cycle、最新 quota policy/reset_day、面板时区、lifecycle/admin_enabled 与 allocation revision 后计算新周期；采集提交按样本完成时间确保已跨边界的增量进入正确的新周期，而不是仍写入过期 open cycle。覆盖 rollover 与 reset_day/时区修改、禁用、重置及采集在两种提交顺序下的确定性测试，断言唯一 open cycle、周期边界与恢复操作均正确且流量只记一次 per data-model: Write Ordering Under Contention / FR-016 / FR-018 / FR-021 / FR-032 (partial: T129)
- [ ] T146 在 `internal/application/traffic_service.go` 合并 20-ID 分批结果时校验每批 `InstanceObservation` 属于同一 boot epoch 且时间顺序一致；若批间 Xray 重启或 observation 不一致，整轮不得提交任何 cursor/total/quota 变更，必须安全重试并记录连续性/健康诊断。增加 21+ allocation 在第一、第二批之间重启的 fake 回归测试，断言无混合 epoch、负增量、重复计量或漏掉的配额封禁 per FR-013 / FR-017 / FR-023 / SC-007 (partial: T137)
- [ ] T147 统一 `internal/application/auth_service.go`、`internal/web/handlers/auth.go` 与 SQLite session store 的认证结果语义：失败/限流登录的审计写入错误不得被无条件吞掉；logout 的当前 session 撤销与审计结果必须一致，只有确认撤销后才能记录/返回 succeeded，任一步失败都不得留下“登出成功但 session 仍有效”的审计状态。增加 audit insert、session delete/commit 故障注入测试，并断言 HTTP 不返回成功、session 可用性与 succeeded/failed 审计结果吻合且不泄露用户名或密码 per FR-002 / FR-025 / FR-026 / FR-027 / SC-008 (partial: T140)
