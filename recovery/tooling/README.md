# Recovery CLI

Python 3.9+ 标准库，无第三方依赖。命令从仓库根目录执行；`PYTHONDONTWRITEBYTECODE=1` 避免产生缓存文件。

```sh
PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=recovery/tooling python3 -m recoverykit --help
PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=recovery/tooling python3 -m recoverykit contracts --root recovery/contracts
```

模块独立：`evidence/` 管理文本证据，`workspace/` 管理 WIP 快照，`contracts/` 管理发现清单，`wire/` 校验人工观察与源码的关联，`cli/` 只负责参数、组合调用和安全摘要。错误返回码为 2，仅输出错误类型，不输出文件内容、补丁或敏感参数。

`lab/` 提供显式 opt-in 的[专用本地 Docker 端点只读预检](recoverykit/lab/README.md)。它不沿用默认context，不启动/构建/删除资源，成功也不代表可以执行工作负载；普通 `check` 只跑合成测试，不检查实际Docker。镜像与执行链的固定百分比见[交付台账](../../docs/plans/isthmus-container-delivery-v1.md)。

## 文本证据

先在 manifest 中显式登记相对路径、来源、大小、SHA-256；不支持通配符、目录打包或自动发现。以下 `/absolute/...` 都是需替换的明确本地路径，不是默认目录。

```sh
PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=recovery/tooling python3 -m recoverykit evidence verify --manifest recovery/baselines/isthmus/manifest.json --source-root /absolute/reviewed-source
PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=recovery/tooling python3 -m recoverykit evidence preserve --manifest recovery/baselines/isthmus/manifest.json --source-root /absolute/reviewed-source --destination /absolute/private-parent/evidence-new
PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=recovery/tooling python3 -m recoverykit evidence verify-preserved --directory /absolute/private-parent/evidence-new
```

目标目录必须全新、绝对路径、位于 Git 工作区和来源树之外；其父目录须已存在。目录权限 0700，文件 0600。拒绝软链接/特殊文件、路径穿越、已有目标、超限内容、非 UTF-8/二进制、敏感文件名和可识别秘密。该筛查不是完整秘密检测，显式白名单仍需人工审查；不要把保全结果直接公开或纳入 Git。

成功产物为 `files/`、`manifest.json`、最后写入的 `receipt.json`。失败可能留下私有的不完整目录，工具不自动删除或覆盖它；换用新目录重试。验证会检查清单、哈希、实际文件集合和私有权限。它能发现意外损坏，不是带签名的防篡改存证。

本阶段只允许经过审阅的文本参考资料，不复制 ELF、镜像层、数据库、日志或压缩归档，也不执行被保全的代码。完整运行包的加密备份是后续单独工作。

## 静态接口观察

```sh
PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=recovery/tooling python3 -m recoverykit wire verify --observations /absolute/private/observations.json --manifest /absolute/private/artifacts.json --source-root /absolute/private/source --catalog-root recovery/contracts
```

四个输入均须显式指定；source-root 必须是绝对路径。观察文档结构与边界见 [wire 说明](../docs/wire.md)。工具对照来源文件整体 SHA-256、片段哈希/UTF-8 字节位置及既有 API 记录，拒绝篡改、软链接、越界、未知引用和可识别秘密；不执行源码、不修改合同、不打印原文。成功只返回 `source_anchored` 和 `business_verification=false`，不表示人工解读正确或客户端已兼容。

## 工作区留档

```sh
PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=recovery/tooling python3 -m recoverykit workspace snapshot --repo /absolute/sub2api --destination /absolute/private-parent/workspace-new
PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=recovery/tooling python3 -m recoverykit workspace verify --directory /absolute/private-parent/workspace-new
```

快照记录 HEAD、分支、Git 状态、暂存/未暂存 binary diff 和未跟踪文件；不执行 stash、checkout、reset、add、commit、push 或自动恢复。忽略文件、Git 对象历史、外部子模块内容、数据库和运行环境不属于 WIP 快照。敏感/不支持的材料会使操作失败，不会静默跳过后声称完整。

Git hooks/fsmonitor 被禁用；有效配置包含 clean/process filter（例如某些 LFS 设置）时直接失败，不自动改用户 Git 配置。请勿为绕过失败直接关闭安全检查，应先核实外部内容与单独保全方式。

Git子进程输出在读取时限量：stdout最多64MiB；stderr只计数丢弃，最多1MiB。单次命令总deadline为60秒，异常清理另最多等待1秒；超限或超时只终止本次创建的进程组并回收直接子进程，不完整缓冲后才检查，不等待后代持有的管道无限结束。相关实现单独放在`workspace/process.py`，错误不回显输出或命令参数。

恢复须在隔离副本进行：先校验快照并确保源仓库的记录 HEAD 可用，再核对补丁的目标和模式，分别恢复 index 与工作区差异、未跟踪文件，最后对照状态清单。不要在当前脏工作区直接 apply；本阶段不提供自动 restore 命令。保留 Git 仓库历史和离线副本是另一项必要措施。

只有本机副本不能解决换电脑/磁盘损坏。加密异机存放、密钥保管、真实数据一致性备份及异机恢复演练仍需完成，不能把 `verified` 当作“已灾备”。

## 验证入口

```sh
make -C recovery check
```

包含 Python 单元测试、仓库合同清单验证和 Bun 协议测试。全部使用合成 fixture，不访问生产服务。GitHub Actions 使用相同入口；工具链安装由官方 [setup-python](https://github.com/actions/setup-python) 和 [setup-bun](https://github.com/oven-sh/setup-bun) action 完成，测试本身不下载或执行线上材料。

`check` 也包括独立 [isthmus基础镜像工程](../../execution-plane/isthmus-runtime/image/README.md) 的离线上下文测试；可单独运行 `make -C recovery image`。测试不会构建或启动镜像，不能当作实际Docker验收。
