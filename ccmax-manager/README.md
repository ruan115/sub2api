# CCMAX Manager

从 sub2api `ccmax` 分支独立出的 CCMAX 账号池、代理池、用户调度与计费项目。账号、代理、RPM、API Key 和调度行为沿用原项目的字段与规则，管理界面保持独立项目现有风格。

## 功能

- CCMAX 账号池：A/B 分组、用途切换、状态、优先级、并发、过期时间、账号价格和计费倍率。
- 账号代理：选择代理池、手动指定代理、自动匹配代理；自动匹配模式保证一个账号独占一个代理 IP。
- 强制代理：CCMAX 授权、Token 刷新、配额查询和消息网关只使用账号绑定的有效代理；未绑定或代理异常时拒绝请求，不回退直连。
- 代理池：支持批量导入 `socks5/http/https`，或通过代理 API 拉取纯文本、JSON 数组及常见对象格式。
- RPM：原版 `tiered` 三区模型、`sticky_exempt` 粘性豁免、粘性缓冲区和用户消息队列配置。
- 用户权限：管理员、只读管理员、普通用户；管理员可逐个配置普通用户和只读管理员的可见页面，普通用户按 A/B 分组隔离且只能管理自己的 SK。
- API 调度：每个用户独立生成、禁用 `sk-` 密钥，支持 `/v1/messages`、`/v1/messages/count_tokens`、`/v1/models` 和 `/v1/models/{id}`；同时提供 `/models` 兼容别名。
- CCMAX 全链路：复用 Sub2API 的 Claude Code 客户端识别、OAuth 请求转换、计费指纹、CLI 请求头、beta 合并、缓存断点限制、模型映射、粘性调度、账号故障切换和实时 SSE 转发。
- 双支线请求：分组可在 Sub2API 原版完整链路与蒸馏兼容链路间切换；蒸馏兼容链路不注入 Sub2API 扩展提示词，仅保留直连 Anthropic OAuth 必需的两个身份块，并复现目标渠道的参数过滤、工具调用和响应 usage。账号仍可单独开启“原始请求完整透传”，保留客户端请求体的原始字节和全部参数。
- CCMAX 授权：可使用 Claude Session Key 自动换取 Access Token 与 Refresh Token，也可发起 OAuth / Setup Token 浏览器授权；临近过期时自动刷新，失效后标记为“需要重新授权”。
- 批量上号：最多一次提交 200 个 Session Key，每个成功账号分配独享代理，并读取授权 Token 中的邮箱作为账号名称和订阅类型。
- 配额：记录每个账号的 5 小时、7 天使用率和刷新时间，支持主动刷新，也会从上游响应头被动更新。
- 账号统计：展示订阅类型、上号时间、存活时间、最近使用、累计请求、Token、账号总计费，并按正常、暂不可调度和错误统一筛选。
- 计费：自动同步 Sub2API 同源的 Anthropic 模型价格，同时保留手动价格；支持账号价格、分组收入倍率、账号成本倍率、筛选区间汇总和用量流水。
- 运营统计：独立展示死亡账户、每日请求/Token/计费/账号生命周期，以及包含账号、代理 IP 和成功状态的授权日志。
- 操作审计：记录登录、创建、编辑、状态变更、删除、授权及同步操作，密码、Token、Session Key 和凭证内容自动脱敏。
- 管理界面：分组复用 Sub2API 的 Claude 图标；桌面侧栏支持展开、收缩和状态记忆，移动端使用横向底部导航。

只读管理员只能查看管理员分配的页面，所有写接口都会在服务端返回 `403`。普通用户同样只能读取分配页面，只有本人 API Key 的创建、编辑、禁用和删除接口例外。

## 本地运行

```bash
go run .
```

默认地址为 `http://127.0.0.1:8088`，数据库为当前目录的 `ccmax-manager.db`。首次启动会创建管理员：

```text
用户名：admin
密码：ccmax-admin
```

部署前必须通过环境变量修改初始密码。管理员只在数据库首次初始化时创建，已有数据库不会因环境变量变化而重置密码。

```bash
CCMAX_ADDR=0.0.0.0:8088 \
CCMAX_DATA=/data/ccmax-manager.db \
CCMAX_ADMIN_USER=admin \
CCMAX_ADMIN_PASSWORD='replace-with-a-strong-password' \
go run .
```

| 变量                         | 默认值             | 说明                                          |
| ---------------------------- | ------------------ | --------------------------------------------- |
| `CCMAX_ADDR`                 | `127.0.0.1:8088`   | HTTP 监听地址                                 |
| `CCMAX_DATA`                 | `ccmax-manager.db` | SQLite 数据文件                               |
| `CCMAX_ADMIN_USER`           | `admin`            | 首次初始化的管理员用户名                      |
| `CCMAX_ADMIN_PASSWORD`       | `ccmax-admin`      | 首次初始化的管理员密码                        |
| `CCMAX_AUTH_DISABLED`        | `false`            | 仅用于本机测试；设为 `1` 会关闭管理端认证     |
| `CCMAX_PRICING_AUTO_SYNC`    | `true`             | 启动后和定时自动同步模型价格；设为 `0` 可关闭 |
| `CCMAX_PRICING_SYNC_MINUTES` | `10`               | 自动检查模型价格的间隔分钟数                  |
| `CCMAX_PRICING_REMOTE_URL`   | Sub2API 同源地址   | 自定义 HTTPS 模型价格 JSON 地址               |
| `CCMAX_PRICING_HASH_URL`     | Sub2API 同源地址   | 自定义 HTTPS 价格文件哈希地址，可留空         |

## API 调度

在“用户与 SK”中创建用户、分配允许使用的 A/B 分组，再生成绑定分组的 SK：

```bash
curl http://127.0.0.1:8088/v1/messages \
  -H 'Authorization: Bearer sk-...' \
  -H 'anthropic-version: 2023-06-01' \
  -H 'content-type: application/json' \
  -d '{
    "model":"claude-sonnet-4-5",
    "max_tokens":1024,
    "messages":[{"role":"user","content":"hello"}],
    "metadata":{"user_id":"stable-session-id"}
  }'
```

也支持 `x-api-key: sk-...`。`metadata.user_id`、`x-session-id` 或 `session-id` 会用于 15 分钟粘性调度。成功响应自动写入计费流水并累加 SK 配额。

模型列表使用同一个用户 SK 鉴权，不请求 Anthropic 上游。它与 Sub2API 保持一致：优先汇总该 SK 所属分组当前可调度账号配置的 `model_mapping` 键；没有映射或暂时没有可调度账号时，返回 Sub2API 同源的 Claude 默认模型列表：

```bash
curl http://127.0.0.1:8088/v1/models \
  -H 'Authorization: Bearer sk-...'
```

`/v1/models/{id}` 可读取单个模型；`/models` 与 `/models/{id}` 是兼容别名。列表响应和模型字段使用 Sub2API 的 Anthropic 格式。模型价格配置只用于计费，不控制 `/v1/models` 的可见模型。

OAuth 账号不是原样直通：真实 Claude Code 请求保留客户端 system prompt 和缓存结构；其他客户端会按 Sub2API 规则转换成 Claude Code 请求，补齐 billing attribution、稳定账号身份、CLI 字段和 beta。客户端 Cookie、下游鉴权头及非白名单头不会转发到上游。流式响应会在收到 SSE 事件后立即 flush，账号遇到授权失败、429 或可重试的 5xx 时会切换到同组的其他可用账号。

账号默认关闭“原始请求完整透传”，此时直接复用 Sub2API 原版 CCMAX 的请求变换、指纹、metadata、模型映射、beta、缓存控制和响应还原链路。开启后 `/v1/messages` 与 `/v1/messages/count_tokens` 都保留请求体原始字节和客户端参数，不再执行任何 body 变换；客户端请求头也会原样转发，但下游鉴权、Cookie、Host、Content-Length 和逐跳头会被剔除，用户 SK 始终替换为账号上游凭证。

分组的“蒸馏兼容模式”与账号的“原始请求完整透传”互相独立。蒸馏兼容模式关闭时保持 Sub2API 原版 CCMAX 行为；开启后保留客户端 `system`、消息、工具、工具选择、停止序列和流式设置，过滤目标渠道不接受的未知顶层参数，并按 Fable 5 的实测行为忽略客户端采样与 thinking 配置。直连 OAuth 会使用服务端 adaptive thinking，响应中的 thinking signature 原样保留，最终 usage 会包含 `iterations`。原始透传优先级更高，开启原始透传的账号不执行上述兼容变换。

账号凭证 JSON 支持：

```json
{ "access_token": "OAuth token" }
```

或：

```json
{ "api_key": "Anthropic API key" }
```

扩展数据中的 `custom_forward_url` 或 `base_url` 可覆盖默认 Anthropic 地址；公网地址必须使用 HTTPS。

账号扩展数据还支持模型限制和映射：

```json
{
  "supported_models": ["claude-sonnet-*", "claude-opus-4-6"],
  "model_mapping": {
    "claude-sonnet-latest": "claude-sonnet-4-6"
  }
}
```

`supported_models` 或 `available_models` 为空时允许所有模型；支持精确名称和末尾 `*`。`model_mapping` 的键是客户端模型名，值是实际发送给上游的模型名。

## 账号授权与配额

账号列表中的“更新授权”支持两种路径：

- 输入 Claude Session Key，服务端通过该账号绑定的独享代理完成组织查询、授权码申请和 Token 交换。
- 获取 OAuth 链接，在浏览器完成授权后粘贴 `code#state`，由服务端换取并保存 Token。

选择“Setup Token”时使用 inference-only scope；选择“OAuth”时使用完整配额 scope。完整 Session Key、授权码和 Token 不会返回到账户列表，也不会以明文写入审计日志；系统仅保存不可逆的 Session Key 脱敏标识用于流水追踪。

批量上号页面按行接收 Session Key。每次组织查询、授权码申请和 Token 交换都强制经过本次分配的代理；授权成功后才创建账号并占用该代理。失败项不会创建账号，结果与授权日志会保留失败原因和代理 IP，但不会记录完整 Session Key。

OAuth 账号可以在列表中主动刷新 5h/7d 配额。正常 API 调度也会解析 Anthropic 限流响应头，更新配额、刷新时间和账号授权状态。

## 模型价格

服务启动后会立即拉取一次模型价格，默认每 10 分钟检查远程哈希；远端未变化时不会重复写入。管理端“模型价格”页面可以查看同步状态、手动触发同步，并添加不受远程同步覆盖的手动价格。

## 代理导入

每行一个代理，支持以下格式：

```text
socks5://user:pass@host:port
http://host:port
https://user:pass@host:port
host:port
host:port:user:pass
user:pass:host:port
user:pass@host:port
host:port@user:pass
[2001:db8::1]:1080
```

代理 API 可返回同样的纯文本、字符串数组，或包含 `host`、`port`、`protocol`、`username`、`password` 的 JSON 对象。

## Docker

```bash
docker build -t ccmax-manager .
docker run --rm -p 8088:8088 \
  -v ccmax-data:/data \
  -e CCMAX_ADMIN_PASSWORD='replace-with-a-strong-password' \
  ccmax-manager
```

## 验证

```bash
go test ./...
go test -race ./...
go vet ./...
```

`*` 模型价格仍作为远程模型未匹配时的可编辑回退项。部署后应确认同步状态和实际采购计费口径一致。
