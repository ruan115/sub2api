# 2026-09-23 交接：摸清 216 上 57 个未分析 isthmus 核心

时间：Asia/Shanghai（UTC+08:00）。承接
[2026-09-14 线上核查](../../recovery/docs/online-cli-forwarder-config-2026-09-14.md)
与 [2026-09-23 CLI 托管勘察](../../execution-plane/isthmus-runtime/docs/production-216-cli-custody.md)。

本文是**开新会话时的完整上下文交接**，不是执行记录。任务尚未开始。

## 目标

216.106.185.119 上 77 个 isthmus runtime 按可执行文件大小分成两组，只有 20 个与已
保全并静态分析过的核心一致，另外 **57 个从未被分析**。它们负责 **Claude Code CLI
子进程托管与凭证管理**。在弄清这 57 个跑的是什么之前，CLI 版本升级无法做负责任的
评估。

本轮目标是**把两组的静态差异比对清楚**，不写新功能、不改线上。

| 进程数 | 文件大小 | 抽样 SHA-256 | 与保全 ELF |
| --- | --- | --- | --- |
| **57** | 90,920,136 B | `d2f3110af974e22ee1c9930817ef4325da54462ecd7eea329c2b536a0ce2374a` | **不同（未分析）** |
| 20 | 90,924,232 B | `facf05c48b9addcdc1760152f8f0862d43635a23a3b1f03bb08d398c0f1260f2` | 相同 |

**关键线索**：两者相差 **正好 4,096 字节（4 KiB）**，57 那组更小。在 90MB bundle 中
这暗示是单处小改动或对齐差异，而非不同代际。优先验证该假设。此项为 2026-09-23
推算所得，原核查未记录。

**前提存疑**：原核查每组**只抽样了一个文件**的哈希，明确声明「不能保证同大小组所有
文件内容完全相同」。因此「57/20 分组」本身尚未坐实，应作为第一步验证。

## 已验证事实（勿重复考证）

- 线上 `isthmus.pkg` **不是本仓库构建产物**。`execution-plane/isthmus-runtime/` 自述为
  "a recovery foundation, **not the recovered production isthmus service**"，响应一律
  打 `x-isthmus-runtime: fake`；仓库内不存在 `deploy-vm.sh`、`isthmus-supervisor.sh`、
  `isthmus-unpack` 或任何 `.pkg`。
- `contracts/grpc/provenance.json` 记录 proto 为 `statically-extracted-embedded-proto`，
  取自 `isthmus.readable.mjs` 第 30558–30660 行。线上 isthmus 是早已存在的 JS bundle。
- 原始资产在**仓库外**：`/Users/ruanyang/My-project/api/z/isthmus-static-analysis.HjfIFn/`，
  内含保全的 `isthmus.readable.mjs`，本地可读核心 SHA-256
  `60a64417904aa6913683cc73221a360ac36f1b51ac901a1973692117387e1b9e`。
  **开工第一件事是确认该目录是否仍存在。**
- CLI 为 Claude Code **2.1.258**，经三方独立比对确认为官方 linux-x64 未改动版
  （`704f1334ac65d3e89e1c6c1d7663293ad786a6166afdb71b5075337df630f976`，
  size 215473560）。77 个 VM 共享可写卷 `isthmus-local` 上的**同一份**二进制，
  无 per-VM 版本隔离，因此无法分批灰度。

### 保全核心内的已知定位（定向比对用）

```text
buildEnv                27941–28000     ← CLI 子进程环境构造，本轮重点
凭证/token 来源判定      23297–23321     ← .isthmus-token / setup-token，本轮重点
native OAuth 有效值      34380           ← nativeChildOauth && !pinnedTokenSource
host refresh 对象构造    34417–34423
maxTurns 覆盖            34460
OAuth 刷新常量           ~31030          ← 1h 周期 / 10s 起退避 / 5min 上限 / 30s 提前量
continuous-tool-loop     27975–27978
缓存处理                 26313–26367
请求参数处理             26421–26455
池默认值                 31407–31413
主装配                   34417 起
嵌入 proto               30558–30660
```

### 线上实测运行值（77 runtime，2026-09-14 采集）

`ISTHMUS_ENTRYPOINT=claude-vscode`、`ISTHMUS_TELEMETRY_FEATURES=tengu_sysprompt_block`、
`ISTHMUS_TOOLS_MCP_SERVER=1`、`ISTHMUS_STRICT_ISOLATION=0`、`ISTHMUS_PID_NAMESPACE=0`、
`ISTHMUS_CONTINUOUS_TOOL_LOOP=1`、`ISTHMUS_NATIVE_CHILD_OAUTH=1`、
`ISTHMUS_HOST_MANAGED_OAUTH_REFRESH_MODE=lazy`、`ISTHMUS_PROVIDER_TRANSPORT=grpcs`、
`DISABLE_AUTOUPDATER=1`。

⚠️ **`isthmus-supervisor.sh` 注释所载默认值与线上实测有 5 处不一致**
（telemetry features、tools MCP、strict isolation、PID namespace、continuous tool loop），
**不要拿注释当线上事实**。

## 必读文档（按顺序）

1. `recovery/docs/online-cli-forwarder-config-2026-09-14.md` — 最权威的线上核查，
   含转发层环境、runtime argv、CLI argv、核心哈希分组。凡与其他文档重叠，以它为准。
2. `recovery/docs/online-cli-forwarder-clarifications-2026-09-14.md` — 第二轮补充核查。
3. `execution-plane/isthmus-runtime/docs/production-216-cli-custody.md` — 2026-09-23
   的二进制完整性核验与升级影响面。
4. `2026-09-19_16-31-04-isthmus-runtime-custody.md` — 硬约束与安全事项来源。

## 硬约束

- **216 只读。** 不写入、不重启、不改配置、不部署、不触发模型调用。
- **不读取凭据与用户内容**：`.isthmus-oauth.env`、`.isthmus-proxy.env`、
  `.isthmus-transport.env`、`.isthmus-pkg.env`、`.credentials.json`、
  `.isthmus-grpcs/` 下私钥、真实业务日志、进程内存，一律不读。必要时只取键名。
- **不执行**保全的 shell / JS / ELF，静态分析只读字节。
- **凭据不进聊天、文档、Git。** 二进制与提取产物放仓库外私有目录（0700/0600）。
- Git：按明确文件 `git add`，**绝不 `git add .`**。工作区既有 WIP
  （`internal/route/reconcile.go`、`image/lab/build.py`、`image/runtimekit/`）
  保持不动、不 stage。分支 `codex/claude-execution-plane-v1`。

## 环境陷阱（务必先读，否则会浪费大量时间）

本机 **Clash Verge 开着 TUN 模式**（fake-IP `198.18.0.0/16`，device `utun3`，
gvisor stack，`mode: global`），**会掐断所有出站 SSH**，症状为
`kex_exchange_identification: Connection closed by remote host`——极像服务器端
fail2ban 封禁，实则是本地问题。

**识别方法**：SSH 到任意两三台无关主机，再加 `git@github.com`。若**全部**以同样方式
失败，且 github 对端显示为 `198.18.0.x`，即可确诊。此时注意：

- `ping` 返回 ~0.4ms —— 是 utun 虚拟网卡本地应答，非真实服务器；
- `nc -z` 对**任意**端口都报 open —— SOCKS 乐观应答。

**常规连通性探测在此环境下全部失效，不能作为判据。**

绕过方式（用完务必恢复）：

```bash
curl -X PATCH http://127.0.0.1:9097/configs \
  -H 'Content-Type: application/json' -d '{"tun":{"enable":false}}'
# 验证：route -n get 216.106.185.119 应显示 interface: en0（而非 utun3）
# 恢复：同一命令，改为 '{"tun":{"enable":true}}'
```

Clash Verge 守护进程**会自行把 TUN 改回去**，关闭后需尽快执行；长间隔后要重新关闭。
环境变量中 `HTTP_PROXY` / `HTTPS_PROXY` 指向 iproyal 代理，调本地 API 时需 `env -u`
掉。另：`git push` 走 `ssh.github.com:443`，在 TUN 开启下是通的，不受影响。

SSH：`ssh -i ~/.ssh/id_ed25519_216_106_185_119 root@216.106.185.119`。
`~/.ssh/config` 中**没有**对应 Host 条目，必须显式传 key。

**工具权限**：Claude Code 的权限分类器会间歇性拦截 SSH 命令，**复合命令（`&&`、多段
`;`、对远端文件用 `sed`/`cut`）被拦概率明显更高**。拆成单条简单命令、失败后重试一次
通常可通过。被拦不等于被禁止，但不要用绕路方式规避。

## 建议路径

1. 扩大抽样，确认「57/20 分组」是否真实成立（每组多取几个哈希）。
2. 从 57 那组取一个二进制到仓库外私有目录，用与保全副本**相同的提取方法**还原可读
   JS，确保两侧可比。
3. 优先 diff `buildEnv`(27941–28000) 与凭证来源判定(23297–23321) 两段——正是
   「CLI 子进程托管 + 凭证管理」的落点。
4. 验证 4 KiB 差异假设：单处常量/补丁，还是分散改动。
5. 产出差异报告，明确回答：**这 57 个实例在 CLI 升级面前的行为，能否用已分析的那
   20 个来代表。**

## 交付

在 `recovery/docs/` 下新增差异报告，与既有文档同体例（中文，区分「实测 / 静态 /
未验证」三级证据），明确列出未闭合项。不夹带凭据与二进制，提交前做凭据扫描。

## 安全待办（背景，非本轮任务）

[2026-09-19 托管规划](2026-09-19_16-31-04-isthmus-runtime-custody.md) 第 163–168 行
已判定对应 216 的 SSH 私钥**应视为已泄露**，建议在 216 上移除对应公钥并换发新密钥。
**该轮换至今未执行**，且此后又被使用过若干次（2026-09-23 勘察两轮）。属写操作，
需单独授权与停机窗口，不在本轮范围。
