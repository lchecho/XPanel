# Phase 0 Research: Xray 多用户管理 MVP

**Date**: 2026-09-04  
**Feature**: `001-xray-user-management`

本文件记录实施计划中的技术决策。所有版本号均为本次规划基线；升级必须经过相同的
契约、集成与端到端验证，不能隐式跟随 `latest` 或上游 `main`。

## 1. Go 与 Xray 版本基线

**Decision**: 使用 Go `1.26.8` 工具链；Xray 运行时固定为 `v26.3.27`
（commit `d2758a0`），Go module 固定为 `github.com/xtls/xray-core@v1.260327.0`。
该版本是唯一受支持的部署基线；真实进程契约测试通过 `xray version` 固定二进制，并由
运行时能力探测阻止不兼容 profile 的用户变更。

**Rationale**: Xray 稳定版 module 要求 Go 1.26；Go 1.26.8 是规划时仍受支持且已包含
安全修复的补丁版本。Xray 26.7.x 在规划时属于预发布，且 `main` 已出现配置字段变化。
同时固定二进制与 Go module 可以避免 protobuf type URL、API 方法和运行时行为漂移。
HandlerService/StatsService 没有可靠的版本证明接口，因此不能把“API 可达”误当成版本
认证；部署清单和真实进程契约测试负责版本证明，应用内探测负责能力证明。

**Alternatives considered**:

- Go 1.27：更新但刚进入发布周期，MVP 没有依赖其新增能力。
- Xray 预发布版或 `main`：不可复现且配置/API 语义仍可能变化。
- 自行复制 protobuf：减少直接依赖但会引入来源和类型注册漂移。

**Evidence**:

- [Go release history](https://go.dev/doc/devel/release)
- [Go toolchain selection](https://go.dev/doc/toolchain)
- [Xray v26.3.27](https://github.com/XTLS/Xray-core/releases/tag/v26.3.27)
- [Xray module go.mod](https://github.com/XTLS/Xray-core/blob/v1.260327.0/go.mod)

## 2. Shadowsocks 2022 多用户契约

**Decision**: 首个端到端配置使用 `2022-blake3-aes-256-gcm`；同时支持
`2022-blake3-aes-128-gcm`。固定版 Xray JSON 必须使用非空 `settings.clients`，并至少
包含一个永久 bootstrap 用户。动态添加必须发送 `AddUserOperation`，其 account 类型为
`xray.proxy.shadowsocks_2022.Account`；删除通过稳定统计身份 `email` 完成。

**Rationale**: 固定版在 `clients` 为空时创建单用户服务，不实现 `UserManager`；错误地
使用传统 Shadowsocks account 也会得到 `proxy is not a UserManager` 或不可用用户。
Xray 用户统计名称不含 inbound tag，因此统计身份必须在整个 Xray 实例全局唯一；采用
`xpanel-<allocation-uuid>`，禁止空值和 `>>>`。

AES-256 的服务端密钥与用户密钥分别由 CSPRNG 生成 32 原始字节并使用标准 Base64
编码；AES-128 使用 16 原始字节。客户端 password 为
`<ServerPassword>:<UserPassword>`。XPanel 在 RPC 前完成 Base64、长度与唯一性校验，
不依赖 Xray 替面板生成或校验客户端配置。

**Alternatives considered**:

- 空 `clients` 后动态添加第一个用户：固定版会进入单用户模式，不可用。
- `proxy/shadowsocks.Account`：不是 SS2022 动态用户所需类型。
- 由 XPanel 创建整个 inbound：扩大 MVP 范围，且运行时 inbound 需额外持久化恢复。

**Evidence**:

- [固定版配置构建器](https://github.com/XTLS/Xray-core/blob/v1.260327.0/infra/conf/shadowsocks.go)
- [SS2022 多用户实现](https://github.com/XTLS/Xray-core/blob/v1.260327.0/proxy/shadowsocks_2022/inbound_multi.go)
- [HandlerService protobuf](https://github.com/XTLS/Xray-core/blob/v1.260327.0/app/proxyman/command/command.proto)
- [Shadowsocks 2022 specification](https://github.com/Shadowsocks-NET/shadowsocks-specs/blob/main/2022-1-shadowsocks-2022-edition.md)
- [Xray issue 3943](https://github.com/XTLS/Xray-core/issues/3943)
- [Xray issue 4412](https://github.com/XTLS/Xray-core/issues/4412)

## 3. Xray API 与流量读取

**Decision**: Xray API 只允许回环地址，启用 `HandlerService`、`StatsService`、`stats:{}`，
并为动态用户 level 0 开启上行与下行用户统计。XPanel 每 5 秒使用 `GetStats(reset=false)`
精确读取每个已知用户的两个绝对计数器：

```text
user>>><statistics-id>>>traffic>>>uplink
user>>><statistics-id>>>traffic>>>downlink
```

RPC 在数据库事务外执行；一轮结果在单个短事务中更新游标、累计值、日聚合、配额周期
和首次越界产生的同步操作。缺失计数器与零值必须区分。

**Rationale**: `reset=true` 会先清空 Xray 内存计数；若随后 SQLite 提交失败，该批流量
不可恢复。`QueryStats` 的 pattern 是子串匹配，也可能误清 bootstrap 或非面板用户。
最多 20 个分配时，精确非破坏性读取的 RPC 数量可控，持久游标可防止重复计量。

**Alternatives considered**:

- `QueryStats(..., reset=true)`：存在清零与持久化之间的丢量窗口。
- 从访问日志推算：格式与完整性不足以作为权威计量来源。
- 永久保存每轮原始采样：不增加配额正确性且造成无意义增长。

**Evidence**:

- [Xray API configuration](https://xtls.github.io/en/config/api.html)
- [Xray statistics](https://xtls.github.io/en/config/stats.html)
- [Xray policy](https://xtls.github.io/en/config/policy.html)
- [StatsService implementation](https://github.com/XTLS/Xray-core/blob/v1.260327.0/app/stats/command/command.go)

## 4. Go Web 与前端交付

**Decision**: 使用标准库 `net/http.ServeMux`、`html/template` 和 `embed.FS`。管理页面为
同源 SSR，状态变更使用原生 `POST` 表单及 Post/Redirect/Get；HTMX `2.0.10` 作为固定、
本地嵌入的可选增强，只请求受认证的 HTML fragment。核心流程不依赖 JavaScript。

**Rationale**: Go 1.22 之后的 ServeMux 已支持方法和路径参数；当前页面规模不需要路由
框架。HTMX 可用 5 秒轮询更新 SQLite 中最后确认的状态，同时无脚本路径仍能完成登录、
创建、编辑、启停、轮换、流量重置和删除。单制品交付避免 Node/CDN 与跨域认证复杂度。

**Alternatives considered**:

- React/Vue SPA 与独立 JSON CRUD API：增加两套契约和发布链路。
- WebSocket：当前没有双向实时通信需求。
- 运行时从磁盘加载模板或 CDN 脚本：会造成发布制品和依赖版本漂移。

**Evidence**:

- [`net/http.ServeMux`](https://pkg.go.dev/net/http#ServeMux)
- [`html/template`](https://pkg.go.dev/html/template)
- [`embed`](https://pkg.go.dev/embed)
- [HTMX 2.0.10 documentation](https://github.com/bigskysoftware/htmx/blob/master/www/content/docs.md)

## 5. SQLite 驱动、迁移与写入模型

**Decision**: 使用 `database/sql` + `modernc.org/sqlite`，生产构建保持 CGo-free。数据库
位于本地持久磁盘，启用并验证 WAL、`foreign_keys=ON`、`busy_timeout=5000`、
`synchronous=FULL`。写句柄 `MaxOpenConns(1)`，所有写操作经 Store 的单写路径执行；
读句柄只开放少量连接。固定 `modernc.org/sqlite@v1.58.0` 和
`github.com/pressly/goose/v3@v3.28.0`，使用嵌入式顺序 SQL migration；modernc 驱动与
其 `go.mod` 指定的 libc 版本作为一个兼容单元升级。

**Rationale**: modernc 驱动简化静态交叉构建；WAL 允许读写并行但仍只有一个 writer，
符合单控制实例与低写入量。权限与配额状态安全敏感，`FULL` 的持久性优先于微小吞吐
收益。成熟 migration runner 比自制部分失败和版本冲突逻辑更可靠。

**Alternatives considered**:

- `mattn/go-sqlite3`：成熟，但生产交叉构建需要 CGo 工具链。
- 多写连接或共享 NFS：增加锁竞争或违反 WAL 运行约束。
- ORM：领域事务和精确 upsert 更适合显式 SQL。
- 只维护最新 `schema.sql`：无法安全升级已有部署。

**Evidence**:

- [`modernc.org/sqlite`](https://pkg.go.dev/modernc.org/sqlite)
- [SQLite WAL](https://www.sqlite.org/wal.html)
- [SQLite transactions](https://sqlite.org/lang_transaction.html)
- [goose embedded migrations](https://github.com/pressly/goose)

## 6. 正交状态与持久同步操作

**Decision**: 分别保存 lifecycle、管理员启用意图、quota 与 Xray projection 状态，页面
状态由它们派生。期望存在条件为：

```text
lifecycle_state == active
AND admin_enabled
AND quota_state == within_limit
```

每次影响 Xray 的变更在同一 SQLite 事务中提交业务状态、`desired_revision`、持久同步
操作、浏览器命令结果和审计事件；提交后由单实例 worker 串行调用 Xray。结果只有在
revision 仍为当前值时才能确认。重试采用最大 30 秒的有界指数退避和 full jitter；协调器
在启动、Xray 重连和每 15 秒运行，并只在检测到漂移时创建操作。

**Rationale**: 正交状态可以表达“手动禁用且同时超限”，避免周期恢复误开启用户。
transactional outbox 消除“先 RPC 后落库”的不可恢复窗口，也不虚构跨 SQLite/gRPC
事务。资源 revision 阻止旧页面和旧 worker 覆盖最新管理员意图。

**Alternatives considered**:

- 单一互斥 `status`：不能准确表达并存的阻断原因。
- SQLite 事务内调用 gRPC：长期占用唯一写锁且仍非原子。
- 内存队列或 Adapter 内无限重试：重启丢失或形成请求风暴。

## 7. 计数器 epoch、配额周期与手动重置

**Decision**: 持久化每方向绝对计数游标和 Xray boot/statistics epoch。正常增长使用差值；
重启或计数下降开启新 epoch，记录 continuity event，并按契约测试确定的基线规则处理首个
样本；缺失值不覆盖历史。常规数据只保留 lifetime totals、daily aggregates、quota cycle、
cursor 和异常连续性事件。

配额周期同时保存：

- `gross_*`：不可由管理员重置的真实确认流量；
- `accounted_*`：用于当前配额判断和“本周期用量”展示的流量。

手动重置在一个事务中记录重置前 accounted 值和游标快照，清零 accounted，保留 gross、
每日聚合和周期边界；若超限是唯一阻断原因则产生恢复操作。时区与 reset day 在周期中
保存快照，配置变更从下一周期生效。跨日/周期的一轮增量确定性归入样本完成时所在边界。

**Rationale**: gross/accounted 分离既满足“类似提高配额”的手动清零语义，又不会抹掉
真实历史。游标与聚合的同事务更新使 DB 失败后下一轮可安全重读。异常事件提供诊断，
无需保存每 5 秒采样。

**Alternatives considered**:

- 手动重置 Xray counter：会影响连续性并产生 RPC/DB 非原子窗口。
- 删除每日或 lifetime 历史：破坏审计和趋势含义。
- 变更时区后重算历史周期：会改变已经结算的业务事实。

## 8. 认证、会话、CSRF 与敏感数据

**Decision**: 管理员密码使用 `golang.org/x/crypto@v0.56.0` 的 Argon2id，初始参数为
64 MiB、3 iterations、4 lanes、16 字节随机盐、32 字节输出，并在最低目标机器基准验证。
PHC 风格编码保存算法和参数。使用 `github.com/alexedwards/scs/v2@v2.9.0` 管理服务端
session，并实现复用 modernc 数据库的薄 Store；使用
`github.com/gorilla/csrf@v1.7.3`
和 Go `http.CrossOriginProtection`。所有令牌由 `crypto/rand` 生成。

生产 Cookie 使用 `Secure`、`HttpOnly`、`SameSite=Strict`、`Path=/`；登录后轮换 token，
登出和密码重置撤销 session。管理页面发送 `Cache-Control: no-store` 及严格 CSP 等安全头。
服务端/用户密钥使用 XChaCha20-Poly1305 应用层 AEAD 加密后写 SQLite；32 字节主密钥由
数据库外的 root-only 文件或 secret 注入提供，每个字段使用随机 24 字节 nonce，AAD
绑定表、行、字段和凭证版本。完整 `ss://` 只在认证请求中临时组装，绝不持久化或记录。

**Rationale**: 单管理员仍管理可直接授予代理访问权的敏感控制面。可撤销服务端 session
适合本机密码重置；AEAD 降低数据库或备份副本泄露后的凭证风险。主密钥与数据库分离，
同时保留重放 Xray 用户与再次展示连接信息所需的可恢复密钥。

**Alternatives considered**:

- bcrypt：成熟但非 memory-hard 且存在 72 字节输入限制。
- JWT/localStorage：全局撤销困难并扩大 XSS 后果。
- 只依赖 SameSite 或来源头：不满足显式 CSRF token 要求。
- 只依赖数据库文件权限：备份误传会暴露全部代理凭证。

**Evidence**:

- [RFC 9106](https://www.rfc-editor.org/info/rfc9106/)
- [`x/crypto/argon2`](https://pkg.go.dev/golang.org/x/crypto/argon2)
- [SCS](https://github.com/alexedwards/scs)
- [Gorilla CSRF](https://github.com/gorilla/csrf)

## 9. HTTP 幂等、并发与错误语义

**Decision**: 表单为 `application/x-www-form-urlencoded`。每个变更携带 CSRF token、
一次性 `_request_id`，以及适用时的 `_version`。成功使用 `303 See Other`；Xray 离线但
期望状态已提交时返回成功重定向并显示 `pending_sync`。版本冲突返回 409，字段校验返回
422，认证/CSRF 分别返回安全的 401/403。Fragment 与完整页面复用相同服务与权限规则。

**Rationale**: `_version` 解决并发编辑，`_request_id` 解决双击和网络重试，二者职责不能
由 CSRF token 代替。PRG 让无脚本与 HTMX 路径共享稳定语义，并避免浏览器刷新重复提交。

**Alternatives considered**:

- 仅在浏览器禁用按钮：无法处理网络重试且依赖 JavaScript。
- 所有错误返回 200：难以区分校验、冲突和基础设施失败。
- 等 Xray RPC 成功再保存：把运行时可用性错误地变成业务事实的前置条件。

## 10. CLI、日志与测试策略

**Decision**: 同一二进制提供 `xpanel serve`、`xpanel admin init` 和
`xpanel admin reset-password`。密码默认经无回显 TTY 输入两次；自动化只能显式使用
`--password-stdin`，不得提供 `--password` 参数。密码重置事务同时更新哈希版本、撤销
全部 session 并写入 `actor=local-cli` 的脱敏审计。

使用标准库 `log/slog` JSONHandler；固定字段包含 component、request/operation/allocation
标识、目标状态、结果、耗时和安全错误类别，集中脱敏。测试以标准库为主：表驱动单元
测试、真实临时文件 SQLite 集成测试、`httptest`、手写 fake Adapter，以及固定 Xray
二进制的真实进程契约测试。常规门禁为 `go test ./...`、`go vet ./...`；原生 CI 另跑
`go test -race ./...`，关键解析器和统计名称使用 fuzz test。

**Rationale**: CLI 复用应用层规则且不把密码暴露在 shell history。结构化日志支持故障
关联但不能代替 SQLite 审计。真实 Xray 契约测试能捕获 TypedMessage、配置分支、重启、
统计和 SS2022 固定版内部错误处理等 mock 无法发现的问题。

**Alternatives considered**:

- 网页/邮件找回：扩大公开攻击面并超出范围。
- 直接用 sqlite3 修改管理员记录：绕过密码策略、会话撤销与审计。
- 只使用 mock Xray：无法证明固定运行时的真实认证与统计行为。
