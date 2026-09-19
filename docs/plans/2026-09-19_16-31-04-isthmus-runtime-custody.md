# 2026-09-19 16:31:04 — 实例授权 → 连接托管 → 失租回收 闭环规划

时间：Asia/Shanghai（UTC+08:00）。承接
[2026-09-18 规划](2026-09-18_13-17-37-isthmus-claude-handoff.md)。

## 0. 开工核实（已完成）

- 分支 `codex/claude-execution-plane-v1`，HEAD `f03744e`（与给定基线一致）。
- 未提交 WIP 原样保留，**不得覆盖或混入本轮提交**：
  - `execution-plane/isthmus-runtime/image/lab/build.py`（已改 +36/−10）
  - `execution-plane/isthmus-runtime/image/runtimekit/`（未跟踪目录）
- 基线测试：44 包 PASS、0 FAIL。真实依赖用例此前全部 **SKIP**。

### 真实依赖现状（PASS / FAIL / SKIP 明确区分）

| 依赖 | 状态 | 说明 |
| --- | --- | --- |
| Redis | **PASS** | 本轮新起**项目专用**实例 `127.0.0.1:63799`，独立目录 `/tmp/isthmus-p5-redis`，`--save ''`、`--appendonly no`。DBSIZE 起始为 0，非共享实例，未对任何共享 Redis 执行 FLUSHDB。`TestRedisBackendIntegration` 由 SKIP 转为 **PASS**。 |
| MySQL | **SKIP（受阻）** | 本机无 `mysqld`；`colima start` 被权限策略拦截，**未绕过**。`EXECUTION_MYSQL_TEST_DSN`、`EXECUTION_CCMAX_MYSQL_TEST_DSN` 仍未设置，相关用例继续 SKIP。需用户定夺（见第 4 节）。 |

## 1. 本轮目标与不做的事

目标：把 P4b 的 `internal/runtimeregistry` 从「有能力、无调用方」变成**真实运行链路
的一部分**——每实例授权、连接托管、失租回收。

不做：不改 Sub2 登录/权限/模型价格/用户倍率/终端用户计费；不删 CCMAX 服务账号、
额度与成本账本；不换虚拟化架构（沿用 Docker/runc）；不动 216；不使用真实账号、
Token、代理口令、CA 私钥；不触发真实模型调用；不连线上数据库或上游模型服务。

**权威归属（不得制造本地假租约）**：

- 授予/续期/撤销执行租约的权威是控制面的 `lease.Coordinator`
  （Redis 令牌围栏 + SQL 持久记录，见 [P4a 设计](../../openspec/changes/complete-ccmax-execution-acceptance/p4a-lease-authority.md)）。
- host-agent **不签发**租约。它从 START 命令拿到 `(slot, node, epoch, owner)`，
  并在托管连接之前**向权威校验**——`runtimeregistry.Adopt` 内部经
  `lease.Fencer.Admit` 调用 `Validate`，校验不过就不托管。
- 因此 host-agent 侧不存在可绕过权威的本地租约路径；没有权威可达时**拒绝托管**。

## 2. 阶段划分

### 阶段 1 — START 把连接交给托管（本轮主体，不依赖 MySQL）

`internal/hostagent/lifecycle/startup.go` 现在拿到 `*Runtime` 后立即 `Close()`，
注释写明「未来的 runtime registry 必须取得自己的授权生命期」。本阶段接上：

- 新增可选 custodian。**未配置时行为与今天逐字节一致**（仍然立即关闭），
  因此默认关闭，不改变既有默认路径。
- 所有权转移：托管成功后连接归注册表；**失败一律回滚并关闭**，不泄漏。
- 重复 START：同一 `(epoch, generation)` 重复托管被拒（`ErrAlreadyHeld`），
  且拒绝时关闭本次新建的连接——它未被接管。
- 替换：更新代次顶替旧代并关闭旧连接；旧 `epoch/generation` 因撤销水位线
  **不可重新接管**。
- 关闭顺序：先从注册表摘除并释放 fencer 名额，再关闭连接，全程在锁外关闭。

### 阶段 2 — 周期校验、显式撤销与停机接入 daemon 生命周期

- daemon 运行期周期性 `Fencer.Revalidate`，失租即回收对应实例连接。
- DESTROY/DRAIN 等显式命令 → `Registry.Revoke`（屏障语义）。
- 停机 → 释放全部托管连接，不留悬挂连接。
- **单实例回收不得影响其他实例，更不得触碰节点级控制连接**（控制流是节点级的，
  一个节点服务多个槽位）。

### 阶段 3 — 真实 MySQL 上的权威闭环（阻塞于第 4 节）

- 接线 `Coordinator` 的 Grant/Renew/Revoke，Redis 与 SQL 双写。
- 在**项目专用**数据库上验证幂等、并发、超时与失租回收整链。
- 不做生产迁移，不复制线上证书身份。

## 3. 每阶段流程

实现 → 独立 adversarial review → 修复 → 离线全量 `go test -race`/`go vet`/
linux-amd64 编译 + 关键包多轮 race + 变异验证 → `make -C recovery check` →
按明确文件 `git add`（**绝不 `git add .`**）→ 本地提交，不 push。

## 4. 需要用户定夺（已受阻，不绕过）

真实 MySQL 访问。可选路径：

1. 用户启动 Docker Desktop，我在其上创建**项目专用**容器（独立容器名/端口/数据卷，
   不复用 Sub2 compose 的名称或数据目录）。
2. 授权我执行 `colima start`（本机 Linux VM，同样只建项目专用容器）。
3. 使用已授权的 170.106.159.197：先只读预检 4 个业务容器元数据，不符即停；
   只新增项目专用容器/网络/端口/卷，不停止、不重建、不清理、不复用既有资源。

在此之前，阶段 1、2 照常推进并交付。

## 5. 执行记录

- 2026-09-19 16:31:04 +08:00 规划创建，基线 `f03744e`。
- **阶段 1 完成**：托管接入认证 START 路径。核心成果是端到端证明——托管子测试以
  **真实两库 `lease.Coordinator`** 为权威，**仅在持久层撤销**（Redis 令牌故意保持
  存活）即回收 worker 连接。
  [实证](../../openspec/changes/complete-ccmax-execution-acceptance/verification.md#p5a连接托管进入真实-start-链路)。
  Review 修掉 5 项，其中 **owner 被伪造为节点级常量是 HIGH**：`LeaseOwnerID`
  在仓库中一贯按绑定取值，同节点不同槽位可以不同，写死会让权威校验拒绝、START
  反而失败。现改为经已认证的命令元数据 `lease_owner_id` 逐次传入。
- 阶段 1 遗留的工作区无关改动：`internal/route/reconcile.go` 有**既有的** gofmt
  未对齐，被全目录 `gofmt -w` 顺手修正。**未纳入本轮提交**（`git checkout --`
  被权限策略拦截，故改为不 stage）。请用户决定是否单独提交。
- 阶段 2（未做）：`daemon.Config` 缺 Redis/持久库字段，无法构造 `Custody`，因此
  `Custody.Run` 的周期校验在真实 daemon 中尚未运行，显式撤销也未接命令路径。
- 阶段 3（受阻）：真实 MySQL 闭环，见第 4 节。
