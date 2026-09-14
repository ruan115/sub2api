# 线上 CLI / 转发层配置核查

日期：2026-09-14。目标服务器：216.106.185.119。范围：CCMAX 复刻所需的转发、isthmus 与 Claude CLI 参数；不恢复第二套登录、权限或计费权威。

## 结论与证据等级

存在可在线上读取的配置，而且不只有 5m / 1h。最直接对应这一记忆的是运行中 Portunex 的 `PORTUNEX__ANTHROPIC__DOWNGRADE_1H_CACHE_TO_5M`，blue、green 两个进程的启动环境均为 `false`，与各自 Docker Config.Env 一致。不能由此推断每个请求最终使用 1h；它只证明没有通过这个启动配置开启 1h → 5m 降级。

Anthropic 协议中的 `cache_control.ttl` 可取 `5m` / `1h`，省略时默认为 5m。这是提示缓存保留时间，不是任务最多执行多久，也不是账号配额窗口。参考 [Messages API](https://platform.claude.com/docs/en/api/messages/create) 与 [Prompt caching](https://platform.claude.com/docs/en/build-with-claude/prompt-caching)。此协议说明不是线上请求已携带某 TTL 的证据。

本报告区分三个层次：

- **运行快照**：容器元数据、运行进程初始环境与 argv，经过远端白名单筛选，仅输出数字、布尔、已限定标识符。环境值不等于已经完成逐请求行为验证。
- **静态行为**：已保全的 isthmus JS / shell。部署脚本哈希与线上一致；核心可执行文件只有一个抽样版本与保全版本一致，另一个版本不同。
- **未验证**：真实请求最终正文、上游缓存命中、动态配置覆盖、流量切换、用户/project/local 设置。没有为此读取真实用户内容或发起付费请求。

## 1. 配置所在层次

```text
Portunex 转发进程环境
  ├─ 请求处理流水线 / 缓存降级 / 路由黏性 / 连接参数
  └─ WS / gRPC(S) → isthmus runtime
                       ↑
deploy-vm → common.sh → supervisor argv
                       ↓
                 CLI buildEnv + argv
                       ↓
                 Claude stream-json
```

isthmus Docker Config.Env 只有基础 PATH / DEBIAN_FRONTEND，不能靠 `docker inspect` 的 Env 判断所有配置都没设置。实际设置经部署脚本和 supervisor 进入运行进程，再由核心构造 CLI 子进程环境；匹配版本的 `buildEnv` 不直接继承全部父环境。

脚本把 supervisor 的启动参数一次性组装。改宿主 shell 环境不会自动更新已有进程；本轮未尝试改配置或重启来验证加载行为。

## 2. 转发层启动配置：blue / green

以下值分别从两个运行进程读取，白名单部分与 Docker Config.Env 无差异。两个版本都在运行，但本次未确定谁承接公开入口流量，不能称为当前主流量配置。

isthmus 的启动 transport 为 `grpcs`。转发层存在 WS 参数不代表这条实际调用链正在使用 WS；`CLAUDE_CONSOLE_PIPELINE` 也不能直接当成所有 Claude Code 请求都经过的流水线。具体路由与默认分支仍需核查。

### Anthropic 参数

本表键名前缀为 `PORTUNEX__ANTHROPIC__`。

| 键 | blue / green 值 | 解释边界 |
| --- | --- | --- |
| `DOWNGRADE_1H_CACHE_TO_5M` | `false` | 1h → 5m 缓存降级开关未开启；不是强制所有请求 1h |
| `WS_CONNECT_TIMEOUT_MS` | `15000` | WS 连接超时配置 15 秒 |
| `WS_PREAMBLE_TIMEOUT_MS` | `60000` | WS preamble 阶段超时配置 60 秒，不等于整个生成超时 |
| `WS_IDLE_TTL_SECS` | `30` | WS 空闲保留配置 30 秒，不是 prompt cache TTL |
| `WS_MAX_IDLE_PER_KEY` | `8` | 每 key 最大空闲连接配置；不推断 key 的业务组成 |
| `PREFER_CLIENT_SESSION_ID` | `false` | 未开启优先采用客户端 session ID 的开关 |
| `STRICT_CLIENT_RESTRICTION` | `false` | 启动开关值，不代表没有其他访问控制 |
| `RAW_PASSTHROUGH_KINDS` | 空串 | 不能把空串直接解释成任意请求原样透传 |
| `TOOL_NAME_REWRITE_MODE` | 空串 | 不代表未改写工具名；见下面显式流水线 |
| `CLAUDE_CODE_PIPELINE` | 空串 | 内部默认或分支行为仍需核实 |
| `CLAUDE_DEFAULT_PIPELINE` | 空串 | 同上 |
| `REFUSAL_ZERO_BILLING` | `false` | 仅记配置存在；计费仍属 Sub2API，不迁移该业务权威 |

`CLAUDE_CONSOLE_PIPELINE` 存在明确版本差异，以下只记录启动配置中的阶段名称，不把名称当作已验证实现：

```text
blue:
strip_fallbacks,apply_thinking_display_default,create_client,
rewrite_tool_names,mutate_rewritten_tool_names

green:
reject_enabled_thinking,guard_reasoning_extraction,
downgrade_web_search_tool_version,strip_fallbacks,create_client,
rewrite_tool_names,mutate_rewritten_tool_names,force_global_inference_geo
```

blue 的 `REFUSAL_GUARD_{BLOCK,DUMP,LEARN,PROVIDER_PROTECTION}_ENABLED` 都为 `false`；`REFUSAL_GUARD_PROVIDER_THRESHOLD=2`、`REFUSAL_GUARD_PROVIDER_WINDOW_HOURS=24`。green 的 `REASONING_EXTRACTION_GUARD_ENABLED=false`，对应 `REASONING_EXTRACTION_PROVIDER_THRESHOLD=2`、`REASONING_EXTRACTION_PROVIDER_WINDOW_HOURS=24`。阈值存在不代表开关已经开启。

### 路由、连接与缓存

本表键名前缀为 `PORTUNEX__`；两版本相同。

| 键 | 值 |
| --- | --- |
| `UPSTREAM__CONNECT_TIMEOUT_MS` | `10000`（10 秒） |
| `UPSTREAM__CUSTOM_HOST_NETWORK_ERROR_CRITICAL` | `true` |
| `ROUTING__STICKY_TTL_HOURS` | `24`（会话黏性配置，不是 prompt cache） |
| `ROUTING__STICKY_FOLLOW_SPILL` | `true` |
| `ROUTING__AFFINE_WAIT_MS` | `0` |
| `ROUTING__CONCURRENCY_SPILL_WAIT_MS` | `500` |
| `ROUTING__CONCURRENCY_HEADROOM_RESERVE` | `2` |
| `ROUTING__SLOT_RELEASE_DELAY_MS` | `0` |
| `ROUTING__P2C_CHOICES` | `2` |
| `ROUTING__FLOOD_SENTINEL_SESSION_THRESHOLD` | `3` |
| `ROUTING__LOAD_AWARE_PLACEMENT` / `LOAD_AWARE_SPILL` | `claude_console` |
| `ROUTING__EVEN_NO_STICKY` | `codex`（只记录，不纳入本次 CCMAX 实现范围） |
| `CACHE__STICKY_L1_ENABLED` / `STICKY_L1_TTL_SECS` | `true` / `30` |
| `CACHE__SNAPSHOTS_ENABLED` / `SNAPSHOT_REFRESH_SECS` | `true` / `15` |
| `CACHE__RATE_LIMITED_REFRESH_SECS` | `5`（不能解释成每 5 秒允许一次请求） |
| `REDIS__ENABLED` | `false` |
| `REDIS__STICKY_TTL_SECS` / `PROVIDER_TTL_SECS` / `MODEL_ALIAS_TTL_SECS` | 均 `600`；Redis 开关关闭，不能声称这些 TTL 当前生效 |

上表未取得 Portunex 对应源码实现，因此仅按名称说明用途；不据此确定排队、重试、流中超时或熔断的完整算法。

## 3. isthmus 执行层实际启动参数

快照中有 77 个 `isthmus-vm-base` 容器、77 个 supervisor、77 个 launcher 与 77 个实际 `.isthmus-*` runtime。采样容器使用 Docker `runc`；不能把镜像名称里的 VM 当作已验证的 KVM / Firecracker。

下表来自 77 个实际 runtime 的环境与 argv；端口特例另注明。

| 配置 | 观察值 |
| --- | --- |
| `ISTHMUS_MAX_PROCS` / `--max-procs` | `1024`（上限配置，不是已经启动 1024 个 CLI） |
| `ISTHMUS_MIN_PROCS` / `--min-procs` | `0` |
| `ISTHMUS_ONE_SHOT` | `0`，带 `--no-one-shot` |
| `ISTHMUS_ONE_SHOT_STRATEGY` | `session-reset`；one-shot 关闭，不能解释为每请求重置 |
| `ISTHMUS_CONTINUOUS_TOOL_LOOP` | `1` |
| `ISTHMUS_TOOLS_MCP_SERVER` | `1` |
| `ISTHMUS_STRICT_ISOLATION` | `0`，带 `--no-strict-isolation` |
| `ISTHMUS_PID_NAMESPACE` | `0`，带 `--no-pid-namespace` |
| `ISTHMUS_ENTRYPOINT` | `claude-vscode` |
| `ISTHMUS_NATIVE_CHILD_OAUTH` | `1` |
| `ISTHMUS_HOST_MANAGED_OAUTH_REFRESH_MODE` | `lazy`；是否适用受 native OAuth 分支影响 |
| `ISTHMUS_PROVIDER_TRANSPORT` | `grpcs` |
| `ISTHMUS_GRPCS_PORT` / `--grpcs-port` | `10765` |
| `--port` | `8765` |
| `--grpc-port` | 20 个显式为 `9765`，57 个未显式传参；未传参不等于没有默认监听 |
| `ISTHMUS_REFUSAL_CUTOFF` | `0` |
| `ISTHMUS_TELEMETRY_FEATURES` | `tengu_sysprompt_block` |
| `DISABLE_AUTOUPDATER` | `1` |

隔离注意：这里关闭的是 isthmus 的严格隔离 / 子进程 PID namespace 选项，不是证明 Docker 没有 namespace。匹配版本中 `strictIsolation=false` 会走共享 session anchor 的池分支。不能把这些设置直接作为新 execution-plane 的安全默认值。

## 4. Claude CLI 实际启动参数

多次只读快照观察到 35–37 个 Claude 进程；数量随业务变化，最后缓存环境核查为 36 个。已观察进程可执行路径中的版本均为 `2.1.258`。

| 配置 | 观察值 |
| --- | --- |
| 输入 / 输出 | `--input-format stream-json` / `--output-format stream-json` |
| thinking | `--max-thinking-tokens 31999` |
| 权限交互 | `--permission-prompt-tool stdio` |
| 设置来源 | `--setting-sources=user,project,local` |
| 重试 | `CLAUDE_CODE_MAX_RETRIES=0` |
| 自动压缩 / 更新 | `DISABLE_AUTO_COMPACT=1` / `DISABLE_AUTOUPDATER=1` |
| 工具超时 | `MCP_TOOL_TIMEOUT=2147483647` |
| 工具空闲超时 | `CLAUDE_CODE_MCP_TOOL_IDLE_TIMEOUT=2147483647` |
| 工具细粒度流 | `CLAUDE_CODE_ENABLE_FINE_GRAINED_TOOL_STREAMING=1` |
| 提示历史 | `CLAUDE_CODE_SKIP_PROMPT_HISTORY=1` |
| blocking override | `CLAUDE_CODE_BLOCKING_LIMIT_OVERRIDE=100000000`；只记录内部变量，不承诺上游支持 |
| 调试参数 | `--debug`、`--debug-to-stderr`、`--verbose` |
| 协议辅助参数 | `--enable-auth-status`、`--include-partial-messages`、`--replay-user-messages`、`--no-chrome` |

匹配源码中两个工具超时以毫秒为单位，约 24.9 天，并非 5 分钟或 1 小时。只是配置上限；其他层的取消、断连、租约、业务超时仍可能提前终止任务。

还观察到以下设置：`CLAUDE_CODE_DISABLE_AGENTS_FLEET`、`CLAUDE_CODE_DISABLE_AUTO_MEMORY`、`CLAUDE_CODE_DISABLE_BACKGROUND_TASKS`、`CLAUDE_CODE_DISABLE_CRON`、`CLAUDE_CODE_DISABLE_NONSTREAMING_FALLBACK`、`CLAUDE_CODE_DISABLE_TERMINAL_TITLE` 均为 `1`；`CLAUDE_CODE_EMIT_TOOL_USE_SUMMARIES`、`CLAUDE_CODE_ENABLE_AWAY_SUMMARY`、`CLAUDE_CODE_ENABLE_PROMPT_SUGGESTION`、`CLAUDE_CODE_ENABLE_REMOTE_RECAP`、`ENABLE_CLAUDEAI_MCP_SERVERS`、`ENABLE_TOOL_SEARCH` 均为 `false`。

线上 CLI 可执行文件中确有 `CLAUDE_CODE_PROMPT_CACHE_TTL`、`CLAUDE_CODE_SUBAGENT_PROMPT_CACHE_TTL`、`cache_control` 以及 5m / 1h usage 字段标识符。但最后 36 个 CLI 进程的这两个 TTL 环境变量，以及 `DISABLE_PROMPT_CACHING` / `_HAIKU` / `_SONNET` / `_OPUS` 均未设置。二进制存在标识符只能证明构建包含相关代码，不能证明这次请求选择了哪个 TTL。

没有读取 user / project / local 设置文件或真实请求，因此仍不能排除其中覆盖行为。debug / telemetry 的存在提示后续需检查日志脱敏，但本轮没有读取真实日志，不能据此断言已经记录或泄露提示内容。

## 5. 另外一组“1 小时 / 5 分钟”不是缓存限制

保全核心 `isthmus.readable.mjs:31030` 附近存在 host-managed OAuth 刷新常量：周期 1 小时、刷新失败重试从 10 秒开始且最大退避 5 分钟、持久化重试最大 60 秒、过期安全提前量 30 秒。`fast` / `lazy` 分支行为不同，`lazy` 不做启动 / 到期 / 每小时主动交换。

更重要的是，匹配版本 `34380` 的有效 native 值是 `nativeChildOauth && !pinnedTokenSource`，不是直接使用环境开关。`23297–23321` 检查 cwd 下 `.isthmus-token` 或 home 下 `setup-token` 来源；本轮未读取这些真实凭据文件。`34417–34423` 仅在有效 native 为 false 时构造 host refresh 对象。当前启动设置为 `nativeChildOauth=1`，但缺少启动时 pinned-token 来源证据，不能断言 host refresh 对象一定存在或不存在，更不能把上述 1h / 5m 当成当前已启用的宿主刷新时钟。

其他静态默认值（不是全部实例的已验证有效值）：池空闲 TTL 30 分钟、获取池资源超时 30 秒、控制请求超时 5 秒、ready 等待 30 秒；HTTP / WS `idleTimeout=0`；WS 最大载荷 64 MiB。非 one-shot 的构造器 maxTurns 默认 200，但 `34460` 在 pinned-token 来源存在且非 one-shot 时覆盖为 100000。`27975–27978` 的 continuous-tool-loop + tools-MCP 组合则不注入 CLI 的 `CLAUDE_CODE_MAX_TURNS=1`；这是两个不同层级，不能声称线上固定 200 轮或无限轮。

保全核心还处理 `max_tokens`、`stop_sequences`、`temperature`、`top_p`、`top_k`、`thinking`、`output_config`、`context_management`、`tools` / `tool_choice` 等请求字段。缓存相关分支存在删除部分 `cache_control` 或调整 `scope` 的行为；没有找到固定写入 `ttl=5m/1h` 的实现。这些是静态分支事实，不代表所有请求都经历同一种改写。

## 6. 版本与复核证据

实际运行核心按可执行文件大小分为两组：

| 进程数 / 文件大小 | 每组一个样本的 SHA-256 | 与保全 ELF 关系 |
| --- | --- | --- |
| 57 / 90,920,136 bytes | `d2f3110af974e22ee1c9930817ef4325da54462ecd7eea329c2b536a0ce2374a` | 不同 |
| 20 / 90,924,232 bytes | `facf05c48b9addcdc1760152f8f0862d43635a23a3b1f03bb08d398c0f1260f2` | 相同 |

只对每个大小组抽样一次哈希，不能保证同大小组所有文件内容完全相同。因此既不能把两组当同一构建，也不能把保全源码的未显式参数默认值推到全部 77 个实例。

共享部署脚本线上与保全文件哈希一致：

| 文件 | SHA-256 |
| --- | --- |
| `bin/deploy-vm.sh` | `f303608b82cd87bba610c2a3d640218b299a0e2f0b37c2d4d010ffbcb6d7bac8` |
| `bin/isthmus-supervisor.sh` | `1f78315cecb82c20d81cf95eaa28d19814653b751a85dc368584b553afd20f74` |
| `bin/lib/common.sh` | `3dea8593c5ce389ad1cc9b02098b57cb82dcf45877f3f24f4c811b84af78f845` |
| `bin/lib/backend/docker.sh` | `4cd3d23a0d8c5c5358d9ec5f8312240bfa0debaf9d657926aa326f3e58a2df83` |
| `bin/lib/pkg.sh` | `0e1503ac3339d276f241e3c6fdd725ced795f22088c2fc089bc3d2e30757623e` |

本地可读核心 SHA-256：`60a64417904aa6913683cc73221a360ac36f1b51ac901a1973692117387e1b9e`。源码定位：`buildEnv` 27941–28000；缓存处理 26313–26367；请求参数 26421–26455；池默认 31407–31413；主装配 34417 起。

原始私有资产位于仓库外 `isthmus-static-analysis.HjfIFn`；本次临时白名单采集器位于仓库外 `isthmus-config-audit.hqPAM9`。不把完整 environ、argv、docker inspect、凭据、账号 home 或二进制加入 Git。采用 web-reverse-master 的离线证据核对流程；其离线 selftest 7/7 通过，不代表线上功能测试通过。采集器通过 SSH stdin 在远端运行 Python 标准库，只读取元数据/文件；没有执行保全的 shell / JS / ELF，没有读取进程内存。

## 7. 对本地复刻的影响与剩余核查

下一步应把配置分成 `transport`（连接/流/大小）、`pool`（并发/回收/隔离）、`cli`（启动/工具循环）、`request-policy`（缓存 TTL/请求字段兼容）四组，分别建类型、默认值、允许覆盖范围与测试。此处只是分析建议，本轮不修改应用代码或任何线上配置。

缓存 TTL 应显式保留请求值或按受审计策略处理，并区分 prompt cache、会话黏性、连接池 TTL、OAuth 退避。不要依据某个同名时间常量恢复出错误规则；不要自动复制超长工具超时、关闭隔离或历史请求改写策略。

仍缺：转发流量实际指向哪个版本、空流水线的默认含义、另一核心构建的静态差异、设置文件的非敏感覆盖项、一个受控合成请求的逐跳参数对照。未经这些验证，不宣称已完成线上行为复刻或生产验收。后续如需请求级测试，应使用明确授权的合成请求，不读取历史用户内容来代替测试。

本轮未修改、重启或部署线上服务，未读取数据库、账号凭据和真实业务日志，未触发模型调用。

文档验收：空白检查通过；独立 review 未发现可行动问题，重点复核运行值与请求行为、缓存与 OAuth、pinned-token 覆盖、哈希抽样和主流量未知等证据边界。review 是文档与证据等级核查，不是第二次独立现场采集。本轮只有分析文档，不以应用测试通过或生产验收通过作结。
