# Implementation Plan: Xray 多用户管理 MVP

**Branch**: `001-xray-user-management` | **Date**: 2026-09-04 | **Spec**: [spec.md](spec.md)

**Input**: Feature specification from `specs/001-xray-user-management/spec.md`

## Summary

构建一个 Go 模块化单体，通过服务端渲染页面管理单个 Xray 实例中预配置的
Shadowsocks 2022 AES 多用户入站。SQLite 保存管理员、访问配置、用户、凭证、配额、
流量聚合、同步意图和审计的权威状态；Xray 仅作为可重建运行时投影。后台采集器每 5 秒
读取非破坏性用户计数，短事务更新游标与聚合并触发配额封禁；持久化同步 worker 与
协调器负责用户增删、轮换、重启恢复和失败重试。

浏览器界面使用 `html/template` 和原生表单，HTMX 只增强局部只读刷新。Xray protobuf
被隔离在固定版本 Adapter 内；同一二进制同时提供 HTTP 服务、首次管理员初始化和本机
密码重置命令。

## Technical Context

**Language/Version**: Go `1.26.8`；`go`/`toolchain`、CI 和发布镜像使用同一补丁基线  
**Primary Dependencies**: 标准库 `net/http`, `html/template`, `embed`, `log/slog`；
`github.com/xtls/xray-core@v1.260327.0`；`modernc.org/sqlite@v1.58.0`；
`github.com/pressly/goose/v3@v3.28.0`；`golang.org/x/crypto@v0.56.0`；
`github.com/alexedwards/scs/v2@v2.9.0`；`github.com/gorilla/csrf@v1.7.3`；
本地嵌入 HTMX `2.0.10`  
**Storage**: 本地 SQLite，WAL、foreign keys、5 秒 busy timeout、`synchronous=FULL`、
嵌入式顺序 migrations；数据库外 root-only 32-byte AEAD 主密钥  
**Testing**: 标准库 `testing`/`httptest`、表驱动和 fuzz tests、真实临时文件 SQLite、
手写 Xray fake、固定 Xray v26.3.27 真实进程契约测试、`go test -race`；合并门禁
`make check` 包含契约套件，`XPANEL_REQUIRE_CONTRACT=1` 时缺少 `XRAY_BIN` 视为失败  
**Target Platform**: 与 Xray 同机的 Linux amd64/arm64 单实例；开发支持 macOS；生产构建
`CGO_ENABLED=0` 单二进制  
**Project Type**: 同源 SSR Web 应用 + 后台 worker + 本机管理 CLI 的模块化单体  
**Performance Goals**: 95% 管理操作 2 秒内反馈；95% 流量展示不晚于 10 秒；95% 正常
配额越界 10 秒内阻止新连接，全部案例 30 秒内完成或显示待同步；重连后 60 秒内收敛  
**Constraints**: 单控制进程、Xray API 仅回环、预配置 inbound、无 Node/CDN/WebSocket、
不保存轮询原始样本、不承诺切断已建立连接、所有敏感输出 no-store 且日志脱敏  
**Scale/Scope**: 单 Xray 实例、一个或多个预配置 profile、单管理员、最多 20 个活跃用户/
访问分配、每用户恰好一个 allocation

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-checked after Phase 1 design.*

| Gate | Pre-Research | Post-Design | Evidence |
|---|---|---|---|
| SQLite 是唯一权威状态 | PASS | PASS | 所有业务事实、游标、聚合、意图与审计落库；Xray 仅为投影 |
| Xray 集成隔离且版本化 | PASS | PASS | 固定 runtime/module；protobuf 只在 `internal/adapter/xray` |
| 流量核算准确且语义诚实 | PASS | PASS | 5 秒非破坏性读取；游标与日/周期聚合同事务；软配额文案 |
| 状态变更幂等且可恢复 | PASS | PASS | 正交状态、revision、domain command、持久同步操作和协调器 |
| 安全与可观测性默认开启 | PASS | PASS | 回环 gRPC、Argon2id、session/CSRF、AEAD、审计与 slog 脱敏 |
| SSR 与渐进增强优先 | PASS | PASS | 原生 POST/PRG；HTMX 只增强 fragment；模板/静态资源 go:embed |
| 技术与运行约束 | PASS | PASS | Go 单体、SQLite 单 writer、短事务、无 Redis/队列/微服务 |
| 开发与发布质量门禁 | PASS | PASS | unit/integration/handler/Xray contract/E2E/backup-restore 覆盖已规划 |

没有宪章违规或需要例外审批的复杂度项。

## Project Structure

### Documentation (this feature)

```text
specs/001-xray-user-management/
├── spec.md
├── plan.md
├── research.md
├── data-model.md
├── quickstart.md
├── contracts/
│   ├── http.md
│   ├── cli.md
│   ├── config.md
│   └── xray-adapter.md
├── checklists/
│   └── requirements.md
└── tasks.md                 # 由 $speckit-tasks 生成，本命令不创建
```

### Source Code (repository root)

```text
go.mod
go.sum
cmd/
└── xpanel/
    └── main.go              # serve/admin init/admin reset-password 入口

internal/
├── config/
│   ├── config.go            # JSON 配置、权限和回环目标校验
│   └── config_test.go
├── domain/
│   ├── identity.go
│   ├── profile.go
│   ├── user.go
│   ├── quota.go
│   ├── traffic.go
│   ├── sync.go
│   └── *_test.go            # 状态、周期、计数与边界单元测试
├── application/
│   ├── auth_service.go
│   ├── profile_service.go
│   ├── user_service.go
│   ├── quota_service.go
│   ├── traffic_service.go
│   ├── reconciliation_service.go
│   └── *_test.go
├── ports/
│   ├── store.go             # domain/application 所需持久化 port
│   ├── xray.go              # 稳定的项目类型 Adapter port
│   └── clock.go
├── persistence/
│   └── sqlite/
│       ├── db.go
│       ├── store.go
│       ├── sessions.go
│       ├── queries/
│       ├── migrations.go
│       ├── migrations/
│       │   └── 00001_initial.sql
│       └── *_test.go
├── adapter/
│   └── xray/
│       ├── client.go
│       ├── handler.go
│       ├── stats.go
│       ├── errors.go
│       └── *_test.go
├── security/
│   ├── password.go          # Argon2id PHC 编码与比较
│   ├── secrets.go           # XChaCha20-Poly1305 字段加密
│   ├── tokens.go
│   └── *_test.go
├── worker/
│   ├── collector.go
│   ├── synchronizer.go
│   ├── reconciler.go
│   ├── scheduler.go
│   └── *_test.go
└── web/
    ├── server.go
    ├── routes.go
    ├── middleware/
    ├── handlers/
    ├── views/
    ├── templates/
    │   ├── layouts/
    │   ├── pages/
    │   └── fragments/
    ├── static/
    │   ├── app.css
    │   └── htmx-2.0.10.min.js
    └── *_test.go

tests/
├── contract/
│   └── xray/                # 固定真实 Xray binary/module 契约
├── integration/             # SQLite、HTTP/auth/CSRF、worker 与 fake Xray
└── e2e/                     # 浏览器请求到 SQLite 和 Xray 测试替身

deploy/
├── xpanel.example.json
├── xray-v26.3.27.example.json
└── xpanel.service
```

**Structure Decision**: 采用单 Go module 的端口/适配器式模块化单体。领域与应用层不导入
HTTP、SQLite 或 Xray protobuf；适配器分别实现持久化和 Xray port。Web handlers 只解析/
呈现，后台 worker 调用相同 application service。模板和 HTMX 静态文件位于 `internal/web`
以便直接 `go:embed`，migrations 位于 SQLite package 内并随同一二进制发布。

## Phase 0 Research Outcome

[research.md](research.md) 已解决全部 Technical Context 未知项，主要结论为：

- 固定 Xray 稳定运行时与 module，针对固定版的 `clients` 字段和 SS2022 account 建立契约。
- 精确读取 `reset=false` 绝对用户计数，以 SQLite 持久游标计算 delta。
- modernc SQLite + goose migrations 提供 CGo-free 单制品与版本化升级。
- 正交业务状态、revision 和 transactional outbox 实现幂等最终一致。
- gross/accounted 双计数使手动配额重置不删除真实历史。
- SSR/PRG 是权威交互，HTMX 仅轮询持久状态 fragment。
- Argon2id、服务端 session、显式 CSRF 和数据库外主密钥构成安全边界。

## Phase 1 Design Outcome

### Data and transactions

[data-model.md](data-model.md) 定义一对一用户分配、统一 Xray 统计身份、版本化凭证、
配额周期、累计/日聚合、方向级游标、连续性事件、幂等命令、持久同步操作和 append-only
审计。所有影响访问权的动作先在单个 SQLite 事务中写业务事实、同步意图和审计，RPC
始终在提交后执行。

计数下降规则区分已确认重启与未知回退：新 boot epoch 的首个绝对值作为新流量计入；
未知回退只建立新基线并记录事件，优先避免重复计量。手动重置仅清零当前周期 accounted
值，保留 gross、lifetime、daily 和原周期边界。

### Interface contracts

- [HTTP/HTML contract](contracts/http.md)：同源 SSR 路由、POST/303、CSRF、资源版本、
  请求幂等、fragment、安全头与无障碍行为。
- [Local CLI contract](contracts/cli.md)：初始化和密码重置的 TTY/stdin、安全输出与事务语义。
- [Deployment config contract](contracts/config.md)：严格 JSON、权限、回环目标、worker 间隔与
  数据库外根密钥规则。
- [Xray Adapter contract](contracts/xray-adapter.md)：稳定项目类型、错误映射、SS2022 protobuf、
  非破坏性统计、读后写收敛和真实运行时兼容门禁。

### Background execution

- Collector：每 5 秒在事务外读取最多 20 个用户的精确上下行绝对计数；一次短写事务
  更新全批游标、累计、日/周期聚合，并为首次越界写 remove 操作。
- Synchronizer：单实例串行领取有租约的到期操作，事务外调用 Xray，按 revision 确认
  或以最大 30 秒、full jitter 的有界退避重排。任何写入同步操作的事务提交后，通过
  进程内通知立即唤醒 synchronizer（兜底轮询间隔 1 秒），使健康 Xray 上的创建、
  配额封禁与恢复通常在数秒内确认；创建成功页在确认前显示“启用中（待同步）”。
- Reconciler：启动、Xray 重连和每 15 秒比较实际用户与最新期望；只管理 `xpanel-`
  命名空间，保留 bootstrap 和未知外部用户。
- Scheduler：按 IANA 时区计算日/配额边界，幂等关闭旧周期并打开新周期；停机恢复时
  收敛到唯一 open 周期。

### Startup and shutdown order

1. 加载 JSON 配置，验证目录/文件权限、外部主密钥和 Xray 回环地址。
2. 打开 SQLite，设置并验证 PRAGMA，执行嵌入式 migrations。
3. 初始化单例行：`panel_settings` 不存在时按 `initial.quota_timezone` 创建；
   `managed_xray_instances` 按配置 upsert `api_endpoint` 与 `supported_runtime_version`。
4. 初始化 Store、security、application services、Xray Adapter 和 HTTP routes。
5. 启动 synchronizer/reconciler/collector/scheduler，再开放 readiness。
6. 优雅关闭时先停止接收写请求，再取消后台 RPC、完成短事务并释放 lease/连接。

### Validation

[quickstart.md](quickstart.md) 定义固定 Xray 配置、构建初始化、profile 能力验证、真实
TCP/UDP 流量、配额/手动重置、生命周期、轮换、重启恢复、认证与备份恢复的验收路径。

## Complexity Tracking

无。设计没有违反宪章，也没有引入需要记录例外的额外项目、数据库或运行时服务。
