-- +goose Up
-- 每用户专属入站：访问配置改造为入站模板，新增专属入站与端口分配。
--
-- 破坏性变更：001 的共享入站模型下的用户在本模型中无法运行（没有端口、没有专属入站），
-- 因此本迁移删除全部访问模型数据（模板、身份、分配、凭证、配额、流量、同步意图）。
-- 管理员、会话、面板设置、实例与审计记录予以保留。执行前 MUST 按 docs/operations.md §2 生成可验证备份。

DELETE FROM traffic_continuity_events;
DELETE FROM daily_traffic_aggregates;
DELETE FROM traffic_cursors;
DELETE FROM allocation_traffic_totals;
DELETE FROM quota_reset_events;
DELETE FROM quota_cycles;
DELETE FROM quota_policies;
DELETE FROM access_credentials;
DELETE FROM synchronization_operations;
DELETE FROM drift_removals;
DELETE FROM access_allocations;
DELETE FROM managed_users;
DELETE FROM xray_user_identities;
DELETE FROM access_profiles;

-- 重命名后 SQLite 会自动改写其它表中指向本表的外键引用（legacy_alter_table 默认关闭）。
ALTER TABLE access_profiles RENAME TO inbound_templates;

-- 重建为入站模板的列形态：新增监听地址与端口池，移除服务端密钥、bootstrap 身份与公开端口。
-- inbound_tag 属于 UNIQUE 约束的一部分，无法用 DROP COLUMN 移除，因此整表重建。
CREATE TABLE inbound_templates_new (
    id TEXT PRIMARY KEY,
    instance_id TEXT NOT NULL REFERENCES managed_xray_instances(id),
    name TEXT NOT NULL,
    normalized_name TEXT NOT NULL,
    public_host TEXT NOT NULL,
    listen_address TEXT NOT NULL,
    port_pool_start INTEGER NOT NULL CHECK (port_pool_start BETWEEN 1024 AND 65535),
    port_pool_end INTEGER NOT NULL CHECK (port_pool_end BETWEEN 1024 AND 65535),
    method TEXT NOT NULL CHECK (method IN ('2022-blake3-aes-128-gcm','2022-blake3-aes-256-gcm')),
    network TEXT NOT NULL CHECK (network IN ('tcp','udp','tcp_udp')),
    compatibility_state TEXT NOT NULL CHECK (compatibility_state IN ('unverified','compatible','incompatible','unreachable')),
    compatibility_reason TEXT,
    last_validated_at INTEGER,
    revision INTEGER NOT NULL DEFAULT 0 CHECK (revision >= 0),
    archived_at INTEGER,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    CHECK (port_pool_end >= port_pool_start)
) STRICT;
DROP TABLE inbound_templates;
ALTER TABLE inbound_templates_new RENAME TO inbound_templates;
CREATE UNIQUE INDEX inbound_templates_active_name_idx ON inbound_templates(normalized_name) WHERE archived_at IS NULL;

-- 身份表：外键改指入站模板，kind 只保留 managed（bootstrap 概念随共享入站模型一并退役）。
CREATE TABLE xray_user_identities_new (
    id TEXT PRIMARY KEY,
    instance_id TEXT NOT NULL REFERENCES managed_xray_instances(id),
    template_id TEXT NOT NULL REFERENCES inbound_templates(id),
    statistics_id TEXT NOT NULL CHECK (length(statistics_id) > 0 AND instr(statistics_id, '>>>') = 0),
    kind TEXT NOT NULL CHECK (kind IN ('managed')),
    created_at INTEGER NOT NULL,
    UNIQUE(instance_id, statistics_id)
) STRICT;
DROP TABLE xray_user_identities;
ALTER TABLE xray_user_identities_new RENAME TO xray_user_identities;

-- 同步阶段扩展：create_inbound / remove_inbound。SQLite 无法直接改 CHECK，改用重建列约束的等价做法：
-- 由于本迁移已清空 synchronization_operations，直接重建该表最简单也最安全。
CREATE TABLE synchronization_operations_new (
    id TEXT PRIMARY KEY,
    allocation_id TEXT NOT NULL REFERENCES access_allocations(id),
    desired_revision INTEGER NOT NULL CHECK (desired_revision > 0),
    desired_presence INTEGER NOT NULL CHECK (desired_presence IN (0,1)),
    desired_credential_version INTEGER,
    reason TEXT NOT NULL CHECK (reason IN ('create','enable','disable','rotate','delete','quota_block','quota_restore','reconcile')),
    phase TEXT NOT NULL CHECK (phase IN ('create_inbound','remove_inbound','remove_old','add_desired','confirm','done')),
    state TEXT NOT NULL CHECK (state IN ('pending','leased','retry_wait','succeeded','permanent_failed','superseded')),
    idempotency_key TEXT NOT NULL,
    attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    next_attempt_at INTEGER NOT NULL,
    lease_owner TEXT,
    lease_expires_at INTEGER,
    last_error_code TEXT,
    last_error_summary TEXT,
    created_at INTEGER NOT NULL,
    started_at INTEGER,
    completed_at INTEGER
) STRICT;
DROP TABLE synchronization_operations;
ALTER TABLE synchronization_operations_new RENAME TO synchronization_operations;

-- 引用列更名，保持与入站模板一致的命名（SQLite 会同步更新索引与外键定义）。
ALTER TABLE access_allocations RENAME COLUMN profile_id TO template_id;
ALTER TABLE drift_removals RENAME COLUMN profile_id TO template_id;
-- 漂移目标自带入站标签与种类：identity 为面板入站内的未知客户端，inbound 为孤立入站（T061）。
ALTER TABLE drift_removals ADD COLUMN inbound_tag TEXT NOT NULL DEFAULT '';
ALTER TABLE drift_removals ADD COLUMN kind TEXT NOT NULL DEFAULT 'identity';

-- 专属入站：与 access_allocations 一对一，持有端口分配与该入站独立的服务端密钥。
CREATE TABLE dedicated_inbounds (
    allocation_id TEXT PRIMARY KEY REFERENCES access_allocations(id),
    template_id TEXT NOT NULL REFERENCES inbound_templates(id),
    inbound_tag TEXT NOT NULL UNIQUE,
    listen_address TEXT NOT NULL,
    port INTEGER NOT NULL CHECK (port BETWEEN 1024 AND 65535),
    server_key_ciphertext BLOB NOT NULL,
    server_key_nonce BLOB NOT NULL,
    key_encryption_version INTEGER NOT NULL CHECK (key_encryption_version > 0),
    desired_present INTEGER NOT NULL DEFAULT 0 CHECK (desired_present IN (0,1)),
    observed_present INTEGER CHECK (observed_present IN (0,1)),
    last_sync_at INTEGER,
    released_at INTEGER,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
) STRICT;

-- FR-008 端口唯一性的唯一权威保证：实测证明 Xray 不会拒绝重复端口（research.md R-002）。
CREATE UNIQUE INDEX dedicated_inbounds_port_idx ON dedicated_inbounds(listen_address, port) WHERE released_at IS NULL;
CREATE INDEX dedicated_inbounds_template_idx ON dedicated_inbounds(template_id) WHERE released_at IS NULL;

-- +goose Down
-- 回滚只恢复结构，不恢复数据：端口分配与专属入站信息无法还原。
--
-- 顺序要求：必须先把 inbound_templates 改名回 access_profiles，SQLite 才会把其它表里指向它的外键
-- 引用一并改回来（legacy_alter_table 默认关闭）。先删表再改名会留下悬空外键，导致后续迁移失败。
DROP INDEX dedicated_inbounds_template_idx;
DROP INDEX dedicated_inbounds_port_idx;
DROP TABLE dedicated_inbounds;

ALTER TABLE drift_removals DROP COLUMN kind;
ALTER TABLE drift_removals DROP COLUMN inbound_tag;
ALTER TABLE drift_removals RENAME COLUMN template_id TO profile_id;
ALTER TABLE access_allocations RENAME COLUMN template_id TO profile_id;

DROP INDEX inbound_templates_active_name_idx;
ALTER TABLE inbound_templates RENAME TO access_profiles;

CREATE TABLE access_profiles_old (
    id TEXT PRIMARY KEY,
    instance_id TEXT NOT NULL REFERENCES managed_xray_instances(id),
    name TEXT NOT NULL,
    normalized_name TEXT NOT NULL,
    inbound_tag TEXT NOT NULL,
    public_host TEXT NOT NULL,
    public_port INTEGER NOT NULL CHECK (public_port BETWEEN 1 AND 65535),
    method TEXT NOT NULL CHECK (method IN ('2022-blake3-aes-128-gcm','2022-blake3-aes-256-gcm')),
    network TEXT NOT NULL CHECK (network IN ('tcp','udp','tcp_udp')),
    server_key_ciphertext BLOB NOT NULL,
    server_key_nonce BLOB NOT NULL,
    key_encryption_version INTEGER NOT NULL CHECK (key_encryption_version > 0),
    bootstrap_statistics_id TEXT NOT NULL,
    compatibility_state TEXT NOT NULL CHECK (compatibility_state IN ('unverified','compatible','incompatible','unreachable')),
    compatibility_reason TEXT,
    last_validated_at INTEGER,
    revision INTEGER NOT NULL DEFAULT 0 CHECK (revision >= 0),
    archived_at INTEGER,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE(instance_id, inbound_tag)
) STRICT;
DROP TABLE access_profiles;
ALTER TABLE access_profiles_old RENAME TO access_profiles;
CREATE UNIQUE INDEX access_profiles_active_name_idx ON access_profiles(normalized_name) WHERE archived_at IS NULL;

CREATE TABLE xray_user_identities_old (
    id TEXT PRIMARY KEY,
    instance_id TEXT NOT NULL REFERENCES managed_xray_instances(id),
    profile_id TEXT NOT NULL REFERENCES access_profiles(id),
    statistics_id TEXT NOT NULL CHECK (length(statistics_id) > 0 AND instr(statistics_id, '>>>') = 0),
    kind TEXT NOT NULL CHECK (kind IN ('bootstrap','managed')),
    created_at INTEGER NOT NULL,
    UNIQUE(instance_id, statistics_id)
) STRICT;
DROP TABLE xray_user_identities;
ALTER TABLE xray_user_identities_old RENAME TO xray_user_identities;
