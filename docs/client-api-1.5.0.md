# RustDesk 1.5.0 HTTP API 契约基线

COMP12-B 后续证据：settings/personal、标签四写入口与peer删除共七端点，已增加42个生产HTTP/SQLite无效会话组合和同范围正控制；本地logout旧token、禁用、撤销、过期等均验证401且七表原始数据不变。这不是实际Flutter/IdP退出验收，也未新增会话政策。

COMP12-A 校正并修复 personal 关闭分支：旧200 null不能被固定客户端的非nullable Map解析；现返回404进入明确legacy分支。settings始终保持200对象；空body/{}、身份隔离及配置分支的HTTP回归见 `http/addressbook_initialization_test.go`。这是源码契约与HTTP证据，尚未真实Flutter验收。

后续 COMP11 实现：native 登录、OIDC poll、当前用户及用户列表显式返回本地 display_name/avatar，含空字符串；可见性、分页与管理端封套保持原规则。测试见 `http/user_profile_compat_test.go`，真实客户端头像加载/昵称与缓存刷新尚未验收。

后续 COMP10 实现：sysinfo 用户名省略/空串均清旧值，其他基础字段缺失保留/空串写入，显式无效值拒绝；已执行的旧预设副写纳入同事务。原文成功 ACK 只在提交后发送；预设归属/准入及未支持字段仍归 PRE01/02。证据与真实客户端未验收边界见[当前兼容矩阵](client-compatibility.md)。以下固定 API 静态基线描述继续保留为修复前对照。

客户端官方 **1.5.0**，固定 `fada664df7a294d1d1a9ca3e7cd3637069122f17`；hbb_common固定 `229b904508364c8997aad0fb5af57effac859f60`。API静态对照基线 `b59f7a56cd9e90cf48c760d2aca808a7241582f0`（b59f7a5）。只记录客户端 HTTP 契约。

后续实现：COMP09 已新增受鉴权的 `POST /api/ab/get`，读取复用 `GET /api/ab`；生产 HTTP/SQLite 的范围、响应、故障及取消回归见 `http/sciter_addressbook_get_test.go`。当前注册29项、缺失7项；下面 b59f7a5 对照保留修复前的28/8计数。COMP09 写入切片也兼容初始/重置文档省略颜色：保留仍存标签颜色，新标签默认0，显式无效值拒绝。HTTP/SQLite 与 MySQL/PostgreSQL 事务回归见兼容矩阵；真实客户端新增验收尚未完成。

01–35保持编号，36补Sciter `POST /api/ab/get`。**36是包含保留实现的method/path数，不是36项验收通过**：34项有源码调用链，09/35为当前条件不可达的保留实现；28项注册、8项缺路由。HEAD `/api/login-options` 单列为传输探测，不是第37项业务功能。

本文同步独立源码审计及API对照的纠错结论。字段为调用方可评审摘要，未冻结完整JSON Schema；没有推定未实现字段。静态分类不等于新测试或全部语义兼容。历史指名测试及真实Ubuntu验收边界见 [回归矩阵](client-compatibility.md)；机器元数据见 [client-contracts.json](../http/testdata/client-contracts.json)。

## 共有规则

下列F/R/A/S等缩写对应源码包装层；不能据此统一所有API的鉴权和响应。地址簿动作严格空body、分页放URL、受控事件与旧主控备注的差别必须保留。

**base**：src/common.rs:1126-1162: no_register_device returns empty; strip one trailing slash; Windows license API overrides explicit API option, then custom rendezvous port minus two, otherwise https://admin.rustdesk.com; HTTPS :21114 stripped unless allow-https-21114=Y. Audit and sync reject public rustdesk.com and subdomains (1166-1175,1224-1229,sync.rs:281-289).

**F**：Flutter utils/http_service.dart:12-44,54-144: default Content-Type application/json only when headers omitted; supplied header map is not merged. Web/kIsWeb always Dart; native uses Dart unless enable-flutter-http-on-rust or proxy active (21-28). Dart timeout 30s; Rust bridge polls URL-keyed status at 100ms (94-105), no Dart timeout. FFI src/flutter_ffi.rs:1209-1210 -> ui_interface.rs:858-876 -> common.rs:1853-1866.

**R**：Rust common.rs:1533-1540,1605-1695: POST sets Content-Type application/json, optional one Key: Value header, 12s per send; uncached request/TLS failure retries certificate acceptance and native TLS. General get/post/put/delete wrapper 1704-1848 parses JSON headers, optional body and returns status_code,headers,body; same TLS retry. 1475-1524/1563-1601: eligible configured nonpublic API host, WS off, forced raw TCP option or transport/5xx fallback; 4xx no fallback. No binary protocol internals audited.

**A**：Flutter common.dart:2729-2732: Authorization: Bearer <local access_token>. No automatic UUID/key header. Body-bearing AB actions add Content-Type application/json.

**S**：Sciter ui/common.tis:415-432 always calls Rust post_request even if type argument differs, JSON.stringify(params); parses status or {} and invokes success; transport/non-JSON errors become error callback(status,0), so real HTTP status is not forwarded. ui.rs:618-619 -> ui_interface.rs:887-895 -> R. Sciter getHttpHeaders index.tis:1675-1676 returns Bearer token.

**AB_action**：ab_model.dart:2002-2025 accepts precisely HTTP 200 with empty body; otherwise tries jsonDecode(body)['error'].toString(), then HTTP/body fallback. JSON {} becomes string 'null', therefore not a generic success. No business-layer automatic retry.

**AB_empty**：ab_model.dart:2029-2031 explicitly Content-Length:0, application/json, Bearer; body omitted. Paginated POST requests put paging in URL query, not body.

**page**：current starts at 1, pageSize=100, first nonzero total latched; loop current*100<total. Not cursor-based. Consumers deduplicate by group/user/profile name or peer id. JSON error checked before status!=200.

**audit_R**：server/connection.rs:1661-1757 sends same serialized body/nonce sequentially for conn, spawned for file/alarm. Success only 2xx with trimmed-empty body; nonempty 2xx (including error JSON), transport, 5xx,408,429 retry with 10s/30s backoff; maximum 3 attempts and 120s start deadline. Other statuses stop. Header is empty; body id/uuid identity only. Does not prove server authorization or dedup correctness.

**UserPayload**：flutter/lib/common/hbbs/hbbs.dart:26-61: name,display_name,avatar,email,note,verifier,status(0 disabled,-1 unverified,else normal),is_admin==true. Rust account.rs:79-107 requires name,info and access_token/type/user for AuthBody, optional/default other user fields.

**Peer**：flutter/models/peer_model.dart:33-67 parses id,hash,password,username,hostname,platform,alias,tags,forceAlwaysRelay string 'true',rdpPort,rdpUsername,loginName,device_group_name,note(string),same_server. AB profile hbbs.dart:258-274 guid,name,owner,note,info,rule; tag:277-285 name,color; accessible device group:288-294 name.

**HEAD**：hbbs_http/http_client.rs:125-245 and 260-385 sends HEAD when TLS/cache info absent. HTTP status is not used as failure; any returned response counts connectivity success. May retry TLS implementations/certificate mode. Strict helpers require HTTPS and never allow invalid certificates. HEAD is connectivity/size probing, not a business login-options parse.

动态URL/重定向、依赖隐式头像/字体下载和部署Web桥不能据此穷举。公共更新、Telegram、下载资产不是本项目REST。固定子模块版本URL补证见 [version_check_request](https://github.com/rustdesk/hbb_common/blob/229b904508364c8997aad0fb5af57effac859f60/src/lib.rs#L510)。

## method/path 索引

| ID | Method/path | b59f7a5路由 | 静态语义分类 |
| --- | --- | --- | --- |
| [01](#contract-01) | GET `/api/login-options` | 存在 | implemented |
| [02](#contract-02) | POST `/api/login` | 存在 | route_only |
| [03](#contract-03) | POST `/api/currentUser` | 存在 | route_only |
| [04](#contract-04) | POST `/api/logout` | 存在 | implemented |
| [05](#contract-05) | POST `/api/oidc/auth` | 存在 | implemented |
| [06](#contract-06) | GET `/api/oidc/auth-query` | 存在 | implemented |
| [07](#contract-07) | POST `/api/heartbeat` | 存在 | route_only |
| [08](#contract-08) | POST `/api/sysinfo` | 存在 | route_only |
| [09](#contract-09) | POST `/api/sysinfo_ver` | 存在 | conditional |
| [10](#contract-10) | POST `/api/switch-grant` | 缺失 | missing |
| [11](#contract-11) | POST `/api/devices/deploy` | 缺失 | missing |
| [12](#contract-12) | POST `/api/devices/cli` | 缺失 | missing |
| [13](#contract-13) | POST `/api/ab/settings` | 存在 | implemented |
| [14](#contract-14) | POST `/api/ab/personal` | 存在 | route_only（旧盘点校正） |
| [15](#contract-15) | POST `/api/ab/shared/profiles` | 存在 | implemented |
| [16](#contract-16) | GET `/api/ab` | 存在 | implemented |
| [17](#contract-17) | POST `/api/ab` | 存在 | route_only |
| [18](#contract-18) | POST `/api/ab/peers` | 存在 | implemented |
| [19](#contract-19) | POST `/api/ab/tags/:guid` | 存在 | implemented |
| [20](#contract-20) | POST `/api/ab/peer/add/:guid` | 存在 | implemented |
| [21](#contract-21) | PUT `/api/ab/peer/update/:guid` | 存在 | implemented |
| [22](#contract-22) | DELETE `/api/ab/peer/:guid` | 存在 | route_only |
| [23](#contract-23) | POST `/api/ab/tag/add/:guid` | 存在 | route_only |
| [24](#contract-24) | PUT `/api/ab/tag/rename/:guid` | 存在 | route_only |
| [25](#contract-25) | PUT `/api/ab/tag/update/:guid` | 存在 | route_only |
| [26](#contract-26) | DELETE `/api/ab/tag/:guid` | 存在 | route_only |
| [27](#contract-27) | GET `/api/device-group/accessible` | 存在 | conditional |
| [28](#contract-28) | GET `/api/users` | 存在 | implemented |
| [29](#contract-29) | GET `/api/peers` | 存在 | implemented |
| [30](#contract-30) | POST `/api/audit/conn` | 存在 | route_only |
| [31](#contract-31) | POST `/api/audit/file` | 存在 | implemented |
| [32](#contract-32) | POST `/api/audit/alarm` | 缺失 | missing |
| [33](#contract-33) | GET `/api/audit/conn/active` | 缺失 | missing |
| [34](#contract-34) | PUT `/api/audit` | 缺失 | missing |
| [35](#contract-35) | POST `/api/record` | 缺失 | conditional |
| [36](#contract-36) | POST `/api/ab/get` | 缺失 | missing |

`implemented`仅表示静态审查的形状有实现；`route_only`有实际处理但至少一项变体/语义不足；`missing`缺路由；`conditional`须结合条件。均不是本轮运行通过状态。

## 逐项契约

<a id="contract-01"></a>

### 01 · GET `/api/login-options`

API基线：`implemented`；字符串数组common-oidc/JSON及oidc/name格式可解析；配置依赖。业务provider列表本轮未实测

- **OIDC option parse**（`source_reachable`）：Flutter API nonempty
  - Header：F default JSON; no Bearer
  - 请求：no body/query
  - 响应：2xx JSON list<string>; first common-oidc/ prefix suffix decoded as JSON; otherwise oidc/name converted {name}
  - 错误/重试：non2xx throws; malformed list/suffix throws; F/R only
  - 来源：[flutter/lib/models/user_model.dart:240-263](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/flutter/lib/models/user_model.dart#L240)

API入口/实现：[http/router/api.go:34](../http/router/api.go#L34)、[http/controller/api/login.go:118](../http/controller/api/login.go#L118)。

未指名此项全部业务契约的已执行测试，不据相邻接口推导通过。

<a id="contract-02"></a>

### 02 · POST `/api/login`

API基线：`route_only`；普通account返回access_token；email_code/verificationCode/tfaCode/secret未建模或dispatch；多阶段为条件能力，非已批准新增schema。见F3、F7

- **account**（`source_reachable`）：Flutter account login UI; disable-account gate in callers
  - Header：F default JSON; no Bearer
  - 请求：username,password,id,uuid,autoLogin:true,type:account,deviceInfo{os,type,name}
  - 响应：HTTP200 JSON; error throws; type access_token with access_token and optional user; email_check/tfa_check challenge with secret,user,tfa_type
  - 错误/重试：non200 RequestException(status,error); invalid JSON rethrows; F/R only
  - 来源：[flutter/lib/models/user_model.dart:195-216](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/flutter/lib/models/user_model.dart#L195)、[flutter/lib/common/widgets/login.dart:842-849](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/flutter/lib/common/widgets/login.dart#L842)、[flutter/lib/common/hbbs/hbbs.dart:133-196](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/flutter/lib/common/hbbs/hbbs.dart#L133)

- **email_code email verification**（`source_reachable`）：Flutter email challenge
  - Header：F default JSON; no Bearer
  - 请求：username,id,uuid,autoLogin,type:email_code,verificationCode,secret,deviceInfo; no password or tfaCode
  - 响应：same LoginResponse as account
  - 错误/重试：same login parser; F/R only
  - 来源：[flutter/lib/models/user_model.dart:195-216](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/flutter/lib/models/user_model.dart#L195)、[flutter/lib/common/widgets/login.dart:1003-1030](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/flutter/lib/common/widgets/login.dart#L1003)、[flutter/lib/common/hbbs/hbbs.dart:155-176](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/flutter/lib/common/hbbs/hbbs.dart#L155)

- **email_code TOTP verification**（`source_reachable`）：Flutter non-email challenge
  - Header：F default JSON; no Bearer
  - 请求：email verification fields plus tfaCode=verificationCode; type remains email_code, not tfa_code
  - 响应：same LoginResponse
  - 错误/重试：same login parser; F/R only
  - 来源：[flutter/lib/models/user_model.dart:195-216](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/flutter/lib/models/user_model.dart#L195)、[flutter/lib/common/widgets/login.dart:1016-1024](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/flutter/lib/common/widgets/login.dart#L1016)

- **Sciter account**（`source_reachable_preserved_sciter`）：Sciter desktop build
  - Header：S JSON; no Bearer
  - 请求：username,password,id,uuid,type:account,deviceInfo; no autoLogin
  - 响应：error; email_check challenge; otherwise stores access_token/user
  - 错误/重试：S/R only; Sciter code specifically branches email_check
  - 来源：[src/ui/index.tis:1541-1564](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/ui/index.tis#L1541)

- **Sciter email verification**（`source_reachable_preserved_sciter`）：Sciter email challenge
  - Header：S JSON; no Bearer
  - 请求：username,id,uuid,type:email_code,verificationCode,tfaCode:'',secret,deviceInfo
  - 响应：error or access_token,user
  - 错误/重试：S/R only
  - 来源：[src/ui/index.tis:1569-1618](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/ui/index.tis#L1569)

- **Sciter non-email verification**（`source_reachable_preserved_sciter`）：Sciter non-email challenge
  - Header：S JSON; no Bearer
  - 请求：same as Sciter email but tfaCode=verificationCode; type email_code
  - 响应：error or access_token,user
  - 错误/重试：S/R only
  - 来源：[src/ui/index.tis:1569-1618](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/ui/index.tis#L1569)

API入口/实现：[http/router/api.go:35](../http/router/api.go#L35)、[http/controller/api/login.go:34](../http/controller/api/login.go#L34)、[http/request/api/user.go:31](../http/request/api/user.go#L31)、[http/response/api/user.go:21](../http/response/api/user.go#L21)。

未指名此项全部业务契约的已执行测试，不据相邻接口推导通过。

<a id="contract-03"></a>

### 03 · POST `/api/currentUser`

API基线：`route_only`；基本用户/401兼容；已有nickname/avatar不投影display_name/avatar，刷新会清空客户端这两项。见F7 Sciter index.tis:1660调用verify_login(data.verifier,token)，但本commit common.rs:2564恒为true，所以缺verifier不能作为该客户端校验失败证据。Sciter helper将JSON error交给成功回调，客户端仅error==Invalid token重置；API为Unauthorized，可能不会清本地token（静态错误处理差异，未真实运行）。

- **Flutter refresh**（`source_reachable`）：not disable-account; token present; single refresh lock
  - Header：JSON+Bearer
  - 请求：id,uuid
  - 响应：UserPayload directly; body error is server failure
  - 错误/重试：400 resets user;401 resets user+AB/group; finally refreshes models; network errors surfaced; F/R only
  - 来源：[flutter/lib/models/user_model.dart:53-113](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/flutter/lib/models/user_model.dart#L53)

- **Sciter refresh**（`source_reachable_preserved_sciter`）：Sciter token present
  - Header：S+Bearer
  - 请求：id,uuid
  - 响应：UserPayload directly plus verifier checked with handler.verify_login
  - 错误/重试：Invalid token body resets; S error callback always status0 so written 400/401 callback branch cannot inspect actual HTTP status
  - 来源：[src/ui/index.tis:1646-1672](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/ui/index.tis#L1646)

API入口/实现：[http/router/api.go:76](../http/router/api.go#L76)、[internal/httpapi/sessions.go:190](../internal/httpapi/sessions.go#L190)。

既有指名测试（历史部分证据，本切片未重跑）：`http/oidc_integration_test.go:TestProductionOIDCRoutesPublicPKCEAndLocalSession`。

<a id="contract-04"></a>

### 04 · POST `/api/logout`

API基线：`implemented`；成功JSON null；客户端无论结果清理本地。仅撤销当前token

- **Flutter**（`source_reachable`）：Flutter logout; optional API override
  - Header：JSON+Bearer
  - 请求：id,uuid
  - 响应：ignored
  - 错误/重试：2s caller timeout; errors ignored; always clears local login/models; F/R may continue after timeout
  - 来源：[flutter/lib/models/user_model.dart:170-188](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/flutter/lib/models/user_model.dart#L170)

- **Sciter**（`source_reachable_preserved_sciter`）：Sciter desktop
  - Header：S+Bearer
  - 请求：id,uuid
  - 响应：ignored parsed body
  - 错误/重试：local reset immediately; S/R
  - 来源：[src/ui/index.tis:1637-1643](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/ui/index.tis#L1637)

API入口/实现：[http/router/api.go:79](../http/router/api.go#L79)、[internal/httpapi/sessions.go:212](../internal/httpapi/sessions.go#L212)。

既有指名测试（历史部分证据，本切片未重跑）：`http/oidc_integration_test.go:TestProductionOIDCRoutesPublicPKCEAndLocalSession`。

<a id="contract-05"></a>

### 05 · POST `/api/oidc/auth`

API基线：`implemented`；返回顶层code/url；请求apiDomain被忽略，回调采用服务端配置。feature=flutter

实现/触发条件：feature=flutter; provider configured

- **start**（`source_reachable`）：feature flutter; OIDC UI
  - Header：R JSON; no Bearer
  - 请求：op,id,uuid,deviceInfo,apiDomain
  - 响应：direct object code:string,url:URL or error:string; not wrapped in data
  - 错误/重试：invalid shape stops with Invalid auth response; R only
  - 来源：[src/hbbs_http/account.rs:160-176](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/hbbs_http/account.rs#L160)、[src/hbbs_http.rs:24-38](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/hbbs_http.rs#L24)

API入口/实现：[http/router/api.go:42](../http/router/api.go#L42)、[internal/httpapi/oidc.go:38](../internal/httpapi/oidc.go#L38)。

既有指名测试（历史部分证据，本切片未重跑）：`http/oidc_integration_test.go:TestProductionOIDCRoutesPublicPKCEAndLocalSession`。

<a id="contract-06"></a>

### 06 · GET `/api/oidc/auth-query`

API基线：`implemented`；pending顶层error精确为No authed oidc is found，成功access_token/type/user；display_name/avatar见F7

实现/触发条件：feature=flutter; provider configured

- **poll**（`source_reachable`）：feature flutter; current auth attempt
  - Header：empty JSON header map, no Bearer
  - 请求：query code,id,uuid; no body
  - 响应：direct AuthBody or error:string; ignores wrapper HTTP status, parses body
  - 错误/重试：every1s up to180s; No authed oidc is found substring and transport/shape errors continue; other server error terminates; cancel/attempt guard
  - 来源：[src/hbbs_http/account.rs:179-198](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/hbbs_http/account.rs#L179)、[src/hbbs_http/account.rs:291-345](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/hbbs_http/account.rs#L291)

API入口/实现：[http/router/api.go:44](../http/router/api.go#L44)、[internal/httpapi/oidc.go:83](../internal/httpapi/oidc.go#L83)。

既有指名测试（历史部分证据，本切片未重跑）：`http/oidc_integration_test.go:TestProductionOIDCRoutesPublicPKCEAndLocalSession`。

<a id="contract-07"></a>

### 07 · POST `/api/heartbeat`

API基线：`route_only`；策略revision/清空历史已实作；conns被DTO忽略，无sysinfo/disconnect响应。不能整体标全兼容。见F2

实现/触发条件：non-iOS; custom API; service enabled

- **sync**（`source_reachable`）：not iOS; nonpublic API; stop-service false
  - Header：R JSON; no Bearer
  - 请求：id,uuid,ver(numeric),modified_at; conns optional nonempty list
  - 响应：optional sysinfo presence resets upload; disconnect list<int>; modified_at integer; strategy{config_options,extra} parsed but only config_options applied
  - 错误/重试：3s loop; with no conns heartbeat at least15s apart; errors log then later ticks; R
  - 来源：[src/hbbs_http/sync.rs:85-108](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/hbbs_http/sync.rs#L85)、[src/hbbs_http/sync.rs:232-274](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/hbbs_http/sync.rs#L232)、[src/hbbs_http/sync.rs:281-289](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/hbbs_http/sync.rs#L281)、[src/rendezvous_mediator.rs:281](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/rendezvous_mediator.rs#L281)

API入口/实现：[http/router/api.go:28](../http/router/api.go#L28)、[http/controller/api/index.go:44](../http/controller/api/index.go#L44)、[http/request/api/peer.go:78](../http/request/api/peer.go#L78)、[internal/clientstrategy/strategy.go:47](../internal/clientstrategy/strategy.go#L47)。

既有指名测试（历史部分证据，本切片未重跑）：`http/strategy_compat_test.go:TestProductionHeartbeatPolicyDeletionAndClientClearSemantics`；`http/strategy_compat_test.go:TestProductionHeartbeatFailuresCannotBecomeEmptySuccess`；`http/strategy_compat_test.go:TestProductionHeartbeatDeviceAndContextBoundaries`；`internal/clientstrategy/strategy_test.go:TestUnassignedDeviceDoesNotClaimLocalPresetKeys`；`internal/clientstrategy/strategy_test.go:TestPresetAssignmentOverridesPolicyAndRemovalPreservesValidFallback`；`internal/clientstrategy/strategy_test.go:TestCommitFailureDoesNotPublishOrLoseHistory`；`internal/clientstrategy/strategy_test.go:TestConcurrentHeartbeatUsesOneDurableHistory`。

<a id="contract-08"></a>

### 08 · POST `/api/sysinfo`

API基线：`route_only`；SYSINFO_UPDATED文本正确；preset扩展丢失/硬编码owner1/副写失败仍成功/空字段不能清空。见F4-F5

- **upload**（`source_reachable`）：same sync gate; first upload, changed API/id or active username; failed upload throttled120s
  - Header：R JSON; no Bearer
  - 请求：cpu,memory,os,hostname,optional username; version,id,uuid; optional preset-address-book-name/tag/alias/password/note, preset-user-name, preset-strategy-name, preset-device-group-name, preset-note. preset-device-username overwrites username; preset-device-name overwrites hostname; option keys are not transmitted as preset-device-username/name.
  - 响应：plain SYSINFO_UPDATED success, ID_NOT_FOUND retries next tick, other body/failure throttles120s
  - 错误/重试：same sync and R
  - 来源：[src/hbbs_http/sync.rs:126-180](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/hbbs_http/sync.rs#L126)、[src/hbbs_http/sync.rs:211-230](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/hbbs_http/sync.rs#L211)、[src/common.rs:936-979](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/common.rs#L936)、[libs/base/src/config/keys.rs:79-86](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/libs/base/src/config/keys.rs#L79)、[libs/base/src/config/keys.rs:113-115](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/libs/base/src/config/keys.rs#L113)

API入口/实现：[http/router/api.go:57](../http/router/api.go#L57)、[http/controller/api/peer.go:26](../http/controller/api/peer.go#L26)、[http/request/api/peer.go:18](../http/request/api/peer.go#L18)、[service/peer.go:199](../service/peer.go#L199)、[service/addressBook.go:401](../service/addressBook.go#L401)。

未指名此项全部业务契约的已执行测试，不据相邻接口推导通过。

<a id="contract-09"></a>

### 09 · POST `/api/sysinfo_ver`

API基线：`conditional`；路由文本version+starttime存在；当前heartbeat_url拒绝public，而调用分支要求is_public，正常start_hbbs_sync路径下不可达。不能当真实调用验收

实现/触发条件：non-iOS; branch is_public conflicts with heartbeat_url public guard

兼容要点：Public-URL requirement conflicts with heartbeat_url public guard; retained and statically unreachable, not normal client flow.

- **hash version check**（`statically_unreachable_under_current_caller_gate`）：code requires public URL, heartbeat_url already returns empty for public URL and caller continues
  - Header：R JSON; no Bearer
  - 请求：empty body
  - 响应：raw string compared to stored sysinfo_ver
  - 错误/重试：retained code; not a proven actual stock request
  - 来源：[src/hbbs_http/sync.rs:182-209](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/hbbs_http/sync.rs#L182)、[src/hbbs_http/sync.rs:281-289](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/hbbs_http/sync.rs#L281)

API入口/实现：[http/router/api.go:58](../http/router/api.go#L58)、[http/controller/api/peer.go:74](../http/controller/api/peer.go#L74)。

未指名此项全部业务契约的已执行测试，不据相邻接口推导通过。

<a id="contract-10"></a>

### 10 · POST `/api/switch-grant`

API基线：`missing`；id/verifier/timestamp/signature；期望accepted和时钟重试server_time；仅列HTTP，暂停的非HTTP协议不展开

实现/触发条件：feature=flutter; non-Android/non-iOS; custom API

- **register**（`source_reachable`）：feature flutter, desktop nonAndroid/nonIOS, nonpublic API, device signing key present
  - Header：R JSON; no Bearer
  - 请求：id,switch_code_verifier,timestamp(string Unix seconds),signature(base64)
  - 响应：accepted bool mandatory; accepted:false plus server_time:i64 allows retry
  - 错误/重试：fire-and-forget; max2 signed attempts, re-sign once with server time; errors stop; R
  - 来源：[src/hbbs_http/sync.rs:317-385](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/hbbs_http/sync.rs#L317)、[src/ipc.rs:1086](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/ipc.rs#L1086)

API入口/实现：[http/router/api.go:14-130 (route absent)](../http/router/api.go#L14)。

未指名此项全部业务契约的已执行测试，不据相邻接口推导通过。

<a id="contract-11"></a>

### 11 · POST `/api/devices/deploy`

API基线：`missing`；id/uuid/pk；result OK/NOT_ENABLED等分支；不得由sysinfo替代

- **deploy current or supplied ID**（`source_reachable`）：registrable; trimmed nonempty token; CLI additionally installed/root and not outgoing-only; ID-change aftermath excluded from audit
  - Header：R JSON+Bearer deploy token
  - 请求：id,uuid,pk(base64)
  - 响应：JSON result enum OK,NOT_ENABLED,INVALID_INPUT,ID_TAKEN; invalid/unrecognized returns raw text or Unknown response
  - 错误/重试：R only; HTTP status not inspected
  - 来源：[src/ui_interface.rs:1082-1109](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/ui_interface.rs#L1082)、[src/ui_interface.rs:1138-1147](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/ui_interface.rs#L1138)、[src/core_main.rs:647-698](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/core_main.rs#L647)

API入口/实现：[http/router/api.go:14-130 (route absent)](../http/router/api.go#L14)。

未指名此项全部业务契约的已执行测试，不据相邻接口推导通过。

<a id="contract-12"></a>

### 12 · POST `/api/devices/cli`

API基线：`missing`；用户/设备/地址簿参数；客户端以空body视Done。installed+root CLI条件

实现/触发条件：installed administrative CLI path

- **assign**（`source_reachable`）：--assign installed+root; registrable device; --token and at least one primary option
  - Header：R JSON+Bearer CLI token
  - 请求：id,uuid; optional user_name,strategy_name,address_book_name,device_group_name,note,device_username,device_name; address_book_tag/alias/password/note only if address_book_name provided
  - 响应：empty body prints Done!, otherwise prints raw text
  - 错误/重试：no status/business JSON validation; R only
  - 来源：[src/core_main.rs:542-645](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/core_main.rs#L542)

API入口/实现：[http/router/api.go:14-130 (route absent)](../http/router/api.go#L14)。

未指名此项全部业务契约的已执行测试，不据相邻接口推导通过。

<a id="contract-13"></a>

### 13 · POST `/api/ab/settings`

API基线：`implemented`；0代表无限；此处无DB需求

- **capabilities**（`source_reachable`）：Flutter address book enabled/logged in
  - Header：AB_empty
  - 请求：no body
  - 响应：HTTP200 {max_peer_one_ab}, default0
  - 错误/重试：404 indicates shared AB unsupported; JSON error/status fail;401 resets; F/R
  - 来源：[flutter/lib/models/ab_model.dart:230-257](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/flutter/lib/models/ab_model.dart#L230)

API入口/实现：[http/router/api.go:107](../http/router/api.go#L107)、[http/controller/api/ab.go:361](../http/controller/api/ab.go#L361)。

未指名此项全部业务契约的已执行测试，不据相邻接口推导通过。

<a id="contract-14"></a>

### 14 · POST `/api/ab/personal`

API基线：`route_only`（原conditional判定已校正）；Personal=1返回guid/name/rule3；否则旧实现返回200 null。原盘点误判其可触发legacy：固定客户端实际上解析失败；COMP12-A纠正为404兼容分支。

实现/触发条件：Rustdesk.Personal controls personal-profile vs legacy

- **personal GUID**（`source_reachable`）：Flutter AB init
  - Header：AB_empty
  - 请求：no body
  - 响应：HTTP200 {guid};404 leaves guid null and continues legacy mode
  - 错误/重试：other JSON error/status stops pull;401 resets; F/R
  - 来源：[flutter/lib/models/ab_model.dart:262-292](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/flutter/lib/models/ab_model.dart#L262)

API入口/实现：[http/router/api.go:105](../http/router/api.go#L105)、[http/controller/api/ab.go:327](../http/controller/api/ab.go#L327)。

未指名此项全部业务契约的已执行测试，不据相邻接口推导通过。

<a id="contract-15"></a>

### 15 · POST `/api/ab/shared/profiles`

API基线：`implemented`；分页、owner/guid/rule类型；读scope取MAX，后续旧tag/delete授权不一致见F6

- **list shared**（`source_reachable`）：Flutter shared AB supported
  - Header：AB_empty
  - 请求：query current,pageSize
  - 响应：HTTP200 {total,data:[AbProfile]}; page
  - 错误/重试：404 unsupported; errors stop;401 resets; F/R
  - 来源：[flutter/lib/models/ab_model.dart:295-359](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/flutter/lib/models/ab_model.dart#L295)

API入口/实现：[http/router/api.go:109](../http/router/api.go#L109)、[http/controller/api/ab.go:379](../http/controller/api/ab.go#L379)、[internal/clientcompat/addressbook.go:93](../internal/clientcompat/addressbook.go#L93)。

既有指名测试（历史部分证据，本切片未重跑）：`http/addressbook_compat_test.go:TestClientCompatProfilesPaginationMaximumRuleAndScope`。

<a id="contract-16"></a>

### 16 · GET `/api/ab`

API基线：`implemented`；旧personal整本读取；tags数组和tag_colors JSON字符串

- **legacy snapshot Flutter**（`source_reachable`）：Flutter legacy AB selected
  - Header：A+JSON+Accept-Encoding:gzip
  - 请求：no body
  - 响应：null clears; nonempty object data(JSON string),licensed_devices; nested tags,peers,tag_colors(JSON string)
  - 错误/重试：body error handled; no universal status==200 gate in this pull;401 only resets if pullError nonempty; F/R
  - 来源：[flutter/lib/models/ab_model.dart:1008-1052](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/flutter/lib/models/ab_model.dart#L1008)、[flutter/lib/models/ab_model.dart:1325-1350](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/flutter/lib/models/ab_model.dart#L1325)

API入口/实现：[http/router/api.go:92](../http/router/api.go#L92)、[http/controller/api/ab.go:31](../http/controller/api/ab.go#L31)、[internal/clientcompat/addressbook.go:68](../internal/clientcompat/addressbook.go#L68)。

既有指名测试（历史部分证据，本切片未重跑）：`http/addressbook_compat_test.go:TestClientCompatAddressBookPaginationAndWireTypes`。

<a id="contract-17"></a>

### 17 · POST `/api/ab`

API基线：`route_only`；成功JSON null，更新仅actor cid0，省略与空值区分；现有commit-failure用例 Sciter初始/reset文档只有tags,peers（ab.tis:2,675），updateAb:669直接提交，未必有tag_colors；ReplacePersonal:248强制TagColors非空，因此此变体400。已拉取含tag_colors的文档可以保留该字段，不能说所有Sciter写请求必失败。

- **whole snapshot Flutter**（`source_reachable`）：Flutter logged in legacy AB
  - Header：A+JSON
  - 请求：{data:JSONstring({tags,peers:[id,username,hostname,platform,alias,tags,hash],tag_colors:JSONstring(map)})}
  - 响应：HTTP200 empty/null succeeds; or JSON without error and HTTP200 succeeds
  - 错误/重试：failure toast; F/R only
  - 来源：[flutter/lib/models/ab_model.dart:1055-1095](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/flutter/lib/models/ab_model.dart#L1055)、[flutter/lib/models/ab_model.dart:1309-1322](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/flutter/lib/models/ab_model.dart#L1309)、[flutter/lib/models/peer_model.dart:70-82](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/flutter/lib/models/peer_model.dart#L70)

- **whole snapshot Sciter**（`source_reachable_preserved_sciter`）：Sciter AB
  - Header：S+Bearer
  - 请求：{data:JSON.stringify(ab)}
  - 响应：ignored parsed body
  - 错误/重试：error callback empty; S/R
  - 来源：[src/ui/ab.tis:668-671](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/ui/ab.tis#L668)

API入口/实现：[http/router/api.go:94](../http/router/api.go#L94)、[http/controller/api/ab.go:72](../http/controller/api/ab.go#L72)、[internal/clientcompat/addressbook.go:245](../internal/clientcompat/addressbook.go#L245)。

既有指名测试（历史部分证据，本切片未重跑）：`http/addressbook_compat_test.go:TestClientCompatLegacyReplaceIsAtomicAndPersonalOnly`。

<a id="contract-18"></a>

### 18 · POST `/api/ab/peers`

API基线：`implemented`；current/pageSize/ab；data数组、wire字符串、total与licensed_devices

- **list**（`source_reachable`）：Flutter personal GUID/shared AB
  - Header：AB_empty
  - 请求：query current,pageSize,ab=profile.guid
  - 响应：HTTP200 {total,data:[Peer]}; page
  - 错误/重试：JSON error/status fail;401 resets; F/R
  - 来源：[flutter/lib/models/ab_model.dart:1432-1496](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/flutter/lib/models/ab_model.dart#L1432)

API入口/实现：[http/router/api.go:111](../http/router/api.go#L111)、[http/controller/api/ab.go:485](../http/controller/api/ab.go#L485)、[internal/clientcompat/addressbook.go:12](../internal/clientcompat/addressbook.go#L12)。

既有指名测试（历史部分证据，本切片未重跑）：`http/addressbook_compat_test.go:TestClientCompatAddressBookPaginationAndWireTypes`；`http/addressbook_compat_test.go:TestClientCompatQueryFailuresAndCanceledRequestsFailClosed`；`http/addressbook_compat_test.go:TestClientCompatProfilesPaginationMaximumRuleAndScope`。

<a id="contract-19"></a>

### 19 · POST `/api/ab/tags/:guid`

API基线：`implemented`；顶层name/color数组；标签rename/delete仍有语义缺口见F1

- **list**（`source_reachable`）：Flutter personal/shared AB
  - Header：AB_empty
  - 请求：no body; no paging
  - 响应：HTTP200 top-level array [{name,color}]
  - 错误/重试：wrong shape/status fail;401 resets; F/R
  - 来源：[flutter/lib/models/ab_model.dart:1499-1543](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/flutter/lib/models/ab_model.dart#L1499)

API入口/实现：[http/router/api.go:113](../http/router/api.go#L113)、[http/controller/api/ab.go:106](../http/controller/api/ab.go#L106)、[internal/clientcompat/addressbook.go:52](../internal/clientcompat/addressbook.go#L52)。

既有指名测试（历史部分证据，本切片未重跑）：`http/addressbook_compat_test.go:TestClientCompatAddressBookPaginationAndWireTypes`。

<a id="contract-20"></a>

### 20 · POST `/api/ab/peer/add/:guid`

API基线：`implemented`；当前peer字段齐全，限制外属设备metadata补全；空响应正确

- **personal**（`source_reachable`）：personal AB, skip existing, max-device check
  - Header：A+JSON
  - 请求：one peer map per request; password removed, hash allowed
  - 响应：AB_action
  - 错误/重试：stop first error; F/R only
  - 来源：[flutter/lib/models/ab_model.dart:1548-1577](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/flutter/lib/models/ab_model.dart#L1548)、[flutter/lib/models/ab_model.dart:915-920](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/flutter/lib/models/ab_model.dart#L915)

- **shared**（`source_reachable`）：shared AB, skip existing,max-device check
  - Header：A+JSON
  - 请求：one peer map per request; hash removed,password allowed
  - 响应：AB_action
  - 错误/重试：stop first error; F/R only
  - 来源：[flutter/lib/models/ab_model.dart:1548-1577](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/flutter/lib/models/ab_model.dart#L1548)、[flutter/lib/models/ab_model.dart:915-920](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/flutter/lib/models/ab_model.dart#L915)

API入口/实现：[http/router/api.go:115](../http/router/api.go#L115)、[http/controller/api/ab.go:515](../http/controller/api/ab.go#L515)、[internal/clientcompat/addressbook.go:225](../internal/clientcompat/addressbook.go#L225)。

既有指名测试（历史部分证据，本切片未重跑）：`http/addressbook_compat_test.go:TestClientCompatAddUsesWireFieldsAndCannotReadForeignHostMetadata`。

<a id="contract-21"></a>

### 21 · PUT `/api/ab/peer/update/:guid`

API基线：`implemented`；客户端id+tags/alias/note/hash/password/username/hostname/platform各patch可接受；省略保留/显式空值清除，空成功

兼容要点：Personal password save {id,hash} and shared {id,password} are exclusive. Recent sync conditionally patches host fields and personal hash.

- **replace tags**（`source_reachable`）：Flutter writable AB callers
  - Header：A+JSON
  - 请求：{id,tags}; one request per id
  - 响应：AB_action
  - 错误/重试：stop first error; F/R
  - 来源：[flutter/lib/models/ab_model.dart:1581-1603](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/flutter/lib/models/ab_model.dart#L1581)

- **alias**（`source_reachable`）：Flutter writable AB callers
  - Header：A+JSON
  - 请求：{id,alias}
  - 响应：AB_action
  - 错误/重试：F/R only
  - 来源：[flutter/lib/models/ab_model.dart:1607-1623](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/flutter/lib/models/ab_model.dart#L1607)

- **note**（`source_reachable`）：Flutter writable AB callers
  - Header：A+JSON
  - 请求：{id,note}
  - 响应：AB_action
  - 错误/重试：F/R only
  - 来源：[flutter/lib/models/ab_model.dart:1628-1644](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/flutter/lib/models/ab_model.dart#L1628)

- **personal hash**（`source_reachable`）：personal AB only, existing peer required
  - Header：A+JSON
  - 请求：{id,hash}
  - 响应：AB_action
  - 错误/重试：F/R only
  - 来源：[flutter/lib/models/ab_model.dart:1648-1672](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/flutter/lib/models/ab_model.dart#L1648)

- **shared password**（`source_reachable`）：shared AB only
  - Header：A+JSON
  - 请求：{id,password}
  - 响应：AB_action
  - 错误/重试：F/R only
  - 来源：[flutter/lib/models/ab_model.dart:1648-1664](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/flutter/lib/models/ab_model.dart#L1648)、[flutter/lib/models/ab_model.dart:1676-1678](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/flutter/lib/models/ab_model.dart#L1676)

- **recent-peer sync**（`source_reachable`）：changed fields only, existing AB peers
  - Header：A+JSON
  - 请求：{id, optional username,hostname,platform if sameServer!=true and nonempty changed; optional hash if personal and changed}
  - 响应：AB_action
  - 错误/重试：log errors and continue outer peers; no new peers; F/R
  - 来源：[flutter/lib/models/ab_model.dart:1682-1747](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/flutter/lib/models/ab_model.dart#L1682)

API入口/实现：[http/router/api.go:119](../http/router/api.go#L119)、[http/controller/api/ab.go:592](../http/controller/api/ab.go#L592)、[internal/clientcompat/addressbook.go:156](../internal/clientcompat/addressbook.go#L156)。

既有指名测试（历史部分证据，本切片未重跑）：`http/addressbook_compat_test.go:TestClientCompatPatchPreservesOmittedFieldsAndClearsExplicitValues`；`http/addressbook_compat_test.go:TestClientCompatProfilesPaginationMaximumRuleAndScope`。

<a id="contract-22"></a>

### 22 · DELETE `/api/ab/peer/:guid`

API基线：`route_only`；正常空成功；授权First与profile MAX冲突；多ids逐条提交会partial失败，无批次事务。见F6、F8

- **batch IDs**（`source_reachable`）：Flutter writable AB callers
  - Header：A+JSON
  - 请求：top-level array<string> IDs
  - 响应：AB_action
  - 错误/重试：F/R only
  - 来源：[flutter/lib/models/ab_model.dart:1752-1769](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/flutter/lib/models/ab_model.dart#L1752)

API入口/实现：[http/router/api.go:117](../http/router/api.go#L117)、[http/controller/api/ab.go:544](../http/controller/api/ab.go#L544)、[service/addressBook.go:291](../service/addressBook.go#L291)、[service/addressBook.go:153](../service/addressBook.go#L153)。

未指名此项全部业务契约的已执行测试，不据相邻接口推导通过。

<a id="contract-23"></a>

### 23 · POST `/api/ab/tag/add/:guid`

API基线：`route_only`；name/color和空成功兼容；旧First授权分歧与读错误吞掉未覆盖。见F6

- **add**（`source_reachable`）：Flutter writable AB callers
  - Header：A+JSON
  - 请求：{name,color:int}; one per tag
  - 响应：AB_action
  - 错误/重试：stop first error; F/R
  - 来源：[flutter/lib/models/ab_model.dart:1776-1800](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/flutter/lib/models/ab_model.dart#L1776)

API入口/实现：[http/router/api.go:121](../http/router/api.go#L121)、[http/controller/api/ab.go:131](../http/controller/api/ab.go#L131)、[service/tag.go:87](../service/tag.go#L87)、[service/addressBook.go:291](../service/addressBook.go#L291)。

未指名此项全部业务契约的已执行测试，不据相邻接口推导通过。

<a id="contract-24"></a>

### 24 · PUT `/api/ab/tag/rename/:guid`

API基线：`route_only`；old/new接受且空成功；未同步address_books.tags，重拉仍旧peer标签。见F1、F6

- **rename**（`source_reachable`）：new tag not already local
  - Header：A+JSON
  - 请求：{old,new}
  - 响应：AB_action
  - 错误/重试：F/R only
  - 来源：[flutter/lib/models/ab_model.dart:1805-1829](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/flutter/lib/models/ab_model.dart#L1805)

API入口/实现：[http/router/api.go:123](../http/router/api.go#L123)、[http/controller/api/ab.go:180](../http/controller/api/ab.go#L180)、[service/tag.go:96](../service/tag.go#L96)、[service/addressBook.go:291](../service/addressBook.go#L291)。

未指名此项全部业务契约的已执行测试，不据相邻接口推导通过。

<a id="contract-25"></a>

### 25 · PUT `/api/ab/tag/update/:guid`

API基线：`route_only`；name/color及空成功兼容；旧First授权分歧，读DB错误和不存在混同。见F6

- **color**（`source_reachable`）：Flutter writable AB callers
  - Header：A+JSON
  - 请求：{name,color:int}
  - 响应：AB_action
  - 错误/重试：F/R only
  - 来源：[flutter/lib/models/ab_model.dart:1834-1853](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/flutter/lib/models/ab_model.dart#L1834)

API入口/实现：[http/router/api.go:125](../http/router/api.go#L125)、[http/controller/api/ab.go:232](../http/controller/api/ab.go#L232)、[service/tag.go:96](../service/tag.go#L96)、[service/addressBook.go:291](../service/addressBook.go#L291)。

未指名此项全部业务契约的已执行测试，不据相邻接口推导通过。

<a id="contract-26"></a>

### 26 · DELETE `/api/ab/tag/:guid`

API基线：`route_only`；空成功；不移除peer.tags旧标签；旧First授权及批次事务见F1、F6、F8

- **delete**（`source_reachable`）：Flutter writable AB callers
  - Header：A+JSON
  - 请求：top-level single-element array [tag]
  - 响应：AB_action
  - 错误/重试：F/R only
  - 来源：[flutter/lib/models/ab_model.dart:1858-1874](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/flutter/lib/models/ab_model.dart#L1858)

API入口/实现：[http/router/api.go:127](../http/router/api.go#L127)、[http/controller/api/ab.go:278](../http/controller/api/ab.go#L278)、[service/tag.go:91](../service/tag.go#L91)、[service/addressBook.go:291](../service/addressBook.go#L291)。

未指名此项全部业务契约的已执行测试，不据相邻接口推导通过。

<a id="contract-27"></a>

### 27 · GET `/api/device-group/accessible`

API基线：`conditional`；普通用户也会请求；API拒绝且客户端主动忽略旧server错误，因此不阻断全group。完整accessible设备组授权语义仍未证明，不能从admin用例推论普通用户完整实现

实现/触发条件：API requires admin; official client tolerates failure

- **accessible groups**（`source_reachable`）：group panel enabled, logged in, no user network error
  - Header：A only (no JSON header due supplied map)
  - 请求：query current,pageSize
  - 响应：HTTP200 {total,data:[{name}]}; page
  - 错误/重试：error logged/tolerated as legacy compatibility, users/peers still fetched; F/R
  - 来源：[flutter/lib/models/group_model.dart:39-77](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/flutter/lib/models/group_model.dart#L39)、[flutter/lib/models/group_model.dart:103-157](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/flutter/lib/models/group_model.dart#L103)

API入口/实现：[http/router/api.go:86](../http/router/api.go#L86)、[http/controller/api/group.go:119](../http/controller/api/group.go#L119)、[internal/clientcompat/groups.go:105](../internal/clientcompat/groups.go#L105)。

既有指名测试（历史部分证据，本切片未重跑）：`http/addressbook_compat_test.go:TestClientCompatGroupListsPaginateAfterAuthorization`。

<a id="contract-28"></a>

### 28 · GET `/api/users`

API基线：`implemented`；accessible/status1/current/pageSize响应可解析；shared组仅同组、private仅自身是此fork业务规则，不声称等同Pro所有授权；昵称/头像见F7

- **accessible users**（`source_reachable`）：same groups
  - Header：A only
  - 请求：query current,pageSize,accessible:'',status:'1'
  - 响应：HTTP200 {total,data:[UserPayload]}; page
  - 错误/重试：Admin required!/ambiguous column name: status mapped upgrade tip; fails group pull;401 resets user/models; F/R
  - 来源：[flutter/lib/models/group_model.dart:160-221](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/flutter/lib/models/group_model.dart#L160)

API入口/实现：[http/router/api.go:83](../http/router/api.go#L83)、[http/controller/api/group.go:85](../http/controller/api/group.go#L85)、[internal/clientcompat/groups.go:25](../internal/clientcompat/groups.go#L25)。

既有指名测试（历史部分证据，本切片未重跑）：`http/addressbook_compat_test.go:TestClientCompatGroupListsPaginateAfterAuthorization`；`http/addressbook_compat_test.go:TestClientCompatAuthorizationDependencyFailureDoesNotExpandScope`。

<a id="contract-29"></a>

### 29 · GET `/api/peers`

API基线：`implemented`；data.info.username/os/device_name,user_name,device_group_name；当前组模型下实现，不把未定义Pro授权当新模型需求

- **accessible peers**（`source_reachable`）：same groups
  - Header：A only
  - 请求：query current,pageSize,accessible:'',status:'1'
  - 响应：HTTP200 {total,data:[{id,info,status,user,user_name,device_group_name,note}]}; info.username/os/device_name mapped to peer; page
  - 错误/重试：failure sets groupLoadError;401 resets models; F/R
  - 来源：[flutter/lib/models/group_model.dart:224-281](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/flutter/lib/models/group_model.dart#L224)、[flutter/lib/common/hbbs/hbbs.dart:77-104](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/flutter/lib/common/hbbs/hbbs.dart#L77)

API入口/实现：[http/router/api.go:84](../http/router/api.go#L84)、[http/controller/api/group.go:102](../http/controller/api/group.go#L102)、[internal/clientcompat/groups.go:67](../internal/clientcompat/groups.go#L67)。

既有指名测试（历史部分证据，本切片未重跑）：`http/addressbook_compat_test.go:TestClientCompatGroupListsPaginateAfterAuthorization`；`http/addressbook_compat_test.go:TestClientCompatAuthorizationDependencyFailureDoesNotExpandScope`。

<a id="contract-30"></a>

### 30 · POST `/api/audit/conn`

API基线：`route_only`；new/auth/close、u64、nonce、空成功已实现；旧note-only同path被显式400拒绝。见F9

兼容要点：Event and legacy note send no Bearer; local token gates legacy URL only; legacy body ignored. Server rejects legacy note-only, not covered by event acceptance.

- **new**（`source_reachable`）：controlled-side nonpublic audit URL
  - Header：R JSON; no Bearer
  - 请求：audit_conn_base + action:new,ip,optional conn_audit_ref
  - 响应：audit_R
  - 错误/重试：audit_R
  - 来源：[src/server/connection.rs:1513-1521](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/server/connection.rs#L1513)、[src/server/connection.rs:1550-1562](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/server/connection.rs#L1550)、[src/server/connection.rs:1690](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/server/connection.rs#L1690)

- **authorization**（`source_reachable`）：controlled-side accepted connection
  - Header：R JSON; no Bearer
  - 请求：audit_conn_base + peer:[id,name],type:0(remote)/1(file)/2(port/RDP)/3(camera)/4(terminal),optional primary_auth,two_factor; no action field
  - 响应：audit_R
  - 错误/重试：audit_R
  - 来源：[src/server/connection.rs:1900-1931](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/server/connection.rs#L1900)、[src/server/connection.rs:1550-1562](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/server/connection.rs#L1550)、[src/server/connection.rs:1690](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/server/connection.rs#L1690)

- **close**（`source_reachable`）：controlled-side connection loop ends
  - Header：R JSON; no Bearer
  - 请求：audit_conn_base + action:close
  - 响应：audit_R
  - 错误/重试：audit_R
  - 来源：[src/server/connection.rs:1227-1229](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/server/connection.rs#L1227)、[src/server/connection.rs:1550-1562](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/server/connection.rs#L1550)、[src/server/connection.rs:1690](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/server/connection.rs#L1690)

- **legacy/controller note**（`source_reachable`）：session audit accessor requires local token and nonpublic API; token is gate only
  - Header：R JSON; NO Bearer despite local token check
  - 请求：id(peer session target),session_id,note; no uuid/nonce
  - 响应：body ignored
  - 错误/重试：R only, not audit_R retry wrapper
  - 来源：[src/ui_session_interface.rs:580-597](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/ui_session_interface.rs#L580)、[src/ui_session_interface.rs:2086-2089](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/ui_session_interface.rs#L2086)、[src/ui/header.tis:364](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/ui/header.tis#L364)、[src/flutter_ffi.rs:2071-2073](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/flutter_ffi.rs#L2071)

API入口/实现：[http/router/api.go:68](../http/router/api.go#L68)、[http/controller/api/audit.go:71](../http/controller/api/audit.go#L71)、[http/request/api/audit.go:11](../http/request/api/audit.go#L11)、[service/audit.go:22](../service/audit.go#L22)、[internal/clientaudit/audit.go:114](../internal/clientaudit/audit.go#L114)。

既有指名测试（历史部分证据，本切片未重跑）：`internal/clientaudit/http_test.go:TestAuditHTTPPreservesFullUint64AndEmptyACK`；`internal/clientaudit/http_test.go:TestAuditHTTPMissingConnectionAndDatabaseFailureStayRetryable`；`internal/clientaudit/http_test.go:TestAuditHTTPCommitFailureDoesNotACKOrBurnNonce`；`internal/clientaudit/http_test.go:TestAuditHTTPCrossActionNonceIsConflictAndFailureDoesNotBurnReceipt`；`internal/clientaudit/http_test.go:TestAuditHTTPPropagatesInflightDeadline`；`internal/clientaudit/audit_test.go:TestCrossStoreSQLiteHandlesCommitOneReceiptAndEvent`；`internal/clientaudit/audit_test.go:TestCrashReconnectAssociatesAuthAndCloseWithLatestOpen`；`internal/clientaudit/audit_test.go:TestCloseAfterCrashBeforeAuthorizationUsesLatestZeroOpen`；`http/audit_compat_test.go:TestProductionAuditRoutesUseAssembledTransactionalWriter`。

<a id="contract-31"></a>

### 31 · POST `/api/audit/file`

API基线：`implemented`；info JSON字符串、数值大小、is_file/path适配；提交后空200，失败非2xx；未证明生产数据库/真实客户端

- **type0 RemoteSend**（`source_reachable`）：nonpublic audit; clipboard callers Windows or unix-file-copy-paste; file/remote-print callers own job gates
  - Header：R JSON; no Bearer
  - 请求：audit_file_base; type:0; clipboard path empty, transfer path populated; remote printer file label Remote print
  - 响应：audit_R
  - 错误/重试：audit_R
  - 来源：[src/server/connection.rs:927-932](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/server/connection.rs#L927)、[src/server/connection.rs:3444-3453](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/server/connection.rs#L3444)、[src/server/connection.rs:5329-5334](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/server/connection.rs#L5329)、[src/server/connection.rs:5459-5464](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/server/connection.rs#L5459)、[src/server/connection.rs:1581-1615](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/server/connection.rs#L1581)、[src/server/connection.rs:1690](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/server/connection.rs#L1690)

- **type1 RemoteReceive**（`source_reachable`）：nonpublic audit; clipboard Windows or unix-file-copy-paste
  - Header：R JSON; no Bearer
  - 请求：audit_file_base; type:1; clipboard empty path, transferred file path otherwise
  - 响应：audit_R
  - 错误/重试：audit_R
  - 来源：[src/server/connection.rs:3385-3394](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/server/connection.rs#L3385)、[src/server/connection.rs:3707-3712](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/server/connection.rs#L3707)、[src/server/connection.rs:1581-1615](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/server/connection.rs#L1581)、[src/server/connection.rs:1690](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/server/connection.rs#L1690)

API入口/实现：[http/router/api.go:70](../http/router/api.go#L70)、[http/controller/api/audit.go:91](../http/controller/api/audit.go#L91)、[http/request/api/audit.go:56](../http/request/api/audit.go#L56)、[service/audit.go:32](../service/audit.go#L32)、[internal/clientaudit/audit.go:231](../internal/clientaudit/audit.go#L231)。

既有指名测试（历史部分证据，本切片未重跑）：`internal/clientaudit/http_test.go:TestAuditHTTPFileRetryAndConflict`；`internal/clientaudit/http_test.go:TestAuditHTTPReceiptPreservesOriginalFileSemantics`；`internal/clientaudit/audit_test.go:TestConcurrentRetriesStoreOneOpenAndFilePerWriter`；`internal/clientaudit/audit_test.go:TestFailedAuditWritesDoNotBurnNonceAndRetrySucceeds`；`internal/clientaudit/audit_test.go:TestReceiptsSurviveBusinessChangesAndDeletionWithoutRecreatingRows`；`http/audit_compat_test.go:TestProductionAuditRoutesUseAssembledTransactionalWriter`。

<a id="contract-32"></a>

### 32 · POST `/api/audit/alarm`

API基线：`missing`；typ/info字符串、conn_id/conn_audit_ref；客户端post_audit_async要求2xx空响应

- **typ0 IP whitelist**（`source_reachable`）：nonpublic audit, blocked IP
  - Header：R JSON; no Bearer
  - 请求：audit_alarm_base; typ0; info{ip}; optional conn_audit_ref
  - 响应：audit_R
  - 错误/重试：audit_R
  - 来源：[src/server/connection.rs:1430-1433](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/server/connection.rs#L1430)、[src/server/connection.rs:1618-1641](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/server/connection.rs#L1618)、[src/server/connection.rs:1690](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/server/connection.rs#L1690)

- **typ1 >30 attempts**（`source_reachable`）：nonpublic audit and threshold
  - Header：R JSON; no Bearer
  - 请求：audit_alarm_base; typ1; info{ip,id,name}
  - 响应：audit_R
  - 错误/重试：audit_R
  - 来源：[src/server/connection.rs:4500-4507](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/server/connection.rs#L4500)、[src/server/connection.rs:1618-1641](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/server/connection.rs#L1618)、[src/server/connection.rs:1690](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/server/connection.rs#L1690)

- **typ2 six/minute**（`source_reachable`）：nonpublic audit and threshold
  - Header：R JSON; no Bearer
  - 请求：audit_alarm_base; typ2; info{ip,id,name}
  - 响应：audit_R
  - 错误/重试：audit_R
  - 来源：[src/server/connection.rs:4511-4518](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/server/connection.rs#L4511)、[src/server/connection.rs:1618-1641](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/server/connection.rs#L1618)、[src/server/connection.rs:1690](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/server/connection.rs#L1690)

- **typ6 IPv6 prefix attempts**（`source_reachable`）：nonpublic audit, IPv6 prefix threshold
  - Header：R JSON; no Bearer
  - 请求：audit_alarm_base; typ6; info{ip,id,name}
  - 响应：audit_R
  - 错误/重试：audit_R
  - 来源：[src/server/connection.rs:4418-4425](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/server/connection.rs#L4418)、[src/server/connection.rs:1618-1641](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/server/connection.rs#L1618)、[src/server/connection.rs:1690](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/server/connection.rs#L1690)

- **typ7 terminal OS login backoff**（`source_reachable`）：nonpublic audit; TerminalOsLogin failure scope
  - Header：R JSON; no Bearer
  - 请求：audit_alarm_base; typ7; info{ip,id,name}
  - 响应：audit_R
  - 错误/重试：audit_R
  - 来源：[src/server/login_failure_check.rs:56-59](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/server/login_failure_check.rs#L56)、[src/server/connection.rs:4460-4470](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/server/connection.rs#L4460)、[src/server/connection.rs:1618-1641](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/server/connection.rs#L1618)、[src/server/connection.rs:1690](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/server/connection.rs#L1690)

- **typ8 terminal OS login concurrency**（`source_reachable`）：Windows terminal concurrent-login guard; nonpublic audit
  - Header：R JSON; no Bearer
  - 请求：audit_alarm_base; typ8; info{ip,id,name}
  - 响应：audit_R
  - 错误/重试：audit_R
  - 来源：[src/server/connection.rs:4230-4237](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/server/connection.rs#L4230)、[src/server/connection.rs:1618-1641](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/server/connection.rs#L1618)、[src/server/connection.rs:1690](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/server/connection.rs#L1690)

- **typ9 session scope violation**（`source_reachable`）：nonpublic audit, scope violation
  - Header：R JSON; no Bearer
  - 请求：audit_alarm_base; typ9; info{id,name,ip,conn_type,message}
  - 响应：audit_R
  - 错误/重试：audit_R
  - 来源：[src/server/connection.rs:1644-1658](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/server/connection.rs#L1644)、[src/server/connection.rs:5774](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/server/connection.rs#L5774)、[src/server/connection.rs:1618-1641](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/server/connection.rs#L1618)、[src/server/connection.rs:1690](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/server/connection.rs#L1690)

- **typ10 ID whitelist**（`source_reachable`）：nonpublic audit, ID whitelist rejects
  - Header：R JSON; no Bearer
  - 请求：audit_alarm_base; typ10; info{id,ip,name}; optional conn_audit_ref
  - 响应：audit_R
  - 错误/重试：audit_R
  - 来源：[src/server/connection.rs:1462-1465](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/server/connection.rs#L1462)、[src/server/connection.rs:1618-1641](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/server/connection.rs#L1618)、[src/server/connection.rs:1690](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/server/connection.rs#L1690)

API入口/实现：[http/router/api.go:14-130 (route absent)](../http/router/api.go#L14)。

未指名此项全部业务契约的已执行测试，不据相邻接口推导通过。

<a id="contract-33"></a>

### 33 · GET `/api/audit/conn/active`

API基线：`missing`；id/session_id u64/conn_type→JSON guid字符串或null；end-note feature触发

实现/触发条件：allow-ask-for-note-at-end-of-connection

- **query GUID**（`source_reachable`）：Flutter account enabled, nonempty token/nonpublic API, allow-ask-for-note-at-end-of-connection true, no cached GUID
  - Header：F+Bearer+JSON
  - 请求：query id=peerId,session_id,conn_type=0/1/2/3/4; RDP2
  - 响应：HTTP200 JSON nullable string GUID
  - 错误/重试：6 attempts on empty GUID/exception; delays1,1,2,2,3 seconds; non200 stops; session ID change stops
  - 来源：[flutter/lib/models/model.dart:1247-1325](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/flutter/lib/models/model.dart#L1247)、[src/ui_session_interface.rs:580-588](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/ui_session_interface.rs#L580)

API入口/实现：[http/router/api.go:14-130 (route absent)](../http/router/api.go#L14)。

未指名此项全部业务契约的已执行测试，不据相邻接口推导通过。

<a id="contract-34"></a>

### 34 · PUT `/api/audit`

API基线：`missing`；guid/note→HTTP200；与old note-only POST变体是两条调用链 结束弹窗要求GUID非空；只看HTTP200且不检查错误body；不存在自动fallback到旧POST。

实现/触发条件：end-of-connection note dialog

兼容要点：Nonempty GUID required; only HTTP200 checked, no JSON error parsing. Legacy POST note separate, not fallback.

- **end-note by GUID**（`source_reachable`）：allow-ask-for-note-at-end-of-connection true, nonempty audit server/token, cached GUID, last audit note empty; controlling-side close must actually close session; only nonempty note is submitted.
  - Header：F+Bearer+JSON
  - 请求：guid,note
  - 响应：only HTTP200 checked; body ignored including error JSON
  - 错误/重试：Only status200 considered success, response JSON ignored. Log-only failures; caller6s timeout does not cancel request; F/R only. No legacy fallback from this dialog when GUID missing.
  - 来源：[flutter/lib/common/widgets/dialog.dart:1637-1680](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/flutter/lib/common/widgets/dialog.dart#L1637)、[flutter/lib/common/widgets/dialog.dart:1700-1725](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/flutter/lib/common/widgets/dialog.dart#L1700)、[flutter/lib/common/widgets/dialog.dart:1590-1601](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/flutter/lib/common/widgets/dialog.dart#L1590)

API入口/实现：[http/router/api.go:14-130 (route absent)](../http/router/api.go#L14)。

未指名此项全部业务契约的已执行测试，不据相邻接口推导通过。

<a id="contract-35"></a>

### 35 · POST `/api/record`

API基线：`conditional`；接口缺失，但ENABLE默认false且整份src未找到写true。库存latent合同，不能算当前普通客户端必需请求；new/part/tail/remove及offset/length二进制流，不可用multipart代替

实现/触发条件：record_upload ENABLE default false; no enabling writer found

兼容要点：Private ENABLE defaults false, no writer in src, unique run gated by is_enable; retained/statically_unreachable_current_tree.

- **type=new**（`statically_disabled_private_flag_false`）：ENABLE private false; no writer found in source; actual stock emission not proven
  - Header：none explicit; no Bearer/JSON
  - 请求：query type=new,file=basename; empty bytes
  - 响应：if body parses map with error then failure; otherwise succeeds regardless HTTP status or nonJSON
  - 错误/重试：failure stops uploader; no retry
  - 来源：[src/hbbs_http/record_upload.rs:20-24](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/hbbs_http/record_upload.rs#L20)、[src/server/video_service.rs:1154-1156](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/server/video_service.rs#L1154)、[src/hbbs_http/record_upload.rs:93-114](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/hbbs_http/record_upload.rs#L93)、[src/hbbs_http/record_upload.rs:126](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/hbbs_http/record_upload.rs#L126)

- **type=part**（`statically_disabled_private_flag_false`）：same dormant ENABLE; if activated at least1s and >=1MiB delta unless flush
  - Header：none explicit
  - 请求：query type=part,file,offset,length; binary file delta
  - 响应：same record_new
  - 错误/重试：same record_new
  - 来源：[src/hbbs_http/record_upload.rs:93-114](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/hbbs_http/record_upload.rs#L93)、[src/hbbs_http/record_upload.rs:135-174](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/hbbs_http/record_upload.rs#L135)、[src/server/video_service.rs:1154-1156](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/server/video_service.rs#L1154)

- **type=tail**（`statically_disabled_private_flag_false`）：same dormant ENABLE; flush remaining part first
  - Header：none explicit
  - 请求：query type=tail,file,offset=0,length; first <=1024 bytes
  - 响应：same record_new
  - 错误/重试：same record_new
  - 来源：[src/hbbs_http/record_upload.rs:93-114](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/hbbs_http/record_upload.rs#L93)、[src/hbbs_http/record_upload.rs:177-200](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/hbbs_http/record_upload.rs#L177)、[src/server/video_service.rs:1154-1156](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/server/video_service.rs#L1154)

- **type=remove**（`statically_disabled_private_flag_false`）：same dormant ENABLE
  - Header：none explicit
  - 请求：query type=remove,file; empty bytes
  - 响应：same record_new
  - 错误/重试：same record_new
  - 来源：[src/hbbs_http/record_upload.rs:93-114](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/hbbs_http/record_upload.rs#L93)、[src/hbbs_http/record_upload.rs:204-208](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/hbbs_http/record_upload.rs#L204)、[src/server/video_service.rs:1154-1156](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/server/video_service.rs#L1154)

API入口/实现：[http/router/api.go:14-130 (route absent)](../http/router/api.go#L14)。

未指名此项全部业务契约的已执行测试，不据相邻接口推导通过。

<a id="contract-36"></a>

### 36 · POST `/api/ab/get`

API基线：`missing`；Sciter address-book read, body {}; expects top-level data JSON string, then JSON.parse(data.data). GET /api/ab cannot satisfy a POST to /api/ab/get. First-pass independent matrix omitted this source path; correction is based on direct fixed-commit source inspection.

实现/触发条件：Sciter desktop build/UI

兼容要点：Preserved Sciter emits POST /ab/get, not interchangeable with Flutter GET /ab.

- **legacy snapshot Sciter**（`source_reachable_preserved_sciter`）：Sciter AB
  - Header：S+Bearer
  - 请求：{}
  - 响应：data JSON string,updated_at read; tags/peers default empty
  - 错误/重试：S/R, body error sets AB error
  - 来源：[src/ui/ab.tis:646-665](https://github.com/rustdesk/rustdesk/blob/fada664df7a294d1d1a9ca3e7cd3637069122f17/src/ui/ab.tis#L646)

API入口/实现：[http/router/api.go:14-130 (POST /api/ab/get absent; GET /api/ab is distinct)](../http/router/api.go#L14)。

未指名此项全部业务契约的已执行测试，不据相邻接口推导通过。

## 静态差距索引（固定b59f7a5）

下列F编号仅解释上文基线注记，不预判并行权限/标签补丁已完成；新增测试与状态变化应另行记录。

**F1 · Tag rename/delete successful response leaves peer memberships unchanged**：PUT tag/rename仅TagService.Update(tags.name)，DELETE tag仅Delete(tags row)。客户端Pro路径只调用一次动作然后pullNonLegacyAfterChange；不会另发peer patch来修正成员关系。LegacyAb实现会同步peer.tags，提供同客户端预期的旁证。 影响：将tag A改为B后，重新拉取tags得到B而peers仍带A；按B筛选丢设备，删除后孤立旧标签残留。

**F2 · Heartbeat policy slice is implemented, full response contract is not**：客户端POST包含conns，并处理sysinfo/disconnect/modified_at/strategy；PeerInfoInHeartbeat没有conns；Index.Heartbeat只输出strategy/modified_at或{}。clientstrategy有id+uuid隔离、事务、历史managed keys、删除回退及空对象策略处理。 影响：策略下发不能证明连接上报/远程断开/强制sysinfo重报功能存在。{}

**F3 · POST login supports password account, not client multi-stage verification**：官方login.dart实际可发type=email_code、verificationCode/tfaCode/secret且省略password；LoginForm没有这三项，Login始终username/password并固定access_token响应。type字段存在却不dispatch。 补核Sciter index.tis:1588-1599也确实发送type=email_code、verificationCode、tfaCode、secret，并非只有Dart DTO常量。 影响：若要支持客户端email_check/TFA后续验证，该同path能力尚未实现；当前服务没有启用这套challenge流程的证据，不据此声称当前账号被绕过。

**F4 · Sysinfo presets are partly ignored and side effects can be acknowledged before storage**：客户端可附preset-address-book-tag/password/note、preset-username/strategy-name/device-group-name、preset-note；PeerForm只有name/alias两个preset字段。ApplyPresetToAddressBook固定user_id=1，DB失败静默，外层仍SYSINFO_UPDATED。 影响：成功文本能让客户端认为本轮sysinfo已传完，但不能证明预设归属/标签/密码/备注/策略/组已落库。owner1还使同名非owner1地址簿不可自动匹配。

**F5 · Sysinfo empty username cannot clear stored value**：sync.rs明确Windows用户名可在登录/登出时变化并触发重新上报；PeerService.Update使用GORM struct Updates(fpe)，会省略零值。 影响：username从非空变空时请求成功，数据库仍留旧用户；hostname等允许清空的字段有同类风险。此处只是HTTP写入映射审查。

**F6 · Read/profile MAX permissions disagree with tag/delete legacy First permissions**：Store.Profiles及scope按MAX(rule)得到有效权限；tag add/rename/update/delete和PeerDel仍走AddressBookService.UserMaxRule，每类规则First后比较，没有MAX或排序。 影响：同一资源有多个匹配规则时profile显示fullControl但tag/delete可拒绝；已存在MAX权限读测试没有覆盖这些旧写handler。这里指出现有实现内冲突，未规定新共享模型。

**F7 · User projection drops supported display name/avatar**：UserPayload.fromJson消费display_name/avatar，_parseAndUpdateUser在每次refresh覆盖本地值；ClientCurrent与api.UserPayload.FromUser没有投影这两项，尽管auth.User/model.User已有Nickname/Avatar。 影响：昵称/头像在客户端显示为空或刷新清空；JSON仍可解析，所以属于语义不完整而非解析报错。

**F8 · Legacy batch delete can partially commit before returning one error**：PeerDel/TagDel按数组逐个查询并删除，无事务；后一个不存在或DB错误时直接error返回，前一项已删除。TagService读取还不向caller返回DB错误。 影响：客户端只有整次布尔结果，无法获知已完成部分。是否要求原子性要确认；报告为事务风险/测试缺口，不凭客户端源码虚构服务端原子性协议。

**F9 · Same POST audit/conn path has implemented events but rejected legacy notes**：Session.send_note仍构造/api/audit/conn，发{id,session_id,note}无Bearer；AuditConn显式form.Note!=nil→400。new/auth/close的事务receipt、u64 session_id和空200不是这条变体的证据。 独立入口showAuditDialog在dialog.dart:1567调用sessionSendNote；Sciter src/ui/header.tis:364也调用handler.send_note。get_audit_server仅检查本地token非空才生成URL，send_note实际POST第三参为空，因此网络请求无Bearer。GUID结束备注入口要求GUID非空，不会自动回退旧POST。 影响：旧备注调用失败；新增GET active和PUT audit亦未实现，故两种备注路径都未完整支持。

## 未冻结与实际验收边界

- account/email_code/TOTP、心跳conns/sysinfo/disconnect、组/标签权限和派生引用等差距仍需逐变体验证。后续权限/标签补丁不在本基线预先标通过。
- 09公共URL条件与外层过滤冲突；35私有ENABLE默认false且无writer。保留实现不能当作标准客户端正常发出的请求。
- 2026-10-19官方1.5.0 Ubuntu OIDC登录/Account及My address book基本功能已由真实客户端/用户确认；不能扩大成备注缺失保留/清空、千条分页、策略实际应用、真实远控审计或每个HTTP变体通过。没有用真实客户端logout验证撤销。
- 本切片仅同步文档/JSON。历史Go/SQLite及MySQL/PostgreSQL证据见 [回归矩阵](client-compatibility.md) 和 [数据库验证](database-integration.md)。本次未执行Go、DB、GUI、CI，不宣称新提交36项验收或生产恢复演练完成。
