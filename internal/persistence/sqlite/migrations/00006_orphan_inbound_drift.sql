-- +goose Up
-- 孤立入站的移除意图不再依赖入站模板：面板命名空间内、面板无记录的入站必须能被清理，
-- 即使此时一个模板都没有、或全部模板都已归档（FR-031）。因此 template_id 改为可空，
-- 并把开放意图与未取代失败意图的唯一性索引改为按 COALESCE(template_id,'') 归组，
-- 以免 NULL 在唯一索引中互不相等导致同一入站被重复排队。
CREATE TABLE drift_removals_new (
    id TEXT PRIMARY KEY,
    template_id TEXT REFERENCES inbound_templates(id),
    statistics_id TEXT NOT NULL CHECK (length(statistics_id) > 0 AND instr(statistics_id, '>>>') = 0),
    state TEXT NOT NULL CHECK (state IN ('pending','leased','retry_wait','succeeded','permanent_failed')),
    attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    next_attempt_at INTEGER NOT NULL,
    lease_owner TEXT,
    lease_expires_at INTEGER,
    last_error_code TEXT,
    last_error_summary TEXT,
    created_at INTEGER NOT NULL,
    completed_at INTEGER,
    superseded_by TEXT REFERENCES drift_removals(id) DEFAULT NULL,
    inbound_tag TEXT NOT NULL DEFAULT '',
    kind TEXT NOT NULL DEFAULT 'identity' CHECK (kind IN ('identity','inbound'))
) STRICT;
INSERT INTO drift_removals_new SELECT id,template_id,statistics_id,state,attempt_count,next_attempt_at,lease_owner,
    lease_expires_at,last_error_code,last_error_summary,created_at,completed_at,superseded_by,inbound_tag,kind
    FROM drift_removals;
DROP INDEX drift_removals_open_idx;
DROP INDEX drift_removals_unsuperseded_failed_idx;
DROP TABLE drift_removals;
ALTER TABLE drift_removals_new RENAME TO drift_removals;
CREATE UNIQUE INDEX drift_removals_open_idx ON drift_removals(COALESCE(template_id,''), statistics_id) WHERE state IN ('pending','leased','retry_wait');
CREATE INDEX drift_removals_unsuperseded_failed_idx ON drift_removals(COALESCE(template_id,''), statistics_id) WHERE state = 'permanent_failed' AND superseded_by IS NULL;

-- +goose Down
-- 回滚前必须先清理没有归属模板的意图，否则 NOT NULL 会拒绝。
DELETE FROM drift_removals WHERE template_id IS NULL;
CREATE TABLE drift_removals_old (
    id TEXT PRIMARY KEY,
    template_id TEXT NOT NULL REFERENCES inbound_templates(id),
    statistics_id TEXT NOT NULL CHECK (length(statistics_id) > 0 AND instr(statistics_id, '>>>') = 0),
    state TEXT NOT NULL CHECK (state IN ('pending','leased','retry_wait','succeeded','permanent_failed')),
    attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    next_attempt_at INTEGER NOT NULL,
    lease_owner TEXT,
    lease_expires_at INTEGER,
    last_error_code TEXT,
    last_error_summary TEXT,
    created_at INTEGER NOT NULL,
    completed_at INTEGER,
    superseded_by TEXT REFERENCES drift_removals(id) DEFAULT NULL,
    inbound_tag TEXT NOT NULL DEFAULT '',
    kind TEXT NOT NULL DEFAULT 'identity'
) STRICT;
INSERT INTO drift_removals_old SELECT id,template_id,statistics_id,state,attempt_count,next_attempt_at,lease_owner,
    lease_expires_at,last_error_code,last_error_summary,created_at,completed_at,superseded_by,inbound_tag,kind
    FROM drift_removals;
DROP INDEX drift_removals_open_idx;
DROP INDEX drift_removals_unsuperseded_failed_idx;
DROP TABLE drift_removals;
ALTER TABLE drift_removals_old RENAME TO drift_removals;
CREATE UNIQUE INDEX drift_removals_open_idx ON drift_removals(template_id, statistics_id) WHERE state IN ('pending','leased','retry_wait');
CREATE INDEX drift_removals_unsuperseded_failed_idx ON drift_removals(template_id, statistics_id) WHERE state = 'permanent_failed' AND superseded_by IS NULL;
