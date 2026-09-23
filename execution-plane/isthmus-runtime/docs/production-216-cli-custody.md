# 生产节点 216 的 CLI 托管现状

状态：2026-09-23 只读勘察记录。**本文仅描述观察到的线上现状，不是设计方案，也未对
216.106.185.119 做任何修改。** 全程只执行读取类命令，未写入、未重启、未改配置。

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

supervisor 通过 `EXTRA_ARGS` 向 isthmus 透传的部署期开关（`isthmus-supervisor.sh:102-114`，
默认值取自同段注释）：

| 开关 / 能力 | 默认 |
| --- | --- |
| `--entrypoint=` | `claude-vscode` |
| `--refusal-cutoff` | off |
| `--telemetry-features=` | none |
| `--pulse` | off（3000ms 请求触发 Ping，主进程启动 1000 Pings）|
| tools MCP server | off |
| host-managed OAuth refresh | lazy |
| 请求并发上限 / 最小预热数 | 1024 / 0 |
| session-id isolation | strict on |
| PID namespace | on |

注释同时点名的能力：**session-id isolation、selected telemetry corrections、
refusal interception、tools MCP exec bridge、body-rewrite threading、
child identity profile**。

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

⚠️ **范围限定**：本节全部来自仓库内 `isthmus-runtime/`，而该 slice 按
[fake-transport-slice.md](fake-transport-slice.md) 自述为「synthetic fixtures only，
does not turn the recovered bundle into a service」——它是**探针/实验台**，不是线上
运行的那个 isthmus。佐证：`src/runtime/cli/config.ts` 用 `SYNTHETIC_TOKEN`、
`PROBE_MODEL = "claude-sonnet-5"`、`max_tokens` 固定 128；且 supervisor 透传的
`--entrypoint` / `--refusal-cutoff` / body-rewrite 在该 slice 内**完全没有实现**。

线上跑的是加密的 `/opt/isthmus/dist/isthmus.pkg`（经 `isthmus-unpack` 内存执行），
其源码是否在本仓库内尚未确认。**因此本节结论不能直接外推到生产行为。**

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

1. **无法灰度**。77 VM 共享单副本，切换是原子全量的。这是架构层面的硬限制，
   与 CLI 版本无关。
2. **协议契约破裂**。若线上 isthmus 也采用 6.3 式的穷举白名单，2.1.258 → 2.1.267
   跨 9 个版本，新增事件/字段的概率不低，表现为整批 VM 同时报错。
3. **新旧混跑窗口**。Linux inode 引用计数使已运行进程继续持有旧文件，但新拉起的
   子进程立即是新版。勘察时 `pgrep -fc "versions/2.1.258"` 为 **0**，无活跃会话。
4. **供应链校验断裂**。手工替换会使线上与 6.1 的 5 处闸门全部失配，阻塞后续
   runtimekit 流程。

**此前列为高风险的「自动更新漂移」应予降级**：仓库 `config.ts:44-54` 在 spawn CLI 时
显式注入 `DISABLE_AUTOUPDATER: "1"`（同时还有 `DISABLE_TELEMETRY`、
`CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC`、`CLAUDE_CODE_DISABLE_CRON` 等）。
容器镜像层 Env 里看不到它，是因为它由 isthmus **逐进程注入**而非镜像固化。
线上是否同样注入未核验，但从 9/9 至 9/23 零漂移看，大概率已生效。

## 8. 未核验项

- 四个 `.isthmus-*.env` 的变量集合与取值。
- **线上 `isthmus.pkg` 的源码归属**——第 6 节全部结论悬于此。若线上是另一套代码，
  耦合面需重新测绘。这是升级决策的**首要前置条件**。
- 线上 spawn CLI 时是否真的注入 `DISABLE_AUTOUPDATER=1`。
- 容器到 `downloads.claude.ai` 的实际出网连通性。
- 官方清单 GPG 验签。

## 9. 安全事项

[托管规划第 163-168 行](../../../docs/plans/2026-09-19_16-31-04-isthmus-runtime-custody.md)
已记载：2026-09-19 会话中出现过对应 216 的 SSH 私钥文本，按项目边界应视为**已泄露**，
建议在 216 上移除对应公钥并换发新密钥。

**该轮换至今未执行。** 本轮勘察继续使用了本地同名私钥完成 SSH。这不改变原结论——
轮换仍是待办，且随每次使用而更紧迫。
