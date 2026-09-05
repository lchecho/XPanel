# XPanel 运维手册：备份、恢复、主密钥与时钟

本文件是 `plan.md §Validation` 与 `contracts/config.md` 引用的运维责任说明，配合发布门禁 `make check` 使用。

## 1. 需要备份的内容

| 项目 | 位置（示例配置） | 说明 |
|---|---|---|
| SQLite 数据库 | `/var/lib/xpanel/xpanel.db` 及 `-wal`/`-shm` | 唯一权威状态：用户、凭证密文、配额、流量聚合、同步意图、审计 |
| 主密钥文件 | `/etc/xpanel/root.key` | 32 字节 Base64，`0600`；**与数据库分开备份、分开存放** |
| 应用配置 | `xpanel.json` | 不含任何密钥或密码，可与数据库一起备份 |

丢失主密钥意味着所有服务端密钥与用户密钥密文不可恢复：用户需要重新登记访问配置并轮换全部凭证。
把主密钥与数据库放在同一份备份里会使字段加密失去意义。

## 2. 在线备份

数据库以 WAL 模式运行，可以在服务不停机时做一致性备份。两种等价方式：

```sh
# 方式 A：sqlite3 命令行（推荐）
sqlite3 /var/lib/xpanel/xpanel.db ".backup '/var/backups/xpanel/xpanel-$(date +%F).db'"

# 方式 B：VACUUM INTO（生成紧凑副本）
sqlite3 /var/lib/xpanel/xpanel.db "VACUUM INTO '/var/backups/xpanel/xpanel-$(date +%F).db'"
```

备份文件权限设为 `0600`，目录 `0700`。不要直接复制 `xpanel.db` 文件本身（WAL 未合并会导致副本不一致）；
若必须复制文件，先执行 `PRAGMA wal_checkpoint(TRUNCATE);` 并停止服务。

主密钥单独备份到另一介质或密钥管理系统：

```sh
install -m 0600 /etc/xpanel/root.key /secure/offline/xpanel-root.key
```

## 3. 恢复步骤

1. 停止服务：`systemctl stop xpanel`。
2. 恢复主密钥到 `security.root_key_file` 指定路径，`chmod 0600`，属主为 XPanel 服务账号。
3. 把备份数据库复制到 `storage.database_path`，删除残留的 `-wal`/`-shm` 文件，`chmod 0600`。
4. 启动服务：`systemctl start xpanel`。启动时会：
   - 校验主密钥能解密 `panel_settings.key_verifier`，不匹配则以 `root_key_mismatch` 拒绝就绪；
   - 执行迁移、加锁 `<database>.lock`（单写入实例）；
   - 由协调器把 Xray 实际用户集合收敛到数据库中的期望状态。
5. 验证：
   - `curl -fsS http://127.0.0.1:8080/readyz` 返回 `ready`；
   - 登录后仪表盘节点健康为“健康”，无“持续不同步”标记；
   - 应启用用户在 60 秒内重新出现在 Xray（`GET /users` 状态为“已启用”），手动禁用、配额超限、已删除用户保持不可用；
   - 审计页可见 `reconcile` 相关同步确认。

自动化证据：`tests/integration/backup_restore_test.go` 用 `VACUUM INTO` 备份、在新路径恢复并对模拟重启的 Xray 执行协调，验证上述收敛规则。

## 4. 主密钥丢失或不匹配

- 启动日志出现 `root_key_mismatch`：主密钥文件不是创建数据库时使用的那一份。找回正确的备份后重启。
- 确认无法找回：只能重新初始化数据库（用户与访问配置需重新登记），审计与流量历史可从旧库只读导出保存。
- 第一版不提供主密钥轮换；轮换需要未来的版本化重加密工具。

## 5. 时钟、单实例与日志

- 面板不校验 NTP。启动时若系统时间早于构建时间或数据库中最新的 `updated_at`，会输出警告。请为主机启用 NTP；
  配额周期边界一次计算并以 UTC 存储，时钟回拨不会重开已关闭周期。
- 同一数据库只能由一个 XPanel 进程写入。第二个进程启动会因 `<database>.lock` 被占用而以退出码 3 结束。
- 结构化日志输出到标准错误，由 systemd/journald 或运维日志轮转管理；面板不承诺日志保留期。
  日志已对密码、密钥、令牌与完整 `ss://` URI 脱敏，但仍应按敏感控制面日志保护。
- 审计事件与统计连续性事件不会自动清理；容量规模（≤20 个活跃分配）下增长可控。
