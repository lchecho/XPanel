-- +goose Up
-- 能力世代的推进信号不能只靠 boot epoch 差值：epoch 由整秒 uptime 推算，
-- 「上一个进程只跑了很短时间」或「新旧进程的 epoch 恰好相同/只差一秒」的重启会被秒级量化吞掉。
-- 额外持久化最近观测到的 uptime：它在重启后必然回落，是不会被量化吞掉的可靠信号（T098）。
ALTER TABLE managed_xray_instances ADD COLUMN last_uptime_seconds INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE managed_xray_instances DROP COLUMN last_uptime_seconds;
