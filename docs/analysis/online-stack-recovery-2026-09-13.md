# 线上整套系统恢复：分析基线

日期：2026-09-13。状态：规划级分析完成，行为恢复尚未完成。

用户已确认：范围包含 Portunex 后端、管理网页、数据库业务和 isthmus；第一版优先让现有调用方不改 HTTP/WebSocket/gRPC 接口。

## 1. 最重要的结论

这不是“把一个 Go 服务重新编译出来”，而是三类恢复工作：

| 层 | 线上实证 | 可恢复内容 | 尚不能承诺 |
| --- | --- | --- | --- |
| Portunex 后端 | stripped Rust ELF，Axum/SQLx/Tonic 标记 | 可执行基线、接口/模块/SQL线索、发布骨架 | 原 Rust 源码、类型、Cargo.lock、全部业务算法 |
| Portunex 网页 | React 19.2.1、React Router，生产 JS/CSS | 页面资源、路由、交互与 API 调用契约 | 原 TS/TSX、组件目录、完整依赖锁与测试 |
| isthmus | Bun 编译 ELF，内嵌 JS/bytecode | 已提取业务 JS、Messages proto、worker、Shell/Dockerfile | 原 TS 类型、全部原名、原仓库历史 |
| 业务数据库 | PostgreSQL 18.6，36 表/387 列/28 外键 | 当前结构、迁移清单、数据模型 | 尚未核验历史记录完整性、账务一致性或备份可恢复性 |

线上库和物理数据目录仍存在；旧电脑源码遗失，不代表线上业务记录必然全部丢失。但本轮没有读取业务行，不能据此保证历史数据完整。

## 2. 检查边界与证据等级

- **LIVE-META**：通过 SSH 只读检查文件哈希、容器/镜像元数据和数据库结构。
- **STATIC**：从编译产物、脚本、前端 manifest 得到的实现线索。
- **LOCAL-CODE**：当前分支源码和测试结果。
- **UNVERIFIED**：需要后续受控 fixture、隔离运行或真实 canary 验证的行为。

静态存在不代表生产启用；库测试通过不代表进程组合或上线完成。下文不把这些证据等级互相替代。

本轮没有读取用户记录、密码/Token、环境配置内容、私钥或生产请求正文；没有运行提取的程序，没有部署、重启或数据库迁移。数据库查询使用 `BEGIN READ ONLY`，服务端返回 `transaction_read_only=on`。

## 3. 线上部署

只读快照看到 77 个运行中的 isthmus Docker 容器，以及 Portunex green/blue、Caddy、Traefik、PostgreSQL 共 5 个容器。容器 running 不等于每个业务实例都已健康。

### Portunex 后端

- 部署路径：服务器 `/opt/gateway/bin/portunex-server`。
- 大小：45,810,904 字节；Linux x86_64 静态 stripped ELF。
- SHA-256：`5097f64d5042f1e43178df14decfb94ead48e96c16a4fae15bfc1fea14d7014a`。
- green/blue 运行主二进制哈希均与该文件一致；镜像创建时间不同不代表运行主程序不同。
- 没有 Go buildinfo、`.gopclntab`、`.symtab`、`.debug_*` 或 `.bun`。
- 存在 Rust/Axum/SQLx/Tonic 标记和 149 个应用 `.rs` 路径：routes 36、services 55、repositories 32、handlers 8、utils 17、models 1。
- 存在 118 个 CREATE TABLE、57 个 ALTER TABLE 字符串标记；这些不是完整、按序、可直接执行的迁移脚本。

`/opt/gateway/server.zip` 是部署输出，不含 `.rs/.go`、Cargo/go manifest。`public/gateway.tar` 是部署目录快照，不是源码仓库或 Docker image archive，也没有发现 `.git/src/Cargo.toml/go.mod`。

### Portunex 网页

- Caddy 静态 bind：服务器 `/opt/gateway/public` → 容器 `/usr/share/caddy`。
- 150 个静态文件，其中 135 个 JS、2 个 CSS；JS+CSS 合计 3,115,051 字节。
- `web.zip` 大小 2,496,248 字节；未发现应用 source map 或源码工程。
- 路由证据：服务器 `/opt/gateway/public/assets/manifest-c2858866.js:1`，23 条 route 记录。
- 页面涵盖首页、dashboard、用户/API Key/Provider 及统计、用量、请求追踪、模型别名及定价、Provider 类型价格、窗口配置、密码/magic-link/OAuth 登录、OIDC consent。

已观察的页面路径：

```text
/
/dashboard
/dashboard/api-keys                  /dashboard/api-keys/:id/stats
/dashboard/users                     /dashboard/users/:id/stats
/dashboard/providers                 /dashboard/providers/:id/stats
/dashboard/logs
/dashboard/request-logs              /dashboard/request-logs/:traceId
/dashboard/provider-type-pricing
/dashboard/window-configs
/dashboard/model-aliases
/dashboard/model-alias-pricing
/dashboard/pricing
/auth                                /auth/magic
/auth/oauth/callback
/oidc/authorize                       /403
```

23 是 manifest 记录数，不等于 23 个不同 URL，不能用它推导完整后端路由数量。

已观察的 API 分组：

| 分组 | 路径线索 |
| --- | --- |
| 登录/用户 | `/portunex/auth/*`、`/portunex/users/me`、`/portunex/oauth/*` |
| 用户 API Key | `/portunex/api-keys`，含 rotate/settings/stats |
| 管理用户/Key/session | `/portunex/admin/users`、`api-keys`、`sessions` |
| Provider 与授权 | `/portunex/admin/providers`、`/portunex/admin/oauth/{claude,openai,gemini,antigravity}/*` |
| 用量/追踪 | `/portunex/admin/{usage,stats,request-logs}`、用户侧对应接口 |
| 定价/窗口 | `/portunex/admin/{window-configs,model-aliases,model-alias-pricings,provider-type-pricings}` |
| 用户价格 | `/portunex/pricing/models` |
| OIDC | 已观察 `/oidc/authorize/approve`；完整 discovery/token/userinfo/JWKS 等须后续核实 |
| 模型消息 | 前端引用 `/v1/messages`、`/v1/models`；不能据此排除其他模型协议 |

前端只证明调用契约线索，不证明服务端鉴权、账务事务、限流算法和所有路径。后端字符串还提示 OpenAI/Claude/Gemini 等转发，完整协议清单必须继续冻结。

### isthmus

先前提取材料位于本机仓库外的私有目录：
`/Users/ruanyang/My-project/api/z/isthmus-static-analysis.HjfIFn/`。

- 原始 JS：1,050,518 字节，SHA-256 `8a3f86d446e52785d9361644e49ef8cabba9d820f31cbaf1c2c1da625996f92a`。
- 可读版：34,548 行；它是静态转换的审阅副本，不是可直接部署项目。
- 完整 `isthmus.v1.Messages` proto：Create / CreateStream，7 个 message。
- 独立 worker：17,527 字节。
- 已取得 Dockerfile 和 18 个 Shell 脚本。
- 基础镜像 ID：`sha256:07133e34395851570153cee759bf42d247b28a9401a53f2ab02f38171f0fcc94`。
- 本次复核 Dockerfile、deploy、supervisor、pkg 哈希与 2026-09-12 一致；未重新提取或逐一核验 77 个应用进程。

基础镜像不含完整应用，实际运行依赖共享 `/app`、Bun、Claude 工具卷和独立 home。取样运行使用 Docker/runc；真实 KVM/Firecracker 不是当前复刻前提。

## 4. 数据模型

数据库 `portunex`：PostgreSQL 18.6，public schema 36 表，387 列，28 外键。
`_sqlx_migrations` 有 28 条成功迁移元数据，版本从 `20251221001` 到 `20260906002`。本轮只读迁移 version/description/success，不读取业务行。

| 域 | 当前表 |
| --- | --- |
| 用户/登录 | users、api_keys、auth_sessions、magic_link_tokens、oauth_identities、oauth_states |
| OIDC | oidc_clients、oidc_authorization_codes、oidc_consents、oidc_tokens |
| Provider | providers、provider_credentials、provider_refusal_events |
| 模型与价格 | model_aliases、model_alias_pricing、provider_model_pricing、provider_type_pricing、vendor_models |
| 窗口/路由 | provider_window_configs、provider_window_resets、provider_window_states、sticky_routing |
| 交易与积分 | orders、payment_channels、points_details |
| 订阅 | subscription_plans、user_subscriptions、subscription_usage_states、subscription_consume_details |
| 兑换 | redemption_codes、redemption_records、redemption_attempts |
| 用量/规则/其他 | usage_records、reasoning_extraction_dynamic_rules、kv_store、_sqlx_migrations |

关键迁移风险：

- `users.points`、价格及消费为 PostgreSQL NUMERIC；不能直接转成当前项目的 float balance，亦不能先验认定积分等于货币余额。
- 用户/API Key/订阅等表与 Sub2API 有同名但不同结构，必须隔离 schema/数据库和 ID namespace。
- 原用户有 `password_phc` 字段；原 session/API Key 存储不同于当前 JWT/Key 模型。兼容旧登录和旧 Key 需要独立验证，不能强制全部重新注册冒充兼容。
- `pricing_snapshot`、settled/settle_attempts、日/周/月 usage window 表说明计费和结算需要恢复事务规则，不只是 CRUD。
- JSONB 字段、触发器、函数、CHECK、索引定义、NUMERIC 精度以及算法细节尚未完整归档，本轮结构清单不是可执行 DDL。

## 5. 当前项目差距

基线：`codex/claude-execution-plane-v1`，HEAD `cab5ef0ca4b87d89235164d7e1c09142d34e855b`。
开始分析时有 46 个 tracked 修改和 45 个 untracked 状态条目，合计 91；均是既有工作，未覆盖。

| 现有部分 | 可复用 | 不能直接当作已恢复 |
| --- | --- | --- |
| backend | Go/Gin、PG/Redis、认证与网关/计费公共基础 | 与 Portunex 的账号、积分、OIDC、订单及 API 合同并不相同 |
| frontend | Vue3/Pinia/Vue Router/Vite 及现有管理功能 | 不能直接接入 React 生产 chunk 当 Vue 源组件 |
| ccmax-manager | Claude 账号、代理、调度、Outbox、执行客户端 | 不是整个 Portunex 的多 Provider、用户和账务系统 |
| execution-plane | Docker provider、票据、slot/epoch、固定出口、Vault、安全上号库 | CLI runtime、生产 host-agent 数据面、gateway dispatch、完整流式等未完成 |

源码锚点：

- `execution-plane/internal/worker/process.go:169`：CLI mode 为 not_implemented。
- 同文件 204、248、275：只接受 oauth_api，先 `io.ReadAll`，请求/响应限制 2 MiB，再一次发送 body；不是 isthmus 实时流。
- `execution-plane/internal/hostagent/runtimeclient.go:369`：Execute 将所有响应聚合进 slice。
- `execution-plane/internal/hostagent/dataplane.go:7`：只是 route 广告合并与校验，不是数据面 RPC server。
- `execution-plane/internal/service/bootstrap.go:13`：host-agent 入口没有完成生产运行图装配。
- `ccmax-manager/execution_settings.go:301`：dispatch decision 存在，但没有接入 gateway。
- `backend/internal/service/ccmax_compat_adapter.go:3`：已有窄适配器避免重复兼容转换；可复用经审查的基础能力，不应复制成第三套近似实现。
- `backend/internal/repository/api_key_cache.go:19`、`billing_cache.go:21` 和 `backend/internal/service/subscription_expiry_service.go:22`：现有认证/订阅缓存失效频道和任务锁具有全局命名；仅隔离PG不足以保护新业务域，Redis/pubsub/锁/后台任务也必须分域。

原 PRD 与完整恢复还有明确冲突：原稿排除生产 MITM；CLI 定义为每回合进程、默认候选版本与线上不同；原稿不含全部 Portunex/OIDC/React/数据库业务。因此不能只继续旧 5.6 并把它称作整套复刻。

## 6. 优先风险

1. **静态目录备份归档**：服务器 `/opt/gateway/public/gateway.tar` 为 141,004,800 字节、1928 条目，含 `.env`、日志和 PostgreSQL 物理数据目录。它位于 Caddy 静态根。只检查了条目，未读取内容、未验证公网可下载；已询问用户是否允许移出网站目录，未经授权不移动。
2. **物理归档不能当可靠备份**：未知打包时数据库是否运行，是否含所需 WAL；不能直接用 tar 替换新集群目录。
3. **财务/授权兼容不明**：定价优先级、窗口结算、密码哈希、Session、OIDC 流程均需后续隔离验证。
4. **没有异机恢复证据**：本机分析目录和线上单盘都不是完成灾备；必须建立源码、运行物、数据库和密钥四类独立恢复流程。

## 7. 本轮验证

- `go test ./internal/worker ./internal/hostagent ./internal/service ./internal/route`：通过，结果使用缓存，仅表示当前库测试基线。
- isthmus 提取器 7 项离线测试：通过。
- `web-reverse-master` 离线 selftest 7/7：通过，属于工具自测，不是线上业务验收。
- 未进行真实登录、支付、模型调用、数据导出、在线变更、完整 Go/Rust/前端构建或整套 E2E。

下一步执行以新 PRD 与任务计划为准，不将本分析标成“完整恢复已完成”。
