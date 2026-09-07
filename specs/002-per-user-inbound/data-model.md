# Phase 1 Data Model: 每用户专属入站

**Feature**: `002-per-user-inbound` | **Date**: 2026-09-06

本文件描述相对 001 已实现模式的**增量**。未提及的表（`administrators`、`admin_sessions`、
`panel_settings`、`managed_xray_instances`、`quota_policies`、`quota_cycles`、
`allocation_traffic_totals`、`traffic_cursors`、`daily_traffic_aggregates`、
`traffic_continuity_events`、`synchronization_operations`、`domain_commands`、`audit_events`、
`drift_removals`）结构不变，语义沿用 `specs/001-xray-user-management/data-model.md`。

## 规格实体到存储的映射

| spec.md 实体 | 存储位置 |
|---|---|
| Inbound Template | `inbound_templates`（由 `access_profiles` 改造） |
| Dedicated Inbound | `dedicated_inbounds`（新表，与 `access_allocations` 一对一） |
| Port Assignment | `dedicated_inbounds` 的 `(listen_address, port)` 与其部分唯一索引 |
| Managed User / Access Credential / Quota* / Audit* | 沿用 001 同名表 |
| Bootstrap User | **移除**：面板自建入站时由受管客户端本身使 inbound 进入多用户模式 |

## 变更表

### inbound_templates（原 access_profiles）

| Field | Type | Rules |
|---|---|---|
| `id` | UUID | 主键 |
| `instance_id` | UUID | FK |
| `name` / `normalized_name` | text | 唯一显示名 |
| `public_host` | text | 交付给使用者的主机名 |
| `listen_address` | text | 入站监听地址，默认 `0.0.0.0`；MUST 为 IP 字面量 |
| `port_pool_start` / `port_pool_end` | integer | `1024 ≤ start ≤ end ≤ 65535`；面板只在闭区间内分配 |
| `method` | text | 仅 `2022-blake3-aes-128-gcm` / `2022-blake3-aes-256-gcm` |
| `network` | text | `tcp` / `udp` / `tcp_udp` |
| `compatibility_state` | enum | `unverified` / `compatible` / `incompatible` / `unreachable` |
| `compatibility_reason` / `last_validated_at` | text / timestamp nullable | 稳定且脱敏 |
| `revision` | integer | 乐观并发 |
| `archived_at` | timestamp nullable | 归档 |

**移除**：`server_key_ciphertext`、`server_key_nonce`、`key_encryption_version`、
`bootstrap_statistics_id`、`public_port`。服务端密钥改为每条专属入站独立生成并存于
`dedicated_inbounds`；端口改为每用户分配；bootstrap 概念取消。

**规则**：端口池被缩小后，池外的既有分配 MUST 保留可用（FR/边界用例），仅在界面标识；
`port_pool_end - port_pool_start + 1` 即池容量，用于 FR-009 的耗尽判定与 FR-035 的仪表盘展示。

### dedicated_inbounds（新）

| Field | Type | Rules |
|---|---|---|
| `allocation_id` | UUID | 主键，FK → `access_allocations(id)`，一对一 |
| `template_id` | UUID | FK → `inbound_templates(id)` |
| `inbound_tag` | text | 全局唯一；MUST 以面板保留命名空间前缀开头 |
| `listen_address` | text | 创建时从模板复制；模板此后变更不影响已创建入站 |
| `port` | integer | `1024..65535` |
| `server_key_ciphertext` / `server_key_nonce` / `key_encryption_version` | blob / blob / integer | 面板生成的该入站服务端密钥，AEAD 加密存储 |
| `desired_present` | integer | 期望是否监听；由用户生命周期与配额状态派生 |
| `observed_present` | integer nullable | 最近一次已确认的实际监听状态 |
| `last_sync_at` | timestamp nullable | 最近一次确认时间 |
| `released_at` | timestamp nullable | 端口释放时间；非空表示该行只保留历史，端口回到池中 |
| `created_at` / `updated_at` | timestamp | UTC |

**索引与约束**：

- `UNIQUE(inbound_tag)`：标签全局唯一，配合命名空间前缀实现漂移判定。
- `CREATE UNIQUE INDEX dedicated_inbounds_port_idx ON dedicated_inbounds(listen_address, port) WHERE released_at IS NULL;`
  这是 FR-008 端口唯一性的**唯一权威保证**。研究已证实 Xray 不会拒绝重复端口
  （research.md R-002），因此该约束不可省略，也不可下放到应用层判断。
- `CHECK(port BETWEEN 1024 AND 65535)`。

### xray_user_identities

移除 `kind='bootstrap'` 的使用；本功能只写入 `kind='managed'`。`statistics_id` 仍 MUST 在
实例内全局唯一并使用面板命名空间前缀，统计口径不变（research.md R-007）。

### access_allocations

结构不变，语义收窄：`profile_id` 改为指向 `inbound_templates`。`projection_state` 的含义从
“用户是否在共享入站中”变为“该用户的专属入站是否按期望监听”。

### synchronization_operations

`reason` 枚举扩展为：`create`、`enable`、`disable`、`rotate`、`delete`、`quota_block`、
`quota_restore`（语义不变，但 `create`/`enable`/`quota_restore` 现在意味着创建入站，
`disable`/`delete`/`quota_block` 意味着移除入站）。`phase` 扩展：

| Phase | 含义 |
|---|---|
| `create_inbound` | 创建专属入站（含唯一客户端） |
| `remove_inbound` | 移除整条入站 |
| `remove_old` → `add_desired` | 凭证轮换，在既有入站内增删客户端，端口不变 |
| `done` | 已确认 |

## 状态机

### 专属入站

```text
absent --create_inbound--> present
present --remove_inbound--> absent
present --rotate(remove_old→add_desired)--> present   # 端口与监听不中断
```

`desired_present` 由既有正交状态派生，规则与 001 的 `DesiredPresent` 一致：

```text
desired_present = lifecycle == active AND admin_enabled AND quota_state == within_limit
```

因此“禁用”“配额超限”“删除”三条路径统一收敛到 `remove_inbound`，满足 FR-019：任何时刻
不存在 `desired_present=1` 但客户端列表为空的入站。

### 端口

```text
free --分配--> assigned(released_at IS NULL) --删除用户--> released(released_at 非空) --> free
```

端口复用时 MUST 生成新的 `inbound_tag`、新的服务端密钥、新的用户密钥和新的 `statistics_id`，
使旧连接信息在该端口上不可用（spec US3 场景 5）。

## 事务边界

在 001 已确立的边界基础上新增或调整：

1. **创建用户**：单事务内写入 `managed_users`、`xray_user_identities`、`access_allocations`、
   `dedicated_inbounds`（含端口分配）、`quota_policies`、`quota_cycles`、`access_credentials`、
   `domain_commands`、`audit_events` 与一条 `create` 同步操作。端口冲突由部分唯一索引在同一
   事务内拒绝，不产生部分状态（FR-038）。
2. **删除用户**：软删除用户后，移除入站的确认事务中把 `dedicated_inbounds.released_at` 置为当前
   时间，端口即刻回到池中；历史行保留供审计。
3. **禁用 / 配额超限 / 周期恢复**：沿用 001 的事务内重读事实与决策（`DecideTransition`），
   决策结果映射到 `create_inbound` 或 `remove_inbound` 动作；端口分配保持不变，重建复用原端口。
4. **凭证轮换**：不触碰端口与 `dedicated_inbounds`，仅在既有入站内 `remove_old` → `add_desired`。
5. **漂移清理**：面板命名空间内 SQLite 无记录的入站，写入 `drift_removals` 意图后由 synchronizer
   移除；沿用 001 的 `superseded_by` 显式因果链，不使用时间戳启发式。

RPC 一律在写事务之外执行（宪章“技术与运行约束”）。

## 不变量

| 不变量 | 保证方式 |
|---|---|
| 两个活跃用户不得共用 `(listen_address, port)` | `dedicated_inbounds` 部分唯一索引 |
| 每个用户恰好一条专属入站 | `dedicated_inbounds.allocation_id` 主键 + `access_allocations` 一对一 |
| 入站标签全局唯一且带命名空间前缀 | `UNIQUE(inbound_tag)` + 写入前校验前缀 |
| 不存在没有受管客户端的面板入站 | 停止访问只走 `remove_inbound`；`create_inbound` 必须携带客户端 |
| 端口只在池内分配 | 分配逻辑读取模板池边界；池外的既有分配只保留不新增 |
| 统计标识实例内唯一且不复用 | 沿用 001 的 `xray_user_identities` 唯一约束 |

## 迁移

新增顺序迁移 `00004_per_user_inbound.sql`：

1. 改造 `access_profiles` → `inbound_templates`（新增 `listen_address`、`port_pool_start`、
   `port_pool_end`；删除服务端密钥列、`bootstrap_statistics_id`、`public_port`）。
2. 新建 `dedicated_inbounds` 及其两个索引。
3. 清理 `xray_user_identities` 中 `kind='bootstrap'` 的行与相关约束。

001 尚未发布，迁移无需保留旧数据的兼容路径。迁移仍 MUST 版本化、可回滚（提供 `-- +goose Down`），
并在 `docs/operations.md` 的备份步骤中标注为破坏性变更（宪章“技术与运行约束”）。
