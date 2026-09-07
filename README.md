# XPanel

面向少量共享用户的 Xray 管理面板：为每个用户创建一条专属的 Shadowsocks 2022 入站并分配独立端口，
支持创建、修改、禁用、轮换和删除用户，按月度周期统计并限制流量，配额重置日自动恢复访问，
并提供服务端渲染的安全管理界面。每个用户拥有各自的端口与入站，彼此不受影响。

规格与设计见 `specs/002-per-user-inbound/`（spec、plan、research、data-model、contracts、quickstart）。
`specs/001-xray-user-management/` 保留为历史规格，其共享入站模型已被 002 完全替换。

## 前提

- Go `1.26.8`（`go.mod` 的 toolchain 指令会自动获取）。
- Xray `v26.3.27`，API 只监听回环地址且 `api.tag` 非空，`stats` 开启且
  `policy.levels."0".statsUserUplink/statsUserDownlink` 为 `true`（见 `deploy/xray-v26.3.27.example.json`）。
  **不需要**预配置任何 SS2022 入站，面板会在运行时为每个用户创建专属入站。
  没有官方二进制时可从固定模块构建：`GOBIN=$PWD/bin go install github.com/xtls/xray-core/main@v1.260327.0 && mv bin/main bin/xray`。
- 一段可供面板分配的端口区间（端口池），需在防火墙上整段放行，且避开 Xray 管理端口与主机上的其他服务。
- 本地持久磁盘目录（`0700`）存放 SQLite；独立保存的 32 字节主密钥文件（`0600`）。

## 构建与门禁

```sh
make build          # CGO_ENABLED=0，产出 bin/xpanel
make test           # 单元 + 集成 + 端到端（fake Xray）
make check          # fmt + vet + test + test-race + 固定 Xray 契约套件（需 XRAY_BIN）
XRAY_BIN=/usr/local/bin/xray make contract
XRAY_BIN=/usr/local/bin/xray XPANEL_REQUIRE_CONTRACT=1 make check   # 发布门禁：缺少二进制视为失败
```

契约套件会用 `xray version` 与 `go version -m $XRAY_BIN` 双重核对运行时 `26.3.27` 与模块 `v1.260327.0`。

## 运行

```sh
openssl rand -base64 32 > /etc/xpanel/root.key && chmod 0600 /etc/xpanel/root.key
cp deploy/xpanel.example.json /etc/xpanel/xpanel.json    # 按需修改
bin/xpanel admin init --config /etc/xpanel/xpanel.json    # 无回显输入两次密码
bin/xpanel serve --config /etc/xpanel/xpanel.json
```

- `admin init`：仅在没有管理员时可用；自动化场景用 `--username NAME --password-stdin`。
- `admin reset-password`：仅限拥有主机访问权限的操作者，不需要旧密码；成功后撤销全部既有管理会话。
  不提供网页、邮件或短信找回。
- 首次登录后的最小上手路径：
  1. **登记入站模板**：填写公开地址、监听地址（IP 字面量，如 `0.0.0.0`）、端口池起止、加密方式与网络能力。
     模板不含服务端密钥——每条专属入站的密钥由面板独立生成，界面与审计都不会展示密钥。
  2. **等待验证为「兼容」**：面板会在节点上创建一条一次性探针入站再移除，以此证明该节点支持运行时
     入站管理与 SS2022 多用户身份。
  3. **创建用户**：端口留空表示由面板取端口池内最小空闲端口，也可以指定池内的具体端口。
     创建后面板会为该用户创建专属入站。
  4. **交付连接信息**：节点确认后复制 `ss://` 链接，其中的端口即该用户的专属端口。
- 端口池容量即用户数上限。仪表盘的「端口池」区块显示容量、已分配与剩余可分配；扩容流程见
  `docs/operations.md` §6.1。
- Xray 重启会让所有用户端口短暂不可用，面板会按数据库中保存的端口分配自动重建，界面在此期间显示
  「暂时不可用」。
- 端口被面板之外的进程占用时，面板会持续有界退避重试；也可以在用户详情页「更换端口」直接换一个，
  换端口会中断该用户的监听并使旧连接信息失效。

## 配额语义（请如实告知使用者）

配额是**轮询式控制**：每 5 秒读取一次流量计数，达到配额后请求节点拒绝**新**连接。
存在检测延迟，已建立的连接可能继续并造成少量超额；面板不做逐字节硬限额，也不做实时限速。

## 运维

备份、恢复、主密钥、时钟与单实例锁的说明见 `docs/operations.md`。
