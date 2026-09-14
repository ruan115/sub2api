# CLI / 转发层配置：第二轮事实核查

日期：2026-09-14。承接 [第一轮清单](online-cli-forwarder-config-2026-09-14.md)。本轮用户要求“先查清楚，再进行下一步规划”，因此只核查和记录证据，不提出新开发规划，不修改应用、不部署或重启线上服务。

## 1. 本轮闭合了什么

| 问题 | 新证据与结论 |
| --- | --- |
| blue / green 谁在本机入口路由中？ | Traefik 运行中的 `portunex@file`、`portunex-ws@file` 后端都只有 `blue:8080`，状态 UP；不是只看磁盘配置推断 |
| 两种 isthmus 是否只是文件大小不同？ | 对全部 77 个实际 runtime 的内嵌 JS 读取并计算哈希：57 / 20，恰好两种 JS；全部解析成功 |
| 空 `CLAUDE_CODE_PIPELINE` 是否不处理请求？ | 不是。blue 的静态代码走内置 22 步默认列表，包括缓存默认值和缓存规范化两个步骤 |
| `DOWNGRADE_1H_CACHE_TO_5M=false` 是否意味着不改 TTL？ | 不是。该开关只是一项策略；默认 code 流水线的缓存步骤本身会补入或重建 `1h` 标记 |
| CLI 是否还可以通过设置文件决定 TTL？ | 是。除主/子任务 TTL 环境变量外，还有 `promptCacheTtl` / `subagentPromptCacheTtl`，以及强制 5m、启用 1h 开关；本次运行快照未发现显式覆盖 |
| 是否存在另一类时间窗口？ | 是。运行库的规则配置表中有 5h、7d、模型匹配 7d 窗口，本次该表没有 5m / 1h 窗口；限额值均为 null，不能解释成已配置硬限额 |

这些结论仍不等于“每个线上请求最终 TTL 已被抓包证明”。必须分开看 provider 类别、流水线、CLI 自动策略、isthmus 消息合并，以及上游实际响应。

## 2. 当前本机入口及 provider 类别

通过宿主回环地址访问 Traefik 只读管理 API `/api/http/routers`、`/api/http/services`，观察到：

- `portunex@file`：已启用，priority 100，后端 `h2c://blue:8080`，UP。
- `portunex-ws@file`：已启用，priority 200，后端 `http://blue:8080`，UP。
- 两个 service 都没有 green 后端。blue / green 容器仍都在运行，不能把“green 运行中”当作“green 正在承接这两个入口”。
- blue / green 的 Docker provider 自动暴露标签均为 false；使用的是 Traefik file provider。宿主 8080 端口映射到 Traefik web entrypoint。

范围限定：这是本机代理的运行路由快照，不证明外部 DNS、额外上游代理或绕过此入口的内部直连都相同。

数据库仅做 provider **类别计数**：`claude_code` 1 条、`claude_console` 19 条，包含非活动记录。没有读取具体 provider 身份、凭据或地址，也没有确定某次请求最终选中了哪条记录。因此下文必须保留 code / console 分支区分。

## 3. blue 的默认缓存处理，不是仅看环境变量名称

blue ELF：48,446,992 bytes，SHA-256 `de6de2722837eb3a319ff2a342f2cb0ad44b989e4b221eacf68cfdbc44d98763`。复制的是运行进程的可执行文件，不是进程内存；仅静态检查，未执行 ELF。

### 空 code 配置的实际静态调用链

`claude_code_pipeline` 配置槽 → parser `0x288b4d0` → 空值 sentinel → provider-kind 精确匹配 `claude_code` → 默认 22 步列表 → 实际 enum switch。

关键定位：

- 配置名关联：`0x288cc00`；空值分支 `0x288b4ff` → `0x288b9b7`，返回 sentinel `0x8000000000000000`，不是空步骤数组。
- 默认列表 `0x1100805`，22 个 enum；实际选择点 `0x272a798..0x272a7f9`，实际迭代分发 `0x272cbd1..0x272cc31`。
- id 8：`apply_cache_ttl_default` → `0x28a72a0`。
- id 23：`normalize_cache_control` → `0x28a7c00`。
- enum/name 表、跳表、GOT 与目标指令均做独立字节断言；不是凭相邻字符串猜调用关系。

### 两个缓存步骤的已确认行为

| 步骤 | 静态确认 | 不能扩大成 |
| --- | --- | --- |
| id 8 缓存默认值 | 对已有 `ephemeral` 缓存对象，TTL 缺省的 typed 分支写入 `1h`；保留显式 TTL，缺少整个缓存对象时不新建 | 任意原始 JSON / 所有字段都同样处理；JSON helper 特殊表示仍有边界 |
| id 23 缓存规范化 | 清理已识别的 system / tools / messages 缓存标记，然后在特定末尾块重建缓存对象 | 整条流水线最终值、任意输入内容类型的统一规则 |
| id 23 的 system 重建 | 非空 block 数组只在末块添加 `ephemeral / 1h / global`；字符串转单块，该分支没有空字符串检查；缺省或空数组不添加 | 所有 system 块均添加缓存 |
| id 23 的 messages 重建 | 只在最后一条消息的最后一个受支持 content block 添加 `ephemeral / 1h`，无 scope；不按 role 筛选，不向前找替代块 | 自动找到最近一个可缓存历史消息 |
| id 23 的 tools | 清理已识别缓存标记，不重建 | 工具定义完全不再参与上游缓存 |

`CacheControlConfig` 的 type / ttl / scope 字段布局、请求的 system / messages / tools 布局由 JSON serializer 字段名和代码引用交叉定位；id 23 控制流另做独立 review。末块重建支持的类型已交叉确认：`text`、`image`、`document`、`search_result`、`tool_use`、`tool_result`、`container_upload`、`mid_conv_system`；其余已检查枚举走不重建分支。原始 JSON 清理 helper 已确认精确匹配 `cache_control` 后移除 map 项、减少计数，不是只读查找。

仍未完整恢复其他 content-block enum 的类型名、id 8 原始 JSON 特殊值 tag 的语义，以及独立 downgrade 开关的完整消费链。id 8 / id 23 的字段、条件、写入指令和跳表经 44 组静态断言复验通过；这是字节及控制流证据，不是执行原程序所得的请求结果。

当前 blue 显式 `CLAUDE_CONSOLE_PIPELINE` 是：

```text
strip_fallbacks,apply_thinking_display_default,create_client,
rewrite_tool_names,mutate_rewritten_tool_names
```

它没有上述 id 8 / id 23 阶段名称，且使用独立配置槽；不能套用默认 `claude_code` 列表解释所有 `claude_console` 请求。空 `CLAUDE_DEFAULT_PIPELINE` 的完整默认序列也不在本轮已闭合结论中。

## 4. CLI 的 5m / 1h 优先级已定位

实际 CLI 版本路径为 `2.1.258`。静态样本为 215,473,560 bytes，SHA-256 `704f1334ac65d3e89e1c6c1d7663293ad786a6166afdb71b5075337df630f976`。

读取三个有界源码函数：`rVn`（文件 byte offset 185737930）、`MXn`（185745759）、`oO`（185745661）。这是具体分支代码，不只是环境变量字符串命中。

选择顺序从高到低：

1. `FORCE_PROMPT_CACHING_5M`：强制选择 5m。
2. 按请求来源类别选择主任务 `CLAUDE_CODE_PROMPT_CACHE_TTL` 或子/辅助任务 `CLAUDE_CODE_SUBAGENT_PROMPT_CACHE_TTL`。
3. 对应设置项 `promptCacheTtl` / `subagentPromptCacheTtl`。
4. agent frontmatter 提供的 TTL；处于适用的 overage 状态时，frontmatter 的 1h 有抑制条件，不能和显式环境覆盖混同。
5. `ENABLE_PROMPT_CACHING_1H`；或 Bedrock provider 对应的 `ENABLE_PROMPT_CACHING_1H_BEDROCK`。
6. 自动策略：订阅资格、当前 overage 状态和请求来源 allowlist 共同决定 1h / 5m；allowlist 可来自缓存/功能配置，不是所有请求无条件 1h。

主任务默认来源集合包含 `repl_main_thread*`、`sdk`、`auto_mode`、`memdir_relevance`。来源分类不能只靠 `--entrypoint=claude-vscode` 猜测；本轮未读取真实 turn 的来源和账号订阅状态。

使用固定哈希的上述源码函数、合成 auth / settings / feature-flag 依赖，15 项测试通过：覆盖覆盖顺序、主/子任务区分、Bedrock 条件、overage、动态 allowlist。没有执行完整 CLI、登录、联网或发起模型请求；测试不证明某个真实账号选择了哪个分支。

### 实际设置快照

运行 CLI 数量会变化：首次专项检查 39 个，保存证据时 35 个；两次结果一致：

- 主/子任务 TTL、FORCE 5m、ENABLE 1h（含 Bedrock）、各已检查 DISABLE_PROMPT_CACHING 环境项均未显式设置。
- 只对白名单缓存字段解析 user `settings.json`、当前 cwd 的 `.claude/settings.local.json`：没有 `promptCacheTtl` / `subagentPromptCacheTtl` 或上述 env 覆盖。
- 已检查路径中，project settings、上级 local settings 和 `/etc/claude-code/managed-settings.json` 均不存在；不等于覆盖所有可能的插件、进程内设置和远端功能配置。
- 77 个 runtime 的 cwd `.isthmus-token`、home `setup-token` **当前均不存在**，只做 stat，没有读取凭据文件内容。
- 已观察 CLI 的 `CLAUDE_CODE_OAUTH_TOKEN` 环境项均不存在，和 native-child 分支构造相符；这是当前子进程证据，不把“当前文件不存在”倒推为所有进程启动时必然不存在。

## 5. isthmus 层的缓存字段处理

两份主 JS 和先前保全 worker 的两段字段函数经 AST 归一化一致，10 项合成字段测试通过：

- 部分被保留的 CLI 原始 system 边界块会删除整个 `cache_control`，TTL 也随之消失。
- 客户端 system 在特定前言匹配分支中，已有缓存对象的 `scope` 被调整成 `global`；不新增缓存对象，不修改或校验其中 TTL。
- 客户端 messages / tools 的替换不是递归 TTL 清理；客户端顶层 `cache_control` 不在显式复制字段中，不能推广内容块的保留结论。
- `nativeRequest`、auxiliary 请求分类控制 hook 是否进入；`nativeRequest` 不等于启动选项 `nativeChildOauth`。

因此，CLI 自己生成的缓存策略也不自动等于最终发送策略：转发层对客户端请求的处理、isthmus 合并逻辑及实际分支都可能影响最后正文。

## 6. 窗口规则与单 key 覆盖检查

使用 PostgreSQL 只读事务和 3 秒 statement timeout：只查表结构、规则配置、类别计数和固定字段的布尔值聚合，不读取 API key 值、用户身份、请求正文或历史调用日志。

`provider_window_configs` 未删除的配置共返回 3 条：

| provider_kind | window_type | window_seconds | stat_type | model_pattern | enabled | limit_value |
| --- | --- | --- | --- | --- | --- | --- |
| claude_console | 5h | 18000 | requests | `*` | true | null |
| claude_console | 7d | 604800 | requests | `*` | true | null |
| claude_console | 7d_oi | 604800 | requests | `*fable*` | true | null |

该表没有 300 秒 / 3600 秒规则。null 限额不能解读为 0、无限或某个默认硬限额；实际配额来自哪里仍须对应实现证据。此配置核查不改变 Sub2API 对产品计费/权限的所有权，也不授权复制另一套计费业务。

`api_keys.settings` 是 JSONB。仅聚合递归同名 `downgrade_1h_cache_to_5m` 字段，本次没有找到任何该字段出现。不能据此排除别名、其他配置来源、内存旧缓存或将来的单 key 覆盖。

## 7. 两种核心构建差异已缩小范围

| 全量 JS 哈希核查 | runtime 数量 |
| --- | --- |
| `808e4931e5bfcb68331fb80ad2c322476a4f8f3341b5838a23edb2c627803c32` | 57 |
| `8a3f86d446e52785d9361644e49ef8cabba9d820f31cbaf1c2c1da625996f92a` | 20 |

本轮不再仅按文件大小猜同组内容；对每个实际 runtime 单独读取内嵌 JS 并哈希。完整 ELF / bytecode 没有逐个比较，不能把“JS 相同”扩大为整个可执行文件相同。

使用已安装 TypeScript parser/checker 对函数、类、方法和外层变量声明做静态比较。池、CLI argv/env、host refresh、缓存、HTTP/WS/gRPC(S) 和主装配的所选片段匹配；没有发现本次关注的默认值变化，但这不是全程序形式化等价证明。

确认的差异集中在独立 OAuth 登录 worker/coordinator：`808e…` 副本不再通过任务结果返回 `claude_device_ids`；底层设备 ID 生成/保留逻辑仍存在。不能把“删除返回字段”说成“停止生成设备 ID”。两个副本的取得先后不代表发行先后。

## 8. 验证产物、仍未闭合的边界

原始私有证据在仓库外 `/Users/ruanyang/My-project/api/z/isthmus-config-followup.o75ezv`，目录权限 0700；只把本脱敏报告加入 Git。

- `*-evidence.json`：带 UTC 采集时间和 collector SHA 的运行路由、全部核心 JS 分组、缓存设置白名单、数据库规则、CLI 有界源码函数；保存前通过既有 evidence 内容筛查。
- `preview_server_verified_links.json`、`preview_server_cache_verification.json` 与两份静态检查报告：blue pipeline 及缓存 handler 的地址证据和字节断言；主代理已用两个验证器的 `--check-only` 重新验证，通过且不写文件。
- `variant-diff-report.md` / `variant-diff-v4-*.json`：两构建的结构比较；比较器合成测试 7/7。
- `cache-field-proof.cjs` / `.json`：isthmus 两段缓存字段函数，10 项合成测试。
- `cli-cache-proof.cjs` / `.json`：CLI 选择顺序，15 项合成测试。
- web-reverse-master 离线工具 selftest 7/7；这不是线上业务验收。

仍未闭合：独立 downgrade bool 的完整消费链与全局/单 key 优先级；某次实际请求的 provider 选择、CLI 查询来源、订阅/overage/功能配置状态；最终上游正文和真实缓存命中。没有把这些写成已确认事实。

本轮停留在事实核查，不进入开发规划。若要把逐请求行为也闭合，需要有明确授权和专用测试条件的合成端到端请求；不能靠查看真实用户历史提示来替代，也不能仅据启动配置宣称兼容完成。

审阅状态：isthmus 缓存字段、两构建差异以及 Portunex id 23 已分别完成独立模块复核；最后整份报告的独立终审因协作代理额度中断，没有标为完成。主代理已逐项对照留存证据并复跑缓存选择测试、pipeline 链断言和 44 组缓存静态断言。本轮不包含应用代码变更。
