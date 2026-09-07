# Contract: Xray Adapter（每用户专属入站）

**Xray runtime**: `v26.3.27` | **Go module**: `github.com/xtls/xray-core@v1.260327.0`

本契约在 `specs/001-xray-user-management/contracts/xray-adapter.md` 基础上增补入站生命周期。
用户增删、统计读取、错误脱敏与超时规则不变。所有结论已针对固定版本实测（research.md）。

## 新增操作

| 操作 | Xray 调用 | 幂等键 |
|---|---|---|
| `CreateInbound` | `HandlerService.AddInbound` | `inbound_tag` |
| `RemoveInbound` | `HandlerService.RemoveInbound` | `inbound_tag` |
| `ListInbounds` | `HandlerService.ListInbounds` | 无（只读） |

### 入站构建

Adapter MUST 用定向 protobuf 构建，MUST NOT 引入 `infra/conf`（依赖开销见 research.md R-001）：

```text
core.InboundHandlerConfig{
  Tag:              "<面板命名空间前缀><allocation 派生标识>",
  ReceiverSettings: proxyman.ReceiverConfig{ PortList: {From: p, To: p}, Listen: <listen_address> },
  ProxySettings:    shadowsocks_2022.MultiUserServerConfig{
                      Method, Key: <面板生成的服务端密钥>,
                      Users: [ protocol.User{Level:0, Email:<statistics_id>,
                               Account: shadowsocks_2022.Account{Key:<用户密钥>}} ],
                      Network: [TCP, UDP] },
}
```

`Users` MUST 恰好包含一个受管客户端。Adapter MUST 拒绝构建空 `Users` 的入站请求：实测表明
Xray 会接受空用户列表并使该入站退化为“服务端密钥可直连”的单用户模式（research.md R-005）。

## 错误映射

| 观察到的 Xray 行为 | 稳定错误类别 | Retryable | 同步流程处置 |
|---|---|---|---|
| `bind: address already in use` | `port_unavailable` | 否（对单次 RPC） | **入站仍被注册**，MUST 先补偿移除；补偿后 MUST 以有界退避继续重试（占用是外部条件，不是本意图的永久错误），分配保持「待同步」并显示可理解原因，管理员可另行改端口 |
| `existing tag found: <tag>` | `inbound_already_exists` | 否 | 读后写确认；确属本意图则视为已收敛 |
| `handler not found: <tag>`（查询已移除入站，或在缺失入站上增删客户端） | `inbound_not_found` | 否 | 移除意图视为已收敛；轮换途中遇到 MUST 按期望凭证重建整条入站 |
| `common: not enough information for making a decision`（移除不存在入站） | `inbound_not_found` | 否 | 视为已收敛，MUST NOT 记为故障 |
| 面板自身守卫：移除会清空入站 | `last_managed_client` | 否 | 调用方 MUST 改用「移除整条入站」或先放过渡客户端；不得绕过 |
| 连接不可达 / 超时 | 沿用 001 的 `instance_unavailable` / `deadline_exceeded` | 是 | 有界退避重试 |

## 关键语义（实测结论，实现必须遵守）

1. **AddInbound 非原子**：监听失败时错误已返回，但该入站**仍留在处理器注册表中**。
   同步流程 MUST 把 `CreateInbound` 失败当作“可能已部分生效”，先 `ListInbounds` 读后写确认，
   已注册但未监听时先 `RemoveInbound` 补偿，再按退避重试。直接重试会得到 `existing tag found`
   并永久卡住。
2. **Xray 不阻止重复端口**：两条入站声明同一端口时两次 `AddInbound` 均返回成功且都注册成功，
   移除其中一条后端口仍在监听。端口唯一性 MUST 由面板的数据库约束保证，MUST NOT 依赖 Xray 报错。
3. **外部占用可检测**：端口被面板外进程占用时 `AddInbound` 返回明确的 bind 错误；面板 MAY 在
   创建前先自行 `Listen` 探测并立即释放，把多数外部占用转化为创建前的明确拒绝（存在 TOCTOU
   窗口，不作为唯一保证）。
4. **移除释放端口**：当某端口只有一条入站时，`RemoveInbound` 会释放端口；面板 MUST 以读后写
   确认实际状态，不得仅凭 RPC 成功即认定已释放。
5. **重启清空**：Xray 重启后全部运行时入站消失且端口释放，协调器 MUST 按库中端口分配整体重建。
6. **命名空间隔离**：`ListInbounds` 返回全部入站；面板 MUST 只对带面板保留前缀的标签执行**任何变更**，
   其余入站 MUST 只读且不计入面板统计。该守卫覆盖 `RemoveInbound` 与 `AddUser`/`RemoveUser` 三个入口，
   在发起 RPC 之前拒绝，越界请求不得到达 Xray。
7. **轮换与入站解耦**：凭证轮换只在既有入站内完成，端口与入站标签全程不变。
   Xray 不接受同 email 的重复添加，也无法原地替换密钥（实测 `user_already_exists`），只能先删后加；
   为满足「客户端数从不为 0」（FR-019），轮换 MUST 以四步过渡完成，每一步都由读实际状态驱动：

   | 步骤 | 动作 | 客户端集合 |
   |---|---|---|
   | 1 | `AddUser(过渡身份)` | {期望} → {期望, 过渡} |
   | 2 | `RemoveUser(期望身份)` | {期望, 过渡} → {过渡} |
   | 3 | `AddUser(期望身份, 新密钥)` | {过渡} → {期望(新), 过渡} |
   | 4 | `RemoveUser(过渡身份)` | {期望(新), 过渡} → {期望(新)} |

   过渡身份为 `<统计标识>-rotate`，密钥每次现生成、只存在于内存、MUST NOT 进入连接信息或计量口径。
   任一步崩溃或失败后重放都从实际状态续跑；失败 MUST 以有界退避继续推进，MUST NOT 判永久失败，
   也 MUST NOT 用「移除整条入站」来收场（那会中断监听，违背 FR-017）。
   若轮换途中发现入站整体消失（例如 Xray 刚重启），MUST 直接按期望凭证重建整条入站一步收敛。

8. **不得移除最后一个受管客户端**：Adapter 的 `RemoveUser` MUST 在发起 RPC 之前读取该入站的客户端列表，
   当且仅当「目标确实在且它是唯一客户端」时以 `last_managed_client` 拒绝。这是 FR-019 的最终防线：
   即便租约竞态让一个过期的移除请求漏到适配器，它也不会把入站清空。移除本就不存在的客户端不受影响，
   照常返回 `user_not_found`。

## 兼容性门禁

固定二进制的真实进程契约套件 MUST 证明：

1. `xray version` 与 `go version -m $XRAY_BIN` 双向一致，管理端点仅回环。
2. 面板可在运行时创建 SS2022 多用户入站，端口在 `AddInbound` 返回后立即监听。
3. 创建出的入站 `GetInboundUsersCount > 0`，且可在其上增删用户。
4. 空 `Users` 的入站被 Adapter 在构建期拒绝。
5. 真实 AES-256 客户端可经该端口完成 TCP 与 UDP 流量，且两个方向的用户级计数器均增长。
6. 移除入站后新握手被拒、端口释放；已建立连接可能继续。
7. Xray 重启后全部面板入站消失，协调器按原端口整体重建且仅重建应监听的用户。
8. `CreateInbound` 在端口被外部占用时报错且留下已注册入站，补偿移除后可在换端口后收敛。
9. 两个用户的端口相互隔离：对其中一个执行创建、移除、轮换不影响另一个的监听与计数。
10. 面板命名空间之外的入站在全流程中保持不变。

任何 Xray 运行时或模块升级 MUST 重新通过本套件。
