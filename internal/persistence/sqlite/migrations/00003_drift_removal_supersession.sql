-- +goose Up
-- 漂移移除意图的显式因果链：同一 profile/identity 排队新意图时，把此前 permanent_failed 的记录指向新意图（T155）。
-- 只有被后续意图显式取代的永久失败才不再冻结 profile 契约字段；不再依赖 created_at 的时间比较。
ALTER TABLE drift_removals ADD COLUMN superseded_by TEXT REFERENCES drift_removals(id) DEFAULT NULL;
CREATE INDEX drift_removals_unsuperseded_failed_idx ON drift_removals(profile_id, statistics_id) WHERE state = 'permanent_failed' AND superseded_by IS NULL;

-- +goose Down
DROP INDEX drift_removals_unsuperseded_failed_idx;
ALTER TABLE drift_removals DROP COLUMN superseded_by;
