# CLI 缓存实测：借用账号与本地隔离预检

日期：2026-09-15。承接 [第二轮配置核查](online-cli-forwarder-clarifications-2026-09-14.md)。

> 后续状态见 [无真实账号的本地 CLI 缓存验证](local-cli-cache-stub-2026-09-15.md)。本文保留当时的预检事实。用户随后明确要求不影响线上数据、UI 和使用，因此暂不执行本文末尾的借号真实调用步骤；最新验证仅使用假上游与合成凭据。

## 结果与范围

用户已允许借用一个现有线上 CCMAX 账号，供本地隔离的小规模真实请求测试；不部署、不替换、不修改线上服务或账号配置。

本轮找到一个凭据格式和剩余有效期满足预检的候选账号，但**尚未导出 access token、refresh token 或代理凭据，真实模型请求为 0 次**。账号可用性没有经过上游验证，不能把本地执行器失败解释为账号不可用。

阻塞发生在原程序的本地执行环境：保全的 Portunex ELF 使用 AVX2 指令，现有执行器未通过启动检查。原 CLI 的独立版本检查也未在期限内完成。因此本轮没有最终上游正文、真实 usage 或缓存命中证据；此前 5m / 1h 的静态结论没有升级为实测结论。

## 账号预检（线上只读）

- SSH 连接成功；只读查看已授权的运行环境与有界账号元数据，不读取用户历史提示或调用日志。
- 先检查最多 3 个无 CLI 子进程的候选，访问令牌均已过期；没有尝试刷新。
- 再检查有 CLI 子进程的候选，找到一个 `claudeAiOauth` 访问令牌：采样时剩余约 326 分钟，包含 inference scope。
- 进程数只是选择线索，不证明空闲、低负载或可用；元数据预检也不证明上游接受令牌。
- 候选运行环境有带认证的 HTTP 上游代理。只输出代理类型等脱敏元数据；未导出或验证其路由。后续须使用该账号已配置的出口，不自动切换出口。
- 解析凭据文件时内容短暂存在于服务器上预检进程的内存中；本地只收到权限存在性、剩余时间等元数据，没有收到凭据值。

## 新建本地隔离环境

本机原有 Docker 两个入口均不可连接，原 Colima `default` 保持停止。本轮新建且仅操作 `ccmax-cache-probe`：VZ、aarch64、2 CPU、3 GiB 内存、10 GiB 虚拟磁盘，使用已缓存基础镜像。未安装新软件包或更换本机工具链。创建时使用 `--activate=false`，测试显式指定独立 Docker socket；结束核查发现全局 context 为 `desktop-linux`，已恢复预检开始时的 `default`，不声称全程该设置完全未变。

实际检查及修正：

1. Colima 默认加入缓存挂载和自动端口转发，不能仅凭创建参数假定完全隔离。停止新 VM 后移除额外缓存共享，并将新实例的 Lima TCP/UDP 转发设为 `ignore: true`；保留运行时管理 SSH 和 Docker Unix socket。
2. 重启后核对实际 virtiofs，移除旧缓存挂载点；最终只有空的任务目录只读共享，以及 VZ 所需的 Rosetta 运行时共享，没有宿主 home、仓库或凭据目录共享。
3. Docker 客户端会注入用户代理配置。首个容器在执行前被环境变量断言拒绝并移除；后续使用全新的空 Docker client config，明确验证全部容器环境变量。
4. 目标容器均断网、无端口映射、只读根目录、非 root、drop ALL capabilities、no-new-privileges、禁 core dump、限制 CPU/内存/PID，不挂载宿主目录或 Docker socket。CLI 的少量可写目录只用 tmpfs。

配置提醒：测试 VM 后续不能不经检查地用普通 `colima start` 恢复后直接注入凭据，该命令可能重新生成 Lima 转发配置。须重新检查实际 mounts、转发、容器环境和网络隔离。

## 执行结果

| 对象 | 执行器及边界 | 观察 |
| --- | --- | --- |
| 原 Portunex `--help` | Rosetta，断网 scratch 容器，256 MiB | `unhandled auxillary vector type 28`，退出 133；未进入业务服务 |
| 原 Portunex `--help` | 已有静态 QEMU 7.0.0，断网；另测 `-cpu max -strace` | 非法指令；40 秒上限后强制终止，容器退出 137，未标为 OOM |
| 原 CLI `--version` | 同一 QEMU，断网，1 GiB，新 HOME/config/securestore tmpfs | 40 秒内没有版本输出，终止后退出 137，未标为 OOM；不能据此判断 CLI 业务逻辑或凭据问题 |

Portunex trace 的 SIGILL 地址是 `0x1a1d0f8`；对原 ELF 有界反汇编显示该地址为 `vpbroadcastq %xmm0, %ymm0`（AVX2）。这是具体执行/指令证据，不是凭文件名推测。QEMU 项目的 [x86-64-v3 跟踪问题](https://gitlab.com/qemu-project/qemu/-/issues/844) 也记录过 AVX/AVX2 模拟覆盖缺口；不能据该历史问题断言任意新版均已满足需求，替代执行器仍须实际验证。

没有通过修改原二进制、关闭证书校验、扩大容器权限或切到生产执行来绕过失败。

## 保全与测试产物

私有目录：`/Users/ruanyang/My-project/api/z/ccmax-local-probe.YMj99D`，不在 Git 内。

- `account_preflight.py`、`selected_runtime.py`：有界只读元数据检查，无凭据输出。
- `collect_cli_artifact.py`、`fetch_cli_artifact.py`：只读取固定 CLI 和同一容器文件系统内的 ELF loader/NEEDED 动态库，不执行远程目标或 `ldd`，不复制账户目录。ELF 元数据解析与路径解析均有边界。
- `cli-original-rootfs.tar`：8 个 CLI/动态库文件及清单。CLI 为 2.1.258，215,473,560 bytes，SHA-256 `704f1334ac65d3e89e1c6c1d7663293ad786a6166afdb71b5075337df630f976`；各文件在组装镜像前逐个验证 SHA/大小，无符号链接提取。
- 原 Portunex SHA-256 仍为 `de6de2722837eb3a319ff2a342f2cb0ad44b989e4b221eacf68cfdbc44d98763`。
- `prepare_artifact.py`、`prepare_cli_image.py`、`offline_start.py`、`cli_version_check.py`：镜像组装与执行前隔离断言；只在上述新运行时执行目标，不在 Mac 宿主运行保全的程序。
- `gate/`：按模块拆分的 Go 标准库诊断请求限制器及合成测试，不是生产应用代码。凭据通过 stdin 进入内存；目标固定为 Anthropic messages 端点；阻止 OAuth/刷新/辅助路径和重定向；最多 3 次已接受请求、256 KiB 请求体、128 输出 token、90 秒上游期限。只输出缓存字段、正文哈希/大小及白名单数值 usage，不输出正文、响应文本或认证头。

限制器的 stub 明确标识 synthetic usage，没有外部 HTTP client。live 模式只做了替代 transport/本地 httptest 单测，**没有启动主程序监听、没有真实代理/模型调用，也没有与原 CLI/Portunex 接通**。三次预算按进程计算，后续不能通过重启规避总请求上限；Go 不能保证所有内存副本安全清零。

## 验证、清理及下一步门槛

- 请求限制器：16 个顶层测试、14 个命名子用例，合计 30 条 passing test entries；`go test -race -count=1 ./...` 与 `go vet ./...` 通过。主代理复核固定上游、凭据/头处理、预算、脱敏和测试实现，并独立复跑测试。
- `web-reverse-master` 离线 selftest：7/7；不代表业务端到端验收。
- 清理本轮 4 个已停止的诊断容器及其 tmpfs；首个被预执行环境检查拒绝的容器此前已移除。没有凭据或原始业务抓取文件需要清理；源码/二进制保全和合成诊断工具保留在私有目录。
- 新建测试 VM 停止，镜像/磁盘保留用于后续排查；没有停止其他 VM 或线上服务。

已单独询问用户是否允许仅在该新建测试 VM 内下载/安装更合适的执行器。获得许可后仍须先断网通过原程序/CLI 启动验证，再验证合成请求与刷新阻断，最后才按现有借号授权即时读取一个仍有效的 access token 做少量真实请求。不要预先导出 refresh token，也不要把借号授权扩大成线上部署或配置修改。

本轮仅记录预检事实与诊断门槛，没有进入后续业务开发规划；未解决独立 downgrade 开关完整消费链、实际 provider 选择、最终缓存 TTL 与真实缓存命中等遗留问题。
