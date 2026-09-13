# 本地演示切片 ADR-002

用户已确认继续。遵循ADR-001；本文件在Go实现前写入，React/runtime分别在各模块记录子结构。

## 目录与装配

```text
backend/internal/portunex/
  platform/namespace/           # ID/cache/pubsub/lock的隔离命名
  identity/                     # 合成身份、内存会话与角色
  users/                        # 用户列表模型与纯查询
  apikeys/                      # 脱敏Key列表与owner过滤
  providers/                    # Provider元数据列表，无credential
  platform/listing/             # 搜索、有界分页与共用页结构
  app/                          # 演示路由、安全边界、依赖装配、集成测试
backend/cmd/portunex-demo/       # 仅loopback启动；不加入现有生产wire
portunex-web/src/
  app/                          # route/ports装配
  modules/{identity,users,apikeys,providers}/
  shared/                       # 受限UI/数据接口与合成适配
execution-plane/isthmus-runtime/
  src/runtime/turn/             # fake核心与事件接口
  src/transport/{http,websocket}/
  src/app/                      # 本地开发装配
```

## 内部演示协议，不是旧兼容接口

仅 `/__recovery__/v1` 下提供 `POST /login`、`POST /logout`、`GET /session`、`GET /users`、`GET /api-keys`、`GET /providers`。明确JSON错误envelope、`query/page/page_size`查询、camelCase页面模型，留待真正恢复适配器与旧DTO隔离。响应带演示标记；旧`/portunex/*`不会被本切片接管。

合成账户限固定`.invalid`地址，无注册、密码变更、真实Key或Provider凭据。会话用随机opaque token，只在内存保存摘要，有TTL和数量上限；浏览器用HttpOnly SameSiteStrict cookie，不用localStorage保存token。此模块不提供生产密码存储策略。admin可见三列表，普通用户仅查看自己的Key，不能通过前端隐藏按钮代替服务端授权。

React的三个列表页面均是管理员页面；普通用户UI目前仅展示工作台。后端`GET /api-keys`另外提供仅自己Key的受保护查询，留待后续个人页面使用。

演示启动命令只绑定127.0.0.1；handler限制loopback Host/peer及同源Origin，不信任Forwarded/X-Forwarded-*，不设置开放CORS。Vite通过固定loopback代理同源访问Go。不允许配置生产DSN/Redis/upstream；空间命名工具是隔离基础，不等于真实DB/Redis隔离验收。

## 未确定的旧契约

SSH与静态资源不可读时，记录故障，不推断login字段、session格式、列表envelope或分页规则。合成DTO和测试只证明本地骨架协作。后续恢复旧接口需独立证据与适配，不迁动本地合成记录到生产。

## 验证

Go单测/race覆盖会话到期/撤销/上限、匿名401、成员403、owner过滤、分页边界、输入限制、恶意Origin/Host、跨命名空间同值ID；React覆盖登录/角色/loading/error/empty并typecheck/build；isthmus验证真正逐块输出、取消、busy/复用和资源清理。所有fixture合成，禁止真实上游。
