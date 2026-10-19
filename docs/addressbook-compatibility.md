# 地址簿兼容切片

固定客户端提交 `53996e3c404ea5083df0775057e4149e49102084`。客户端列表使用
`current/pageSize`，服务端按物理主键稳定分页，返回权限过滤后的真实 total；
不再把个人地址簿、共享 profiles 或 accessible 列表截断为 1000 条。

新增 `address_books.note`：每行仍是原有的所属用户、地址簿和设备记录，
沿用原有共享读写权限。新增字段缺失时保留，空串明确清除；旧客户端整本
替换和后台旧表单更新保留未提供的备注。设备 wire DTO 保持
`forceAlwaysRelay` 为字符串、`same_server` 为布尔值。

数据库版本 276 先为已有表增加可空 TEXT 列、只把 NULL 补为空串，再收紧
NOT NULL。没有 SQL TEXT DEFAULT；应用的 GORM model 创建会明确写入字符串
零值。重试同样补齐半完成列中的 NULL，不覆盖已有备注或更新时间。
SQLite schema 和版本同事务；MySQL DDL 仍可能部分生效，失败不发布版本，
下次启动重试。不得把 SQLite 测试视为 MySQL/PostgreSQL 验证。

定向测试使用真实 HTTP handler 和 SQLite，覆盖多页总量、权限、缺失与清空、
旧整本替换、事务失败，以及 275 升级、半完成列重试和版本失败回滚。
真实 Rust/Flutter 客户端与 MySQL/PostgreSQL 升级仍待联调。
