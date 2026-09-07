-- +goose Up
-- 模板的兼容性证据必须绑定到当前 Xray 的启动纪元（能力世代）。
--
-- 原因：能力门禁验证的是「这个正在运行的 Xray 进程」是否具备创建/移除入站、多用户身份与
-- 用户级统计的能力。节点重启后配置可能已经变了（例如运维把 policy 去掉），旧的 compatible
-- 结论对新进程不成立。记录通过门禁时的 boot epoch，重启后即失效并重跑门禁（FR-005/FR-029）。
ALTER TABLE inbound_templates ADD COLUMN validated_boot_epoch TEXT;

-- +goose Down
ALTER TABLE inbound_templates DROP COLUMN validated_boot_epoch;
