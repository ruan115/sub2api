# S2b5b / P2：真实 provider 双实例管理与 mTLS

2026-09-18。承接 P1/S2b5a `2b41e90` 与时间命名规划
[2026-09-18_13-17-37-isthmus-claude-handoff.md](../../../docs/plans/2026-09-18_13-17-37-isthmus-claude-handoff.md)。
不重做禁止-swap 策略，不启用 `execution_onboarding`，不把账号标为 `migrated`，
不恢复 Portunex，不改调用方 HTTP/WS/gRPC 合同。暂停中的 `image/lab/build.py`
与 `image/runtimekit/` 继续绕开。

## 与 S2b2-live 的差

S2b2-live 用 `network=none`、实验性只读 host bind `/worker`，以及 Lab 直调 Engine
create；明确不走生产 provider 接纳，也不能从宿主 TCP 直达实例。P2 必须：

- 使用 **同一** `provider/docker` Create / 只读接纳 / 严格 START 代码；
- 每槽位独占 `Internal=true` IPv4 bridge，记录准确 network ID 与物理 CID；
- 公开程序放在 **最小不可变派生镜像** 内，禁止 host bind / named volume /
  docker.sock；
- 从可信宿主按 `RuntimeEndpoint` 私网地址做 worker mTLS，不以 TCP 连通冒充认证。

合成 control / lease 夹具可以检验管理流，但不能声称真实 SQL/Redis 已验收。
N3/N4/K3/K4 仍须逐项 Linux 实证，不因本切片源码或 fake Engine 自动加分。

## 实验资源合同

| 对象 | 固定身份 | 资源 |
| --- | --- | --- |
| 槽位 A | `slot-p2-a` / `account-p2-a` / epoch 1 / generation 1 | 独占 `execution-net-slot-p2-a` |
| 槽位 B | `slot-p2-b` / `account-p2-b` / epoch 1 / generation 1 | 独占 `execution-net-slot-p2-b` |
| 镜像 | 本地已有 base digest 上 COPY `/worker` 的派生 digest | 不可变 `@sha256:`，无 latest |
| 用户 | 与派生镜像一致的非 root（base 为 1000） | cap-drop ALL、NNP、只读根 |
| 内存 | 正 Memory，且 `MemorySwap = Memory` | 本地合成 512MiB；专用 Linux 1GiB |
| tmpfs | 仅 `/tmp` 与 `/run` | provider 匿名 tmpfs，无 `/home` 宿主挂载 |
| 监听 | `0.0.0.0:<runtime-port>` | 仅 Internal 网可达，不 Publish |

清理只针对本任务记录且 owner/label 匹配的 CID 与 network ID。不确定的创建结果
不得猜 ID、不得 prune、不得按模糊名称删网络。清理失败单独报告，不能输出完整
PASS。不删除已有镜像或业务容器。

## 派生镜像

`isthmus-runtime/image/lab/providerlifecycle/Dockerfile` 是实验派生配方，不是
完整 runtime 发布。构建只允许：

- `FROM` 已审查、本机已有的 base digest（ARG，禁止可变 tag）；
- `COPY` 当前仓库源码编出的 linux worker 到 `/worker`；
- `USER` 非 root；`ENTRYPOINT ["/worker"]`；
- 无 secret、无私钥、无 host 路径、无二次通用构建框架。

未提供本机 base digest 时不得 pull、不得把 Go-only 进程称作完整 isthmus 制品。

## 拒绝矩阵

| 编号 | 输入 | 必须拒绝 | 本切片证据 |
| --- | --- | --- | --- |
| R1 | 非 Internal / 共享网 / host 网 | 创建或接纳失败 | fake Engine + 真实 provider |
| R2 | host bind / docker.sock / named volume | 创建请求不含这些字段；接纳失败 | 同上 |
| R3 | `MemorySwap` 缺/0/-1/不等于 Memory | 创建与接纳失败 | 沿用 P1，本切片复验 Create |
| R4 | 用 B 的 CID/slot/epoch/generation 操作 A | ValidateExisting / START 失败 | fake 双实例 |
| R5 | 交叉安装证书或错误 CA | bootstrap/mTLS 失败 | 本切片本地组合；跨容器实跑另记 |
| R6 | 实例替换后仍用旧 CID | 物理 ID 不匹配则失败，不 Create 替代 | 合同测试 |
| R7 | lease 撤销后再签发/START | 签发或 START 失败 | 沿用 S2b3 夹具；权威 Redis writer 仍缺 |
| R8 | 公网 dataplane endpoint | 路由/拨号拒绝 | 已有 route 校验；P2 仅私网 |

允许对照：正确身份 + 活租约 + 匹配物理 CID 的 Create→接纳→（实跑时）START/mTLS。

## 只读预检与运行边界

本地：编译/race/vet 与 fake Engine 合同。只有明确本机 Docker socket、已有镜像
digest、且预检确认无业务容器冲突时，才允许 `EXECUTION_PROVIDER_LIFECYCLE_DOCKER=1`
实跑。默认 skip。

远端：`216.106.185.119` 仍禁止。`170.106.159.197` 每次操作前重核 4 个业务容器
元数据；数量/身份不符即停。本次切片 **不 SSH**。cgroup swap 内核实证留在专用
Linux，不把宿主无 swap 或容器 `free` 当作证据。

## 停止线

- `/readyz` 保持 503 / `production_ready=false`。
- 缺权威 `execution:lease:v1:` writer 时生产签发继续拒绝。
- 不打开业务数据面、出口服务或 CCMAX 网关。
- 不把本切片标为 K3/K4/N3 关闭或总体分数提高，除非专用 Linux 双 Internal 网
  mTLS 与 cgroup 证据齐备。
