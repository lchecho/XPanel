-- +goose Up
-- 新增同步意图原因 port_change：管理员在端口被面板外进程占用时更换该用户的专属端口（FR-010）。
-- 该意图在一次同步步骤内「先移除旧入站再按新端口重建」，中间状态是合法的（端口不监听），
-- 与轮换不同，它允许监听中断。
--
-- 同时修复 00004 重建该表时丢失的两个唯一约束：idempotency_key 的唯一性是「同一请求只产生一条意图」
-- 的最终保证，UNIQUE(allocation_id, desired_revision) 保证同一分配的同一版本只有一条意图。
CREATE TABLE synchronization_operations_new (
    id TEXT PRIMARY KEY,
    allocation_id TEXT NOT NULL REFERENCES access_allocations(id),
    desired_revision INTEGER NOT NULL,
    desired_presence INTEGER NOT NULL CHECK (desired_presence IN (0,1)),
    desired_credential_version INTEGER,
    reason TEXT NOT NULL CHECK (reason IN ('create','enable','disable','rotate','delete','quota_block','quota_restore','reconcile','port_change')),
    phase TEXT NOT NULL CHECK (phase IN ('create_inbound','remove_inbound','remove_old','add_desired','confirm','done')),
    state TEXT NOT NULL CHECK (state IN ('pending','leased','retry_wait','succeeded','permanent_failed','superseded')),
    idempotency_key TEXT NOT NULL UNIQUE,
    attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    next_attempt_at INTEGER NOT NULL,
    lease_owner TEXT,
    lease_expires_at INTEGER,
    last_error_code TEXT,
    last_error_summary TEXT,
    created_at INTEGER NOT NULL,
    started_at INTEGER,
    completed_at INTEGER,
    UNIQUE(allocation_id, desired_revision)
) STRICT;
INSERT INTO synchronization_operations_new SELECT id,allocation_id,desired_revision,desired_presence,desired_credential_version,
    reason,phase,state,idempotency_key,attempt_count,next_attempt_at,lease_owner,lease_expires_at,last_error_code,
    last_error_summary,created_at,started_at,completed_at FROM synchronization_operations;
DROP TABLE synchronization_operations;
ALTER TABLE synchronization_operations_new RENAME TO synchronization_operations;
CREATE INDEX synchronization_operations_due_idx ON synchronization_operations(state, next_attempt_at);
CREATE INDEX synchronization_operations_allocation_idx ON synchronization_operations(allocation_id, desired_revision);

-- +goose Down
-- 回滚前必须先清理 port_change 意图，否则 CHECK 会拒绝。
DELETE FROM synchronization_operations WHERE reason='port_change';
CREATE TABLE synchronization_operations_old (
    id TEXT PRIMARY KEY,
    allocation_id TEXT NOT NULL REFERENCES access_allocations(id),
    desired_revision INTEGER NOT NULL,
    desired_presence INTEGER NOT NULL CHECK (desired_presence IN (0,1)),
    desired_credential_version INTEGER,
    reason TEXT NOT NULL CHECK (reason IN ('create','enable','disable','rotate','delete','quota_block','quota_restore','reconcile')),
    phase TEXT NOT NULL CHECK (phase IN ('create_inbound','remove_inbound','remove_old','add_desired','confirm','done')),
    state TEXT NOT NULL CHECK (state IN ('pending','leased','retry_wait','succeeded','permanent_failed','superseded')),
    idempotency_key TEXT NOT NULL UNIQUE,
    attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    next_attempt_at INTEGER NOT NULL,
    lease_owner TEXT,
    lease_expires_at INTEGER,
    last_error_code TEXT,
    last_error_summary TEXT,
    created_at INTEGER NOT NULL,
    started_at INTEGER,
    completed_at INTEGER,
    UNIQUE(allocation_id, desired_revision)
) STRICT;
INSERT INTO synchronization_operations_old SELECT id,allocation_id,desired_revision,desired_presence,desired_credential_version,
    reason,phase,state,idempotency_key,attempt_count,next_attempt_at,lease_owner,lease_expires_at,last_error_code,
    last_error_summary,created_at,started_at,completed_at FROM synchronization_operations;
DROP TABLE synchronization_operations;
ALTER TABLE synchronization_operations_old RENAME TO synchronization_operations;
CREATE INDEX synchronization_operations_due_idx ON synchronization_operations(state, next_attempt_at);
CREATE INDEX synchronization_operations_allocation_idx ON synchronization_operations(allocation_id, desired_revision);
