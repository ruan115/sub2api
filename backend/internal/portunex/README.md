# Portunex recovery domain

本目录按业务模块恢复Portunex能力。当前可运行入口是**独立的合成本地演示**，不是原Rust服务的替代品，也没有挂到现有Sub2API生产router或wire。

- `identity/`：固定合成身份、随机opaque session、TTL/撤销/容量限制。只保存token摘要；公开的演示密码比较逻辑不得用于真实密码存储。
- [`identity/password/`](identity/password/README.md)：独立本地 Argon2id PHC 验证能力，显式参数/资源预算和并发取消边界。未接入演示或旧路由，不等于已确认旧生产密码算法。
- [`identity/repository/postgres/`](identity/repository/postgres/README.md)、[`users/repository/postgres/`](users/repository/postgres/README.md)：固定恢复 schema 的会话存储原语和用户读取；非旧认证/HTTP DTO。
- [`migrations/`](migrations/README.md)、[`platform/postgres/testcluster/`](platform/postgres/testcluster/README.md)：三表合成恢复库迁移与显式 tag 的私有 PostgreSQL 测试集群，普通入口不启动数据库。
- `users/`、`apikeys/`、`providers/`：各自拥有列表模型/查询，Key仅脱敏提示，Provider没有凭据字段。
- `platform/listing/`：搜索与有界分页；`platform/namespace/`：ID/cache/pubsub/lock隔离命名。
- `app/`：有限装配、专用演示HTTP协议与Host/Origin/peer边界、跨模块集成测试。

数据库迁移和上述 repository 已通过独立 PostgreSQL 18.6 合成测试，但未接到演示/生产入口。
Redis客户端、真实认证/账务/Provider调用尚未实现；数据库验证也不等于真实缓存、后台锁或完整旧业务已隔离/兼容。

## 启动和测试

从仓库根目录：

```sh
cd backend
go test -race ./internal/portunex/... ./cmd/portunex-demo
go vet ./internal/portunex/... ./cmd/portunex-demo
go run ./cmd/portunex-demo
```

要求backend已有的Go1.26.6工具链。仅监听 `127.0.0.1:18090`，可用 `-port` 改本地端口，不能更换绑定地址。没有生产DSN、Redis地址、OAuth、账号导入或上游配置项。Ctrl+C会关闭服务，内存数据和session随进程结束消失。

与 `portunex-web/` 联调时，前端必须明确设置 `VITE_RECOVERY_BACKEND=go` 并经Vite的固定loopback代理请求；默认前端仍为mock。代理保留原始Host/Origin，不开放CORS，不读取Forwarded头来判断是否本地。

## 专用演示API

前缀为 `/__recovery__/v1`，不是旧 `/portunex` 接口。

| 操作 | 方法/路径 | 数据 |
| --- | --- | --- |
| 登录 | POST `/login` | JSON `{email,password}` → `{user,expiresAt}` + HttpOnly SameSiteStrict cookie |
| 当前会话 | GET `/session` | `{user:{id,email,displayName,role},expiresAt}`，无session返回401 |
| 登出 | POST `/logout` | 204，撤销session并清cookie；重复操作仍204 |
| 用户/Key/Provider | GET `/users`、`/api-keys`、`/providers` | `query/page/page_size` → `{items,total,page,pageSize}` |

错误结构固定 `{error:{code,message}}`，不会输出请求、密码、token或内部错误细节。响应含 `X-Recovery-Mode: synthetic` 和 `Cache-Control: no-store`。

普通成员无users/providers权限，仅能取自己的Key；owner过滤先于搜索/分页/total。列表只读，未知query/重复参数/无效页码会拒绝。此演示没有生产限流器或真实账号密码验证，不能暴露到公网，也不要通过外部隧道转发。

公开的合成演示登录：

- 管理员：`admin@example.invalid` / `Demo-admin-2026!`
- 成员：`member@example.invalid` / `Demo-member-2026!`

以上不是线上账户。旧接口字段/分页/鉴权仍需从已授权资料补证据；不得导入真实用户或把这里的SHA-256 fixture比较器当生产密码哈希方案。未来接真实Key时需要受限脱敏构造入口，不能直接将真实Key填进字符串`KeyHint`。
