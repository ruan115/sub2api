# 会话存储原语

本模块仅访问新建的 `portunex_identity.auth_sessions`。它不是旧登录、Bearer
中间件或 logout 实现，未接入 demo，也不自行连接、迁移、生成 ID/token 或设置 TTL。

- `Create`：调用方明确传入 ID、用户 ID、**数据库存储 token 值**、到期时间、创建时间，
  以及可 NULL 的 IP/UA；last_used_at 与创建时间一致，对应已观察到的 INSERT SQL。
- `FindActiveByStorageToken`：仅使用观察到的 token 相等、`expires_at > NOW()`、
  `deleted_at IS NULL` 条件，不续期、不 touch、不校验用户状态。这是存储读取，
  绝不能当作已完成认证或授权。
- `Touch`：显式写 last_used_at，只要求尚未删除，不增加过期策略。
- `Revoke`：恢复项目新增的单行软删除原语，时间显式传入；**不证明旧 logout 的撤销范围**。
  已删除/不存在返回 `ErrNotFound`。不实现批量清理、10 会话上限或任何推断出的调用顺序。

`StorageToken` 仅标记数据库值；尚不清楚它与外部 Bearer credential 之间是否存在变换。
不要传入它就宣称旧服务明文存储。普通格式化做脱敏、Record 不序列化 token，
但不防止显式类型转换/其他反射；调用方仍不得打印 Record 或凭据。

ID 为 int64，可空字段保持 SQL NULL，timestamptz 保持时间点。所有参数均绑定到固定
schema-qualified SQL；不从参数拼接表名/值。错误只返回固定 sentinel 或已知 context
错误，不包装/暴露底层 pq/Scan 错误中的 token、PHC、邮箱或连接细节。
超时/取消或连接错误不是写入未发生的证明；调用方不可未经状态核实就重试创建。

普通 sqlmock 测试不启动数据库。`portunex_integration` tag 的真实 PostgreSQL 测试
使用 `testcluster.Start` 自建私有临时集群，显式事务加载合成迁移；仅写合成用户/会话，
不读取真实账户，也不能用环境 DSN 指向现有数据库。
