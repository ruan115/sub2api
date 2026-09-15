# 无真实账号的本地 CLI 缓存验证

日期：2026-09-15。承接 [隔离执行预检](local-cli-probe-preflight-2026-09-15.md) 与 [线上配置核查](online-cli-forwarder-clarifications-2026-09-14.md)。

## 结果与授权边界

已用官方同版本 Linux ARM64 CLI 2.1.258，在专用本地 VM 内完成 3 组真实 CLI → 合成上游的请求字段验证。三组 CLI 均正常完成，真实模型请求 **0 次**，没有借用或导出账号凭据。

用户最新要求“不影响线上数据和 UI，以及使用”优先于此前借号安排。真实请求即使不改服务器配置也可能消耗额度、产生调用记录，因此本轮不执行借号实测。没有连接生产 SSH、读取新生产数据、部署、重启或修改生产配置/UI。只在专用测试 VM 下载公开软件制品并运行隔离测试；本机工具链与线上程序均未替换。

本轮只能证明 CLI 侧的出站请求形状，不能证明完整 Portunex / isthmus 转发链、线上默认 TTL、真实账号可用性或缓存命中。

## 执行器与制品来源

此前 QEMU 7.0 无法执行原 Portunex 的 AVX2 指令。本轮在已授权的专用测试 VM 内刷新 APT 索引，并下载 Ubuntu 的 `qemu-user-static`，仅解包到新目录，显式指定可执行文件运行；没有安装到系统路径、注册 binfmt 或替换旧执行器。

| 制品 | 固定版本 / 校验 | 结果 |
| --- | --- | --- |
| Ubuntu ARM64 包 | `1:8.2.2+ds-0ubuntu1.18`；16,942,584 bytes；SHA-256 `0565144632acc09b92da93e5ef9a2f44fa092797c5b0949744351b184ad948a8` | 与签名 APT 索引校验匹配 |
| 解包的 QEMU x86-64 模拟器 | ARM64 静态 ELF；SHA-256 `de72ec1143909f0f495a53c36d9215cb86e2952517aad87044c741a1088dc60f` | 只在测试 VM 的隔离容器执行 |
| 官方 Linux x64 CLI | 2.1.258；215,473,560 bytes；SHA-256 `704f1334ac65d3e89e1c6c1d7663293ad786a6166afdb71b5075337df630f976` | 官方 manifest 与此前保全的线上 CLI 完全匹配 |
| 官方 Linux ARM64 CLI | 2.1.258；215,014,736 bytes；SHA-256 `43dc490af55262edcb3e9b1cb315de22cc09ccb08bd52a4c39bc5eabaa63100f` | 下载后验证大小和 SHA，再执行 |

QEMU 官方 [7.2 发布说明](https://www.qemu.org/2022/12/14/qemu-7-2-0/) 列出新增 AVX / AVX2 支持；本轮使用的包来自 [Ubuntu noble-updates](https://packages.ubuntu.com/noble-updates/qemu-user-static)。这解释了升级方向，但实际兼容性仍以执行结果为准。

CLI 按 [官方安装验证说明](https://code.claude.com/docs/en/troubleshoot-install) 获取 [2.1.258 manifest](https://downloads.claude.ai/claude-code-releases/2.1.258/manifest.json) 与其签名，使用独立 GPG 公钥目录验证 `VALIDSIG`，签名指纹为 `31DDDE24DDFAB679F42D7BD2BAA929FF1A7ECACE`。Manifest 标记构建提交 `b3cd543a1f6fcdf4d8fabc0f5e5538d2ee7f38e1`、构建时间 `2026-09-01T22:02:56Z`。ARM64 运行库只取自本轮新建测试 VM，不含账号目录或宿主配置。

### 启动结果

| 对象 | 隔离执行观察 | 能证明什么 |
| --- | --- | --- |
| 原 Portunex + QEMU 8.2 | 断网；退出 1，`missing configuration field "db"` | 已越过此前非法指令，进入配置读取；没有启动数据库或业务服务 |
| 原 x64 CLI + QEMU 8.2 | 断网；`QEMU internal SIGSEGV {code=MAPERR, addr=0x20}`；45 秒后终止，退出 137，非 OOM | 当前模拟环境仍不兼容；不是账号错误 |
| 官方同版本 ARM64 CLI | 断网 `--version` 输出 `2.1.258 (Claude Code)`，退出 0 | 原生架构启动通过 |

QEMU 的 [已知问题 2168](https://gitlab.com/qemu-project/qemu/-/issues/2168) 有相似 ARM64 / x86 程序崩溃记录，但没有取得本地调用栈，不将其认定为本次已确认根因。没有修改原二进制或放宽权限绕过失败。

## ARM64 与线上 x64 的静态交叉校验

对 3 个此前定位的缓存决策/字段构造函数进行 AST 比较，保留属性键、常量、运算符与标识符引用关系，并记录跨构建的名称映射：

| x64 函数 | ARM64 函数 | 标识符归一化后的结构 |
| --- | --- | --- |
| `rVn` | `Eqn` | 一致 |
| `MXn` | `tXn` | 一致 |
| `oO` | `oF` | 一致 |

比较器合成自测 6/6 通过；函数结构匹配 3/3，函数原始字节匹配 **0/3**。这是有限函数范围的结构证据，不是整份 CLI 等价证明，也不证明其外部依赖函数、账号状态或平台运行时完全一致。

## CLI → 假上游测试边界

- 只启动专用 `ccmax-cache-probe` VM，原 `default` VM 保持停止；Docker 命令显式使用专用 socket 和空白客户端配置，避免继承宿主代理。
- 实际 VM 共享仅为空任务目录的只读挂载和 VZ 所需的 Rosetta 运行时共享；没有宿主 home、仓库、凭据共享，没有 SSH agent 转发。应用 TCP / UDP 自动转发关闭，保留管理 SSH / Docker Unix socket。
- CLI 与 gate 在随机命名的 Docker `--internal` 网络内，没有外部转发路径、宿主端口发布或宿主 bind mount；gate 的 stub 分支不创建外部 HTTP client。
- 容器只读根目录、非 root、drop ALL capabilities、no-new-privileges、禁 core dump，并限制 CPU / 内存 / PID。CLI 的 HOME、config、securestore、工作目录和临时目录使用新的 tmpfs。
- gate 要求随机 `/capture-` 前缀，仅允许精确匹配的 POST messages 路径及可选 `beta=true` 查询参数。最多接受 3 次请求，正文不超过 256 KiB，`max_tokens` 不超过 128。
- CLI 仅有明确标为 synthetic 的假 OAuth 字符串；无 access token、refresh token、真实 API key 或代理凭据。禁用工具、用户/项目设置、MCP、会话持久化及非必要后台行为；单轮、无自动重试。
- 只保留白名单结构化摘要。没有保存原始请求正文、认证头、CLI 原始输出或模型响应正文。usage 是 stub 构造值，不是真实 tokenizer / 缓存 / 计费结果。

技能 `web-reverse-master` 与 `z-ccmax-skill` 用于约束证据分层、随机隔离入口、合成 usage 标识和收尾清理；没有走浏览器抓取或真实转发路径。

## 三组实际出站请求

三组均为一个新的 CLI 进程、空本地配置和合成提示；`--model sonnet` 在该固定版本解析为 `claude-sonnet-5`，这只是 CLI 输出的标识，不是该模型的线上可用性验证。

| case | 额外环境变量 | 三处缓存标记 | 请求正文大小 | CLI 结果 |
| --- | --- | --- | --- | --- |
| `default` | 无 TTL 覆盖 | `type=ephemeral, ttl=1h` | 15,313 bytes | exit 0 / success |
| `force_5m` | `FORCE_PROMPT_CACHING_5M=1` | `type=ephemeral`，**省略 ttl** | 15,280 bytes | exit 0 / success |
| `force_1h` | `CLAUDE_CODE_PROMPT_CACHE_TTL=1h` 且 `ENABLE_PROMPT_CACHING_1H=1` | `type=ephemeral, ttl=1h` | 15,313 bytes | exit 0 / success |

三处路径分别是 `$.messages[1].content[0].cache_control`、`$.system[1].cache_control`、`$.system[2].cache_control`；摘要中没有 `scope` 字段。

按 [Anthropic 缓存协议文档](https://platform.claude.com/docs/en/build-with-claude/prompt-caching)，`ephemeral` 省略 TTL 采用默认 5 分钟。因此 `force_5m` 的请求形状符合 5 分钟协议表达，但本轮没有真实上游，不能把它说成缓存已经写入或命中。特别注意：此前 Portunex 静态证据显示某些处理阶段会把缺失 TTL 补成 `1h`；CLI 侧强制 5m **不等于** 整条链最终一定为 5m。

解释限制：

1. `default=1h` 只在本次空配置、假 OAuth、原生 ARM64 条件下成立，不代表线上所有请求默认 1h。
2. `force_1h` 同时设置两个变量，这次运行不能独立区分每个变量的因果作用；更细优先级仍引用此前固定版本的静态函数测试。
3. 每组另有 1 次请求被严格 method / path / query 入口规则拒绝，共 3 次；只记录固定拒绝原因，没有具体路径，不能断言是 OAuth、计数或遥测请求。
4. 三次返回的 `input_tokens=1`、`output_tokens=1` 及缓存 usage 为 0 都是合成值，不能用于估算实际 token 数、缓存命中率或费用。

## 产物、验证与收尾

私有产物仍在 `/Users/ruanyang/My-project/api/z/ccmax-local-probe.YMj99D`，不在 Git 中。主要新增：

- `offline-v2-results.json`、`native-version-result.json`：断网启动结果。
- `native-cli-cache-parity-v2.json`、`compare_native_cli_cache.cjs`：固定制品的静态结构比较与自测。
- `native-stub-results.json`、`probe_stub.py`：三组 CLI → stub 脱敏摘要与监督脚本；摘要不含原始业务 fixture。
- `gate/`：按配置、路由、合成响应、摘要和测试拆分的私有 Go 工具；本轮增加强制随机前缀校验，不是生产模块。
- 官方 ARM64 CLI、QEMU、动态库 tar 与隔离镜像：保留作后续本地调查，不在宿主直接执行，不加入 Git。

主代理复跑 gate 的 `go test -race -count=1 ./...` 与 `go vet ./...` 通过；比较器 6/6 与结构比较 3/3 通过；`web-reverse-master` 离线 selftest 7/7 通过。另有独立代理复核 stub 的无外呼分支、隔离参数、三组结果及证据措辞，并独立通过 gate race / vet。

Review 发现私有监督脚本在前置检查失败或创建资源后断言失败时可能遗漏清理。已补上创建前登记、每次运行的资源归属标签、逐项尽力清理与失败汇总，保留原异常且清理失败不写成功报告；没有改变 CLI 请求参数。新增 `test_probe_stub_cleanup.py`，13 个 mock-only 异常注入测试由作者与主代理分别通过，不启动 Docker / VM / CLI。此补丁没有重跑目标程序或覆盖三组既有证据；它的异常清理覆盖是离线测试证据，不冒称经过真实故障注入。文档另经独立只读 review，未发现阻碍提交的问题。

本次正常运行创建的 CLI / gate 容器及私有网络均已删除；结束再次查询专用 Docker 运行时，容器列表和任务标签网络列表均为空。专用测试 VM 已停止，原 `default` VM 仍停止，宿主 Docker context 为 `default`。私有制品和 VM 磁盘保留供复现，未删除用户其他环境。

## 仍未闭环，暂不进入线上操作

- 原 Portunex 完整隔离启动与合成数据库配置，尚未接入 CLI → isthmus → Portunex 完整链路。
- 实际 provider 选择、独立 `DOWNGRADE_1H_CACHE_TO_5M` 完整消费链、最终转发正文仍待验证。
- 真实缓存创建/命中、实际账号状态与计费未测；当前不影响线上使用的边界下，不通过借号调用补证。
- 下一步只可继续基于已保全制品的本地静态分析与假上游合同测试，不把本轮合成验证当作生产上线验收，不修改线上 UI 或服务。
