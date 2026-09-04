# Data Model: Xray 多用户管理 MVP

**Date**: 2026-09-04  
**Feature**: `001-xray-user-management`  
**Storage**: SQLite（WAL、foreign keys、STRICT tables）

## Conventions

- 业务 ID 使用不可变 UUID 文本；显示名称和 Xray `email` 不作为数据库主键。
- 内部时间统一存 UTC Unix 毫秒；配额边界另存 IANA 时区名称和本周期快照。
- 字节计数使用非负 64 位整数，所有累加在写入前检查溢出。
- 布尔值使用 `INTEGER NOT NULL CHECK(value IN (0,1))`。
- 枚举使用 `TEXT NOT NULL CHECK(...)`；未知枚举不得静默映射为默认值。
- 服务端密钥、用户密钥使用应用层 AEAD 加密；密码仅保存不可逆 Argon2id 哈希；
  session 仅保存 token digest。
- 外键默认限制删除。需要保留历史的实体使用软删除或脱敏，不级联删除流量及审计。

## Relationship Overview

```text
Administrator 1 ── * AdminSession
PanelSettings  1
ManagedXrayInstance 1 ── * AccessProfile
AccessProfile 1 ── * XrayUserIdentity
ManagedUser 1 ── 1 AccessAllocation ── 1 QuotaPolicy
AccessAllocation 1 ── * AccessCredential
AccessAllocation 1 ── * QuotaCycle
AccessAllocation 1 ── 1 TrafficCursor
AccessAllocation 1 ── 1 AllocationTrafficTotal
AccessAllocation 1 ── * DailyTrafficAggregate
AccessAllocation 1 ── * TrafficContinuityEvent
AccessAllocation 1 ── * QuotaResetEvent
AccessAllocation 1 ── * SynchronizationOperation
DomainCommand 1 ── * AuditEvent
```

## Core Entities

### PanelSettings

单例面板设置。

| Field | Type | Rules |
|---|---|---|
| `id` | integer | 固定为 `1` |
| `quota_timezone` | text | 必须是可加载的 IANA 时区名称 |
| `key_verifier` / `key_verifier_nonce` | bytes | 固定明文 `xpanel-root-key-verifier` 的 AEAD 密文；启动时解密失败即主密钥不匹配 |
| `key_encryption_version` | integer | 主密钥版本，MVP 固定为 1 |
| `revision` | integer | 非负，修改时递增 |
| `created_at` | timestamp | UTC |
| `updated_at` | timestamp | UTC |

全局时区的变更不重写已经打开或关闭的周期；新值从下一配额周期生效。

### Administrator

| Field | Type | Rules |
|---|---|---|
| `id` | UUID | 单例管理员 |
| `username` | text | 规范化后唯一、非空 |
| `password_hash` | text | 带算法和参数的 PHC 风格 Argon2id 字符串 |
| `password_version` | integer | 初始化为 1，重置时递增 |
| `created_at` | timestamp | UTC |
| `updated_at` | timestamp | UTC |

数据库不得内置默认管理员或默认密码。初始化只允许在管理员不存在时执行。

### AdminSession

| Field | Type | Rules |
|---|---|---|
| `id` | UUID | 主键 |
| `administrator_id` | UUID | FK |
| `token_digest` | bytes | 唯一，不保存浏览器原 token |
| `password_version` | integer | 必须与管理员当前版本匹配 |
| `data` | bytes | SCS 服务端 session 数据，不含明文凭证 |
| `created_at` | timestamp | UTC |
| `last_seen_at` | timestamp | UTC |
| `idle_expires_at` | timestamp | UTC |
| `absolute_expires_at` | timestamp | UTC |
| `revoked_at` | timestamp nullable | 非空即不可用 |

登录成功轮换 token；登出撤销当前 session；密码重置在同一事务中撤销全部 session。

### ManagedXrayInstance

MVP 中只有一行，但保留不可变 ID 以支持审计和未来迁移。

| Field | Type | Rules |
|---|---|---|
| `id` | UUID | 主键、单例 |
| `name` | text | 非空 |
| `api_endpoint` | text | 仅允许回环地址或等价本机安全通道 |
| `supported_runtime_version` | text | 固定 `v26.3.27`，用于部署状态展示 |
| `health_state` | enum | `unknown`, `healthy`, `unreachable`, `incompatible` |
| `boot_epoch` | text nullable | 基于 uptime/观测时间推导的当前启动纪元 |
| `last_success_at` | timestamp nullable | UTC |
| `last_error_code` | text nullable | 稳定、脱敏错误类别 |
| `last_error_summary` | text nullable | 不含上游原始敏感数据 |
| `updated_at` | timestamp | UTC |

### AccessProfile

管理员登记的、已由运维预配置的 SS2022 入站。XPanel 不创建或删除该入站。

| Field | Type | Rules |
|---|---|---|
| `id` | UUID | 主键 |
| `instance_id` | UUID | FK；与 `inbound_tag` 组合唯一 |
| `name` | text | 未归档配置中规范化后唯一 |
| `inbound_tag` | text | 非空，不允许控制字符 |
| `public_host` | text | 非空；用于客户端配置 |
| `public_port` | integer | 1–65535 |
| `method` | enum | 仅两个受支持的 SS2022 AES method |
| `network` | enum | `tcp`, `udp`, `tcp_udp` |
| `server_key_ciphertext` | bytes | AEAD 密文 |
| `server_key_nonce` | bytes | 每次写入随机且唯一 |
| `key_encryption_version` | integer | 主密钥版本 |
| `bootstrap_statistics_id` | text | 对应静态 bootstrap 用户，全实例唯一 |
| `compatibility_state` | enum | `unverified`, `compatible`, `incompatible`, `unreachable` |
| `compatibility_reason` | text nullable | 安全摘要 |
| `last_validated_at` | timestamp nullable | UTC |
| `revision` | integer | 乐观并发版本 |
| `archived_at` | timestamp nullable | 被引用时只能归档，不能硬删除 |
| `created_at` / `updated_at` | timestamp | UTC |

只有 `compatible` 的 profile 能用于新建用户。`unreachable` 与 `incompatible` 必须分开，
避免暂时断线被误判为永久配置错误。服务端密钥编辑页不得回显现值；空输入表示不修改。

### XrayUserIdentity

统一注册 bootstrap 和面板受管用户的 Xray 统计身份，保证跨 profile 全局唯一。

| Field | Type | Rules |
|---|---|---|
| `id` | UUID | 主键 |
| `instance_id` | UUID | FK |
| `profile_id` | UUID | FK |
| `statistics_id` | text | `UNIQUE(instance_id, statistics_id)`；非空且不含 `>>>` |
| `kind` | enum | `bootstrap`, `managed` |
| `created_at` | timestamp | UTC |

bootstrap identity 不得关联 AccessAllocation，也不得进入用户数量、配额或分享查询。

协调器把 Xray 中带 `xpanel-` 前缀、但 SQLite 中不存在或已删除的身份视为漂移：移除并写
`reconcile_removed_unknown` 审计；非 `xpanel-` 前缀的身份（含 bootstrap）一律保留且不计入
面板统计。

### ManagedUser

| Field | Type | Rules |
|---|---|---|
| `id` | UUID | 主键 |
| `display_name` | text | 去除首尾空白后非空 |
| `normalized_name` | text | 未删除用户中唯一 |
| `lifecycle_state` | enum | `active`, `deleted` |
| `revision` | integer | 乐观并发版本 |
| `created_at` / `updated_at` | timestamp | UTC |
| `deleted_at` | timestamp nullable | 软删除时间 |

部分唯一索引保证 `deleted_at IS NULL` 时名称唯一；删除后可以重用显示名称，但新用户必须
获得全新的 ID、统计身份和流量历史。

显示名称规则：去除首尾空白后 1–64 个字符，不含控制字符与换行；`normalized_name` 为
NFKC 规范化后转小写并折叠连续空白的结果；列表页超过 32 个字符时截断显示并以 `title`
属性给出全名。

### AccessAllocation

每个 ManagedUser 恰好对应一个 allocation，`user_id` 必须具有唯一约束。

| Field | Type | Rules |
|---|---|---|
| `id` | UUID | 主键 |
| `user_id` | UUID | FK、UNIQUE、NOT NULL |
| `profile_id` | UUID | FK、NOT NULL |
| `identity_id` | UUID | FK 到 kind=`managed` 的唯一 identity |
| `admin_enabled` | boolean | 管理员启用意图 |
| `quota_state` | enum | `within_limit`, `exceeded` |
| `projection_state` | enum | `unknown`, `pending`, `present`, `absent`, `error` |
| `observed_present` | boolean nullable | 未知时为 null |
| `desired_revision` | integer | 影响 Xray 的意图变化时递增 |
| `synced_revision` | integer | 最近确认同步的 revision |
| `desired_credential_version` | integer | 应在 Xray 中生效的凭证版本 |
| `synced_credential_version` | integer nullable | 最近确认版本 |
| `last_sync_at` | timestamp nullable | UTC |
| `last_sync_error_code` | text nullable | 稳定错误类别 |
| `last_sync_error_summary` | text nullable | 脱敏摘要 |
| `created_at` / `updated_at` | timestamp | UTC |

期望存在的唯一判定：

```text
user.lifecycle_state == active
AND allocation.admin_enabled
AND allocation.quota_state == within_limit
```

页面状态按下表派生（自上而下第一条命中者生效）；同步错误作为附加标记显示，不得覆盖
手动禁用或配额超限的真实业务原因：

| 条件 | 页面状态 | 标签文案 | FR-008 筛选归属 |
|---|---|---|---|
| `lifecycle_state = deleted` | `deleted` | 已删除 | 已删除 |
| `admin_enabled = false` 且 `projection_state ∈ {present, pending}` | `disabling` | 手动禁用（移除待同步） | 手动禁用、待同步 |
| `admin_enabled = false` | `disabled` | 手动禁用 | 手动禁用 |
| `quota_state = exceeded` 且 `projection_state ∈ {present, pending}` | `quota_disabling` | 配额超限（移除待同步） | 配额超限、待同步 |
| `quota_state = exceeded` | `quota_exceeded` | 配额超限 | 配额超限 |
| 期望存在且 `projection_state ≠ present` | `enabling` | 启用中（待同步） | 待同步 |
| 期望存在且 `projection_state = present` | `active` | 已启用 | 启用 |

- “待同步”筛选 = `desired_revision ≠ synced_revision` 或 `projection_state ∈ {unknown, pending, error}`。
- `projection_state = error` 或 `last_sync_error_code` 非空时追加“同步失败：<安全摘要>”标记。
- 手动禁用与配额超限同时成立时显示“手动禁用”，详情页列出全部阻断原因。

### AccessCredential

| Field | Type | Rules |
|---|---|---|
| `id` | UUID | 主键 |
| `allocation_id` | UUID | FK |
| `version` | integer | `UNIQUE(allocation_id, version)`，从 1 递增 |
| `state` | enum | `pending`, `active`, `retired`, `destroyed` |
| `key_ciphertext` | bytes nullable | pending/active 必须存在；destroyed 必须为空 |
| `key_nonce` | bytes nullable | 与密文同步存在 |
| `key_encryption_version` | integer nullable | 主密钥版本 |
| `created_at` | timestamp | UTC |
| `activated_at` / `retired_at` | timestamp nullable | UTC |

每个 allocation 最多一个 pending 和一个 active credential。新凭证仅在 AddUser 被确认后
变为 active 并允许展示；旧凭证在轮换成功后立即标记 destroyed 并清空密文。

## Quota and Traffic Entities

### QuotaPolicy

| Field | Type | Rules |
|---|---|---|
| `allocation_id` | UUID | PK/FK |
| `limit_bytes` | integer nullable | null 表示无限；否则必须大于 0 且不超过 2^62 |
| `reset_day` | integer | 1–28 |
| `revision` | integer | 修改时递增 |
| `created_at` / `updated_at` | timestamp | UTC |

时区来自 PanelSettings，不在用户级覆盖。reset day 或全局时区变更从下一周期生效。

### QuotaCycle

| Field | Type | Rules |
|---|---|---|
| `id` | UUID | 主键 |
| `allocation_id` | UUID | FK |
| `starts_at_utc` / `ends_at_utc` | timestamp | 结束时间必须晚于开始时间 |
| `timezone_name` | text | 创建周期时的 IANA 时区快照 |
| `reset_day` | integer | 创建周期时的策略快照 |
| `status` | enum | `open`, `closed` |
| `gross_uplink_bytes` / `gross_downlink_bytes` | integer | 真实确认流量，非负 |
| `accounted_uplink_bytes` / `accounted_downlink_bytes` | integer | 当前配额计量值，非负 |
| `manual_reset_count` | integer | 非负 |
| `opened_at` / `closed_at` | timestamp | UTC；closed_at 可空 |

`UNIQUE(allocation_id, starts_at_utc)`；部分唯一索引确保每个 allocation 最多一个 open 周期。
配额判断使用 accounted 总和；真实趋势与累计使用 gross/daily 数据。

展示规则：总用量 = accounted 上行 + 下行；剩余量 = `max(limit_bytes − 总用量, 0)`，超出
部分另以“超额 X”显示；`limit_bytes` 为 null 时剩余量显示“无限制”且不显示百分比；百分比
允许超过 100%，按整数显示。

### AllocationTrafficTotal

| Field | Type | Rules |
|---|---|---|
| `allocation_id` | UUID | PK/FK |
| `uplink_bytes` / `downlink_bytes` | integer | lifetime 已确认物理流量，非负 |
| `updated_at` | timestamp | UTC |

### TrafficCursor

| Field | Type | Rules |
|---|---|---|
| `allocation_id` | UUID | PK/FK |
| `boot_epoch` | text nullable | 当前 Xray 启动纪元 |
| `uplink_counter` / `downlink_counter` | integer nullable | 最近确认绝对值 |
| `uplink_epoch` / `downlink_epoch` | integer | 两方向独立递增 |
| `last_observed_at` | timestamp nullable | UTC |
| `last_success_at` | timestamp nullable | UTC |
| `missing_since` | timestamp nullable | 首次缺失时间 |
| `updated_at` | timestamp | UTC |

方向级 delta 规则：

1. 同一 boot epoch 且当前值不小于游标：`delta = current - previous`。
2. 已确认新 boot epoch：`delta = current`，记录 `node_restart`，再保存新游标。
3. 未确认重启但计数下降：`delta = 0`，递增方向 epoch，记录 `counter_decrease` 并建立
   新基线，优先避免重复计量。
4. 计数缺失：不产生 delta、不覆盖历史，维护 `missing_since`。
5. 首个样本（该方向游标为 null）：`delta = current`。受管身份只由面板创建，其计数在加入
   Xray 时从零开始；记录 `baseline` 事件仅用于诊断。
6. 单方向缺失：仅对缺失方向执行规则 4，另一方向照常提交；配额判定使用两方向最近已确认
   的 accounted 值。
7. 迟到或乱序响应：`observed_at` 早于游标 `last_observed_at` 的样本整体丢弃并记录日志，
   不写入任何聚合。
8. 溢出：`current` 超过 `2^63−1`，或累加后任一聚合超过 `2^63−1` 时，本样本按规则 3 处理
   并记录 `overflow` 事件，不写入溢出值。

### DailyTrafficAggregate

| Field | Type | Rules |
|---|---|---|
| `allocation_id` | UUID | FK |
| `day_start_utc` | timestamp | 与 allocation 组合唯一 |
| `local_date` | date text | 面板配额时区中的日期 |
| `timezone_name` | text | 当日聚合创建时的时区快照 |
| `uplink_bytes` / `downlink_bytes` | integer | 非负真实确认流量 |
| `updated_at` | timestamp | UTC |

轮询跨边界时，本轮 delta 归入样本完成时间所在的日期和周期；超过一个采集间隔的边界
缺口记录 continuity event。日边界使用所属 open QuotaCycle 的 `timezone_name` 快照计算，
而不是 PanelSettings 的最新值；全局时区变更只从下一周期起同时影响周期与日边界。

### TrafficContinuityEvent

只保存异常，不保存普通轮询样本。

| Field | Type | Rules |
|---|---|---|
| `id` | UUID | 主键 |
| `allocation_id` | UUID | FK |
| `type` | enum | `counter_decrease`, `missing`, `reappeared`, `boundary_gap`, `node_restart`, `baseline`, `overflow` |
| `direction` | enum nullable | `uplink`, `downlink` 或全局 |
| `old_counter` / `new_counter` | integer nullable | 非负 |
| `counter_epoch` | integer nullable | 对应方向 epoch |
| `occurred_at` | timestamp | UTC |
| `safe_summary` | text | 不含凭证或上游原始响应 |

### QuotaResetEvent

| Field | Type | Rules |
|---|---|---|
| `id` | UUID | 主键 |
| `allocation_id` / `quota_cycle_id` | UUID | FK |
| `command_id` | UUID | FK，唯一以保证幂等 |
| `previous_accounted_uplink` / `previous_accounted_downlink` | integer | 非负 |
| `uplink_cursor` / `downlink_cursor` | integer nullable | 重置时确认游标快照 |
| `actor_id` | UUID | FK 到管理员 |
| `occurred_at` | timestamp | UTC |

手动重置清零当前周期 accounted 值，gross、lifetime、daily 和周期边界保持不变。

## Coordination and Audit Entities

### DomainCommand

保存浏览器或 CLI 命令的幂等结果。

| Field | Type | Rules |
|---|---|---|
| `id` | UUID | 主键，也是 `_request_id` 的服务端记录 |
| `actor_type` | enum | `administrator`, `local_cli`, `system` |
| `actor_id` | UUID nullable | 系统或本机恢复可为空 |
| `command_type` | text | 稳定动作名 |
| `target_type` / `target_id` | text / UUID | 目标引用 |
| `request_fingerprint` | bytes | 不含密钥或密码 |
| `state` | enum | `accepted`, `completed`, `failed` |
| `result_reference` | text nullable | 规范重定向或安全结果引用 |
| `created_at` / `completed_at` | timestamp | UTC；completed 可空 |

重复 id 且 fingerprint 相同则返回原结果；相同 id 但不同 fingerprint 返回冲突。

### SynchronizationOperation

| Field | Type | Rules |
|---|---|---|
| `id` | UUID | 主键 |
| `allocation_id` | UUID | FK |
| `desired_revision` | integer | 与 allocation 组合唯一 |
| `desired_presence` | boolean | 目标是否存在于 Xray |
| `desired_credential_version` | integer nullable | 添加/轮换所需版本 |
| `reason` | enum | `create`, `enable`, `disable`, `quota_block`, `quota_restore`, `rotate`, `delete`, `reconcile` |
| `phase` | enum | `remove_old`, `add_desired`, `confirm`, `done` |
| `state` | enum | `pending`, `leased`, `retry_wait`, `succeeded`, `superseded`, `permanent_failed` |
| `idempotency_key` | text | UNIQUE |
| `attempt_count` | integer | 非负 |
| `next_attempt_at` | timestamp | UTC |
| `lease_owner` / `lease_expires_at` | text / timestamp nullable | 有限租约 |
| `last_error_code` / `last_error_summary` | text nullable | 稳定且脱敏 |
| `created_at` / `started_at` / `completed_at` | timestamp | 后两者可空 |

Worker 只在事务外调用 Xray。领取、结果确认和重试排程分别使用短事务；旧 revision 必须
标记 superseded。删除不存在视为收敛；添加结果不确定时先读取实际用户，再决定重放。

### AuditEvent

| Field | Type | Rules |
|---|---|---|
| `id` | UUID | 主键 |
| `occurred_at` | timestamp | UTC |
| `actor_type` / `actor_id` | enum / UUID nullable | 操作者 |
| `target_type` / `target_id` | text / UUID | 目标 |
| `action` | text | 稳定动作名 |
| `result` | enum | `accepted`, `succeeded`, `failed`, `superseded` |
| `command_id` / `operation_id` | UUID nullable | 关联标识 |
| `safe_summary` | text nullable | 不含任何可复用秘密 |

审计表 append-only；应用账号不提供 UPDATE/DELETE 审计记录的常规业务路径。

保留策略：MVP 不自动清理审计事件与连续性事件；结构化日志交由 journald 或运维日志轮转
管理，面板不承诺日志保留期。

## State Transitions

### User access

| Event | Stored change | Expected Xray projection |
|---|---|---|
| Create | active + admin enabled + within limit；credential v1 pending | add v1 |
| Add confirmed | projection present；credential v1 active | present |
| Manual disable | admin disabled；revision +1 | remove |
| Manual enable under quota | admin enabled；revision +1 | add desired credential |
| Quota reached | quota exceeded；revision +1 | remove |
| Raise/unlimited quota | recompute within limit；if admin enabled revision +1 | add |
| Manual traffic reset | accounted=0；if admin enabled revision +1 | add if absent |
| New cycle | close/open cycle；clear quota block；honor admin enabled | add only if eligible |
| Delete | lifecycle deleted；admin disabled；revision +1 | remove, then destroy key |

### Credential rotation

1. 事务生成并加密保存下一版本，设置 desired credential version、递增 desired revision，
   创建 phase=`remove_old` 的操作；旧连接信息仍标为即将失效。
2. Worker 删除相同 statistics id 的旧用户；超时后读取实际状态，必要时重放。
3. 持久化 phase=`add_desired` 后，用同一 statistics id 和新 key 添加用户。
4. 确认存在后，新 credential 变为 active，旧 credential 变为 destroyed 并清空密文；
   历史流量、allocation 和 quota cycle 不变。
5. 任一中间失败均保持 pending/error 并重试；不得重新发布旧密钥或虚报轮换完成。
6. 轮换进行中到达的新意图：禁用或删除 → pending 凭证保持不激活，remove 操作以新 revision
   执行并 supersede 轮换操作；再次轮换 → 返回 409，直到当前轮换 succeeded 或
   permanent_failed；调额与手动重置 → 不影响轮换操作。

### Profile compatibility

```text
unverified ──probe success──> compatible
unverified ──network error──> unreachable
unverified ──contract fail──> incompatible
unreachable ──retry success──> compatible
compatible ──version/config drift──> incompatible | unreachable
```

只有 compatible 允许新建用户；已有期望状态仍保存在 SQLite，但不兼容时停止盲目变更并
显示明确故障。各状态下的规则：

| 状态 | 可编辑 | 可重新验证 | 可选为新用户目标 | 已有分配的同步操作 | 可归档 |
|---|---|---|---|---|---|
| `unverified` | 是 | 自动排队 | 否 | 保持 pending，不领取 | 是 |
| `compatible` | 是 | 是 | 是 | 正常执行 | 仅当无未删除分配 |
| `unreachable` | 是 | 协调器每 `reconcile_interval` 自动重试 | 否 | 保持 pending/retry_wait，退避照常但不标记 permanent_failed | 仅当无未删除分配 |
| `incompatible` | 是 | 仅手动 | 否 | 保持 pending，不领取；页面显示“配置不兼容，等待运维修复” | 仅当无未删除分配 |

- 修改 `inbound_tag`、`method` 或服务端密钥后回到 `unverified` 并重新验证；仅修改
  `name`、`public_host`、`public_port` 不触发验证。
- 漂移检测：synchronizer 或 collector 收到 `incompatible_profile`、`unsupported_protocol`
  或 `profile_not_found` 时把 profile 置为 `incompatible`；连续 3 次 `instance_unavailable`
  或 `deadline_exceeded` 时置为 `unreachable`；协调器每次 `ValidateProfile` 成功后恢复
  `compatible`。
- 归档：`archived_at` 非空的 profile 不再出现在选择列表与协调范围；归档前其下不得有未
  删除的分配。
- 用户详情与仪表盘：所属 profile 非 `compatible` 时显示 profile 状态标记，隐藏启用、轮换、
  恢复等需要 Xray 投影的操作，仍允许编辑显示名称、配额以及禁用/删除意图。

## Atomic Transaction Boundaries

1. 创建：command、user、identity、allocation、credential、policy、open cycle、cursor、
   sync operation 和 audit 一次提交。
2. 编辑/启停/调额：校验 revision，更新业务状态，重算 quota，必要时递增 desired revision，
   upsert operation 与 audit 一次提交。
3. 采集：一轮 RPC 完成后，用一个短事务更新所有返回用户的 cursor、totals、daily、cycle，
   并写入首次越界的 remove operation 与审计。
4. 手动重置：reset event、accounted 清零、quota 状态、恢复 operation 与审计一次提交。
5. 周期切换：关闭旧周期、创建新周期、清除 quota block、恢复 operation 与审计一次提交。
6. 密码重置：新 password hash/version、全部 session 撤销和审计一次提交。
7. 轮换：校验 revision，生成并加密保存下一版本 credential（pending）、更新
   desired_credential_version、递增 desired revision、创建 phase=`remove_old` 的 operation
   与 audit 一次提交；确认事务中把新 credential 置为 active、旧 credential 置为 destroyed
   并清空密文。
8. 删除：校验 revision，lifecycle 置为 deleted、admin_enabled 置为 false、递增 desired
   revision、创建 remove operation 与 audit 一次提交；移除确认后在独立事务销毁全部
   credential 密文。
9. RPC 调用永远在事务外；结果用 revision 条件在独立短事务中确认。

## Write Ordering Under Contention

调额、手动重置、周期切换与采集越界都经同一写入路径串行提交，并以 allocation 的
`revision`/`desired_revision` 判定先后：

- 采集越界与手动重置：以提交顺序为准；重置事务读取的是已提交的 accounted 值，重置后的
  下一轮采集从新游标起算。
- 调额与采集越界：调额事务重算 `quota_state`；若采集事务先提交并已写 quota_block 操作，
  提高配额的事务再写 quota_restore 并 supersede 前者。
- 周期切换与其他写入：scheduler 使用按边界时刻设置的定时器排队一次切换，并每 60 秒
  兜底扫描；切换事务重新读取最新策略与 admin_enabled，因此边界附近的调额或禁用无论
  先后都得到同一最终状态；`UNIQUE(allocation_id, starts_at_utc)` 保证只执行一次。
- 时钟：边界一次计算并以 UTC 存储；时钟回拨不会重开已关闭周期，也不会重复结算；时钟
  前跳或停机跨越多个边界时按顺序补做，最终只保留一个 open 周期。

## Migration Rules

- 使用嵌入式、顺序编号 SQL migrations；已经发布的 migration 不得修改或重编号。
- 迁移必须先于 HTTP listener、collector 和 reconciler 启动。
- 迁移失败时 `/readyz` 不就绪，任何写服务和后台任务不得启动。
- 破坏性迁移前创建可验证备份；SQLite 重建表采用“新表—复制—校验—替换”流程。
- 数据库目录权限为 `0700`，数据库/WAL/SHM/备份为最小权限；备份与 AEAD 主密钥分离。
