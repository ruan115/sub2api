# 用户读取存储原语

本模块仅访问新建的 `portunex_identity.users`，不访问生产库，不启动服务，
不提供旧 `/portunex/users/me` DTO、密码认证或角色授权。

- `New(*sql.DB)`：调用方提供已明确隔离的连接；不读取环境、不自行连接/迁移。
- `FindByID`、`FindByEmail`：只读取 `deleted_at IS NULL`，不存在返回 `ErrNotFound`。
  邮箱参数不 trim/lowercase；使用显式 `public.citext` 和其 equality operator，
  保留数据库比较语义，不依赖 `public` 是否在 search_path。
- `Record` 保留全部列的 NULL 状态、`int64` ID 和 exact decimal；不是可以直接序列化的 HTTP DTO。
  `PasswordPHC` 不参与 JSON 序列化，但调用方仍不得记录整个 Record 或 PHC。
  有限 NUMERIC 值以 decimal 无损读取；PostgreSQL 特殊值 `NaN` 不受当前 decimal 库支持，
  读取时安全失败，不转换为零。本切片不据此增加数据库约束或宣称覆盖全部旧业务值。
- 数据库/扫描错误只返回固定 sentinel 或 `context.Canceled` / `DeadlineExceeded`，
  不包装底层错误，不回显邮箱、PHC、查询或连接细节。两个 repository 的 sentinel
  独立定义，业务层应分别映射；不得把所有类别直接转换成旧 HTTP 错误格式。

普通测试使用 sqlmock，不启动数据库。`portunex_integration` tag 使用平台 helper
新建的本地私有集群和合成数据，显式事务应用恢复迁移；未提供隔离运行时则失败，
不会回退到现有/生产数据库。
