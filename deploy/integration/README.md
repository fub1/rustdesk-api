# 共享 ECS 联调部署

这些脚本针对已授权的 Ubuntu 24.04 x86_64 联调主机与固定域名 `rustdesk.viget.net`，不是通用生产安装器。依赖既有 PostgreSQL、Nginx、Certbot/systemd；不安装 Docker，不改已有 VMS/IdP 数据库、账号或配置。源码不包含凭据、服务器私钥、客户端配置、会话、授权 URL 或二进制。

## 部署顺序

1. 以 root 运行 `python3 prepare-host.py`，建立专用 `rustdesk-integration` 用户及目录、`rustdesk_integration` PostgreSQL 角色/数据库、HTTP ACME vhost。生成凭据只写 `/etc/rustdesk-integration/credentials.json`（0600）；API 配置为该目录的 `config.yaml`（0640）。已有不匹配资源必须先人工核对，禁止为使脚本成功而删除/覆盖其他项目资源。
2. 准备官方 [rustdesk-server 1.1.16](https://github.com/rustdesk/rustdesk-server/releases/tag/1.1.16) Linux amd64 资产。下载 zip SHA256 必须为 `0565c41affe6c3f409b0bddfaf5a24ccbf3f64f5f8e3fec250b69a0d5f6bdbcf`。解出 hbbs/hbbr，放到 `server/bin/`，该目录不入库。`server/install-server.sh` 再校验两个二进制摘要，只安装，不自动启动。
3. 按顺序启用 `rustdesk-integration-hbbs.service`、`rustdesk-integration-hbbr.service`。hbbs 首次生成密钥，hbbr 的预检等待完整匹配密钥对；已有部分/损坏密钥拒绝启动。数据与私钥仅在 `/var/lib/rustdesk-integration/server`，禁止复制/输出私钥。向客户端分发 `.pub` 内容。
4. 在安全组及主机防火墙放行原生客户端 TCP 21115–21117、UDP 21116；API 21114 仅监听 loopback。既有 80/443 用于 HTTPS，Web 客户端 21118/21119 暂不放行。脚本不自动更改防火墙，不以本机监听、TCP connect 或代理 fake-IP 作为 UDP 公网验收。
5. 从成功 Actions 下载同 SHA 的 Linux API 制品，验证外层 SHA256SUMS、内部 REVISION/GO_VERSION。用同一 Git SHA 的 `resources/`、LICENSE 及本目录 API unit 组装 bundle，根目录必须含 `rustdesk-api`、`REVISION`、`GO_VERSION`、`resources/`、`deployment/rustdesk-integration-api.service`、LICENSE。计算 bundle SHA256 后上传，运行 `python3 install-api.py bundle.tar.gz <40位提交SHA> <64位bundleSHA256>`。资源未在原 Actions 二进制归档中，禁止混用其他提交资源。
6. `python3 enable-https.py` 使用唯一现有 ACME 账号签发证书，不替用户接受新 ToS。已有有效证书仅更新代理时用 `--existing-certificate`，避免重复 ACME 网络访问。先校验配置再 reload；失败还原旧 vhost。当前共享主机已有 443 protocol-options 告警，保留其他 vhost 不擅改。
7. `python3 configure-provider.py` 仅调用新 API 的认证管理入口，轮换本项目初始化管理员密码并配置测试 PUBLIC/none/S256 provider。可从私有 pending 记录恢复改密前/后中断；仅明确密码错误才尝试旧密码，网络/5xx 不触发降级。`admin.json`（0600）及应用 bootstrap 日志不可复制到仓库或输出到共享终端。此脚本不会在 IdP 创建/重置业务用户。

集成环境 provider `creatury` 使用 `https://vms.creatury.cn/idp`、`openid profile`、已登记的精确回调 `https://rustdesk.viget.net/api/oidc/callback`，不发送 secret/Basic。`auto_register=true` 只允许已通过 IdP 的普通本地账号准入；账号权限仍由现有业务及 IdP ACTIVE 规则决定。这里的 PUBLIC client_id 是公开注册标识，不是密码。

## 失败恢复边界

API 安装先检查 unit 所有权、语法与 bundle 摘要，再切换 root-owned `current`。切换前保存 `deployment-state.json` 和 `previous-unit-<SHA>.service`；重试同 SHA 保留原 previous_release。失败返回非零，不把存活旧进程当作新部署成功。切换后的启动、迁移或 readiness 失败需要人工恢复：先停止本项目 API、查看私有日志和迁移版本，确认新旧二进制与 schema 兼容后，才恢复记录中的旧 unit/link 或从数据库备份恢复。**不会自动把旧二进制启动到可能已迁移的新 schema。** 首次安装无旧版本时保留失败现场，不声称存在回滚版本。

主机准备、安装及配置更新须由单一维护者串行执行；不承诺并行部署、数据库自动降级、跨进程挑战恢复或多副本。失败残留 `.staging-*` 只能核对完整路径后定点清理。共享 PostgreSQL、VMS、IdP、Nginx 主服务不作为本项目停止/回滚对象。

三个服务当前共用一个低权限 Unix 用户；systemd 可写目录限制防止常规越界写入，但这不是 API 与 hbbs/hbbr 的强安全隔离。同 UID 的文件读取边界仍存在，正式生产若要求隔离应另行设计用户/密钥分发。

HTTP/HTTPS access_log 只记录 `$uri`，不记录查询串。Nginx 原生 error_log 会追加完整 request_line，无法用 access_log 格式脱敏，所以仅授权码 callback 的四个别名 `/api/oidc/callback`、`/api/oidc/login`、`/api/oauth/callback`、`/api/oauth/login`（区分大小写、首尾锚定正则 `^/api/(?:oidc|oauth)/(?:callback|login)$`）的错误输出和 HTTP 重定向错误输出丢弃；这些路径的状态与错误类别通过脱敏 access/API 日志诊断。其他 HTTPS 路径保留专用 error_log。此取舍须与日志运维同步。

## 本次实际验证与待验收

2026-10-19 已部署 API `75a51e5ae3ecdca99800ba63649ea3eb6ab17301`（[CI](https://github.com/fub1/rustdesk-api/actions/runs/39205951533) 成功）及官方 server 1.1.16，三服务 enabled/active，运行文件摘要匹配；专用 PostgreSQL 迁移至 278。Windows 与 WSL HTTPS 验证通过；根 `/api/` 首次响应 302，跟随后 200。VMS 根页、IdP Discovery 仍 200，既有服务保持运行。

已提取并运行官方 [RustDesk 1.5.0](https://github.com/rustdesk/rustdesk/releases/tag/1.5.0) `rustdesk-1.5.0-x86_64.deb`，SHA256 `4e2b9701d25a7ef373f3d09bda584a37d6902f97a6ab15f2e86d4a5783e2a0ec`。隔离 WSL 账号、配置、Xvfb/DBus；未改 Windows 1.4.6。真实 UI 已显示版本、成功匿名 sysinfo 上报、取得 provider 并触发 OIDC auth/poll，登录 URL 含 S256/state/nonce，未用模拟 HTTP 冒充 UI 验收。

后续用户提供独立 Ubuntu 26.04.1 GNOME 测试机，使用同一官方 1.5.0 原包，在新 HOME/XDG 中运行（共享桌面 UID/显示/DBus，不声称 UID 隔离；未安装 root RustDesk 服务）。新机器的直连 TCP21115–21117、有效 TLS、真实外网 UDP21116 双向及 hbbs 新公钥设备登记已通过，客户端显示 Ready、公钥确认。用户在 Ubuntu RustDesk 中手动完成 OIDC，Account 显示 Logout；私有本地 token/user_info 非空，PG 新增普通用户、身份关联、有效 native session 和登录记录，session 与 peer 匹配。没有要求 IdP 改协议。

WSL 原 fake-IP/宿主路径失败保留为历史观察，当前新机已通，不能继续把“云安全组待放行”当作确定阻断。**尚未完成**：实际地址簿/分页/备注、策略/审计 UI，双端直连/中继、Wayland 被控权限及 root 服务流程。不可把注册/登录扩大为这些验收，更不宣布第一阶段全部完成。

离线故障恢复测试：`sudo python3 -B -m unittest discover -s deploy/integration -p 'test_*.py' -v`。测试仅写临时目录，不调用真实服务、不读真实凭据；CI 同时保留真实 MySQL/PostgreSQL、Redis、全仓 race/vet/build。备份恢复演练、整机故障与生产规模负载不在当前结果中。
