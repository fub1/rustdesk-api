# 当前客户端 HTTP 契约与回归矩阵

本矩阵固定 RustDesk 客户端提交 `53996e3c404ea5083df0775057e4149e49102084`，API 盘点基于 `91c5b74786c79b622926b9f4c17d2a3897540a43` 及 ARCH07 工作树。它是回归任务清单，不是 35/35 通过声明。逐项机器可读元数据见 `http/testdata/client-contracts.json`；完整客户端分析见工作区 `research/02-client-api-contract.md`。

`route_present` 只表示生产路由存在；`covered_partial` 表示有下列指名测试但未覆盖该项全部业务；`deferred_missing` 表示当前没有固定客户端端点；`conditional_unreachable` 表示源码存在但当前调用条件不能同时成立。后续执行者必须补记真实测试名与数据库断言，不能用路由黄金文件代替协议与持久化测试。

| 编号 | 回归范围 | HTTP 与数据库必须共同验证 | 盘点状态 |
| --- | --- | --- | --- |
| 01 | GET login-options；HEAD 连通性探测另列 | 顶层字符串数组；OIDC provider 名称；HEAD 不带响应体，不与能力支持计数混用 | 路由存在，待业务回归 |
| 02 | POST login | account 登录、deviceInfo、顶层 token/type/user.info；禁用/坏密码；失败不写会话或登录日志 | 路由存在，待生产 handler 密码登录回归 |
| 03–04 | currentUser/logout | 顶层用户、401 清账号；撤销 DB token；退出不依赖客户端收到 ACK | 已有部分真实路由+SQLite 回归 |
| 05–06 | OIDC auth/auth-query | code/url、固定 pending 原文、native envelope；路由/设备隔离；真实 TLS IdP→身份→本地 session 事务 | 已有部分真实路由+SQLite+TLS 回归 |
| 07 | heartbeat | 匿名设备上报形状；相同版本 `{}`；修改、删除、默认切换后的 config_options；UUID 边界；失败不当成功 | COMP05 已实现，核心与生产 HTTP+SQLite 定向 race 通过；未运行真实 Rust 客户端 |
| 08 | sysinfo | 必须原文 `SYSINFO_UPDATED`；新增/更新回读；上报不能改管理员绑定与共享授权 | 路由存在，待真实 DB 回归 |
| 09 | sysinfo_ver | 原文版本文本；直接 HTTP smoke 可测，但不可当作固定客户端正常可达闭环 | 条件不可达 |
| 10–12 | switch-grant / devices/deploy / devices/cli | 保留明确不支持状态；后续各自验证签名、操作 token、result 枚举或空文本 | 缺失，后续阶段 |
| 13–14 | settings/personal | 空 body 可用；settings 对象、personal guid；不存在接口时 404 降级 | 路由保留；本轮没有指名这两个入口的新增 HTTP 业务测试 |
| 15 | shared profiles | total/data、current/pageSize；授权规则取最大且去重；拒绝伪造或越权 guid | 生产路由+SQLite 已覆盖 1021 个 profile 和权限过滤 |
| 16–17 | legacy 整本地址簿 GET/POST | data 是 JSON 字符串，tag_colors 再编码；个人簿整本更新与 tag 同事务；保留扩展字段和共享/他人数据 | 生产路由+SQLite 已覆盖 1007 peers/1003 tags 读取及写失败回滚 |
| 18–19 | peers / tags | 空 body；peers current/pageSize/ab；tags 顶层数组；权限过滤；超过 1000 条不截断 | COMP02 生产路由+SQLite 已覆盖分页、旧 page 别名、非法参数及 wire 类型 |
| 20–21 | peer add/update | 单对象新增；字段缺失不改、空串清除；note/主机/密码/hash；forceAlwaysRelay 字符串与 same_server 布尔 | COMP03 生产路由+SQLite 已覆盖部分更新、note 清空/回读及跨用户主机元数据保护 |
| 22 | peer delete | DELETE JSON 数组 body；资源范围和持久化删除 | 路由保留；本轮未新增此入口的定向 HTTP 业务测试 |
| 23–26 | tag add/rename/update/delete | 颜色整数；重命名同步 peer 引用；DELETE 数组 body；越权拒绝 | 路由保留；本轮已测 tag 读取/legacy tag 事务，尚未逐个定向验证这四个变更入口 |
| 27–29 | accessible groups/users/peers | current/pageSize、真实 total、授权后分页；Peer.info.device_name 映射；授权依赖故障不得扩权 | COMP02 生产路由+SQLite 已覆盖千条级分页、角色/组范围和依赖失败 |
| 30–31 | conn/file audit | 事务提交/已提交重复后才 2xx 空 body；完整 u64 session_id；receipt 并发去重与冲突；连接 new/auth/close | COMP04 核心+真实 controller HTTP/SQLite 已覆盖；完整生产 router 另验证装配、u64、空 ACK、重复和依赖失败 |
| 32–35 | alarm / active conn / audit note / record | 保留明确不支持；后续验证空 ACK、GUID JSON string、授权备注与二进制上传协议 | 缺失，后续阶段 |

当前 35 项中 28 个固定 method/path 有生产路由（含当前条件不可达的 09），7 项缺失。HEAD 不是第 36 项业务功能。现有 `http/http_test.go` 的路由 inventory 证明注册形状；`TestProductionOIDCRoutesPublicPKCEAndLocalSession`、`TestProductionOIDCAdminEnvelopeAndRouteAudience` 等证明部分真实登录/当前账号/退出边界。本轮地址簿、分页、策略与审计已有明确测试证据，但不能由路由计数推导全部契约或外部运行验收通过。

## COMP02/03 地址簿、分页与字段

`internal/clientcompat` 使用显式 DB/context，controller 保留客户端响应映射。`http/addressbook_compat_test.go` 经真实 `NewHandler` 生产路由、会话与隔离 SQLite 验证：

- `TestClientCompatAddressBookPaginationAndWireTypes`：1007 个 peer（含重复业务 ID）按稳定 row_id 分页；legacy GET 完整读取；1003 个 tag 顶层数组；forceAlwaysRelay 为字符串、same_server 为布尔；非法/重复分页参数拒绝。
- `TestClientCompatProfilesPaginationMaximumRuleAndScope`：1021 个 profile，个人/组共享授权去重并取最大有效 rule，拒绝只读写入和伪造 guid。
- `TestClientCompatGroupListsPaginateAfterAuthorization`：千条级 users/peers/device groups，在授权与状态过滤后分页，真实 total、主机/分组映射和管理员边界。
- `TestClientCompatPatchPreservesOmittedFieldsAndClearsExplicitValues`、`TestClientCompatAddUsesWireFieldsAndCannotReadForeignHostMetadata`：缺失字段不改，空串/空数组明确清除；note 多行与空值回读；忽略输入所有权，不能通过猜 ID 获取他人主机信息；写入失败回滚并返回安全错误。
- `TestClientCompatLegacyReplaceIsAtomicAndPersonalOnly`：data 二次 JSON 编码、peer/tag 同事务，失败无部分提交；不改共享/他人记录；保留旧客户端省略的扩展字段，显式空 note 清除，个人簿空快照只清个人数据。
- `TestClientCompatQueryFailuresAndCanceledRequestsFailClosed`、`TestClientCompatAuthorizationDependencyFailureDoesNotExpandScope`：读取、授权依赖和请求取消不能变成空成功或全量可见。

本轮指名回归未覆盖 settings/personal 初始化、细粒度 peer DELETE 及四个 tag 变更入口，不把相邻读取或 legacy 写测试标成这些入口已经通过。

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

本地 `hbb_common` 子模块未初始化；补充只读核对了锁定提交 `5d52530b636fb296a324260b074780cf559e0093` 的 [config.rs](https://github.com/rustdesk/hbb_common/blob/5d52530b636fb296a324260b074780cf559e0093/src/config.rs#L1148)。`1148–1177,2570–2596` 确认配置优先级及保存过滤；`2616–2645` 的 HARD_SETTINGS 开关只查 HARD map，不会自动回退普通 Config。旧 `docs/peer_strategy.md` 的 HARD 回退说明不能直接当作本版本事实。

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

已通过的本地定向 race 包括 `./internal/clientstrategy`、`./internal/clientaudit`、`./http -run ProductionHeartbeat` 和 `./http -run TestClientCompat`（均带 `-mod=readonly -race -count=1`）；相应核心/HTTP 包 vet 已通过。元数据指名测试与本段命令是局部证据，不是 35 项完整通过或发布验收。

真实 Rust/Flutter 客户端、完整代理/部署链路、生产 MySQL/PostgreSQL 上的迁移及并发行为仍待实测。版本 276/277/278 已分别集成升级、重复启动、版本写入失败回滚/重试的真实 SQLite 测试；覆盖 note 补空/非空约束、收据不推断历史、策略历史不回填及显式 peer FK/删除级联。这些不代替生产数据库升级验证。CI 与制品按对应提交记录，不能由本矩阵推导整阶段验收。
