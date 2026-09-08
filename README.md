<div align="center">

# XPanel

**一用户一入站、一用户一端口的 Xray 管理面板**

面向少量共享用户的自托管面板：为每个用户创建独立的 Shadowsocks 2022 入站与端口，
按月度周期统计并限制流量，配额周期开始时自动恢复访问。

[![Go](https://img.shields.io/badge/Go-1.26.8-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![Xray-core](https://img.shields.io/badge/Xray--core-v26.3.27-1f6feb)](https://github.com/XTLS/Xray-core)
[![SQLite](https://img.shields.io/badge/SQLite-embedded-003B57?logo=sqlite&logoColor=white)](https://sqlite.org/)
[![License](https://img.shields.io/badge/License-MIT-green)](LICENSE)

[快速开始](#-快速开始) · [工作原理](#-工作原理) · [配额语义](#-配额语义请如实告知使用者) · [运维文档](docs/operations.md)

</div>

---

## ✨ 特性

- **端口级隔离** —— 每个用户拥有独立的入站与端口，创建、禁用、轮换、删除任一用户都不影响其他人。
- **面板直接管理入站** —— 无需预配置任何 SS2022 入站，面板在运行时通过 Xray API 创建与移除，
  不写 Xray 配置文件。
- **端口池自动分配** —— 在入站模板上配置端口区间，创建用户时自动取最小空闲端口，也可手动指定；
  端口冲突与池耗尽由数据库约束保证，不靠应用层先查后写。
- **能力门禁** —— 创建用户前先在一次性探针入站上跑一次经过身份认证的真实流量，
  证实该节点支持运行时入站管理、SS2022 多用户身份与用户级流量统计；节点重启后自动重跑门禁。
- **诚实的流量计量** —— 计数器缺失不当作零、下降不产生负增量、重启与重复请求不重复计量。
- **可恢复的状态机** —— 所有变更先落库再投影到 Xray；进程崩溃、租约丢失、RPC 超时后都能从持久意图续跑。
- **服务端渲染界面** —— `html/template` + HTMX，无构建步骤；纯键盘可完成全部管理流程。
- **单二进制** —— `CGO_ENABLED=0` 静态编译，嵌入迁移与前端资源，仅依赖本地 SQLite 文件。

## 📦 环境要求

| 项目 | 要求 |
|---|---|
| Go | `1.26.8`（`go.mod` 的 toolchain 指令会自动获取） |
| Xray-core | `v26.3.27`，API 仅监听回环地址、`api.tag` 非空、`stats` 开启，且 `policy.levels."0".statsUserUplink` / `statsUserDownlink` 为 `true`（见 [`deploy/xray-v26.3.27.example.json`](deploy/xray-v26.3.27.example.json)） |
| 端口池 | 一段连续端口区间，防火墙整段放行，避开 Xray 管理端口与主机其他服务 |
| 存储 | 本地持久目录（`0700`）存放 SQLite；独立保存的 32 字节主密钥文件（`0600`） |

> [!IMPORTANT]
> **不需要**预配置任何 SS2022 入站——面板会为每个用户在运行时创建专属入站。
> 但 `policy` 里的用户级统计必须开启，否则所有用户的用量恒为零、配额形同虚设；
> 面板会在模板校验阶段检出该缺失并拒绝创建用户。

没有官方 Xray 二进制时，可从固定模块自行构建：

```sh
GOBIN=$PWD/bin go install github.com/xtls/xray-core/main@v1.260327.0 && mv bin/main bin/xray
```

## 🚀 快速开始

```sh
# 1. 构建（CGO_ENABLED=0，产出 bin/xpanel）
make build

# 2. 生成主密钥并准备配置
openssl rand -base64 32 > /etc/xpanel/root.key && chmod 0600 /etc/xpanel/root.key
cp deploy/xpanel.example.json /etc/xpanel/xpanel.json    # 按需修改

# 3. 初始化管理员（无回显输入两次密码）
bin/xpanel admin init --config /etc/xpanel/xpanel.json

# 4. 启动
bin/xpanel serve --config /etc/xpanel/xpanel.json
```

生产部署可直接使用 [`deploy/xpanel.service`](deploy/xpanel.service)（systemd，已启用 `ProtectSystem=strict` 等加固项）。

### 首次登录后的四步

1. **登记入站模板** —— 填写公开地址、监听地址（IP 字面量，如 `0.0.0.0`）、端口池起止、加密方式与网络能力。
   模板不含服务端密钥：每条专属入站的密钥由面板独立生成，界面与审计都不会展示。
2. **等待验证为「兼容」** —— 面板会创建一条一次性探针入站、跑一次认证流量、回读计数器，再把探针移除。
3. **创建用户** —— 端口留空即自动分配池内最小空闲端口，也可指定池内具体端口。
4. **交付连接信息** —— 节点确认后复制 `ss://` 链接，其中的端口即该用户的专属端口。

### CLI

| 命令 | 说明 |
|---|---|
| `xpanel serve --config <path>` | 启动面板 |
| `xpanel admin init --config <path>` | 仅在没有管理员时可用；自动化场景用 `--username NAME --password-stdin` |
| `xpanel admin reset-password --config <path>` | 仅限拥有主机访问权限的操作者，不需要旧密码；成功后撤销全部既有管理会话 |

> [!NOTE]
> 面板不提供网页、邮件或短信找回密码的通道。忘记密码只能由主机操作者用 `admin reset-password` 重置。

## 📐 工作原理

```
管理员 ──HTTP──▶ XPanel ──写事务──▶ SQLite（唯一权威状态）
                    │                    │
                    │              持久化的同步意图
                    │                    ▼
                    └──gRPC──▶ Xray HandlerService / StatsService
                                （入站与客户端只是可重建的投影）
```

- **SQLite 是唯一权威状态**：用户、端口分配、专属入站、配额与审计全部落库；
  Xray 中的入站与客户端都是投影，节点重启后按库中记录的端口整体重建。
- **变更先落库再投影**：每次变更写入一条带幂等键的同步意图，由 worker 以租约领取并投影到 Xray；
  RPC 结果不确定时先读后写确认，失败按有界退避重试。
- **命名空间边界**：面板只操作 `xpanel-` 前缀的入站；运维自建的入站全程只读且不计入面板统计。
- **凭证轮换不中断端口**：轮换在既有入站内以四步过渡完成（加过渡客户端 → 删旧 → 加新 → 删过渡），
  任何 RPC 边界上入站的受管客户端数都不为 0。

## ⚠️ 配额语义（请如实告知使用者）

配额是**轮询式控制**：每 5 秒读取一次流量计数，达到配额后请求节点拒绝**新**连接。

- 存在检测延迟，**已建立的连接可能继续**并造成少量超额。
- 面板**不做**逐字节硬限额，也**不做**实时限速。
- Xray 重启会让所有用户端口短暂不可用，面板会按库中的端口分配自动重建，界面在此期间显示「暂时不可用」。
- 端口被面板之外的进程占用时，面板会持续有界退避重试；也可以在用户详情页「更换端口」直接换一个
  （换端口会中断该用户的监听并使旧连接信息失效）。

## 🧪 开发与门禁

```sh
make test           # 单元 + 集成 + 端到端（fake Xray）
make check          # fmt + vet + test + test-race + 固定 Xray 契约套件

# 发布门禁：缺少真实 Xray 二进制视为失败
XRAY_BIN=/usr/local/bin/xray XPANEL_REQUIRE_CONTRACT=1 make check
```

契约套件会用 `xray version` 与 `go version -m $XRAY_BIN` 双重核对运行时 `26.3.27` 与模块 `v1.260327.0`，
并在真实进程上验证入站生命周期、端口隔离、真实 SS2022 流量与计数、重启恢复与故障补偿。

## 📖 文档

| 文档 | 内容 |
|---|---|
| [`docs/operations.md`](docs/operations.md) | 备份与恢复、主密钥、时钟与单实例锁、端口池扩容、迁移的破坏性说明 |
| [`specs/002-per-user-inbound/`](specs/002-per-user-inbound/) | 当前规格与设计：spec、plan、research、data-model、contracts、quickstart |
| [`specs/001-xray-user-management/`](specs/001-xray-user-management/) | 历史规格，其共享入站模型已被 002 完全替换 |

## 📄 许可

[MIT](LICENSE) © lchecho
