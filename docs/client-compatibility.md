# 当前客户端 HTTP 契约与回归矩阵

COMP12-B 用生产路由补齐地址簿会话边界：settings、personal、tag add/rename/color/delete、peer delete 七个端点，分别验证匿名、坏token、实际本地logout后的旧token、禁用用户、数据库撤销和数据库过期会话，共42个命名子测试。全部期望401及固定客户端错误对象；准备失效状态后读取users/user_tokens/tags/collections/rules/peers/address_books七张表的原始列值、时间和行ID，请求后精确比较不变。

每个拒绝用例还用同body、同具名范围和有效owner会话执行正向控制，核对响应、实际标签/peer效果及异owner/异collection同名哨兵，避免以坏输入冒充鉴权通过。证据为 `http/addressbook_session_boundaries_test.go:TestClientAddressBookSessionBoundaries`，没有生产鉴权逻辑、会话政策、IdP行为、新路由或模型变更。这里是HTTP/SQLite和服务端本地退出，不代表实际Flutter/IdP退出联动已验；真实客户端及实机数据细项仍单列。

COMP12-A 补齐 settings/personal 初始化的指名回归，并修正 personal 关闭分支：Personal=1 仍返回认证用户的顶层 guid/name/rule；任何非1配置返回404，让固定1.5.0继续 legacy 初始化。旧200 null会在Dart非nullable Map解析处失败，不能触发该降级。settings在各配置仍返回200对象，max_peer_one_ab为整数0；两个端点均继续受客户端会话校验，空 body 与 {} 可用，body/query不决定身份。

`http/addressbook_initialization_test.go` 验证 settings 字段类型、个人身份/伪造参数隔离和关闭配置的明确404分支；这是生产HTTP/SQLite及固定源码契约证据，未实际运行Dart或Flutter界面。逐端点无效会话矩阵已由COMP12-B补齐，无新模型、路由或数据库写路径。

COMP11 对账号登录、OIDC auth-query、POST currentUser、GET user/info 和 GET users 显式投影本地 `Nickname → display_name`、`Avatar → avatar`。空值仍返回字符串，便于客户端刷新清空缓存；保留 name/email/note/is_admin/status/info 的已有形状，不用邮箱或 IdP claims 覆盖本地资料。native 当前用户复用既有响应 DTO；领域列表 DTO 保持独立，不反向依赖 HTTP 层。没有新增资料模型或持久化 claims。

`http/user_profile_compat_test.go` 覆盖非空/空串、账号登录、两个 current 入口、真实 TLS 测试 IdP 的 native/admin poll 及本地资料变更、用户列表共享/私有范围、状态过滤与分页；严格响应键名单确保内部备注、凭据、归属等字段不扩散。管理端原 nickname/avatar/token/route_names 封套保持既有形状。这里是生产 HTTP/SQLite 及本地 TLS fixture，未运行真实 Flutter 的昵称展示、头像加载与资料清空刷新验收。

COMP10 将匿名 sysinfo 基础写入收敛到 `internal/clientsysinfo` 的显式 DB/Context 事务和独立 DTO：固定1.5.0 在无活动用户时省略 username，因此省略或显式空串均清空旧用户名；cpu/hostname/memory/os/uuid/version 缺失保留、显式空串写入，已识别字段 null 拒绝400。id 必须为非空字符串，未知字段不进入持久化白名单。普通基础报告保留 owner/group/alias/ab/preset/在线记录等管理列，不改变其他调用方的通用 PeerService.Update。

PRE01-A 按已确认管理员CLI准入移除legacy预设helper及owner=1入簿/别名写入。任何非空preset-*在DB前403固定error，已出现null/非字符串400，空串no-op可继续基础ACK；未知未来/大小写/重复键亦受同一边界。新匿名设备owner0，不查历史登录日志；已有owner/group/alias/ab/preset/在线列保留。基础资料写入仍在同一事务、提交后才精确SYSINFO_UPDATED。管理预设转受鉴权CLI，A不新增该路由或模型，也不宣称预设业务已全部支持。

`http/sysinfo_compat_test.go` 验证真实生产路由的无 Bearer 上报、空值/白名单、错误与回滚；`internal/integration/sysinfo_test.go` 验证 MySQL/PostgreSQL 的快照、部分字段并发与真实 COMMIT 回滚/重试。Store 的依赖已固定在实例内，HTTP 边界仍使用现有 service.DB/config 装配，不声称整个 legacy HTTP 已完成多实例解耦。PRE01-A的HTTP与Store回归另见http/sysinfo_admission_test.go及internal/clientsysinfo/admission_test.go；真实客户端用户名切换、带预设失败节流及后续CLI执行尚未验收。基础hostname/username仍可由匿名报告覆盖；管理权限保护不等于资料永久优先。

COMP09 读取切片新增受客户端会话校验的 `POST /api/ab/get`，复用 `GET /api/ab` 的当前用户个人簿读取与失败处理。生产 HTTP/SQLite 回归覆盖双入口响应一致、双层 JSON 字符串、空数组、个人范围、匿名拒绝、数据库故障和取消。本切片未进行真实 Sciter 进程或标准 Flutter GUI 的新增验收；整本写入的后续兼容见下段。

COMP09 写入切片允许 Sciter 初始/重置文档真正省略 `tag_colors`：`tags`/`peers` 仍必须是数组；按提交的 tags 事实重建个人簿，保留仍存在标签的颜色，新标签默认0，删掉的标签不会因旧颜色复活。显式 `"{}"`/部分颜色对象继续保留缺少键的既有颜色；显式0覆盖为0。字段 null/空串/非字符串、字符串内无效对象及颜色成员 null/非整数均拒绝400。没有新增字段或迁移。

`http/sciter_write_compat_test.go` 覆盖初始化/重置、元数据与扩展字段保留、无效输入不写、旧颜色查询失败、第二个标签写失败及明确到达 COMMIT 的回滚；`internal/integration/sciter_addressbook_test.go` 在 MySQL8.0.46/PostgreSQL18.4 验证精确标签名/颜色、个人范围和真实 COMMIT 失败后的完整回滚及重试。这里的 HTTP fixture 与实库测试不代表真实 Sciter 交互验收。

COMP07 已在 COMP08 的统一范围上补齐标签 rename/delete 与 `address_books.tags` 引用的同事务更新：覆盖同 owner/collection 的全部物理行，保留无关标签顺序/重复、其他列和时间戳；只有参与改名的行才合并新名称碰撞。坏 JSON、查询/写入/原始值 CAS 冲突、取消和 COMMIT 失败均整体回滚，不返回空成功。

客户端标签名按精确字符串处理，MySQL 的默认大小写/重音/尾空格折叠不会扩大匹配范围；后续写入按已识别 tag 主键进行。八个客户端写动作（标签四项、peer 增改删、legacy 整本替换）在首次快照读取前按 user ID 升序协调 actor/owner 锁，保持现有 user→session 锁序。管理端并非全部参与此锁规则；SQLite 使用现有事务写锁，冲突失败回滚。CAS 和本次事务不阻止以后一个显式新请求重新提交旧标签。

新增证据：`http/tag_references_compat_test.go` 的 1007 条引用、个人/共享、重复物理 ID、扩展字段/时间戳保护及失败闭合；`internal/clientcompat/tag_concurrency_test.go` 的 SQLite 独立连接池；`internal/integration/tag_references_test.go` 的 MySQL/PostgreSQL 不同标签并发保全、同目标冲突、精确名称和真实 COMMIT 失败/重试。COMMIT 注入测试显式验证到达故障点，避免以提前失败冒充提交回滚。以上定向用例本地通过，最终全仓及同 SHA 验证见相应提交 Actions；本切片未进行真实 GUI 或部署验收。

COMP08 后续切片已统一 tag add/rename/color/delete 与 peer delete 的共享写权限：复用 `current/scope` 的有效 MAX 规则、owner/collection 范围及请求 Context；批量删除同事务，缺项/写入/提交失败整体回滚，重复输入去重，历史重复物理行按精确资源范围全部删除。输入只接客户端字段，查询故障不再继续创建或泄露数据库错误。

COMP08 切片已通过 Go1.23.10/CGO1 的全仓 race/vet/build、生产 router+SQLite 回归，以及独立 MySQL8.0.46/PostgreSQL18.4 的 MAX/错误 owner/批量回滚和真实 COMMIT 失败测试；确切提交 CI 见 Actions，不能从本地结果推定。没有进行这一切片的真实客户端操作。新增证据为 `http/shared_write_permissions_test.go`、`internal/clientcompat/writes_test.go` 和 `internal/integration/shared_write_test.go`。当时未含的标签引用/精确名称/写入协调由上述 COMP07 补齐；下表及 1.5.0 文档中的 b59f7a5 静态 GAP 仍保留为修复前对照。

本矩阵固定官方 RustDesk **1.5.0** 客户端 `fada664df7a294d1d1a9ca3e7cd3637069122f17`，API静态盘点固定 `b59f7a56cd9e90cf48c760d2aca808a7241582f0`（b59f7a5）。这是 **36项（含保留实现）的回归清单，不是36/36通过声明**。旧01–35稳定，36补Sciter `POST /api/ab/get`。机器元数据见 [client-contracts.json](../http/testdata/client-contracts.json)；字段、同路径变体、条件与固定源码链接见 [1.5.0 HTTP契约](client-api-1.5.0.md)。

`route_present` 只表示生产路由存在；`covered_partial` 表示有下列指名测试但未覆盖该项全部业务；`deferred_missing` 表示客户端调用存在但API缺对应路由；`conditional_unreachable` 表示源码存在但当前调用条件不能同时成立。后续执行者必须补记真实测试名与数据库断言，不能用路由黄金文件代替协议与持久化测试。

| 编号 | 回归范围 | HTTP 与数据库必须共同验证 | 盘点状态 |
| --- | --- | --- | --- |
| 01 | GET login-options；HEAD 连通性探测另列 | 顶层字符串数组；OIDC provider 名称；HEAD 不带响应体，不与能力支持计数混用 | 路由存在，待业务回归 |
| 02 | POST login | account 登录、deviceInfo、顶层 token/type/user.info；禁用/坏密码；失败不写会话或登录日志 | COMP11 已测资料投影；SEC08-A 验证26个未支持阶段/混入验证字段请求400且零副写、2个正常分支令牌回读；依最新决定不建设本地邮箱/TOTP挑战；普通登录真实回归待验 |
| 03–04 | currentUser/logout | 顶层用户、401 清账号；撤销 DB token；退出不依赖客户端收到 ACK | 已有部分真实路由+SQLite 回归 |
| 05–06 | OIDC auth/auth-query | code/url、固定 pending 原文、native envelope；路由/设备隔离；真实 TLS IdP→身份→本地 session 事务 | 已有部分真实路由+SQLite+TLS 回归 |
| 07 | heartbeat | 匿名设备上报形状；相同版本 `{}`；修改、删除、默认切换后的 config_options；UUID 边界；失败不当成功 | COMP05 已实现，核心与生产 HTTP+SQLite 定向 race 通过；未运行真实 Rust 客户端 |
| 08 | sysinfo | 必须原文 `SYSINFO_UPDATED`；新增/更新回读；上报不能改管理员绑定与共享授权 | COMP10 基础快照/空值与事务已补 HTTP/实库回归；预设业务和真实客户端验收仍待完成 |
| 09 | sysinfo_ver | 原文版本文本；直接 HTTP smoke 可测，但不可当作固定客户端正常可达闭环 | 条件不可达 |
| 10–12 | switch-grant / devices/deploy / devices/cli | 保留明确不支持状态；后续各自验证签名、操作 token、result 枚举或空文本 | 缺失，后续阶段 |
| 13–14 | settings/personal | 空 body 可用；settings 对象、personal guid；不存在接口时 404 降级 | COMP12-A 已补初始化具名回归；personal关闭改404降级，COMP12-B 已补会话矩阵，真实客户端另列 |
| 15 | shared profiles | total/data、current/pageSize；授权规则取最大且去重；拒绝伪造或越权 guid | 生产路由+SQLite 已覆盖 1021 个 profile 和权限过滤 |
| 16–17 | legacy 整本地址簿 GET/POST | data 是 JSON 字符串，tag_colors 再编码；个人簿整本更新与 tag 同事务；保留扩展字段和共享/他人数据 | 生产路由+SQLite 已覆盖 1007 peers/1003 tags 读取及写失败回滚 |
| 18–19 | peers / tags | 空 body；peers current/pageSize/ab；tags 顶层数组；权限过滤；超过 1000 条不截断 | COMP02 生产路由+SQLite 已覆盖分页、旧 page 别名、非法参数及 wire 类型 |
| 20–21 | peer add/update | 单对象新增；字段缺失不改、空串清除；note/主机/密码/hash；forceAlwaysRelay 字符串与 same_server 布尔 | COMP03 生产路由+SQLite 已覆盖部分更新、note 清空/回读及跨用户主机元数据保护 |
| 22 | peer delete | DELETE JSON 数组 body；资源范围和持久化删除 | COMP08 已补生产 HTTP/SQLite 的范围、权限、重复/缺项、批量失败回滚及实库验证 |
| 23–26 | tag add/rename/update/delete | 颜色整数；重命名同步 peer 引用；DELETE 数组 body；越权拒绝 | COMP08 已测四个写入口的共享规则/失败；COMP07 已测改名删除引用事务、精确名称与实库并发 |
| 27–29 | accessible groups/users/peers | current/pageSize、真实 total、授权后分页；Peer.info.device_name 映射；授权依赖故障不得扩权 | COMP02 生产路由+SQLite 已覆盖千条级分页、角色/组范围和依赖失败 |
| 30–31 | conn/file audit | 事务提交/已提交重复后才 2xx 空 body；完整 u64 session_id；receipt 并发去重与冲突；连接 new/auth/close | COMP04 核心+真实 controller HTTP/SQLite 已覆盖；完整生产 router 另验证装配、u64、空 ACK、重复和依赖失败 |
| 32–35 | alarm / active conn / audit note / record | 空ACK、GUID JSON string、授权备注、字节上传分别验证；34只判HTTP200且需非空GUID；35私有ENABLE=false无writer | 缺路由，35为静态不可达保留实现 |
| 36 | legacy Sciter POST /api/ab/get | 请求{}；data为JSON字符串、updated_at可选；不能以Flutter GET /api/ab代替 | COMP09 读取切片已补生产路由与 HTTP/SQLite 回归；真实 Sciter 验收待完成 |

固定 b59f7a5 盘点为28项注册、8项缺路由；COMP09 读取切片后为29项注册、7项缺路由。36个固定method/path中，34项有源码调用链，09/35为条件不可达保留实现。HEAD `/api/login-options`另列传输探测，不属于36项业务功能。Sciter入口不能等价成标准Flutter GUI必然触发。现有 `http/http_test.go` 的路由 inventory 证明注册形状；`TestProductionOIDCRoutesPublicPKCEAndLocalSession`、`TestProductionOIDCAdminEnvelopeAndRouteAudience` 等证明部分真实登录/当前账号/退出边界。本轮地址簿、分页、策略与审计已有明确测试证据，但不能由路由计数推导全部契约或外部运行验收通过。

## COMP02/03 地址簿、分页与字段

`internal/clientcompat` 使用显式 DB/context，controller 保留客户端响应映射。`http/addressbook_compat_test.go` 经真实 `NewHandler` 生产路由、会话与隔离 SQLite 验证：

- `TestClientCompatAddressBookPaginationAndWireTypes`：1007 个 peer（含重复业务 ID）按稳定 row_id 分页；legacy GET 完整读取；1003 个 tag 顶层数组；forceAlwaysRelay 为字符串、same_server 为布尔；非法/重复分页参数拒绝。
- `TestClientCompatProfilesPaginationMaximumRuleAndScope`：1021 个 profile，个人/组共享授权去重并取最大有效 rule，拒绝只读写入和伪造 guid。
- `TestClientCompatGroupListsPaginateAfterAuthorization`：千条级 users/peers/device groups，在授权与状态过滤后分页，真实 total、主机/分组映射和管理员边界。
- `TestClientCompatPatchPreservesOmittedFieldsAndClearsExplicitValues`、`TestClientCompatAddUsesWireFieldsAndCannotReadForeignHostMetadata`：缺失字段不改，空串/空数组明确清除；note 多行与空值回读；忽略输入所有权，不能通过猜 ID 获取他人主机信息；写入失败回滚并返回安全错误。
- `TestClientCompatLegacyReplaceIsAtomicAndPersonalOnly`：data 二次 JSON 编码、peer/tag 同事务，失败无部分提交；不改共享/他人记录；保留旧客户端省略的扩展字段，显式空 note 清除，个人簿空快照只清个人数据。
- `TestClientCompatQueryFailuresAndCanceledRequestsFailClosed`、`TestClientCompatAuthorizationDependencyFailureDoesNotExpandScope`：读取、授权依赖和请求取消不能变成空成功或全量可见。

COMP02/03 当时未覆盖的 peer DELETE 与 tag 四个变更入口，已由后续 COMP08 的 TestClientCompatSharedWritesUseMaximumScopedRule、TestClientCompatBatchDeletesRollbackMissingAndWriteFailures 等生产 HTTP/SQLite 与两引擎测试补齐；COMP07 另补标签引用事务。settings/personal 初始化已由COMP12-A补齐指名回归；本批七入口会话负向已由COMP12-B补齐，其他入口和真实客户端细项继续归COMP12，不把既有自动测试重新列为未实现。

## COMP04 审计投递与关联

`internal/clientaudit` 将 receipt PK（kind/device/uuid/nonce 的 SHA-256）及请求语义摘要与业务写入放在同一事务。失败后在已回滚/结束的事务外查询已提交 receipt：相同摘要是重复成功，不同摘要固定 409；业务审计行删除后 receipt 仍保留，重试不复活业务行。无 nonce 明确仅尽力写入，不虚构幂等键。匿名设备字段和 receipt 不是密码学设备身份证明。

`internal/clientaudit/http_test.go` 通过真实审计 controller 与 SQLite 验证完整 u64（含 18446744073709551615）、200 空 body ACK、无效请求 400、nonce 冲突 409、缺连接/存储错误 503、deadline 504、写失败和真实 COMMIT 失败后的相同 nonce 重试。文件摘要基于适配前 Info/is_file，保留大整数，并对 JSON 对象键顺序/空白做语义归一。旧 note-only POST 形状明确拒绝 400，不能当作受控连接事件或假 ACK；新版备注路由仍在缺失项中。

核心测试 `TestConcurrentRetriesStoreOneOpenAndFilePerWriter`、`TestCrossStoreSQLiteHandlesCommitOneReceiptAndEvent`、`TestConcurrentDifferentPayloadsShareOneReceiptKey` 验证并发/独立连接实例下 receipt 与业务记录唯一、不同内容冲突；`TestReceiptsSurviveBusinessChangesAndDeletionWithoutRecreatingRows` 验证历史重试。`TestCrashReconnectAssociatesAuthAndCloseWithLatestOpen`、`TestCloseAfterCrashBeforeAuthorizationUsesLatestZeroOpen` 补齐崩溃未 close 后的重连：在同 device/uuid/conn_id 下，对 live exact session 与 live session0 一起按最新 id 取候选，其他非零 session 不可候选；已提交旧投递先由 receipt 去重。

`http/audit_compat_test.go:TestProductionAuditRoutesUseAssembledTransactionalWriter` 另经完整 `NewHandler` 验证 writer 装配、两个生产路由、u64 持久化、重复空 ACK、冲突及依赖失败。详细失败矩阵仍在直接挂 controller 的 HTTP fixture 中验证，不把此集成样本推导为全链路验收。

协议没有 open 代次或事件发生时间。升级前记录或没有 receipt 的未见旧投递仍可能与新代混淆；已接受的最新候选规则不能宣称 exactly-once 业务时序。代理与真实客户端链路仍待联调。

## COMP05 策略清理的已证实边界

客户端 `src/hbbs_http/sync.rs:243–271` 在心跳中提交本地 `strategy_timestamp`，收到整数 `modified_at` 后只比较不相等，不要求变大。`292–308` 从现有配置开始逐键修改：省略键保留旧值；必须显式传 `"key":""` 才会清除（当 DEFAULT_SETTINGS 同键非空时保留一个空值以压过定制默认）。这不保存或恢复策略前个人配置。

旧实现只对四个 preset 键显式写空，其余策略删除丢失历史键；设备/默认/预设取最大时间戳也可遮住变更。本轮心跳已改由 `internal/clientstrategy.Store.Heartbeat` 生成完整有效 payload，原 `service/peerStrategy.go` 的 CRUD 保留，旧 `GetEffectiveModifiedAt` 不再用于心跳版本。

用户已确认最终清理语义：只追踪该设备由服务端准备下发并提交历史的键，当前有效策略仍有该键就下发当前值；否则显式空串；从未管理的本地键不触碰。删除设备策略时同键若仍在默认策略中，则恢复默认策略的值。客户端定制 OVERWRITE 仍高于普通配置，不能被普通策略覆盖。空串可能压过非空定制 DEFAULT、不恢复策略前个人值，均为已接受边界；不再把精确恢复个人值/定制默认列作待实现客户端任务。

已批准模型 `PeerStrategyState` 对应 `peer_strategy_states`：一个已登记设备一行，`peer_row_id`（PK+FK→peers.row_id，删除设备 CASCADE），`managed_keys`（TEXT 保存规范化去重 JSON 字符串数组），仅两个业务列，不保存值、密码、时间或操作人。H∪C 在成功返回前事务提交，其中 C 只包含当前策略或真实关联实际赋值的键。四个 preset 不会在无关联、无策略赋值时自动置空并纳入历史；真实 Alias/AbId 关联按已有业务覆盖策略 preset，撤关联后回到有效策略 preset，否则仅历史键补空。历史状态不是客户端已应用 ACK。旧版已物理删除且没有历史的键无法追溯。历史键持续受管，未来其它策略变化仍可能重清这些键；此语义已经确认，不设计清完后释放或额外 ACK 状态。

`modified_at` 已改为最终规范化 `config_options` 的 SHA-256 前 63 位正整数（零映射为 1），与锁定客户端的整数不等比较兼容；不保存 revision 列、不挤占 `PresetModifiedAt`。它避免原时间戳遮挡和同毫秒问题，有限摘要仍有理论碰撞。事务使用显式请求 context 和 Serializable；非 SQLite 以现存父 peer 行锁序列化首次历史创建，SQLite 使用现有连接事务机制。DB/配置或历史 JSON 错误固定安全 503，取消/超时分别 408/504；未知设备/UUID 不符保持 `{}`。历史提交失败不发布配置，BufferHeartbeat 失败也返回固定 503。

管理员策略接口是全量设置：默认策略请求 `{}`、设备策略省略 config_options（仍需合法 id/peer_id），或显式 `config_options:null`，都按清空整份策略处理，统一存成 `{}`，不是 PATCH。`TestProductionHeartbeatNilStrategyOptionsClearAsJSONObject` 用生产路由和 SQLite 验证全局默认、未绑定默认及设备策略的省略/null/空对象设置，随后心跳明确清除历史键。历史库已经存在的 `null`、`[]`、损坏 JSON 或 AbId 指向缺失集合不会被读取端自动当空成功，需管理员明确重存有效对象或修正关联；本修复不推断迁移这些旧数据。

1.5.0 的 `hbb_common` 固定为 `229b904508364c8997aad0fb5af57effac859f60`，见 [config.rs](https://github.com/rustdesk/hbb_common/blob/229b904508364c8997aad0fb5af57effac859f60/src/config.rs)。本节实现/算法测试属于既有策略切片证据，下述历史分析行号不作为本轮1.5.0逐字段核验；精确触发与响应见 [1.5.0契约07](client-api-1.5.0.md#contract-07)。旧策略文档的HARD回退说明不能直接视作当前源码事实。

可明确验证的空值行为包括：经过 `option2bool` 读取的 `enable-*` 空串为 true、`allow-*` 空串为 false（客户端 `flutter/lib/common.dart:1616–1629`，Rust 锁定 config.rs:2649–2661 同规则）；`access-mode` 清空后退出 full/view 强制模式，回到逐项权限判断（`src/server/connection.rs:2676–2690`）；`whitelist` 清空后没有 IP 限制项（`:1418–1435`）；四个 preset 值可置空。上述均不是永久密码恢复、用户原值恢复或既有地址簿成员关系撤销。网络地址/API/key 等配置清空可能切断后续心跳，不应从“支持写空”推出可无条件回滚整个设备。

### 策略真实 HTTP+DB 验证清单

1. 管理员创建默认/设备策略→匿名心跳→回读 DB；设备同键优先，未配置键继承；显式空串覆盖默认。
2. 客户端回传相同 revision 得 `{}`；默认修改被较大设备/预设旧时间戳覆盖、同毫秒修改、集合改名仍产生不同有效版本。
3. 删除设备策略→同键回默认；无默认同键→发空串；默认删键与默认整清空同样发送旧键空串。
4. 已绑定/未绑定默认切换时清前一默认独有键；从未管理的本地键不在 payload 中。
5. 发出策略响应后模拟丢包、API 重建/重启、再次心跳，清理键不丢；DB 写失败不提前 ACK；取消请求不留下不完整状态。
6. 空串客户端语义分别验证 DEFAULT_SETTINGS 为空、非空及 OVERWRITE 存在三种情形；可用精确算法的协议模拟作服务器测试，真实 Flutter 配置文件验证仍单独记录。
7. JSON 历史不合法、配置损坏、未知设备、UUID 不符、非管理员 CRUD、保留默认 peer_id、并发修改/心跳均应有固定结果；不可把匿名上报变为管理员绑定写入口。

`TestUnassignedDeviceDoesNotClaimLocalPresetKeys`、`TestPresetAssignmentOverridesPolicyAndRemovalPreservesValidFallback` 专门验证 preset 历史修复。其余核心测试覆盖默认/设备叠加、时间戳遮挡、scope 切换、集合改名、损坏 JSON、取消/超时、并发首次心跳、真实 FK 删除级联及 deferred FK 在 COMMIT 失败后的重试；HTTP 测试覆盖真实管理员 CRUD→心跳、空版本响应、读写失败、worker 停止、UUID 和 context 边界。客户端行为仅运行锁定源码等价算法样本，分别断言空默认/非空 DEFAULT/OVERWRITE 和未管理个人键保留。

## 本轮验证与剩余运行验收

已通过的本地定向 race 包括 `./internal/clientstrategy`、`./internal/clientaudit`、`./http -run ProductionHeartbeat` 和 `./http -run TestClientCompat`（均带 `-mod=readonly -race -count=1`）；相应核心/HTTP 包 vet 已通过。元数据指名测试与本段命令是局部证据，不是36项完整通过或发布验收。

版本 276/277/278 已分别集成升级、重复启动、版本写入失败回滚/重试的真实 SQLite 测试；覆盖 note 补空/非空约束、收据不推断历史、策略历史不回填及显式 peer FK/删除级联。后续真实 MySQL/PostgreSQL 迁移、事务失败及并发已在 `internal/integration` 与 CI 门禁补齐，见 [database-integration.md](database-integration.md)；这些不等于生产备份恢复演练。CI 与制品按对应提交记录，不能由本矩阵推导整阶段验收。

2026-10-19 另行运行官方 Flutter RustDesk 1.5.0 amd64 release（非本地改编客户端）于 Ubuntu 26.04.1 GNOME。API 部署版本为 `75a51e5`，官方 hbbs/hbbr 为 1.1.16。真实外部 UDP 双向、公钥设备登记、客户端 Ready 与 key_confirmed 已核对；用户在 Ubuntu 客户端完成 OIDC，Account 显示 Logout，本地 token/user_info 非空且服务器存在匹配普通用户/身份/有效 native 会话/设备记录。配置使用独立 HOME/XDG，但与桌面共享 UID、显示和 DBus；原版 deb 解包运行，没有安装 root RustDesk 服务。

用户另在该官方Ubuntu客户端手工确认 **My address book基本可用**。这不包括逐项note缺失保留/空清除、千条分页、策略实际应用或真实远控审计；没有因此执行logout，也未确认双端直连/中继或Wayland被控权限。UI基本可用不能推导36项/所有同路径变体通过；八个缺失路由仍待实现，其中record保持静态不可达条件。

## COMP01 1.5.0同步边界

本切片仅同步文档/JSON，没有重跑Go、实库、GUI或CI；保留指名测试作为历史证据。检查覆盖36项唯一method/path、01–35编号稳定、Sciter补漏、固定源码与本地测试/文档引用。

独立复核修正：legacy `POST /api/audit/conn` 无Bearer，本地token仅入口门禁，忽略业务body；Flutter `PUT /api/audit` 只判HTTP200且需已有GUID，与legacy备注入口独立，并非fallback。个人密码保存 `{id,hash}` 与共享 `{id,password}` 互斥；record私有false开关无writer。完整字段Schema和全部错误路径尚未冻结。
