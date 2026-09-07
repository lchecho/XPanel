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

**C-004（对 R-005 的重要澄清）：只有「以空 Users 创建」的入站会退化为服务端密钥可直连；
把已创建入站的唯一客户端移除不会。** 实测（真实 26.3.27）：创建带 1 个客户端的多用户入站，
再经 AlterInbound 移除该客户端后，端口仍在监听，但用被移除的用户密钥和服务端密钥直连都无法完成
握手（两者均超时）。原因是单用户/多用户模式在入站构造时确定，事后移除用户不会重建为单用户服务。

影响：轮换过程中的「空入站」窗口是**可用性缺口**，不是「服务端密钥开放」的安全缺口。
FR-019 仍按字面执行（不得让任何专属入站处于没有受管客户端的状态），但补偿动作可以从容选择：
先尝试立即补回客户端，失败才移除整条入站。

**C-005（对 R-007 的补充，已被 C-007 修正）：零流量时用户级统计能力无法被证实。** 实测：无论 policy 是否开启
`statsUserUplink`/`statsUserDownlink`，在 AddUser 之后、产生任何流量之前，
`GetStats("user>>>…>>>traffic>>>uplink")` 都返回 NotFound，两种配置不可区分（计数器在首次流量时
才注册）。若配置里干脆没有 `stats` 块或 api.services 不含 StatsService，则连 `Probe`（GetSysStats）
都失败，节点在实例健康检查阶段就被判为不可达，轮不到模板校验。

因此模板能力门禁只能硬性验证「可创建入站」「多用户身份」「**探针入站可被移除**」三项；
用户级统计缺失只能在采集阶段作为健康诊断暴露（contracts/config.md），并在文档中作为部署前置条件。

**C-006（对 C-004 与 T078 的修正）：轮换必须用过渡客户端，「未持久化的空窗」不算故障安全。**
T078 把轮换收进单个租约步骤，认为空窗只存在于两次 RPC 之间因而可以接受。这不成立：进程在两次 RPC
之间崩溃、或租约竞态让一个过期的 `RemoveUser` 漏到 Xray，都会留下真正的空客户端入站，而它对用户
表现为「端口在听但谁也连不上」。实测（真实 26.3.27）确认同一入站可以同时容纳两个客户端，
且四步过渡（加过渡 → 删旧 → 加新 → 删过渡）全程客户端数为 1 或 2、端口持续监听，
因此轮换改用过渡客户端实现，见 contracts/xray-adapter.md 第 7 条。

同时在适配器层加了最终防线：`RemoveUser` 在发起 RPC 前读客户端列表，目标确实在且是唯一客户端时
以 `last_managed_client` 拒绝（第 8 条）。这条守卫与租约无关，因此能挡住任何竞态下的过期请求。

**C-007（对 C-005 的修正）：产生一次认证流量后，用户级统计能力是可以被证实的。**
C-005 只测了「AddUser 之后、无任何流量」的情形，据此得出「无法证实」的结论，这个结论过窄。
补测（真实 26.3.27，同一探针入站，用 sing-shadowsocks 的 SS2022 客户端在进程内直连探针端口、
目标指向面板自己起的本地回显监听）：

| policy | 回显 | `user>>>探针身份>>>traffic>>>uplink/downlink` |
|---|---|---|
| 开启 `statsUserUplink`/`statsUserDownlink` | 成功 | 两个计数器都存在，均为 23 字节（恰好是载荷长度） |
| 未配置 `policy` | 成功 | 两个计数器都不存在（3 秒轮询内始终 NotFound） |

两种配置因此完全可区分。模板兼容性校验改为硬门禁：创建探针入站 → 产生一次认证回显流量 →
回读两个方向的计数器 → 移除探针。漏配 policy 的节点在创建任何用户之前就被判为不兼容，
配套中文原因直接指向要改的配置键。

只做 TCP 不做 UDP：用户级计数器是按身份聚合的，与传输协议无关，TCP 一次往返已经同时证实
上行与下行两个计数器可读；UDP 路径另有 `TestLiveTrafficCountersAndInboundRemovalSemantics`
以真实客户端覆盖。探针流量的目的地是面板自己起的本地回显端口，不产生任何外部流量。
