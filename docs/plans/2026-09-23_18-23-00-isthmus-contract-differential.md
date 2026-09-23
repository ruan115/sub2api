# 2026-09-23 18:23 — 契约对拍：把复原锚点从「实现字节」换成「协议契约」

时间：Asia/Shanghai（UTC+08:00）。承接
[业务逻辑总结 §10b.5](2026-09-23_18-05-00-isthmus-business-logic-summary.md)。
本文把该节的方法论建议细化为可执行方案。**尚未开工。**

## 0. 为什么要换锚点

线上由他人以**每日多次**的节奏发布。2026-09-23 一次全量部署就作废了 9/14 的静态
分析基线；57/20 构建分裂消失不是债务减少，而是**清零重置**——新构建同样无人分析。

当前方法（提取 `isthmus.readable.mjs` 做 diff）产生知识的速度慢于知识失效的速度，
**结构性追不上**。

换锚点后：把线上当黑盒，用**契约 + 对拍**验证兼容性。同事再发十个版本，对拍只会
告诉你「这里变了」，不会让既有工作作废。每次发版从「推倒重来」变成「自动产出一条
新知识」。

依据：`contracts/grpc/messages.proto` 是逐字节保全的，**协议的变化速度远低于实现**。

## 1. 已有的地基（不重造）

本方案几乎不需要新建基础设施。仓库里已经有完整的「真实 CLI × 合成上游」跑道：

| 部件 | 路径 | 现状 |
| --- | --- | --- |
| 合成上游 stub | `isthmus-runtime/test/cli/stub.ts`（9.5 KB） | 已可用，返回 fixture，非模型 |
| 真实 CLI 往返 | `isthmus-runtime/test/cli/roundtrip.smoke.ts`（8.7 KB） | 已可用，显式 opt-in |
| CLI 启动契约 | `isthmus-runtime/src/runtime/cli/config.ts` | argv/env 集中定义 |
| 流式解码器 | `isthmus-runtime/src/runtime/cli/events.ts`（13 KB） | 穷举白名单，fail-closed |
| 子进程管理 | `isthmus-runtime/src/runtime/cli/process.ts` | 单子进程、超时、进程组回收 |
| 保全 proto | `isthmus-runtime/contracts/grpc/messages.proto` | 逐字节保全 + provenance |

链路已经是 `HTTP → 真实 CLI → 合成 loopback 上游 → JSON/SSE`，运行在
`network=none`、UID 1000、只读无特权容器内。

**缺的只有一件事**：`config.ts:3` 把 CLI 路径写死为单版本——

```ts
export const CLI_PATH = "/opt/isthmus-probe/bin/claude";
```

把它参数化，就能让同一请求同时穿过两个 CLI 版本。**这就是对拍。**

## 2. 分层与优先级

对拍分两层，价值和成本差别很大：

| 层 | 内容 | 成本 | 是否碰生产 |
| --- | --- | --- | --- |
| **B：CLI 契约** | argv/env/stream-json 输入输出 | **零**（合成上游） | 否 |
| **A：线路契约** | Portunex ↔ isthmus，messages.proto | 零（golden 帧） | 否 |
| **L：实时对拍** | 同请求打线上与本地比响应 | 真实计费 | **是** |

**B 层是全部价值的来源**，因为 CLI 升级正是知识失效的主因，而它**零成本、不联网、
不碰生产**。阶段 1–2 全在 B 层。L 层默认不做（见 §7）。

## 3. 阶段 1：CLI 版本对拍（本方案核心）

### 3.1 取得两个 CLI 版本

走既有 `image/lab/acquire.py` 的校验路径（官方下载 + manifest 签名 + SHA-256），
**不从生产机拷贝**——官方下载加哈希校验比复制生产文件更干净，也符合既有设计。

| 版本 | SHA-256 | 大小 |
| --- | --- | --- |
| 2.1.258（线上旧） | `704f1334ac65d3e89e1c6c1d7663293ad786a6166afdb71b5075337df630f976` | 215,473,560 |
| 2.1.280（线上新） | `1e08503dbdf3c2cb0d706d32f3408277388d1c76ef108673e8fe42c1b322925b` | 233,709,640 |

2.1.280 对 `acquire.py` 是新增制品，需放宽其硬编码版本闸门（见 §5）。
制品落仓库外私有目录，不入 Git。

### 3.2 参数化 CLI 路径

`config.ts` 的 `CLI_PATH` 改为可注入（保留当前值为默认），`buildConfig` 接受
版本标识。**不改 argv/env 的任何默认值**——那些本身就是被测契约。

### 3.3 固定请求矩阵

对拍要有覆盖面。每个用例是一条**确定性**请求，由 stub 给出**固定**响应：

| # | 用例 | 探测什么 |
| --- | --- | --- |
| 1 | 单条文本，非流式 | 基础 record 序列 |
| 2 | 单条文本，流式 | `message_start` / `content_block_*` / `message_delta` 形状 |
| 3 | 多段 content_block | 分块边界 |
| 4 | 上游 4xx | 错误信封与 `result` 形状 |
| 5 | 上游 5xx | 重试行为（`CLAUDE_CODE_MAX_RETRIES=0` 下应不重试） |
| 6 | 上游流中断 | 半截流的收尾记录 |
| 7 | 客户端取消 | 取消路径与进程回收 |
| 8 | 超长输出触发 max_tokens | `stop_reason` 取值 |
| 9 | Unicode / 多字节边界 | 分块切在字符中间的处理 |
| 10 | 上游返回未知 usage 字段 | `usageExtensions` 白名单是否 fail-closed |

stub **只返回 fixture，永远不做推理**。用例 10 是故意投毒，用来验证解码器对新字段
的行为——这正是 CLI 升级时最可能踩的坑。

### 3.4 采集与比对

对每个 `(用例, 版本)` 组合采集**原始 stream-json 字节流**，规范化后比对：

- 规范化需剥离：时间戳、session_id、uuid、进程号、耗时字段。
  **规范化规则本身要写成代码并单测**，否则会掩盖真实差异。
- 比对产出三类：`相同` / `新增` / `变更`。
- 结果落 `test/cli/golden/<用例>/<版本>.jsonl`，**进 Git**（合成数据，无凭据）。

### 3.5 阶段 1 验收

- 10 个用例 × 2 版本 = 20 条 golden 全部采集成功。
- 产出一份差异报告：**2.1.258 → 2.1.280 之间 stream-json 契约到底变了什么**。
- 该报告必须能回答：`events.ts` 的五张白名单是否需要更新、哪几项。
- 若差异为空，同样是有效结论（说明这次升级对本适配器无影响），**不算失败**。

## 4. 阶段 2：白名单改为 golden 派生

### 4.1 问题

`events.ts:21-26` 手工维护四张穷举表 + `stop_reason` 取值：

```ts
recordTypes     = ["system","assistant","user","rate_limit_event","result","stream_event"]
eventTypes      = ["message_start","content_block_start","content_block_delta",
                   "content_block_stop","message_delta","message_stop","ping","error"]
systemSubtypes  = ["init","hook_started",...,"local_command_output"]
usageExtensions = ["service_tier","server_tool_use","inference_geo","speed","cache_creation"]
```

手工维护意味着每次 CLI 升级都要人肉比对，**正是当前方法论问题的缩影**。

### 4.2 做法

- 白名单由 golden 语料**生成**，生成物入库并可审查（不是运行期动态放行）。
- 保持 **fail-closed 不变**——这是该切片的正确设计，不能为了「兼容」改成静默降级。
- 新增一个测试：golden 里出现白名单外的取值即 FAIL，并指出是哪个用例哪个字段。

这样，升级 CLI 的动作变成：跑一次采集 → 看差异报告 → 决定是否接纳新取值。
**从「人肉考古」变成「机器告知」。**

## 5. 阶段 3：版本闸门单一化

当前 5 处硬编码 `2.1.258`，升级要改 5 个地方且容易漏：

```text
image/artifacts/binaries.py:49    elif name == "claude" and version == "2.1.258"
image/lab/acquire.py:104          manifest.get("version") != "2.1.258"
image/lab/toolchain.py:61         test "$(../bin/claude --version)" = '2.1.258 (Claude Code)'
image/lab/cli.py:51               同上断言
image/locks/toolchain-linux-2026-09-17.json    sha256 ×2
```

改为**单一真源**：版本与哈希只存在于 lockfile，其余四处从 lockfile 读取。
lockfile 支持同时登记多个版本（对拍需要两个并存）。

**此项排在阶段 1 之后**：先用对拍确认 2.1.280 的契约影响，再决定是否把线上版本
提升为基线。若对拍显示有破坏性差异，可能反而要建议回滚，此时提前对齐是添乱。

## 6. 阶段 4：线路契约 golden（A 层）

proto 逐字节保全，是最稳定的锚点。

- 用保全 proto 生成编解码 golden 帧，覆盖 `messages.proto` 内各消息类型。
- 验证本地实现能正确编解码这些帧。
- **不需要连接线上**——proto 定义即契约。

优先级低于 B 层：线路协议变化频率远低于 CLI。

## 7. L 层：实时对拍（默认不做）

「同一请求同时打线上与本地比响应」是最直接的对拍，但：

- **产生真实计费**（真实模型调用）；
- **触碰生产**，违反既有边界；
- 需要真实 OAuth 凭据；
- 同事正以每日多次的节奏改线上，对拍窗口不稳定。

因此 **L 层不纳入本方案的默认流程**。仅在以下条件全部满足时另行申请：
用户单独授权 + 明确的合成请求 + 计费上限 + 与同事约定的变更窗口。

阶段 1–3 完成后，B 层契约已锁定，L 层的边际价值大幅下降。

## 8. 边界

- **不碰生产**：阶段 1–4 全部离线，不连接 216 与 14.1.29.250，不触发模型调用。
- **stub 永远不是模型**：只返回 fixture；任何「让 stub 更聪明」的改动都偏离目的。
- **不放宽 fail-closed**：解码器遇未知取值仍应失败，只是失败信息要更好。
- **不改既有默认**：`bun test` 默认仍是离线 mock；对拍是显式 opt-in 入口。
- **凭据全合成**，不提供真实 token；制品落仓库外私有目录（0700/0600），不入 Git。
- Git：按明确文件 `git add`，**绝不 `git add .`**；既有 WIP
  （`internal/route/reconcile.go`、`image/lab/build.py`、`image/runtimekit/`）
  保持不动、不 stage。

## 9. 与验收台账的关系

本方案**不自行加分**。但它改变了 I/R 两个模块「什么算通过」的定义：

- `isthmus-container-delivery-v1.md` 第 9 行把 100 分锚定在「已保全的
  2026-09-14/15 材料」，该参照物已随 9/23 部署失效。
- 建议把 I/R/C/L 的验收从「匹配保全构建」改为「**通过契约对拍套件**」。
- 权重亦存疑：L（凭据与完整生命周期）仅 10 分且为 0%，但按真实架构，
  凭据与账号生命周期是系统核心；I（镜像复建）占 15 分却相对容易。

**权重调整与 I1 是否撤分，须用户裁定后另记版本**，不在本方案内静默变更。

## 10. 不做什么

- 不再做 `isthmus.readable.mjs` 的通读式静态考古（定向查证仍可用）。
- 不追求字节级复刻线上实现——**替换的标准是客户端分辨不出差异**，不是内部一致。
- 不因为「对拍不过」就放宽解码器；先判断是契约真变了，还是本地理解错了。
- 不新建第二套镜像构建框架（`image/runtimekit/` 的暂停状态维持不变）。

## 11. 执行顺序

1. 阶段 1.1–1.2：取两个 CLI 版本 + 参数化 `CLI_PATH`。**最小可跑**。
2. 阶段 1.3–1.5：请求矩阵 → 采集 golden → 差异报告。
3. 阶段 2：白名单 golden 派生 + 越界即 FAIL 的测试。
4. 阶段 3：版本闸门单一化（依赖阶段 1 的结论）。
5. 阶段 4：proto golden 帧。

阶段 1 的最小形态（1 个用例 × 2 版本）应当在**半天内跑通**。若三天还没跑通第一条
golden，说明方案过重，应当砍掉用例矩阵先求通路。

## 12. 前置未决项

- 两个 CLI 版本的容器化跑道需要 Linux 环境；`process.ts` 注明「需要单独验证过的
  Linux 容器」。本机 Colima 曾被权限策略拦截，需确认可用宿主。
- `acquire.py` 的版本闸门需先放宽到可接受 2.1.280，否则阶段 1.1 无法取得制品。
- 规范化规则（剥离时间戳/uuid/耗时）需要先看过一条真实 golden 才能写准，
  属阶段 1.3 的产出而非前提。
