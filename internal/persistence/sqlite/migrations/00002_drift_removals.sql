-- +goose Up
-- 漂移移除意图：协调器发现 xpanel- 命名空间内 SQLite 无记录的身份时先落库，再由 synchronizer 在事务外串行执行（Constitution I/IV）。
CREATE TABLE drift_removals (
    id TEXT PRIMARY KEY,
    profile_id TEXT NOT NULL REFERENCES access_profiles(id),
    statistics_id TEXT NOT NULL CHECK (length(statistics_id) > 0 AND instr(statistics_id, '>>>') = 0),
    state TEXT NOT NULL CHECK (state IN ('pending','leased','retry_wait','succeeded','permanent_failed')),
    attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    next_attempt_at INTEGER NOT NULL,
    lease_owner TEXT,
    lease_expires_at INTEGER,
    last_error_code TEXT,
    last_error_summary TEXT,
    created_at INTEGER NOT NULL,
    completed_at INTEGER
) STRICT;
CREATE UNIQUE INDEX drift_removals_open_idx ON drift_removals(profile_id, statistics_id) WHERE state IN ('pending','leased','retry_wait');

-- +goose Down
DROP TABLE drift_removals;
