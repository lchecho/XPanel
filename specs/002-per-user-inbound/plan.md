# Implementation Plan: 每用户专属入站的多用户管理

**Branch**: `002-per-user-inbound` | **Date**: 2026-09-06 | **Spec**: [spec.md](spec.md)

**Input**: Feature specification from `specs/002-per-user-inbound/spec.md`

## Summary

把多用户的承载方式从“运维预配置的一条共享入站 + 面板管理其中的动态用户”替换为“面板为每个用户
创建一条专属入站”。面板在入站模板声明的端口池中为每个用户分配唯一端口，用定向 protobuf 通过
HandlerService 在运行时创建 SS2022 多用户入站（内含恰好一个受管客户端），并自行生成该入站的服务端
密钥与用户密钥。禁用、配额超限和删除统一通过移除整条入站实现，使端口停止监听；凭证轮换在入站内
完成，端口不中断。

SQLite 继续作为唯一权威状态，投影范围从“用户列表”扩大到“入站 + 端口 + 用户列表”。Xray 重启后
运行时入站全部消失，由协调器按保存的端口分配整体重建，面板不写入任何 Xray 配置文件。001 已实现
并通过发布门禁的租约 fencing、周期边界事务、分批采集一致性、认证原子性和漂移因果链全部复用，
改动集中在入站与端口的建模、同步动作扩展和相应的界面与契约。

## Technical Context

**Language/Version**: Go `1.26.8`；`go`/`toolchain`、CI 与发布镜像沿用同一补丁基线

**Primary Dependencies**: 与 001 完全一致，本功能不新增任何依赖（已验证 `go.mod`/`go.sum` 零变化）：
标准库 `net/http`, `html/template`, `embed`, `log/slog`；`github.com/xtls/xray-core@v1.260327.0`；
`modernc.org/sqlite@v1.58.0`；`github.com/pressly/goose/v3@v3.28.0`；`golang.org/x/crypto@v0.56.0`；
`github.com/alexedwards/scs/v2@v2.9.0`；`github.com/gorilla/csrf@v1.7.3`；本地嵌入 HTMX `2.0.10`。
运行时入站构建只使用 Adapter 依赖集合内已有的 `core`、`app/proxyman`、`proxy/shadowsocks_2022`、
`common/net`、`common/protocol`、`common/serial`，明确不引入 `infra/conf`（见 research.md R-001）

**Storage**: 本地 SQLite，WAL、foreign keys、5 秒 busy timeout、`synchronous=FULL`、嵌入式顺序
migrations（本功能新增 `00004` 起）；数据库外 root-only 32-byte AEAD 主密钥

**Testing**: 标准库 `testing`/`httptest`、表驱动与 fuzz、真实临时文件 SQLite、手写 Xray fake、
固定 Xray v26.3.27 真实进程契约测试、`go test -race`；合并门禁 `make check` 含契约套件，
`XPANEL_REQUIRE_CONTRACT=1` 时缺少 `XRAY_BIN` 视为失败

**Target Platform**: 与 Xray 同机的 Linux amd64/arm64 单实例；开发支持 macOS；生产构建
`CGO_ENABLED=0` 单二进制

**Project Type**: 同源 SSR Web 应用 + 后台 worker + 本机管理 CLI 的模块化单体

**Performance Goals**: 沿用 001 基线（1 vCPU / 1 GiB、与 Xray 同机、热状态、单管理员串行操作），
并按 spec 的 SC-002/003/004/005/006 验收。新增约束：20 个用户对应 20 条入站，重启后整体重建
MUST 在 60 秒内完成（SC-006）

**Constraints**: 单控制进程、Xray API 仅回环、无 Node/CDN/WebSocket、不保存轮询原始样本、
不承诺切断已建立连接、所有敏感输出 no-store 且日志脱敏；**新增**：面板只操作面板命名空间内的入站，
不得写入或编辑 Xray 配置文件，不得创建 `Users` 为空的入站，端口唯一性由面板保证而非依赖 Xray 报错

**Scale/Scope**: 单 Xray 实例、一个或多个入站模板、单管理员、最多 20 个活跃用户；每用户恰好一条
专属入站、一个端口、一个受管客户端

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-checked after Phase 1 design.*

| Gate | Pre-Research | Post-Design | Evidence |
|---|---|---|---|
| I. SQLite 是唯一权威状态 | PASS | PASS | 入站、端口分配与用户意图全部落库；Xray 中的入站与用户均为可重建投影，重启后按库中端口分配整体重建 |
| II. Xray 集成隔离且版本化 | **CONDITIONAL** | PASS | 固定 runtime/module，protobuf 只在 `internal/adapter/xray`，不引入 `infra/conf`；宪章已于 commit `5a5867b` 修订至 v1.2.0 授权并约束面板管理入站生命周期 |
| III. 流量核算准确且语义诚实 | PASS | PASS | 沿用用户级计数器与 5 秒非破坏性读取；游标与日/周期聚合同事务；软配额文案不变（research.md R-007） |
| IV. 状态变更幂等且可恢复 | PASS | PASS | 入站创建/移除纳入既有持久化同步操作与租约 fencing；AddInbound 部分失败的补偿路径已显式建模（R-003） |
| V. 安全与可观测性默认开启 | PASS | PASS | 回环 gRPC、AEAD 密钥、审计与 slog 脱敏；新增端口与入站标签进入审计，服务端密钥改由面板生成不再经管理员输入 |
| VI. SSR 与渐进增强优先 | PASS | PASS | 端口分配、入站状态均为服务端渲染字段；无新增脚本依赖 |
| 技术与运行约束 | PASS | PASS | 零新增依赖；单 writer；RPC 不在写事务内；模式变更走版本化迁移 |
| 开发与发布质量门禁 | PASS | PASS | 入站生命周期、端口冲突、重启重建、漂移清理均规划真实 Xray 契约测试与故障矩阵覆盖 |

### Gate II 的处置结果（已完成）

宪章 1.1.0 在原则 I 写有“基础监听、路由和传输配置**可以**由运维管理”，在原则 II 只枚举了
“动态用户操作 MUST 使用 HandlerService；流量读取 MUST 使用 StatsService”。本功能让面板在运行时
创建和移除入站监听：

- 不违反字面禁令：条文用“可以”表述运维管理监听，并未禁止面板管理；面板同样经 HandlerService 且
  仍不编辑配置文件、不重启 Xray。
- 但条文没有授权这一职责，也没有为它设定约束（命名空间隔离、端口唯一性、空用户列表禁令、
  部分失败补偿）。按 Governance“规格、计划、任务和实现与本宪章冲突时 MUST 修改下游文档或先完成
  宪章修订，不得静默绕过”，此处应先修订宪章。

**处置**：已完成。宪章在 commit `5a5867b` 修订至 v1.2.0，在原则 I 把入站与端口分配纳入权威状态并
确立面板保留命名空间边界，在原则 II 增补面板管理入站生命周期的授权与约束，在质量门禁追加端口冲突与
端口被面板外进程占用的强制故障覆盖。落地条文：

> - 面板 MAY 通过 HandlerService 创建和移除入站，但 MUST 限定在面板保留命名空间内；面板命名空间
>   之外的入站、路由与传输配置 MUST 保持只读。
> - 面板创建的入站 MUST 在 SQLite 中持有权威的端口分配与期望状态；端口唯一性 MUST 由面板保证，
>   不得依赖 Xray 拒绝重复端口。
> - 面板 MUST NOT 创建或保留没有受管客户端的入站；停止访问 MUST 通过移除整条入站实现。
> - 入站创建失败 MUST 视为可能已部分生效，MUST 定义读后写确认与补偿移除后再重试。

修订已随宪章 v1.2.0 落地，本计划的 Gate II 由 CONDITIONAL 改判为 PASS，实现阶段无阻塞。

## Project Structure

### Documentation (this feature)

```text
specs/002-per-user-inbound/
├── spec.md
├── plan.md                  # 本文件
├── research.md              # Phase 0 输出
├── data-model.md            # Phase 1 输出
├── quickstart.md            # Phase 1 输出
├── contracts/               # Phase 1 输出
│   ├── xray-adapter.md      # 入站生命周期契约与兼容性门禁（改动最大）
│   ├── http.md              # 端口与入站相关的页面与表单契约
│   └── config.md            # 端口池与部署前置配置
├── checklists/
│   └── requirements.md
└── tasks.md                 # 由 /speckit-tasks 生成，本命令不创建
```

`contracts/cli.md` 不随本功能变化，继续沿用 `specs/001-xray-user-management/contracts/cli.md`。

### Source Code (repository root)

沿用 001 已建立的模块化单体结构，本功能的改动点标注在右侧。

```text
cmd/xpanel/main.go                      # 装配新增端口分配器与入站同步；CLI 入口不变

internal/
├── config/                             # 改：新增端口池与监听地址校验
├── domain/
│   ├── inbound.go                      # 新：专属入站状态机与命名空间标签规则
│   ├── port.go                         # 新：端口池、分配与释放的纯函数与不变量
│   ├── profile.go                      # 改：访问配置 → 入站模板
│   ├── user.go / quota.go / traffic.go / sync.go   # 基本不变，sync 增加入站动作
├── application/
│   ├── template_service.go             # 改自 profile_service：模板登记与能力校验
│   ├── user_service.go                 # 改：创建时分配端口并生成入站意图
│   ├── inbound_service.go              # 新：入站期望状态的编排
│   ├── reconciliation_service.go       # 改：对账范围扩到入站与端口
│   └── traffic_service.go / quota_service.go / auth_service.go  # 基本不变
├── ports/                              # 改：Adapter 接口增加 CreateInbound/RemoveInbound/ListInbounds
├── persistence/sqlite/
│   ├── migrations/00004_*.sql          # 新：入站模板端口池、专属入站、端口分配
│   ├── store_inbounds.go               # 新：入站与端口分配的事务写入
│   └── store_*.go                      # 改：用户创建/删除路径联动端口分配
├── adapter/xray/
│   ├── inbound.go                      # 新：定向 protobuf 构建、错误映射、预绑定探测
│   └── fake/                           # 改：fake 支持入站生命周期与端口语义
├── worker/
│   ├── synchronizer.go                 # 改：处理入站创建/移除动作与部分失败补偿
│   └── reconciler.go                   # 改：重启后整体重建
└── web/                                # 改：端口列、监听状态、模板表单与端口冲突提示

tests/
├── contract/xray/                      # 新增：入站生命周期、端口冲突、部分失败、重启重建
├── integration/                        # 新增：端口唯一性、隔离性、模板端口池变更
└── e2e/                                # 改：成功标准证据覆盖 SC-012/013/014
```

**Structure Decision**: 沿用 001 的模块化单体与 ports/adapters 边界，不新增顶层目录。入站与端口
作为新的领域概念进入 `internal/domain`，其 Xray 侧实现被限制在 `internal/adapter/xray`，
业务层只通过 `internal/ports` 的接口感知“创建/移除入站”，以满足宪章 II 的隔离要求。

## Complexity Tracking

| Violation | Why Needed | Simpler Alternative Rejected Because |
|---|---|---|
| 宪章原则 II 需 MINOR 修订以授权面板管理入站生命周期（**已完成**，v1.2.0 / commit `5a5867b`） | 规格的核心价值是每用户独立入站与端口，必须由面板在运行时创建和移除入站；修订前的条文只授权动态用户与统计读取 | 保持共享入站（即 001 模型）无法提供端口级隔离，与本规格的既定目标冲突；由运维为每个用户手工预配置入站则把面板降级为半自动工具，且无法满足 SC-001 的 5 分钟交付与 SC-013 的端口分配保证 |
| 端口分配引入 SQLite 侧唯一性约束与面板预绑定探测 | 已验证 Xray 不拒绝重复端口且外部占用时仍会留下已注册入站，唯一性与占用检测只能由面板承担 | 依赖 Xray 报错的方案已被实测否定（research.md R-002/R-003） |
