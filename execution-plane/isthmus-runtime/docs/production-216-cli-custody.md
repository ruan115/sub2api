# 生产节点 216 的 CLI 托管现状

状态：2026-09-23 只读勘察记录。**本文仅描述观察到的线上现状，不是设计方案，也未对
216.106.185.119 做任何修改。** 全程只执行读取类命令，未写入、未重启、未改配置。

**前序工作**：[2026-09-14 线上 CLI / 转发层配置核查](../../../recovery/docs/online-cli-forwarder-config-2026-09-14.md)
已对同一主机做过更深入的采集（转发层环境、77 个 runtime 的 argv、CLI 进程参数、
核心哈希分组）。本文聚焦**二进制完整性与升级影响面**，是对该核查的补充而非替代；
凡二者重叠处，以 9/14 的实测值为准，本文已据此修正多处推断。

> **边界冲突声明**：[2026-09-19 托管规划](../../../docs/plans/2026-09-19_16-31-04-isthmus-runtime-custody.md)
> 第 28、95 行写明「不动 216」「216.106.185.119 保持不动」。本轮勘察由用户在
> 2026-09-23 会话中明确指派，属于对该边界的一次显式豁免，且限定为只读。
> 后续任何对 216 的写操作仍应重新取得授权。

## 1. 节点拓扑

主机 `216.106.185.119`，hostname `tianliyun`，Debian 13 (trixie)，内核
`6.12.48+deb13-amd64`。

- 容器共 82 个：77 个 `isthmus-vm-*`（镜像 `isthmus-vm-base:latest`，384MB，13 天前构建），
  外加 `portunex-blue` / `portunex-green` 蓝绿两套（`portunex-server`，157MB）。
- systemd 侧运行 `docker`、`containerd`、`portunex-monitor`、`cron`、`ssh` 等，
  无额外自建服务单元。
- 宿主工具目录 `/opt/isthmus`：`bin/deploy-vm.sh`、`bin/isthmus-supervisor.sh`、
  `bin/isthmus-unpack`、`bin/isthmus-egress.sh`、`dist/isthmus.pkg`（38MB）。
- 宿主 PATH 内**没有** `claude` / `node` / `npm`，CLI 不在宿主层。

## 2. CLI 的实际托管方式：单副本共享卷

每个 VM 挂载四个卷，全部可写（`RW=true`）：

| 挂载点 | 卷名 | 性质 |
| --- | --- | --- |
| `/app` | `isthmus-app` | 共享 |
| `/home/claude` | `isthmus-vm-<N>-home` | **每 VM 独立** |
| `/opt/bun` | `isthmus-bun` | 共享 |
| `/home/claude/.local` | `isthmus-local` | **共享，CLI 在此** |

CLI 布局（原生安装器形态，非 npm 全局安装）：

```text
/home/claude/.local/bin/claude
  -> /home/claude/.local/share/claude/versions/2.1.258   (215,473,560 字节)
```

`isthmus-local` 卷内除上述符号链接与版本文件外，仅有 `state/claude/locks/`，
**没有包装脚本、没有额外二进制、没有配置注入**。

**关键结论**：`docker ps --filter volume=isthmus-local` 返回 **77**。即全部 VM 共用
**同一个 inode 上的同一份二进制**。不存在 per-VM 版本隔离，因此也**不存在按 VM 灰度
的可能**——改动符号链接即全量生效。CLI 未被烘进镜像，与仓库 `runtimekit/` 的设计
不一致（见第 5 节）。

## 3. 完整性核验：二进制未被改动

对 `linux-x64` / 2.1.258 做三方独立比对，全部一致：

| 来源 | SHA256 |
| --- | --- |
| 线上实测 `sha256sum` | `704f1334ac65d3e89e1c6c1d7663293ad786a6166afdb71b5075337df630f976` |
| 官方 `manifest.json`（HTTPS 实时拉取） | 同上 |
| 仓库 `image/locks/toolchain-linux-2026-09-17.json` | 同上 |

文件大小与官方清单的 `215473560` 严格相等。官方清单元数据：commit
`b3cd543a1f6fcdf4d8fabc0f5e5538d2ee7f38e1`，构建于 `2026-09-01T22:02:56Z`。

线上二进制即 Anthropic 官方发布版，逐字节相同，未打补丁、未二次打包。

> 未做：清单 GPG 签名验签（`acquire.py:19` 记录指纹
> `31DDDE24DDFAB679F42D7BD2BAA929FF1A7ECACE`）。但仓库 lockfile 于 9/17 独立生成，
> 与 9/23 实时拉取的官方清单吻合，伪造需同时攻陷两条独立链路。

## 4. 行为层：二进制干净，运行期被深度包装

调用链：`isthmus-supervisor.sh` → `isthmus`（Bun 应用，经 `isthmus-unpack` 解密后
内存执行）→ Claude Agent SDK → `claude` 子进程。supervisor 第 4 行注释原文：
"The Claude Agent SDK's claude subprocess can crash mid-stream"。

supervisor 通过 `EXTRA_ARGS` 向 isthmus 透传部署期开关。下表**左列为 supervisor
注释所载默认值**（`isthmus-supervisor.sh:102-114`），**右列为 77 个 runtime 的实测值**
（引自 [2026-09-14 线上核查](../../../recovery/docs/online-cli-forwarder-config-2026-09-14.md) 第 109-128 行）：

| 开关 | 注释默认 | **线上实测** |
| --- | --- | --- |
| `ISTHMUS_ENTRYPOINT` | `claude-vscode` | `claude-vscode` |
| `ISTHMUS_REFUSAL_CUTOFF` | off | `0` |
| `ISTHMUS_TELEMETRY_FEATURES` | none | **`tengu_sysprompt_block`** |
| `ISTHMUS_TOOLS_MCP_SERVER` | off | **`1`（开启）** |
| `ISTHMUS_STRICT_ISOLATION` | strict on | **`0`（关闭）** |
| `ISTHMUS_PID_NAMESPACE` | on | **`0`（关闭）** |
| `ISTHMUS_CONTINUOUS_TOOL_LOOP` | off | **`1`** |
| `ISTHMUS_MAX_PROCS` / `MIN_PROCS` | 1024 / 0 | `1024` / `0` |
| `ISTHMUS_PROVIDER_TRANSPORT` | — | `grpcs`（端口 10765）|
| `DISABLE_AUTOUPDATER` | — | **`1`** |

**注释默认值与线上实际有 5 处不一致**，不能拿 supervisor 注释当线上事实。

### 4.1 CLI 子进程的实测启动参数

同一核查第 136-154 行记录了 35–37 个 Claude 进程的实际 argv，版本均为 `2.1.258`：

```text
--input-format stream-json  --output-format stream-json
--max-thinking-tokens 31999  --permission-prompt-tool stdio
--setting-sources=user,project,local
--enable-auth-status --include-partial-messages --replay-user-messages
--no-chrome --debug --debug-to-stderr --verbose
```

环境侧：`CLAUDE_CODE_MAX_RETRIES=0`、`DISABLE_AUTO_COMPACT=1`、
**`DISABLE_AUTOUPDATER=1`**、`MCP_TOOL_TIMEOUT=2147483647`（约 24.9 天）、
`CLAUDE_CODE_MCP_TOOL_IDLE_TIMEOUT` 同值、
`CLAUDE_CODE_ENABLE_FINE_GRAINED_TOOL_STREAMING=1`、
`CLAUDE_CODE_SKIP_PROMPT_HISTORY=1`、`CLAUDE_CODE_BLOCKING_LIMIT_OVERRIDE=100000000`。

**`DISABLE_AUTOUPDATER=1` 已在线上确认**，本文第 7 节的「自动更新漂移」风险据此关闭。

### 4.2 body-rewrite 的实际作用面

同一核查第 168 行：保全核心处理 `max_tokens`、`stop_sequences`、`temperature`、
`top_p`、`top_k`、`thinking`、`output_config`、`context_management`、
`tools` / `tool_choice`，且「缓存相关分支存在删除部分 `cache_control` 或调整 `scope`
的行为」。**改写面是 Anthropic Messages API 的请求字段**，而非 CLI 私有结构——这一层
相对稳定，跨 CLI 小版本受影响的概率低于流式事件层。

配置层已核验为干净的部分：

- `~/.claude/settings.json` 内容仅 `{"theme": "dark"}`，无 hooks、无 permissions
  覆写、无 MCP 注入。
- `~/.bashrc` 为 Debian 原版，逐行未改；env 文件不经 shell 加载，由 supervisor 注入。
- 无 `CLAUDE.md` 注入，无 `/etc/claude-code/managed-settings.json`。

每 VM home 内另有 `.isthmus-proxy.env`、`.isthmus-transport.env`、
`.isthmus-oauth.env`、`.isthmus-pkg.env` 及 `.isthmus-grpcs/`（含
`portunex-client.crt`、`ca.crt`、`server.key`）。**这些文件含凭据，本轮未读取内容，
也未取键名**——运行期注入的具体变量集合仍属未核验项。

## 5. 与仓库设计的分歧

仓库内未跟踪的 `image/runtimekit/` 走的是**另一条路线**：构建期把 claude 烘进镜像
`/opt/isthmus/bin/claude`，由 lockfile 钉死 SHA256 并在 `recipe.py:49` 用
`elif name == "claude" and version == "2.1.258"` 硬校验。

**该设计尚未上线。** 线上跑的仍是共享可写卷方案。评估任何 CLI 变更时必须以线上形态
为准，不能按仓库设计推断。

## 6. 仓库侧对 2.1.258 的硬耦合

⚠️ **范围限定（已确证）**：线上的 `isthmus.pkg` **不是本仓库构建产物**。

证据链：

1. 仓库内**不存在** `deploy-vm.sh`、`isthmus-supervisor.sh`、`isthmus-unpack`
   或任何 `.pkg`——全部只存在于 216 的 `/opt/isthmus/`。
2. `isthmus-runtime/README.md:3-8` 自述为 "a recovery foundation, **not the recovered
   production isthmus service**"，且每个响应都打 `x-isthmus-runtime: fake`。
3. `contracts/grpc/provenance.json` 记录 proto 来源为
   `kind: "statically-extracted-embedded-proto"`，取自 `isthmus.readable.mjs`
   第 30558-30660 行——线上 isthmus 是一个**早已存在的 JS bundle**，本仓库是在对它做
   静态分析后的恢复/重写。原始资产在仓库外 `isthmus-static-analysis.HjfIFn`。
4. `src/runtime/cli/config.ts` 用 `SYNTHETIC_TOKEN`、`PROBE_MODEL = "claude-sonnet-5"`、
   `max_tokens` 固定 128、`--setting-sources ""`；而线上实测是真实 OAuth、
   `--setting-sources=user,project,local`（见 4.1）。二者是不同东西。

**更进一步——线上自身就不是一个构建。** [2026-09-14 核查](../../../recovery/docs/online-cli-forwarder-config-2026-09-14.md)
第 172-179 行按可执行文件大小把 77 个 runtime 分成两组：

| 进程数 / 大小 | 抽样 SHA-256 | 与保全 ELF |
| --- | --- | --- |
| **57** / 90,920,136 B | `d2f3110af974e22e…` | **不同** |
| 20 / 90,924,232 B | `facf05c48b9addcd…` | 相同 |

即 **77 个实例里只有 20 个对应已被分析过的那份核心，另外 57 个是未经分析的构建**。
原文亦注明每组只抽样一次哈希，不能保证同组内部完全一致。

因此本节以下内容**仅描述仓库 slice 的自我约束，对线上无约束力**；线上耦合面的权威
参考是上述 9/14 核查，而非本仓库代码。

### 6.1 版本闸门（4 处硬编码）

| 位置 | 形式 |
| --- | --- |
| `image/artifacts/binaries.py:49` | `elif name == "claude" and version == "2.1.258"`，否则 `unsupported_binary_identity` |
| `image/lab/acquire.py:104` | `if manifest.get("version") != "2.1.258": raise release_manifest_version_mismatch` |
| `image/lab/toolchain.py:61` | `test "$(../bin/claude --version)" = '2.1.258 (Claude Code)'` |
| `image/lab/cli.py:51` | 同上断言 |

加上 lockfile 的 SHA256，升级需同步改动 **5 处版本引用 + 2 个哈希**。

### 6.2 命令行契约

`src/runtime/cli/config.ts:37-41` 硬编码调用参数，其中 `--print`、`--safe-mode`、
`--strict-mcp-config`、`--input-format stream-json`、`--output-format stream-json`、
`--include-partial-messages` 均为易变面；`config.test.ts:13-17` 还把这些参数断言死。

### 6.3 流式事件白名单（**fail-closed**）

`src/runtime/cli/events.ts:21-26` 用穷举白名单校验 CLI 输出，越界即 `fail()`：

- `recordTypes` / `eventTypes` / `systemSubtypes` / `usageExtensions` 四张表
- `events.ts:268` — `stop_reason` 仅接受 `end_turn`/`max_tokens`/`stop_sequence`
- `events.ts:61-76` — usage 字段名穷举，新增计数器即 `cli_output_unsupported`
- `events.ts:237` — content block 仅接受纯 `text`，多一个键就拒

**此处修正一个此前的误判**：该 slice 的设计是**失败即报错**，不是静默降级。新版 CLI
一旦引入新事件类型、新 `stop_reason` 或新 usage 字段，会直接抛
`cli_output_unsupported` / `cli_output_invalid`。这对排障是好事——但意味着升级大概率
**立刻可见地失败**，而非悄悄跑偏。

## 7. CLI 升级的影响面

线上 2.1.258（9/9 安装）；官方 stable 频道 2.1.267，npm latest 2.1.280。

按风险排序：

1. **57/77 实例的行为无人知晓**。它们跑的核心从未被静态分析过（见第 6 节）。在这些
   实例上换 CLI 版本等于盲飞——既不知道它们解析 CLI 输出的严格程度，也不知道
   body-rewrite 分支与 20 个已分析实例是否一致。**这是当前最大的单点风险。**
2. **无法灰度**。77 VM 共享单副本二进制，切换是原子全量的。架构层硬限制，与 CLI
   版本无关；即使想只在 20 个已知实例上试，也做不到。
3. **`tengu_sysprompt_block` 的耦合**。线上 `ISTHMUS_TELEMETRY_FEATURES` 实测为该值
   （非注释所称的 none）。`tengu` 是 Claude Code 的内部代号，该 feature 名几乎必然
   匹配 CLI 内部标识符，属典型的跨版本易变面。
4. **新旧混跑窗口**。Linux inode 引用计数使已运行进程继续持有旧文件，新拉起的子进程
   立即是新版。本轮勘察时 `pgrep -fc "versions/2.1.258"` 为 **0**（无活跃会话），但
   9/14 核查观察到 35–37 个 CLI 进程，说明该数值随业务波动，需在操作前即时复查。
5. **供应链校验断裂**。手工替换会使线上与 6.1 的 5 处闸门全部失配，阻塞后续
   runtimekit 流程。

相对**低**的风险（此前高估，现修正）：

- **自动更新漂移——已关闭**。线上实测 `DISABLE_AUTOUPDATER=1`（见 4.1），不会自更新。
- **body-rewrite 破裂**——改写面是 Messages API 请求字段（见 4.2），非 CLI 私有结构，
  跨小版本相对稳定。

## 8. 升级前的前置条件

按依赖顺序：

1. 先弄清 **57 个未分析实例**跑的是什么构建、与 20 个已分析实例差异何在。不解决这条，
   任何升级评估都无效。
2. 确认 `tengu_sysprompt_block` 在目标 CLI 版本中是否仍然有效。
3. 取得一个**受控合成请求**的逐跳参数对照（9/14 核查第 201 行亦列为待办），以确认
   当前请求路径的实际行为，而非静态分支推断。

## 9. 未核验项

- 四个 `.isthmus-*.env` 的变量集合与取值。
- 57 个未分析核心的静态差异（9/14 核查第 201 行「另一核心构建的静态差异」同列此项，
  至今未闭合）。
- 转发流量实际指向 blue 还是 green。
- 容器到 `downloads.claude.ai` 的实际出网连通性。
- 官方清单 GPG 验签。

## 10. 安全事项

[托管规划第 163-168 行](../../../docs/plans/2026-09-19_16-31-04-isthmus-runtime-custody.md)
已记载：2026-09-19 会话中出现过对应 216 的 SSH 私钥文本，按项目边界应视为**已泄露**，
建议在 216 上移除对应公钥并换发新密钥。

**该轮换至今未执行。** 本轮勘察继续使用了本地同名私钥完成 SSH。这不改变原结论——
轮换仍是待办，且随每次使用而更紧迫。
