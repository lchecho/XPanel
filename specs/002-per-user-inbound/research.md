# Phase 0 Research: 每用户专属入站

**Feature**: `002-per-user-inbound` | **Date**: 2026-09-06

本文件记录进入设计前必须解决的未知项。所有标注“已验证”的结论来自针对固定 Xray v26.3.27
（由 `go install github.com/xtls/xray-core/main@v1.260327.0` 构建）的一次性研究探针，探针在结论
记录后删除；正式回归覆盖由 `/speckit-tasks` 生成的契约测试承担。

## R-001 运行时创建入站的构建方式

**Decision**: 在 `internal/adapter/xray` 内用定向 protobuf 类型手工构建 `core.InboundHandlerConfig`：
`proxyman.ReceiverConfig`（`PortList` + `Listen`）承载监听，`shadowsocks_2022.MultiUserServerConfig`
（`Method` / `Key` / `Users` / `Network`）承载协议设置，两者经 `serial.ToTypedMessage` 包装后由
`HandlerService.AddInbound` 提交。

**Rationale**: 已验证该构建方式可被 Xray v26.3.27 接受，创建出的入站进入多用户模式
（`GetInboundUsersCount` 返回 1），现有 `ValidateProfile` 判定其 `compatible=true`，且现有
`AlterInbound` 增删用户路径在其上照常工作。依赖开销为零：所需包已在 Adapter 现有依赖集合内，
`go.mod` 与 `go.sum` 无任何变化。

**Alternatives considered**:

- 复用 Xray 自带的 `infra/conf.InboundDetourConfig.Build()`，即把面板生成的 JSON 交给 Xray 自己
  解析。语义等价性最好，但已测量：该包传递依赖 657 个，比定向构建的 390 个多出 267 个，其中 53 个
  属于 gvisor、WireGuard、netlink、QUIC 等与本功能无关的协议实现，会被链入面板二进制并扩大攻击面；
  `go mod tidy` 需新增 13 条 indirect 依赖。与宪章 II“Xray 集成必须隔离”相悖，否决。
- 由面板生成配置文件片段交由 Xray 加载。与规格 Out of Scope 和宪章“不得编辑 Xray 配置文件”冲突，否决。

## R-002 端口冲突的检测边界（关键）

**Decision**: 端口唯一性由面板在 SQLite 中以唯一约束保证，**不得依赖 Xray 报错**；创建入站前面板
先尝试自行绑定该端口以探测外部占用，绑定成功后立即释放并随即提交 `AddInbound`；`AddInbound` 返回的
`bind: address already in use` 作为第二道检测。

**Rationale**: 已验证两种冲突的表现完全不同。

| 冲突类型 | AddInbound 结果 | Xray 内是否注册 | 端口归属 |
|---|---|---|---|
| 端口被面板外进程占用 | 明确报错 `bind: address already in use` | **仍被注册** | 外部进程 |
| 端口被另一条 Xray 入站占用 | **无错误** | 两条都注册 | 共享监听器 |

Xray 不阻止两条入站声明同一端口，两者都会注册成功且不报错；移除其中一条后端口仍处于监听状态。
因此“两个用户共用一个端口”只能由面板自己防住，这直接支撑 FR-008。已验证面板进程对已占用端口执行
`net.Listen` 会得到 `address already in use`，因此预绑定探测可行；它存在 TOCTOU 竞争窗口，但能把
绝大多数外部占用转化为创建前的明确拒绝，符合 FR-010 的“允许管理员更换端口”。

**Alternatives considered**: 仅依赖 `AddInbound` 报错——无法覆盖入站间冲突，否决。仅依赖 TCP 拨测
探活——外部进程占用时拨测同样成功，会得出错误结论，否决。

## R-003 AddInbound 失败后的部分状态（关键）

**Decision**: `AddInbound` 视为**可能已部分生效**的不确定操作。失败后同步流程 MUST 先执行读后写
（`GetInboundUsersCount` 或 `ListInbounds` 判断标签是否已注册），确认已注册但未监听时先
`RemoveInbound` 补偿，再按有界退避重试。

**Rationale**: 已验证端口被外部占用时，`AddInbound` 返回监听失败错误，但该入站**仍然留在 Xray 的
处理器注册表中**（`GetInboundUsersCount` 返回 1）。若直接重试，会得到
`existing tag found` 而永远无法收敛。这正是宪章 IV“中途失败必须定义补偿或重试行为”所要求的场景，
需在设计中显式建模，而不是留作实现细节。

## R-004 入站移除与端口释放

**Decision**: 停止用户访问统一通过 `RemoveInbound` 实现；移除后以拨测确认端口不再监听。

**Rationale**: 已验证当某端口只有一条入站时，`RemoveInbound` 会释放端口（拨测在 1 秒内转为失败）。
移除不存在的入站返回 `common: not enough information for making a decision`，语义不透明，Adapter
MUST 将其映射为稳定的 `inbound_not_found` 错误类别并按“已收敛”处理，避免把幂等重放当成故障。

## R-005 空用户列表入站的安全隐患

**Decision**: 面板 MUST NOT 创建或保留 `Users` 为空的入站；停止访问一律移除整条入站（对应 FR-019）。

**Rationale**: 已验证 `Users` 为空的 SS2022 入站可以被 `AddInbound` 成功创建，且
`GetInboundUsersCount` 返回 0，即该入站落入单用户模式——此时服务端密钥本身即可直接建立连接。
本项与 001 契约门禁中“空 clients 进入不受支持的单用户模式”的结论一致，是本规格把
“禁用/超限=移除整条入站”写成硬性要求的直接依据。

## R-006 重启语义

**Decision**: Xray 重启后由协调器按 SQLite 中保存的端口分配整体重建全部应监听入站；面板不写配置文件。

**Rationale**: 已验证重启后运行时入站完全消失（查询返回 `handler not found`）且端口不再监听。
与 001 相比不可用面从“用户凭证失效”扩大到“端口不监听”，因此规格用 FR-028 要求界面诚实展示该窗口，
用 SC-006 把恢复时间纳入验收。

## R-007 流量统计口径

**Decision**: 继续使用用户级计数器 `user>>>{statistics_id}>>>traffic>>>{uplink|downlink}`，不改用
入站级计数器。

**Rationale**: 已验证运行时创建的入站上用户级计数器行为与配置文件入站一致（无流量时返回 `NotFound`，
与 001 的“缺失不得当作零”规则匹配）。沿用用户级口径可以完整复用 001 已实现并通过验收的采集、
游标、连续性事件与配额结算逻辑。入站级计数器需要 `policy.system.statsInboundUplink`，且在凭证轮换
或入站重建时语义更复杂，收益不足。

**Dependency**: 用户级统计依赖基础配置中的 `policy.levels."0".statsUserUplink/statsUserDownlink`。
该段位于 Xray 配置文件，面板无法通过 API 设置，因此 MUST 作为部署前置条件写入 quickstart 与
配置契约，并在入站模板兼容性校验中检出。

## R-008 与 001 实现的复用边界

**Decision**: 保留并复用 001 已实现且已通过门禁的全部机制：持久化同步操作与租约 fencing、
配额周期在事务内结算、流量连续性判定、审计与会话/CSRF、漂移移除的显式因果链。改动集中在
“访问配置”到“入站模板 + 专属入站 + 端口分配”的建模替换，以及同步动作从“增删用户”扩展为
“增删入站”。

**Rationale**: 001 的 155 项任务已全部实现并通过含真实 Xray 契约的发布门禁，其中租约 fencing、
周期边界事务、分批采集一致性、认证原子性等属于与入站模型无关的通用正确性资产。重写这些会引入
已消除的缺陷类别。

**Alternatives considered**: 新建独立模块并行实现——与规格“完全替换”决策冲突，且会长期维护两套
同步语义，否决。

## R-009 需要的数据库迁移

**Decision**: 以新增迁移（`00004` 起）完成模型替换：新增入站模板端口池字段、专属入站表、端口分配表；
移除 bootstrap 身份相关约束。因 001 未发布，迁移无需保留旧数据的兼容路径，但 MUST 仍为版本化迁移
并附可验证备份步骤（宪章“技术与运行约束”）。

**Rationale**: 宪章要求模式变更使用版本化迁移且破坏性迁移前生成可验证备份。详见 data-model.md。

## 未解决项

无。规格中的三个方向性决策已在 `spec.md` 的 Clarifications 记录，本轮研究未产生新的
NEEDS CLARIFICATION。唯一需要外部动作的是宪章条款更新，见 plan.md 的 Constitution Check
与 Complexity Tracking。

## 更正与补充记录（实现阶段）

以下三条在实现与真实 Xray 契约套件中发现，已同步修订 contracts/xray-adapter.md：

**C-001（对 R-003 的补充）：`port_unavailable` 不是永久失败。** 契约初稿把它写作「补偿移除后交由
管理员改端口」，实现时按此把操作判为 `permanent_failed`，结果是占用解除后分配也不会自行恢复，
与 contracts/http.md「端口被面板外进程占用 → 保存成功但待同步」相矛盾。修正为：补偿移除之后以
有界退避持续重试，分配保持「待同步」并显示可理解原因，管理员可以改端口但不是唯一出路（宪章 IV）。
故障矩阵新增 `external_port_occupied` 与 `port_conflict` 两列覆盖该路径。

**C-002（对 R-002 的补充）：命名空间守卫要覆盖客户端变更。** 契约初稿只要求 `RemoveInbound` 限定在
面板命名空间内。实测确认 `AlterInbound` 同样能修改运维在配置文件中自建的入站（面板不该有这个能力），
因此 `AddUser`/`RemoveUser` 也加了同一守卫，在发起 RPC 之前拒绝。

**C-003：在缺失的入站上增删客户端返回的是 `inbound_not_found`。** 这影响凭证轮换：Xray 重启后
轮换的两阶段都会撞上它。原实现按不可重试处理导致永久失败；修正为直接按期望凭证重建整条入站，
一步达成「入站在监听且密钥为期望版本」这一轮换目标。
