-- +goose Up
-- 能力世代取代「boot epoch 字符串精确相等」作为兼容性证据的绑定依据。
--
-- 原因：boot epoch 由 uptime（uint32 整秒）推算，同一个 Xray 进程在相邻两次探测之间就会有
-- ±1 秒的量化抖动（research.md R-006 / T150）。用字符串精确不等判定「重启」，会让模板在没有任何
-- 重启的情况下被反复置回待验证。改为持久化一个单调递增的能力世代：只有按 domain.RestartConfirmed
-- （uptime 一秒容差）确认的重启才让它前进，模板记录自己通过门禁时的世代号（FR-005/FR-029）。
ALTER TABLE managed_xray_instances ADD COLUMN capability_generation INTEGER NOT NULL DEFAULT 1;
ALTER TABLE inbound_templates ADD COLUMN validated_generation INTEGER;
ALTER TABLE inbound_templates DROP COLUMN validated_boot_epoch;

-- 升级前通过门禁的模板没有任何世代证据：它们是对「哪个 Xray 进程」验证的已不可考，
-- 一律置回待验证，由协调器对当前世代重跑门禁（T094）。
UPDATE inbound_templates SET compatibility_state='unverified',
    compatibility_reason='capability evidence predates capability generations; the gate must run again',
    last_validated_at=NULL, revision=revision+1
    WHERE archived_at IS NULL AND compatibility_state='compatible';

-- +goose Down
ALTER TABLE inbound_templates ADD COLUMN validated_boot_epoch TEXT;
ALTER TABLE inbound_templates DROP COLUMN validated_generation;
ALTER TABLE managed_xray_instances DROP COLUMN capability_generation;
