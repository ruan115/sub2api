# CCMAX Manager

从 sub2api `ccmax` 分支独立出的 CCMAX 账号池、代理池、用户调度与计费项目。账号、代理、RPM、API Key 和调度行为沿用原项目的字段与规则，管理界面保持独立项目现有风格。

## 功能

- CCMAX 账号池：A/B 分组、用途切换、状态、优先级、并发、过期时间、账号价格和计费倍率。
- 账号代理：选择代理池、手动指定代理、自动匹配代理；自动匹配模式保证一个账号独占一个代理 IP。
- 强制代理：CCMAX 授权、Token 刷新、配额查询和消息网关只使用账号绑定的有效代理；未绑定或代理异常时拒绝请求，不回退直连。
- 代理池：支持批量导入 `socks5/http/https`，或通过代理 API 拉取纯文本、JSON 数组及常见对象格式；每个代理同时展示当前占用账号和历史使用过的不同账号数。
- 一次性 IP：**代理池级**开关，默认开启。开启后该池内的地址一旦绑定过任何账号（含已归档账号）即视为已消耗，删除账号或代理后也不能再次导入或分配。把地址导入到关闭该开关的池仍可复用 —— 这是留给运维的显式逃生口，不是缺陷。
- RPM：原版 `tiered` 三区模型、`sticky_exempt` 粘性豁免、粘性缓冲区和用户消息队列配置。
- 过载保护：分组可配置 529 的账号+模型短熔断，默认 10 秒。
- 429 降权：**分组开关，默认开启**（取代原「429 重试等待」）。开启后账号**第一次**收到网关 429 就降低调度权重并学习临时 RPM 上限。短冷却为 60-120 秒，可单独开启秒级阶梯；降峰时长另有一个**默认关闭**的分组阶梯开关。关闭降峰阶梯时保持旧行为，降峰至 5h 窗口；开启后按「首次分钟 + (N-1) × 递增分钟」计算，最大 315 分钟，临时 RPM 上限与降峰同时到期。5h 用量达到 90% 的模糊 429 直接使用 315 分钟；上游明确返回 `Retry-After` 或 5h/7d 配额刷新时间时，一律优先服从上游，不经本地阶梯。对明确的 5h 配额刷新，分组还可单独开关「刷新后错峰」并配置 0-315 分钟的最小/最大区间，默认保持 15-30 分钟；关闭后在上游 5h 刷新点立即恢复，7d 错峰不受此开关影响。同一账号 1 分钟内的并发 429 只记 1 次；成功请求不会提前解除降峰，到期或管理员手动重置才解除。关闭总开关后，本组不再记录新的本地降权，也不让已有降权影响调度；但上游明确冷却仍始终生效。
- ITPM 口径：上游的输入限流**只计未缓存输入**（`input_tokens + cache_creation_tokens`），`cache_read_tokens` 在所有现役模型上都**不计入**。策略新增 `itpm_limit` 按此口径限流（默认 0 = 不限）；原 `tpm_limit` 含义不变，仍限「总上下文吞吐」，但它不是上游的限流负载，UI 已相应改标。实时监控拆分为 ITPM / 缓存读 / OTPM / 总吞吐四项，避免把缓存命中高的健康账号误判为过载。
- 剩余额度采样：每次上游响应都会记录 `anthropic-ratelimit-input-tokens-remaining` 与 `-reset`。账号的剩余输入额度因此是上游的一手数据，调度无需在发请求前预估 token 数。
- 会话账号钉死：大请求（body ≥ 512KB 或 max_tokens ≥ 16384）在会话已绑定账号时，绑定关系从「排序偏好」升级为「硬约束」—— 容量不足时排队或拒绝，不换号，因为换号会丢弃缓存前缀，把近乎免费的缓存读变成全价缓存创建。该约束覆盖容量压力**与限流**：撞 429 时留在原账号等待，不迁移 —— 换号会把缓存前缀在别处全价重建，而那正是账号撞上限流的原因。学到的 RPM 上限同理不释放约束（它来自 429，是限流信号而非账号故障；阻塞判据是「滚动 60 秒计数 ≥ 上限」，会随流量老化自然放开，容量队列等得到）。只有账号返回 401/403/5xx —— 账号本身可疑 —— 才可用性优先于缓存，按原有预算故障转移。
- 冷缓存单飞：同一 (账号, 会话) 同时只允许一个大请求在途。缓存条目要等第一个响应开始流式返回才可读，并发的同前缀请求会各自付全价缓存创建且互相读不到 —— 串行化让后续请求变便宜，不只是变晚。
- 冷启动落号：无绑定的新会话按上游上报的剩余输入额度挑账号（按 10 万分桶，避免上游取整噪声导致号池抖动）。已绑定会话不参与，归上一条管。
- 缓存前缀审计：对每个会话实际发出请求的 `tools` + `system` 做指纹，变化时记录到 `cache_prefix_events`（保留 7 天）并标注变化段。**纯观测，不修改任何注入逻辑** —— 身份块、指纹、beta 合并存在的目的是让请求与真实 Claude Code 客户端不可区分，改动会危及账号本身。先量出证据，再决定是否值得动。
- 窗口刷新优先：配额窗口刚滚动过的账号额度是满的，在同优先级内**排在普通账号之前** 30 分钟，之后回到普通档。同优先级的调度顺序为：刚刷新 → 普通 → 已降权。账号自身优先级仍然优先于这三档。
- 策略流量分配：分组页面可给组内出现的每个调度策略配权重，把本组流量按比例分给不同策略（如 6:4）。按最近 60 秒滚动窗口计量，某个策略用满份额后请求**不会挤占**其他策略，而是等待自身份额释放；分组开启容量排队时进入排队。权重全为 0 表示不分配，组内未配置权重的策略不受限制。
- 用户权限：管理员、只读管理员、普通用户；管理员可逐个配置普通用户和只读管理员的可见页面，普通用户按 A/B 分组隔离且只能管理自己的 SK。
- API 调度：每个用户独立生成、禁用 `sk-` 密钥，支持 `/v1/messages`、`/v1/messages/count_tokens`、`/v1/models` 和 `/v1/models/{id}`；同时提供 `/models` 兼容别名。
- CCMAX 全链路：复用 Sub2API 的 Claude Code 客户端识别、OAuth 请求转换、计费指纹、CLI 请求头、beta 合并、缓存断点限制、模型映射、粘性调度、账号故障切换和实时 SSE 转发。
- 双支线请求：分组可在 Sub2API 原版完整链路与蒸馏兼容链路间切换；蒸馏兼容链路不注入 Sub2API 扩展提示词，仅保留直连 Anthropic OAuth 必需的两个身份块，并复现目标渠道的参数过滤、工具调用和响应 usage。账号仍可单独开启“原始请求完整透传”，保留客户端请求体的原始字节和全部参数。
- 分组级反蒸馏探测：可识别用户消息中高置信度的系统提示词、隐藏指令、工具定义和内部参数提取行为，在额度预留与账号调度前返回 HTTP 588 `Not allowed`；命中事件只记录 API Key、分组和错误分类，不保存请求正文。
- CCMAX 授权：可使用 Claude Session Key 自动换取 Access Token 与 Refresh Token，也可发起 OAuth / Setup Token 浏览器授权；临近过期时自动刷新，失效后标记为“需要重新授权”。
- 批量上号：最多一次提交 200 个 Session Key，每个成功账号分配独享代理，并读取授权 Token 中的邮箱作为账号名称和订阅类型；邮箱已存在时更新原账号的 OAuth 凭证与授权代理，不创建重复账号。
- 批量编辑：账号池可对已选账号统一修改并发、RPM 策略、优先级、计费参数和分组；名称、凭证与独享代理不会被覆盖。
- 配额：记录每个账号的 5 小时、7 天使用率和刷新时间，支持主动刷新，也会从上游响应头被动更新。
- 账号统计：展示订阅类型、上号时间、存活时间、最近使用、累计请求、Token、账号总计费，并按正常、暂不可调度和错误统一筛选。
- 计费：自动同步 Sub2API 同源的 Anthropic 模型价格，同时保留手动价格；支持账号价格、分组收入倍率、账号成本倍率、筛选区间汇总和用量流水。
- 运营统计：独立展示死亡账户、每日请求/Token/计费/账号生命周期，以及包含账号、代理 IP 和成功状态的授权日志；死亡账户可单个或批量归档，归档保留历史数据并立即释放独享 IP。归档账号通过 `archived_proxy_id` 与代理绑定历史保留 IP 的已消耗状态，恢复账号也不会让该地址重新可分配。
- 操作审计：记录登录、创建、编辑、状态变更、删除、授权及同步操作，密码、Token、Session Key 和凭证内容自动脱敏。
- 报错信息：统一汇总 API 请求失败、账号授权与刷新异常、授权失败、代理检测/同步异常、价格同步异常和后台失败操作，支持来源、分组、关键词、分钟级时间筛选及分页；错误归因同时展示账号负载和按小时/天聚合的时间分布。网关错误默认保留 7 天且不记录请求正文或用户 SK。
- 管理界面：分组复用 Sub2API 的 Claude 图标；桌面侧栏支持展开、收缩和状态记忆，移动端使用横向底部导航。

只读管理员只能查看管理员分配的页面，所有写接口都会在服务端返回 `403`。普通用户同样只能读取分配页面，只有本人 API Key 的创建、编辑、禁用和删除接口例外。

## 本地运行

```bash
CCMAX_MYSQL_DSN='ccmax:password@tcp(127.0.0.1:3306)/ccmax?charset=utf8mb4&collation=utf8mb4_unicode_ci&timeout=5s&readTimeout=30s&writeTimeout=30s' \
go run .
```

默认地址为 `http://127.0.0.1:8088`。生产服务只使用 MySQL，缺少 `CCMAX_MYSQL_DSN` 时会拒绝启动，不会回退 SQLite。首次启动会创建管理员：

```text
用户名：admin
密码：ccmax-admin
```

部署前必须通过环境变量修改初始密码。管理员只在数据库首次初始化时创建，已有数据库不会因环境变量变化而重置密码。

```bash
CCMAX_ADDR=0.0.0.0:8088 \
CCMAX_MYSQL_DSN='ccmax:password@tcp(127.0.0.1:3306)/ccmax?charset=utf8mb4&collation=utf8mb4_unicode_ci&timeout=5s&readTimeout=30s&writeTimeout=30s' \
CCMAX_ADMIN_USER=admin \
CCMAX_ADMIN_PASSWORD='replace-with-a-strong-password' \
go run .
```

| 变量                         | 默认值             | 说明                                          |
| ---------------------------- | ------------------ | --------------------------------------------- |
| `CCMAX_ADDR`                 | `127.0.0.1:8088`   | HTTP 监听地址                                 |
| `CCMAX_MYSQL_DSN`            | 无，必填           | MySQL DSN；生产服务唯一持久化数据库           |
| `CCMAX_REDIS_ADDR`           | 空                 | 可选 Redis 地址，仅用于临时调度协调           |
| `CCMAX_ADMIN_USER`           | `admin`            | 首次初始化的管理员用户名                      |
| `CCMAX_ADMIN_PASSWORD`       | `ccmax-admin`      | 首次初始化的管理员密码                        |
| `CCMAX_AUTH_DISABLED`        | `false`            | 仅用于本机测试；设为 `1` 会关闭管理端认证     |
| `CCMAX_ERROR_LOG_RETENTION_DAYS` | `7` | 网关错误日志保留天数；服务启动并在运行期间定时清理 |
| `CCMAX_PRICING_AUTO_SYNC`    | `true`             | 启动后和定时自动同步模型价格；设为 `0` 可关闭 |
| `CCMAX_PRICING_SYNC_MINUTES` | `10`               | 自动检查模型价格的间隔分钟数                  |
| `CCMAX_TOKEN_REFRESH_ENABLED` | `true` | 后台 OAuth Token 自动刷新；蓝绿预热实例可设为 `0` |
| `CCMAX_PRICING_REMOTE_URL`   | Sub2API 同源地址   | 自定义 HTTPS 模型价格 JSON 地址               |
| `CCMAX_PRICING_HASH_URL`     | Sub2API 同源地址   | 自定义 HTTPS 价格文件哈希地址，可留空         |
| `CCMAX_UPSTREAM_RESPONSE_HEADER_TIMEOUT` | `15m` | 等待上游响应头的最长时间，覆盖长非流请求      |
| `CCMAX_UPSTREAM_REQUEST_TIMEOUT` | `0` | 上游请求总时限；`0` 表示由请求上下文控制      |
| `CCMAX_STREAM_HEARTBEAT_INTERVAL` | `10s` | 流式首事件和事件间隔期间发送 SSE 注释心跳，避免客户端误判超时 |

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

分组的“蒸馏兼容模式”与账号的“原始请求完整透传”互相独立。蒸馏兼容模式关闭时保持 Sub2API 原版 CCMAX 行为；开启后保留客户端 `system`、消息、工具、工具选择、停止序列和流式设置，过滤目标渠道不接受的未知顶层参数，并按 Fable 5 的实测行为忽略客户端采样与 thinking 配置。缓存断点未提供 `ttl` 时默认使用 `5m`，客户端显式提供 `5m` 或 `1h` 时原样保留。蒸馏兼容模式始终保留 billing attribution；独立的“Claude Code 身份句”开关默认关闭，只有显式开启后才注入 `You are Claude Code, Anthropic's official CLI for Claude.`。每个 A/B 分组还能分别允许 `service_tier`、`inference_geo`、`speed` 与客户端 `anthropic-beta` 透传，四项默认关闭；启用 beta 透传时仍会保留 OAuth 必需标记。直连 OAuth 会使用服务端 adaptive thinking，响应中的 thinking signature 原样保留，最终 usage 会包含 `iterations`。原始透传优先级更高，开启原始透传的账号不执行上述兼容变换。

分组可独立开启“请求格式过滤网”。开启后，网关会在账号选择前拒绝同时包含 `temperature` 与 `top_p` 的请求，以及结构不合法的 Anthropic `system` 内容块，直接返回 HTTP 400 `invalid_request_error` 并记录为 `request_format_blocked`。该过滤不会删除或修正参数，也不会占用账号并发、记录账号 RPM 或访问上游；默认关闭以保持现有请求行为。

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

批量上号页面按行接收 Session Key。每次组织查询、授权码申请和 Token 交换都强制经过本次分配的代理；授权成功后才创建或更新账号并占用该代理。若 Token 邮箱已存在，系统会在原账号记录上替换 OAuth 凭证、恢复授权与调度，并绑定本次授权所用代理，原代理自动释放。失败项不会创建或修改账号，结果与授权日志会保留失败原因和代理 IP，但不会记录完整 Session Key。

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
  -e CCMAX_MYSQL_DSN='ccmax:password@tcp(mysql:3306)/ccmax?charset=utf8mb4&collation=utf8mb4_unicode_ci&timeout=5s&readTimeout=30s&writeTimeout=30s' \
  -e CCMAX_ADMIN_PASSWORD='replace-with-a-strong-password' \
  ccmax-manager
```

历史 SQLite 数据只能通过离线迁移入口导入 MySQL，不能作为服务运行库：

```bash
CCMAX_MIGRATE_FROM_SQLITE=/path/to/ccmax-snapshot.db \
CCMAX_MYSQL_DSN='ccmax:password@tcp(127.0.0.1:3306)/ccmax?charset=utf8mb4&collation=utf8mb4_unicode_ci' \
go run -tags sqlite_migrate .
```

## 验证

```bash
go test ./...
go test -race ./...
go vet ./...
```

`*` 模型价格仍作为远程模型未匹配时的可编辑回退项。部署后应确认同步状态和实际采购计费口径一致。
