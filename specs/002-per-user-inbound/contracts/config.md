# Contract: 部署与配置（每用户专属入站增量）

本文件只描述相对 `specs/001-xray-user-management/contracts/config.md` 的变化。

## 面板配置

`xpanel.json` 结构不变。端口池属于入站模板的业务数据，存于数据库并通过界面维护，
MUST NOT 写入配置文件。

## Xray 基础配置（部署前置条件）

面板不再需要运维预配置 SS2022 入站，但仍依赖以下基础配置段，它们无法通过 API 设置：

```json
{
  "api":    { "tag": "api", "listen": "127.0.0.1:10085", "services": ["HandlerService", "StatsService"] },
  "stats":  {},
  "policy": { "levels": { "0": { "statsUserUplink": true, "statsUserDownlink": true } } },
  "outbounds": [ { "protocol": "freedom", "tag": "direct" } ]
}
```

- `api.tag` MUST 非空，否则 Xray 拒绝启动（`API tag can't be empty`）。
- `policy.levels."0".statsUserUplink/statsUserDownlink` MUST 为 `true`，否则面板创建的入站不会
  产生用户级计数器，配额将永远不触发（research.md R-007）。入站模板的兼容性校验 MUST 检出该缺失
  并标记为不兼容。
- `inbounds` MAY 为空，也 MAY 保留运维自有入站；面板只操作自己命名空间内的入站。
- 管理端点 MUST 仅监听回环地址。

## 端口规划

- 端口池 MUST 避开 Xray 管理端口、运维自有入站端口和主机上其他服务端口。面板通过创建失败与
  创建前预绑定探测识别外部占用，MUST NOT 主动扫描主机端口。
- 防火墙 MUST 放行整个端口池范围，否则新建用户虽然创建成功但外部不可达。
- 端口池容量即用户数上限；容量耗尽时面板拒绝新建并提示扩容位置。

## 运维影响

Xray 重启会使所有面板入站消失，全部用户端口在协调完成前不可用（正常为数秒）。
`docs/operations.md` MUST 记录该窗口、迁移 `00004` 的破坏性质与备份恢复步骤。
