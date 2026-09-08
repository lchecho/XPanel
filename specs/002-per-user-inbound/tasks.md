---

description: "每用户专属入站的实现任务清单"
---

# Tasks: 每用户专属入站的多用户管理

**Input**: Design documents from `/specs/002-per-user-inbound/`

**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/（xray-adapter.md、http.md、config.md）, quickstart.md

**Tests**: 项目宪章「开发流程与质量门禁」强制要求单元、集成、Handler、Xray 契约与端到端测试，并在
v1.2.0 追加「面板管理入站时 MUST 追加覆盖端口冲突与端口被面板外进程占用」。因此本清单包含测试任务，
它们是各故事的必交付物，与实现任务同阶段列出，不要求严格 TDD 先行。

**Organization**: 任务按用户故事分组。本功能替换 001 的共享入站模型，因此 Phase 2 的模型替换是所有
故事的阻塞前提；US1 打通「创建用户即得专属入站与端口」，US2–US5 在其上分别覆盖配额、生命周期、
可观测性与故障恢复。

**复用基线**: 001 已实现并通过含真实 Xray 契约的发布门禁。租约 fencing、周期边界事务、分批采集
一致性、认证原子性、漂移 `superseded_by` 因果链等与入站模型无关的正确性资产 MUST 复用，不得重写
（research.md R-008）。

**宪章标注约定**: 任务描述中的 【迁移】【Xray 契约】【重试/协调】【可观测性】【安全】 标签对应宪章
要求显式标注的工作项。

## Format: `[ID] [P?] [Story] Description`

- **[P]**: 可并行（不同文件、不依赖未完成任务）
- **[Story]**: 所属用户故事（US1–US5）
- 每个任务都包含精确文件路径

## Path Conventions

沿用 001 的单 Go module 模块化单体（见 plan.md「Project Structure」）：

- 入口：`cmd/xpanel/`
- 业务与适配：`internal/{config,domain,application,ports,persistence/sqlite,adapter/xray,security,logging,worker,web,testsupport}`
- 测试：`tests/{contract/xray,integration,e2e}` 与各包 `*_test.go`
- 部署样例：`deploy/`

---

## Phase 1: Setup（基线对齐）

**Purpose**: 让规格产物与已落地的宪章 v1.2.0 一致，并给出不含预配置入站的部署样例

- [X] T001 更新 `specs/002-per-user-inbound/plan.md` 的 Constitution Check：宪章已修订至 v1.2.0，
  将原则 II 由 CONDITIONAL 改判为 PASS，并在 Complexity Tracking 中把「需 MINOR 修订」标记为已完成
  （引用宪章 commit）
- [X] T002 [P] 按 contracts/config.md 重写 `deploy/xray-v26.3.27.example.json`：保留 `api.tag`、
  `stats`、`policy.levels."0".statsUserUplink/statsUserDownlink` 与 freedom outbound，移除预配置的
  SS2022 入站与 bootstrap 客户端，并加注释说明入站由面板在运行时创建
- [X] T003 [P] 在 `README.md` 与 `docs/operations.md` 增补端口池规划、防火墙放行整个端口区间、
  Xray 重启期间全部用户端口不可用的说明

---

## Phase 2: Foundational（阻塞性模型替换）

**Purpose**: 把「共享入站 + 动态用户」替换为「入站模板 + 专属入站 + 端口分配」，并让 Adapter 具备
入站生命周期能力。本阶段完成前不得开始任何用户故事。

**⚠️ CRITICAL**: 本阶段跨越领域、端口接口、持久化与适配层，是所有故事的前提

### 领域模型

- [X] T004 [P] 新建 `internal/domain/inbound.go`：面板保留命名空间前缀常量与标签生成/判定函数、
  专属入站期望状态由 `lifecycle == active && admin_enabled && quota_state == within_limit` 派生的
  纯函数、入站必须携带恰好一个受管客户端的不变量校验
- [X] T005 [P] 新建 `internal/domain/port.go`：端口池区间类型（`1024 ≤ start ≤ end ≤ 65535`）、
  容量计算、池内判定、下一个可用端口的确定性选择策略（升序取最小空闲），全部为纯函数
- [X] T006 [P] 新建 `internal/domain/inbound_test.go` 与 `internal/domain/port_test.go`：表驱动覆盖
  命名空间判定、期望状态派生真值表、端口池边界值（下界、上界、单端口池、空洞复用、耗尽）
- [X] T007 改写 `internal/domain/profile.go` 为入站模板：移除服务端密钥与 bootstrap 统计标识字段，
  新增监听地址与端口池区间及其校验；同步更新 `internal/domain/profile_test.go`
- [X] T008 更新 `internal/domain/identity.go`：移除 `IdentityBootstrap` 的使用路径，新增入站创建/
  移除、端口分配/释放对应的审计动作常量

### 端口接口与 Adapter

- [X] T009 更新 `internal/ports/xray.go`：`Adapter` 接口新增 `CreateInbound`、`RemoveInbound`、
  `ListInbounds`，定义 `InboundSpec`（标签、监听地址、端口、方法、服务端密钥、唯一客户端）与
  `RemoteInbound`；新增稳定错误类别 `port_unavailable`、`inbound_already_exists`、`inbound_not_found`
- [X] T010 新建 `internal/adapter/xray/inbound.go`：按 contracts/xray-adapter.md 用定向 protobuf
  （`core.InboundHandlerConfig` + `proxyman.ReceiverConfig` + `shadowsocks_2022.MultiUserServerConfig`）
  实现三个操作，MUST NOT 引入 `infra/conf`；构建期拒绝空客户端列表 【Xray 契约】【安全】
- [X] T011 更新 `internal/adapter/xray/errors.go`：把 `bind: address already in use`、
  `existing tag found`、`handler not found`、`common: not enough information for making a decision`
  映射为 T009 定义的稳定类别，保持脱敏且不含原始错误串 【可观测性】
- [X] T012 在 `internal/adapter/xray/inbound.go` 增加创建前端口预绑定探测（`net.Listen` 后立即释放），
  失败即返回 `port_unavailable`，并在注释中说明其 TOCTOU 局限不作为唯一保证（research.md R-002）
- [X] T013 更新 `internal/adapter/xray/fake/fake.go`：支持入站生命周期与端口语义——按标签维护入站表、
  记录端口占用、可注入 `port_unavailable` 并模拟「返回错误但入站仍注册」的部分失败、`Restart()` 清空
  全部运行时入站与端口

### 持久化

- [X] T014 新建迁移 `internal/persistence/sqlite/migrations/00004_per_user_inbound.sql` 【迁移】：
  `access_profiles` 改造为 `inbound_templates`（新增 `listen_address`/`port_pool_start`/`port_pool_end`，
  删除服务端密钥列、`bootstrap_statistics_id`、`public_port`）；新建 `dedicated_inbounds` 表；
  建立 `UNIQUE(inbound_tag)` 与部分唯一索引 `(listen_address, port) WHERE released_at IS NULL`；
  清理 `kind='bootstrap'` 身份；提供可回滚的 `-- +goose Down`
- [X] T015 更新 `internal/persistence/sqlite/migrations_test.go`：覆盖 00004 的 up/down、部分唯一索引
  确实拒绝重复 `(listen_address, port)`、`released_at` 非空后同端口可再次分配
- [X] T016 新建 `internal/persistence/sqlite/store_inbounds.go`：专属入站与端口分配的事务写入——
  分配端口（依赖唯一索引拒绝冲突而非应用层判断）、释放端口、确认监听状态、按模板统计端口池使用量
- [X] T017 改写 `internal/persistence/sqlite/store_profiles.go` 为入站模板存储：移除服务端密钥读写与
  bootstrap 身份注册，新增端口池字段读写；保留 001 的 revision 条件提交与 `CompleteProfileValidation`
  事务边界
- [X] T018 更新 `internal/persistence/sqlite/store_users.go` 与 `store_quota.go`：用户创建事务内联动
  端口分配与专属入站写入，删除确认事务内置 `released_at`；`readFacts`/`applyDecision` 的决策结果映射到
  `create_inbound` / `remove_inbound` 阶段 【重试/协调】
- [X] T019 更新 `internal/ports/store.go`：新增专属入站与端口分配相关的 Store 方法签名与记录类型，
  与 data-model.md 的实体映射一致

### 测试支撑

- [X] T020 更新 `internal/testsupport/app.go`：`RegisterCompatibleProfile` 改为登记入站模板（含端口池），
  `CreateUser` 返回值包含分配端口；新增按端口查询监听状态的辅助方法
- [X] T021 更新 `internal/application/profile_service.go` → 重命名为 `template_service.go`：
  能力校验改为验证实例支持运行时创建/移除入站、SS2022 多用户与独立用户统计（含 `policy` 缺失检出），
  不再校验预配置入站；同步更新 `internal/application/profile_service_test.go`

**Checkpoint**: 模型替换完成，可编译且既有测试在新模型下通过；用户故事可以开始

---

## Phase 3: User Story 1 - 创建用户并获得专属入口 (Priority: P1) 🎯 MVP

**Goal**: 管理员登记入站模板后创建用户，面板自动分配端口、创建专属入站并给出含该端口的连接信息

**Independent Test**: 创建两个用户，确认各自获得不同端口的专属入站，分别用其连接信息完成一次新连接
认证，即可独立验证从管理到交付的完整价值

### 实现

- [X] T022 [US1] 更新 `internal/application/user_service.go` 的 `CreateUser`：在同一事务内分配端口、
  生成该入站的服务端密钥与用户密钥、写入专属入站与 `create` 同步操作；端口冲突由数据库约束拒绝并
  转换为 409，端口池耗尽与池外端口分别返回明确错误
- [X] T023 [US1] 新建 `internal/application/inbound_service.go`：专属入站期望状态的编排入口，
  供用户、配额与协调路径统一调用，避免各处重复派生规则
- [X] T024 [US1] 更新 `internal/worker/synchronizer.go`：`create_inbound` 与 `remove_inbound` 两个新
  阶段的处理；`CreateInbound` 失败一律按「可能已部分生效」处理——先 `ListInbounds` 读后写确认，
  已注册但未监听时补偿 `RemoveInbound` 再按有界退避重试 【重试/协调】
- [X] T025 [US1] 更新 `internal/application/connection_service.go`：连接信息使用该用户的专属端口与
  该入站的服务端密钥，组合密钥格式不变
- [X] T026 [P] [US1] 更新 `internal/web/handlers/profiles.go` → `templates.go` 与
  `internal/web/templates/pages/profile_{form,detail}.html`、`profiles_list.html`：路由改为
  `/templates`，表单移除服务端密钥与 bootstrap，新增监听地址与端口池区间及字段级校验提示
- [X] T027 [P] [US1] 更新 `internal/web/handlers/users.go` 与
  `internal/web/templates/pages/user_form.html`：创建表单新增可选端口字段，留空表示自动分配；
  按 contracts/http.md 渲染端口超范围 422、端口冲突 409、端口池耗尽 409 的稳定文案
- [X] T028 [US1] 更新 `internal/web/templates/pages/user_detail.html` 与
  `internal/web/handlers/users.go`：详情页展示端口、入站标签与监听状态（监听中 / 未监听 / 待同步）
- [X] T029 [US1] 更新 `internal/logging` 调用点与 `internal/worker/synchronizer.go` 日志字段：
  入站创建/移除与端口分配/释放写入结构化日志，包含端口与入站标签，禁止输出任何密钥 【可观测性】【安全】

### 测试

- [X] T030 [P] [US1] 新建 `tests/contract/xray/inbound_test.go` 【Xray 契约】：覆盖
  contracts/xray-adapter.md 门禁 2–4——运行时创建 SS2022 入站、端口在返回后立即监听、
  `GetInboundUsersCount > 0`、可在其上增删用户、Adapter 在构建期拒绝空客户端
- [X] T031 [P] [US1] 新建 `tests/contract/xray/inbound_failure_test.go` 【Xray 契约】：覆盖门禁 8——
  端口被外部进程占用时 `CreateInbound` 报错且入站仍被注册，补偿移除后换端口可收敛
- [X] T032 [P] [US1] 新建 `tests/integration/port_allocation_test.go`：端口自动分配唯一性（含并发创建）、
  指定池内空闲端口、指定已占用端口 409、池外端口 422、端口池耗尽 409，全部断言无部分状态
- [X] T033 [P] [US1] 新建 `internal/web/handlers/templates_test.go` 与更新
  `internal/web/handlers/users_create_test.go`：模板表单不出现服务端密钥字段、端口字段校验、
  创建成功后详情页展示端口与监听状态
- [X] T034 [US1] 更新 `tests/e2e/create_user_test.go`：从创建到连接信息可复制的完整路径，断言两个用户
  获得不同端口且互不影响

**Checkpoint**: US1 完成后，创建用户即得专属入站与端口，连接信息可直接交付

---

## Phase 4: User Story 2 - 自动执行流量配额 (Priority: P1)

**Goal**: 配额超限时移除该用户整条入站使端口停止监听，新周期以原端口自动重建

**Independent Test**: 为一个用户设置小额配额并触发封禁，确认其端口停止监听而其他用户端口不受影响，
再推进到下一个配额周期验证以原端口自动恢复

### 实现

- [X] T035 [US2] 更新 `internal/persistence/sqlite/store_traffic.go` 的 `CommitTrafficBatch`：越界决策
  产出的同步操作使用 `remove_inbound` 阶段；保留 001 的事务内重读事实与周期结算顺序不变
- [X] T036 [US2] 更新 `internal/persistence/sqlite/store_quota.go`：周期恢复产出的同步操作使用
  `create_inbound` 阶段，且 MUST 复用该用户原有端口分配（不重新分配）
- [X] T037 [US2] 更新 `internal/web/templates/pages/user_detail.html`：配额超限状态下明确展示
  「端口已停止监听」与「已建立连接可能短暂继续」的既有软配额文案

### 测试

- [X] T038 [P] [US2] 新建 `tests/integration/quota_inbound_test.go`：越界后端口停止监听、原端口在新周期
  恢复、手动禁用用户不被恢复、调低配额立即进入超限处理
- [X] T039 [P] [US2] 新建 `tests/integration/isolation_test.go`：对一个用户执行创建、禁用、启用、轮换、
  删除与配额封禁的全过程中，另一个用户的端口可连接性与流量计量零偏差（SC-012）
- [X] T040 [P] [US2] 更新 `tests/e2e/quota_test.go`：配额闭环的端到端证据，断言封禁与恢复都作用于同一端口
- [X] T041 [US2] 更新 `internal/application/traffic_service_test.go` 与 `quota_service_test.go`：
  在新入站模型下复核采集与结算路径未发生语义漂移

**Checkpoint**: US1 与 US2 均可独立工作，配额闭环作用于端口层面

---

## Phase 5: User Story 3 - 管理用户生命周期与端口 (Priority: P2)

**Goal**: 编辑、禁用、启用、凭证轮换、删除都正确作用于专属入站与端口分配

**Independent Test**: 对已有用户依次执行编辑、禁用、启用、凭证轮换和删除，验证每一步的端口监听状态、
页面状态与新连接结果

### 实现

- [X] T042 [US3] 更新 `internal/application/user_service.go` 的 `SetAdminEnabled`：禁用产出
  `remove_inbound`、启用产出 `create_inbound` 且复用原端口；启用前仍先检查当前用量是否符合配额
- [X] T043 [US3] 更新 `internal/application/user_service.go` 的 `RotateCredential`：轮换 MUST NOT 改变
  端口与入站，仅在既有入站内走 `remove_old` → `add_desired`，端口监听不中断
- [X] T044 [US3] 更新 `internal/application/user_service.go` 的 `DeleteUser` 与
  `internal/persistence/sqlite/store_users.go`：移除入站的确认事务内置 `released_at`，端口回到池中，
  历史行与审计保留
- [X] T045 [US3] 更新 `internal/application/template_service.go` 的 `UpdateProfile`：端口池被缩小到
  不包含既有分配时保存仍成功，返回池外端口清单供界面标识；契约字段变更守卫沿用 001 的
  `superseded_by` 因果链判定
- [X] T046 [US3] 更新 `internal/web/templates/pages/profile_detail.html`：标识落在端口池之外的既有分配

### 测试

- [X] T047 [P] [US3] 新建 `tests/integration/lifecycle_inbound_test.go`：禁用/启用复用原端口、
  轮换期间端口持续可连接、删除后端口回池
- [X] T048 [P] [US3] 新建 `tests/integration/port_reuse_test.go`：端口被回收后再分配给新用户时，
  新用户获得新入站标签、新服务端密钥、新用户密钥与新统计标识，旧连接信息在该端口不可用
- [X] T049 [P] [US3] 新建 `tests/integration/template_pool_shrink_test.go`：缩小端口池后既有用户保持
  可用且被标识为池外，新建用户只能落在新池内
- [X] T050 [P] [US3] 更新 `internal/web/handlers/users_lifecycle_test.go`：禁用、启用、轮换、删除四条
  路径的端口与监听状态渲染
- [X] T051 [US3] 更新 `tests/e2e/lifecycle_test.go`：生命周期端到端证据覆盖端口维度

**Checkpoint**: 用户生命周期的每一步都在端口层面可验证

---

## Phase 6: User Story 4 - 查看用量与运行状态 (Priority: P2)

**Goal**: 仪表盘与列表展示端口、监听状态与端口池使用情况

**Independent Test**: 用多个具有不同用量、端口和状态的用户数据打开仪表盘，核对汇总、筛选、趋势与
端口池使用率

### 实现

- [X] T052 [US4] 更新 `internal/application/dashboard_service.go`：汇总新增端口池已分配数、池容量与
  剩余可分配数
- [X] T053 [P] [US4] 更新 `internal/persistence/sqlite/store_users.go` 的 `ListUsers` 与
  `internal/ports/store.go` 的 `UserFilter`：支持按端口搜索
- [X] T054 [P] [US4] 更新 `internal/web/templates/pages/users_list.html` 与
  `internal/web/templates/fragments/users-table.html`：新增端口列与监听状态列
- [X] T055 [P] [US4] 更新 `internal/web/templates/pages/dashboard.html` 与
  `internal/web/templates/fragments/dashboard-summary.html`：展示端口池使用情况

### 测试

- [X] T056 [P] [US4] 更新 `internal/web/handlers/users_list_test.go`：按端口搜索与端口列渲染
- [X] T057 [P] [US4] 更新 `internal/web/handlers/dashboard_test.go`：端口池使用情况渲染与局部刷新
- [X] T058 [US4] 更新 `tests/e2e/dashboard_test.go`：端口维度的可观测性端到端证据

**Checkpoint**: 管理员可以从界面判断端口分配与入站健康

---

## Phase 7: User Story 5 - 故障后自动恢复并可审计 (Priority: P3)

**Goal**: Xray 重启后按原端口整体重建应监听入站，清理命名空间内孤立入站，全过程可审计

**Independent Test**: 构造启用、手动禁用、配额超限用户后重启 Xray，验证应启用用户端口恢复监听、
其余端口保持关闭，并核对审计记录

### 实现

- [X] T059 [US5] 更新 `internal/application/reconciliation_service.go`：对账范围扩展到入站——
  按 SQLite 中的端口分配重建全部应监听入站，保持不应监听用户的端口关闭 【重试/协调】
- [X] T060 [US5] 更新 `internal/application/reconciliation_service.go` 的漂移路径：面板命名空间内
  SQLite 无记录的入站写入移除意图并在完成时释放端口；命名空间之外的入站 MUST 保持不动
- [X] T061 [US5] 更新 `internal/persistence/sqlite/store_drift.go`：漂移意图区分「未知用户身份」与
  「孤立入站」两类目标，沿用既有 `superseded_by` 显式因果链，不引入时间戳启发式
- [X] T062 [US5] 更新 `internal/web/handlers/dashboard.go` 与 `users.go`：Xray 重启导致全部入站待重建
  时，把受影响用户显示为暂时不可用而非已启用（FR-028）
- [X] T063 [US5] 更新 `internal/application/audit_service.go` 与相关写入点：入站创建/移除、端口分配/
  释放进入审计，摘要含端口但不含任何密钥 【可观测性】【安全】

### 测试

- [X] T064 [P] [US5] 【Xray 契约】门禁 7 与 9 的覆盖落在 `tests/contract/xray/recovery_test.go`
  （重启后全部面板入站消失、端口释放、按原端口重建）、`inbound_test.go`
  （两条入站相互隔离）与 `app_test.go`（应用层按库中端口只重建应监听用户）；
  不再单开 inbound_recovery_test.go，避免同一门禁重复启动真实 Xray 进程
- [X] T065 [P] [US5] 【Xray 契约】门禁 10 的覆盖落在 `tests/contract/xray/inbound_test.go`
  （ListInbounds 只对命名空间内入站计数、RemoveInbound 拒绝越界）、`users_test.go`
  （AddUser/RemoveUser 拒绝越界）与 `app_test.go`（生命周期与对账全流程中运维入站保持监听）；
  同样不再单开 namespace_test.go
- [X] T066 [P] [US5] 新建 `tests/integration/inbound_drift_test.go`：命名空间内孤立入站被移除且端口
  释放并写入审计；运维自有入站不受影响（SC-014）
- [X] T067 [P] [US5] 更新 `tests/integration/failure_matrix_test.go`：按宪章 v1.2.0 在既有六类故障之外
  追加「端口冲突」与「端口被面板外进程占用」两列，覆盖全部变更类型
- [X] T068 [US5] 更新 `tests/e2e/recovery_test.go`：重启窗口内所有端口不可用、60 秒内按原端口收敛的
  端到端证据（SC-006）

**Checkpoint**: 全部用户故事可独立验证

---

## Phase 8: Polish & Cross-Cutting Concerns

**Purpose**: 文档、证据与发布门禁

- [X] T069 [P] 更新 `specs/002-per-user-inbound/contracts/xray-adapter.md`：如实现中发现与实测结论不符
  之处，同步修订契约并在 research.md 追加更正记录
- [X] T070 [P] 更新 `docs/operations.md`：迁移 00004 的破坏性说明与可验证备份/恢复步骤、端口池扩容
  流程、Xray 重启窗口的运维预期 【迁移】
- [X] T071 [P] 更新 `README.md`：入站模板与端口池的最小上手路径，移除预配置入站的前置说明
- [X] T072 清理 001 遗留概念：移除 `internal/domain/identity.go`、`internal/application/template_service.go`、
  `internal/testsupport/app.go` 与 `tests/` 下 bootstrap 用户相关的死代码、文案与夹具，
  并以 `grep -rn "bootstrap" internal tests` 确认无残留业务语义
- [X] T073 运行 `make fmt`、`make vet`、`make test`、`make test-race` 并修复全部问题
- [X] T074 以 `XRAY_BIN=<v26.3.27> XPANEL_REQUIRE_CONTRACT=1 make check` 运行完整发布门禁，
  确认 contracts/xray-adapter.md 的 10 项兼容性门禁全部通过 【Xray 契约】
- [X] T075 新建 `specs/002-per-user-inbound/validation-report.md`：记录自动化证据、门禁结果与
  SC-012/013/014 的验收数据，格式参照 001 的同名文件
- [ ] T076 按 `specs/002-per-user-inbound/quickstart.md` §3 执行人工验收并回填
  SC-001、SC-002、SC-010 的计时与成功率
- [X] T077 在 `tests/contract/xray/main_test.go` 与新增的契约测试文件中复核进程清理：清理闭包 MUST
  引用 `liveRuntime` 的当前字段而非启动时的局部变量；运行整套契约测试后以 `ps` 确认无残留 Xray 进程

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup (Phase 1)**: 无依赖，可立即开始
- **Foundational (Phase 2)**: 依赖 Setup；**阻塞全部用户故事**
- **User Stories (Phase 3–7)**: 均依赖 Phase 2 完成
- **Polish (Phase 8)**: 依赖所需用户故事完成

### User Story Dependencies

- **US1 (P1)**: Phase 2 之后即可开始，不依赖其他故事，构成 MVP
- **US2 (P1)**: 依赖 US1 的入站创建/移除同步路径（T024）
- **US3 (P2)**: 依赖 US1；与 US2 无相互依赖，可并行
- **US4 (P2)**: 依赖 US1 的端口字段；与 US2/US3 可并行
- **US5 (P3)**: 依赖 US1；其重启重建断言在 US2/US3 完成后覆盖面最完整

### Within Each User Story

- 领域与存储 → application service → worker → web handler/模板
- 契约测试可与实现并行编写，但 MUST 在故事完成前通过
- 故事完成后再进入下一优先级

### Parallel Opportunities

- Phase 2 中 T004、T005、T006 三个领域任务可并行；T009 完成后 T010–T013 中不同文件的任务可并行
- Phase 3 的 T026 与 T027 分属不同 handler/模板文件，可并行；T030–T033 四个测试任务可并行
- Phase 4 的 T038、T039、T040 可并行
- Phase 5 的 T047、T048、T049、T050 可并行
- Phase 6 的 T053、T054、T055 可并行
- Phase 7 的 T064、T065、T066、T067 可并行
- Phase 8 的 T069、T070、T071 可并行

---

## Parallel Example: User Story 1

```bash
# 测试任务并行：
Task: "新建 tests/contract/xray/inbound_test.go 覆盖门禁 2–4"
Task: "新建 tests/contract/xray/inbound_failure_test.go 覆盖门禁 8"
Task: "新建 tests/integration/port_allocation_test.go"
Task: "新建 internal/web/handlers/templates_test.go"

# 界面任务并行（不同文件）：
Task: "更新 internal/web/handlers/templates.go 与 profile_form.html"
Task: "更新 internal/web/handlers/users.go 与 user_form.html"
```

---

## Implementation Strategy

### MVP First (User Story 1 Only)

1. 完成 Phase 1 Setup
2. 完成 Phase 2 Foundational（关键，阻塞全部故事）
3. 完成 Phase 3 US1
4. **停下来验证**：按 quickstart.md §3.1 与 §3.2 独立验收 US1
5. 此时面板已能交付「每用户独立端口」的核心价值

### Incremental Delivery

1. Setup + Foundational → 模型替换就绪
2. US1 → 独立验收 → MVP
3. US2 → 配额闭环作用于端口
4. US3 → 生命周期与端口回收
5. US4 → 端口维度可观测
6. US5 → 重启恢复与漂移清理
7. Phase 8 → 发布门禁与验收证据

### 风险提示

- Phase 2 的 T014 迁移为破坏性变更，执行前 MUST 按 `docs/operations.md` 生成可验证备份
- T024 的部分失败补偿是本功能最容易出错之处，实测已证明直接重试会因 `existing tag found` 永久卡住
  （research.md R-003），该路径 MUST 有专门的契约测试（T031）覆盖

---

## Phase 9: Convergence

- [X] T078 **CRITICAL** 重构 `internal/worker/synchronizer.go` 的凭证轮换状态机并补充
  `internal/worker/synchronizer_rotation_test.go` 与 `tests/contract/xray/` 真实故障契约，确保正常执行、
  RPC 失败、进程崩溃、租约回收和协调器重放的每个持久阶段都不会保留零受管客户端的专属入站，
  同时保持原端口监听、不可变统计身份和历史流量归属，并使旧凭证在成功后失效 per
  Constitution II / FR-017 / FR-019 (contradicts)
- [X] T079 扩展 `internal/ports/xray.go`、`internal/adapter/xray/inbound.go` 与
  `internal/application/template_service.go` 的入站模板能力门禁，显式验证探针入站能够成功移除且
  用户级上下行统计可用；不得忽略探针清理失败，缺少 HandlerService 移除能力或
  `statsUserUplink`/`statsUserDownlink` 时必须以安全中文原因标记模板不兼容，并增加固定 Xray 契约与
  缺失 policy 的回归测试 per FR-005 / T021 / plan: Xray capability gate (partial)
- [X] T080 在 `internal/application/user_service.go`、`internal/ports/store.go`、
  `internal/persistence/sqlite/` 和 `internal/web/` 增加外部端口占用后的管理员更换端口流程：在单个
  SQLite 事务中校验池范围与唯一性、更新专属入站端口及同步意图、记录旧/新端口审计，并以 revision
  和请求幂等键防止并发或重复提交产生部分状态；补充 Handler、集成及故障矩阵测试 per US1/AC4 /
  FR-010 (missing)
- [X] T081 重构 `internal/application/reconciliation_service.go` 与漂移持久化归属，使面板命名空间内的
  孤立入站在不存在兼容模板、所有模板均归档或模板记录为空时仍能持久化移除意图、完成有界重试、释放
  运行时端口并写入审计；补充零模板和全归档模板场景测试，且保持命名空间外入站不变 per US5/AC4 /
  FR-031 (partial)
- [ ] T082 按 `specs/002-per-user-inbound/quickstart.md` §3 完成 T076 的人工验收，并在
  `specs/002-per-user-inbound/validation-report.md` 回填 SC-001 首次交付计时、SC-002 各类操作至少
  20 次的 P95，以及 SC-010 纯键盘和 360×640、390×844 视口的成功率与观察记录 per
  SC-001 / SC-002 / SC-010 / T076 (missing)
- [X] T083 稳定 `tests/contract/xray/app_test.go` 的
  `TestLiveAppBatchedCollectionToleratesQuantizationButDetectsRestart` 端口准备与同步等待逻辑，使合法的
  临时 `port_unavailable` 不会令 21 用户批量采集断言偶发只看到 20 个目标；重复运行完整
  `XRAY_BIN=<v26.3.27> XPANEL_REQUIRE_CONTRACT=1 make check` 并记录稳定通过证据 per
  plan: Testing / T073-T074 (partial)

---

## Phase 10: Convergence

- [X] T084 **CRITICAL** 修正 `internal/worker/synchronizer.go` 仍会在凭证轮换的两次非原子 RPC 之间留下
  空客户端入站的问题：为固定 Xray v26.3.27 设计并持久化可重放的轮换过渡状态（例如不对外暴露、使用
  独立临时身份与密钥的有界 safety client），使 `RemoveUser` 前、旧客户端移除后、期望客户端加入后及
  清理过渡身份后的每个进程崩溃、租约丢失和 RPC 失败边界都满足入站客户端数从不为 0、端口持续监听，
  最终只保留原不可变统计身份对应的新凭证且旧凭证失效；过渡凭证不得进入连接信息或造成流量归属遗漏。
  同步更新 `spec.md`、`research.md`、`contracts/xray-adapter.md` 对“一个逻辑用户、稳态恰好一个客户端、
  轮换时允许有界内部过渡客户端”的明确约束，并在 `internal/worker/synchronizer_rotation_test.go` 与真实
  Xray 契约中逐个 RPC 边界注入崩溃，断言客户端数始终大于 0、端口可连接、最终客户端数为 1、统计历史
  连续；不得再以“单个租约步骤内未持久化”代替故障安全，也不得用移除整条入站造成监听中断 per
  US3/AC3 / FR-017 / FR-019 / T078 (contradicts)
- [X] T085 完成 FR-005 的用户级统计前置门禁：扩展 `ports.TemplateCapabilities`、
  `adapter/xray.ValidateTemplate` 与模板验证流程，在一次性探针入站上产生经过 SS2022 身份认证的最小
  TCP/UDP 流量并验证该探针身份的 uplink/downlink 两个计数器均可读取，随后可靠移除探针及清理计数；
  缺少 `StatsService`、`statsUserUplink` 或 `statsUserDownlink` 时必须在创建任何用户前把模板标为不兼容
  并展示安全中文原因，不能仅显示事后 `StatsSuspect` 提示。为启用/缺失 policy 的固定 Xray 配置分别增加
  真实契约和应用回归测试，并更正 `research.md`、`contracts/config.md`、
  `validation-report.md` 中与最终可验证语义不一致或提前宣称通过的内容 per FR-005 / T079 (contradicts)

---

## Phase 11: Convergence

- [X] T086 **CRITICAL** 修正面板专属入站内未知客户端的对账与最后客户端防线：
  `internal/application/reconciliation_service.go` 必须把面板命名空间入站中除该分配期望身份之外的所有
  客户端都视为漂移，不得因其统计标识没有 `xpanel-` 前缀而跳过；轮换过渡身份仅在对应开放轮换意图存在
  时临时豁免，意图收敛后必须清理。`internal/adapter/xray/handler.go` 的 `RemoveUser` 守卫必须按移除后
  是否仍有有效受管客户端判断，不能用包含外部/未知身份的总用户数放行；当期望身份缺失时，应先恢复期望
  客户端再清理未知身份，任何顺序都不得制造空受管客户端入站。补充 fake、集成及固定 Xray 契约，覆盖
  面板入站被注入非命名空间身份、只剩未知身份、轮换过渡中对账和重复清理，断言最终恰好一个期望身份、
  未知凭证失效且其他入站不受影响 per Constitution I/II / FR-019 / FR-029 / T061 (contradicts)
- [X] T087 完成 T084 尚缺的真实轮换故障契约：修正
  `internal/worker/synchronizer_rotation_test.go` 的边界循环，使最后一次 `RemoveUser(过渡身份)` 成功后、
  `ConfirmSync` 之前也实际触发崩溃；在 `tests/contract/xray/` 用真实应用 service + synchronizer 而非手工
  Adapter 四步调用，分别在四次变更 RPC 成功后的每个边界中断、回收租约并重放。测试必须用轮换前后的
  完整 SS2022 连接信息发起新握手，证明全程端口监听、恢复后旧凭证拒绝、新凭证成功、最终只有原不可变
  统计身份、过渡身份不进入连接信息或计量且历史流量连续，再据此更正 `validation-report.md` 的 T084 证据
  per T084 / US3/AC3 / FR-017 / FR-019 (partial)
- [X] T088 修正统计能力探针的方法、网络与计数隔离：把模板的 `Method` 和 `Network` 从
  `ports.TemplateProbe` 一直传到 `internal/adapter/xray/probe.go`，禁止
  `exchangeThroughInbound` 硬编码 `2022-blake3-aes-256-gcm`；按模板网络发送 TCP、UDP 或两者，满足
  T085 的 TCP/UDP 探针要求。每次验证使用不可碰撞的探针统计身份，并在结束时重置/清理其 uplink、
  downlink 计数，避免旧探针数据让后续验证误通过。增加 AES-128/AES-256 × TCP/UDP/tcp_udp 的固定
  Xray 矩阵，以及各组合缺失 policy 和重复验证的回归测试，断言探针入站与统计残留均被清理 per
  FR-005 / FR-040 / T085 (partial)
- [X] T089 使模板兼容性证据绑定当前 Xray 启动纪元：在持久化层以版本化迁移记录模板最后通过门禁时的
  boot epoch（或等价能力世代），在协调器探测到 Xray 重启、重连或能力世代变化时原子地把既有兼容模板
  置为待验证并排队重跑真实能力门禁；`UserService.CreateUser` 必须只接受已对当前世代验证通过的模板。
  增加应用和固定 Xray 契约：先在完整 policy 下验证成功，再以同一管理端点重启到缺失 policy 的配置，
  确认旧 compatible 缓存立即失效、新建用户被拒绝，恢复 policy 并重新验证后才允许创建 per
  FR-005 / FR-029 (partial)
- [ ] T090 完成并关闭已有 T076/T082：严格按 `specs/002-per-user-inbound/quickstart.md` §3 执行人工
  验收，在 `validation-report.md` 回填 SC-001 首次交付实测时间、登录/搜索/创建/编辑/状态切换各至少
  20 次的 SC-002 P95、SC-010 纯键盘与 360×640/390×844 两个视口的逐流程成功率和观察记录；不得以
  自动化结构检查替代人工结果，也不得在仍为“待执行”时宣称 Phase 完成 per
  SC-001 / SC-002 / SC-010 / T076 / T082 (missing)
- [X] T091 解决统计探针实现与 `plan.md` “本功能不新增任何依赖、go.mod/go.sum 零变化”决策的冲突：
  优先使用已批准的 Xray Adapter 依赖面实现探针并移除 `github.com/sagernet/sing`、
  `github.com/sagernet/sing-shadowsocks` 的新增直接依赖；若确实无法替代，则必须在 `plan.md` 的 Primary
  Dependencies、Constitution Check 与 Complexity Tracking 中显式记录这两个固定版本依赖的必要性、
  安全边界和单二进制影响。执行 `go mod tidy -diff`、`CGO_ENABLED=0 go build ./cmd/xpanel` 与完整发布
  门禁并更新验证证据 per plan: Primary Dependencies / plan: Constraints (contradicts)

---

## Phase 12: Convergence

- [X] T092 **CRITICAL** 完成 T086 的“有效受管客户端”和轮换意图边界：把
  `internal/application/reconciliation_service.go` 的过渡身份豁免从 `HasOpenOperation` 改为只匹配该
  分配当前未完成且 `reason='rotate'` 的操作，普通 create/disable/reconcile 等未完成意图不得保护遗留
  safety identity；重构 `ports.RemoveUserCommand`、`internal/adapter/xray/handler.go` 与 fake 的最后客户端
  守卫，使其只认可该移除对应的精确期望身份/轮换过渡身份配对，不能把任意 `xpanel-` 前缀身份或用户总数
  当成有效后继。补充 fake、集成和固定 Xray 回归：普通未完成操作期间遗留 safety identity 必须被排队
  清理；“期望身份 + 前缀内未知身份”和“期望身份 + 前缀外未知身份”两种情况下移除期望身份均被拒绝；
  开放轮换时精确 safety identity 仍允许保护过渡；任意执行顺序最终都只剩期望身份 per
  Constitution I/II / FR-019 / FR-029 / T086 (partial)
- [X] T093 完成 T088 的真实网络组合与统计清理证据：`internal/adapter/xray/inbound.go` 创建探针入站时
  必须使用 `probe.Network`，不得继续硬编码 `domain.NetworkTCPUDP`；将
  `tests/contract/xray/template_test.go` 的缺失 policy 契约扩为 AES-128/AES-256 ×
  TCP/UDP/tcp_udp 全六组合，并让每个组合都覆盖首次验证、重复验证和缺失 policy。为每次生成的探针身份
  提供可测试的观察点，逐方向断言验证结束后 uplink/downlink 计数为零且探针入站、端口均已移除；取消或
  deadline 场景下的清零应使用独立有界清理上下文，不得因原请求已取消而跳过 per
  FR-005 / FR-040 / T085 / T088 (partial)
- [X] T094 **CRITICAL** 修正 T089 的能力世代判定和启动迁移门禁：不得用量化 boot epoch 字符串的精确
  不等直接判定重启，应复用 `domain.RestartConfirmed` 的 uptime/一秒容差或持久化稳定能力世代，证明同一
  Xray 进程相邻探测的 ±1 秒量化抖动不会反复使模板失效，而真实重启、任务定义中的断线重连或能力世代变化
  会原子地使旧证据失效并重跑门禁。迁移 `00007`（必要时追加修订迁移）必须把升级前没有
  `validated_boot_epoch` 的 compatible 模板置回待验证；`UserService.CreateUser` 仅在当前实例世代和模板
  验证世代都非空且匹配时放行。增加持久化/应用测试覆盖旧库升级、空 epoch 拒绝、同进程量化抖动、同世代
  重连与真实重启，并保留固定 Xray 缺失/恢复 policy 契约 per FR-005 / FR-029 / T089 (partial)
- [ ] T095 完成并关闭 T076/T082/T090：严格按 `specs/002-per-user-inbound/quickstart.md` §3 执行人工
  验收，在 `validation-report.md` 回填 SC-001 从首次登录到复制连接信息的实测时间；登录、搜索、创建、
  编辑、状态切换各至少 20 次的原始样本或可复核汇总及 P95；纯键盘和 360×640、390×844 两个视口下
  登记模板、创建、编辑、禁用、删除各流程的成功次数、总次数、成功率与观察记录。完成前不得把自动化
  accessibility 测试替代人工结论，也不得宣称 Phase 已完成 per
  SC-001 / SC-002 / SC-010 / T076 / T082 / T090 (missing)
- [X] T096 消除 T091 后 `plan.md` 内部仍存在的依赖决策矛盾：更新 Constitution Check 的“技术与运行
  约束”证据，不得继续写“零新增依赖”，而应与 Primary Dependencies 和 Complexity Tracking 一致，明确
  两个模块由间接提升为固定版本直接依赖、`go.sum` 与最终单二进制代码集合不变；全篇检索并清除仍把当前
  决策描述为 `go.mod` 零变化或无新增直接依赖的陈述，再运行 `go mod tidy -diff`、
  `CGO_ENABLED=0 go build ./cmd/xpanel` 与完整发布门禁并更新验证证据 per
  plan: Primary Dependencies / plan: Constitution Check / plan: Complexity Tracking / T091 (contradicts)

---

## Phase 13: Convergence

- [X] T097 **CRITICAL** 修复 T092 引入的 `RemoveUser` 契约回归：不得继续通过
  `domain.ExpectedIdentityForInbound(inboundTag) == inboundTag` 猜测该入站的期望统计身份；扩展
  `ports.RemoveUserCommand` 显式携带期望统计身份（或提供同等强度且可验证的 Adapter 契约），并更新
  `internal/worker/synchronizer.go` 的未知身份清理与轮换调用、真实 Xray Adapter、fake 及全部契约调用点。
  最后客户端守卫必须只认可命令指定的期望身份和由它派生的精确 safety identity，同时允许在期望身份仍在时
  移除任意前缀内/外未知身份。不得通过删除既有契约或强行令所有测试夹具的 tag 与 identity 相等来掩盖接口
  歧义；恢复 `TestLiveInboundLifecycleContract`、`TestLiveUnknownIdentityInsideAPanelInboundCanBeCleanedSafely`、
  `TestLiveRotationTransitionKeepsAtLeastOneClientAtEveryBoundary`、
  `TestLiveUserMutationContractOnADedicatedInbound`，并补充缺失期望身份参数的前置拒绝测试 per
  Constitution II / Constitution: 开发流程与质量门禁 / FR-019 / T092 (partial)
- [X] T098 **CRITICAL** 修复 T094 的能力世代仍会漏检真实重启且与既定重连语义相反的问题：能力世代推进
  除带一秒容差的 boot epoch 外，还必须使用持久化的 uptime 单调性、已观察到的 unavailable→reachable
  重连或其它不会被秒级量化吞掉的可靠信号；固定 Xray 在相邻启动 epoch 相同或仅差一秒时重启，旧模板证据
  也必须立即失效，新建用户在当前世代门禁完成前被拒绝。按 T089/T094 原文，已被协调器明确观察到的断线重连
  必须触发保守重新验证，不能由 `TestReconnectWithoutRestartKeepsCapabilityEvidence` 锁定为保持 compatible；
  同时更新 `TestLiveAppRestartReconcilesActiveUsersOnly` 以断言稳定的 capability generation 前进，不再要求
  量化 boot epoch 字符串必然变大。覆盖快速重启、同 epoch 重启、显式重连、±1 秒无重启抖动和缺失/恢复
  policy，并更正 `validation-report.md` 后重跑固定 Xray v26.3.27 的完整 `make check` per
  FR-005 / FR-029 / T089 / T094 / Constitution: 开发流程与质量门禁 (contradicts)

---

## Phase 14: Convergence

- [X] T099 **HIGH** 把 T098 的「所有应监听面板入站同时消失」重启旁证从持续电平改为一次性、可持久恢复的
  边沿事件：`internal/application/reconciliation_service.go` 不得在同一批缺失入站尚未恢复期间，每轮都以
  `SuspectedRestart=true` 重复推进 `capability_generation`、反复使刚完成的模板能力验证失效。优先利用每条
  分配已持久化的 `ObservedPresent`（或同等强度的持久化事件标记）识别「此前至少一条已确认存在 → 本轮所有
  期望入站均缺失」的转换；首次转换仍须立即推进世代并触发门禁，后续持续缺失轮次保持同一世代，而入站恢复
  后再次整体消失必须作为新的事件再次推进。补充应用/集成回归，至少连续执行两轮不 drain 的对账并断言首轮
  仅推进一次、第二轮不再 revalidate，再覆盖恢复后第二次整体消失；保留显式 unavailable→reachable 每个独立
  重连事件只推进一次、同进程 ±1 秒抖动不推进以及固定 Xray 快速重启契约，更新验证证据并重跑固定 Xray
  v26.3.27 的完整 `make check` per FR-005 / FR-029 / T089 / T094 / T098 (partial)
