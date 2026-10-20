# 命名策略：定义管理与设备关联

PRE02-C交付命名定义表和管理员CRUD（版本280）；PRE02-D按另节批准的模型增加设备关联、CLI绑定/解绑及心跳合并（版本281）。自动化验证不代表真实客户端已应用配置。

一行表示一个全局命名配置，只有三个字段：

| 字段 | 规则 |
| --- | --- |
| id | 整数自增主键 |
| name | 非空、有效UTF-8、最多255字节，按字节精确唯一；不trim、大小写折叠或Unicode归一化 |
| config_options | 非NULL JSON对象，非空字符串键映射到字符串值，允许空对象与空值 |

不加owner、时间、审计、配置副本或关联历史。数据库版本280只增加定义表，旧设备不推断关联，不修改现有默认/单设备策略。版本281另增peers可空外键及非唯一索引，不改定义三字段。

为保证三个数据库都能保存完整的已批准UTF-8名称范围，物理name采用MySQL VARBINARY(255)、PostgreSQL BYTEA、SQLite BLOB；实例Store绑定二进制值，HTTP仍收发JSON字符串。大小写、重音、尾空格、NFC/NFD和嵌入NUL可区分。直接SQL应使用二进制参数；SQLite有存储类型检查，不能混用TEXT绕过唯一语义。表不进入常规AutoMigrate，后续升级不能先转回默认文本排序规则。

数据库保证主键、name唯一和非NULL；UTF-8/255字节、非空名称及配置对象/键/值校验由Store执行。直接SQL写入坏配置不会被读端静默当作空对象。

## 管理API

使用既有 `api-token` 本地管理员会话，不使用请求体中的owner或角色。中间件校验后，Store在同一事务内重验当前启用用户、管理员角色、唯一会话和有效截止时间；锁序为user→session→definition。定义操作不遍历或锁定设备。

| 方法与路径 | 输入 / 成功data |
| --- | --- |
| GET /api/admin/strategy-definitions | current默认1、pageSize默认20，最大1000；返回list与total |
| POST /api/admin/strategy-definitions | 完整name、config_options；返回新定义 |
| GET /api/admin/strategy-definitions/:id | 返回指定定义 |
| PUT /api/admin/strategy-definitions/:id | 完整name、config_options，全量替换；返回更新定义 |
| DELETE /api/admin/strategy-definitions/:id | 删除定义 |

成功HTTP200/code0；业务参数400、不存在404、重名409、存储503，取消408/超时504，均使用固定安全管理封套。认证失败沿现有管理API封套。创建不能提供id；更新若提供body id必须与路径相同。未知/重复字段、null、错误类型、重复配置键、尾随JSON及非法分页拒绝，正文限1MiB。未接入legacy通用请求体OperationLog，避免配置值进入该日志。

## 持续关联和配置合并

用户已确认设备持续关联一个全局定义，定义更新下次心跳生效，优先级为现有默认→命名定义→单设备配置→既有真实预设。strategy_name缺失保留、空串解绑；重命名保留关联，引用中禁止删除。设备外键模型已确认：peers.strategy_definition_id可空FK→strategy_definitions.id，一设备0/1定义，多设备可共享；旧行NULL不推断，引用中拒删，不复制name/config、不增关联表/历史。PRE02-D已按此落地，定义仍保持三字段。

CLI先按字节精确名称选择定义并检查严格配置，与本次归属/簿/资料写同一事务；失败无部分成功。心跳每次读取关联定义，复用既有最终配置摘要revision和managed_keys，定义更新/删键/解绑按当前有效fallback或历史显式空串处理，不按时间戳推测新旧、不复制定义。改名不改变ID关联；没有配置变更时revision保持。

删除定义在READ COMMITTED事务内于锁父定义前、后各做一次普通引用检查，不锁peer，减少与CLI/心跳peer→definition锁序的冲突。数据库RESTRICT仍是最终完整性保证；不声称未协调的所有SQL writer无死锁，遇到原生序列化/死锁错误应整事务失败后重试。

## 验收边界

本批未部署，未执行真实官方1.5.0命名策略分配/心跳应用/更新/解绑验收。先前普通OIDC与基本My address book手工通过范围保持。定义CRUD、数据库迁移及并发测试是API/存储证据，不是客户端已应用的证据。
