# XPanel

面向少量共享用户的 Xray 管理面板：在运维预配置好的 Shadowsocks 2022 多用户入站上创建、修改、禁用、轮换和删除用户，
按月度周期统计并限制流量，配额重置日自动恢复访问，并提供服务端渲染的安全管理界面。

规格与设计见 `specs/001-xray-user-management/`（spec、plan、data-model、contracts、quickstart、validation-report）。

## 前提

- Go `1.26.8`（`go.mod` 的 toolchain 指令会自动获取）。
- Xray `v26.3.27`，API 只监听回环地址且 `api.tag` 非空，并已配置至少一个保留初始用户的 SS2022 多用户入站（见 `deploy/xray-v26.3.27.example.json`）。
  没有官方二进制时可从固定模块构建：`GOBIN=$PWD/bin go install github.com/xtls/xray-core/main@v1.260327.0 && mv bin/main bin/xray`。
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
- 首次登录后：登记访问配置 → 等待验证为“兼容” → 创建用户 → 节点确认后复制 `ss://` 连接信息。

## 配额语义（请如实告知使用者）

配额是**轮询式控制**：每 5 秒读取一次流量计数，达到配额后请求节点拒绝**新**连接。
存在检测延迟，已建立的连接可能继续并造成少量超额；面板不做逐字节硬限额，也不做实时限速。

## 运维

备份、恢复、主密钥、时钟与单实例锁的说明见 `docs/operations.md`。
