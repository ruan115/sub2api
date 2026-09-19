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

### 阶段 2 — 显式撤销与会话授权接入 daemon 生命周期

**硬约束（用户指定）**：不得只是给 host-agent 加 MySQL/Redis 凭据。**租约权威与
数据库访问留在控制面**；host-agent 的授权只能经由已认证的控制通道取得。

因此阶段 2 拆成两片：

**2a — 显式撤销接入命令路径。** `SlotCommandExecutor.RevokeEpoch` 已经维护
`revokedThrough` 水位线、忘记 startup proof、Drain+Stop 容器，但**不回收托管连接**。
在 `hostagent` 内定义窄接口（`*lifecycle.Custody` 满足它，避免 import 环），
让 RevokeEpoch 与容器销毁路径回收对应槽位的连接。

**2b — 会话授权作为 `lease.Validator`，host-agent 不持有任何数据库凭据。**
授权新鲜度来自两个控制面事实：

1. 控制会话是否仍然新鲜（心跳/流接收时间超过 `OfflineAfter` 即失败关闭）；
2. 控制面推送的 `RevokeEpoch` 水位线（该 epoch 被撤销即失败关闭）。

节点被网络分区、控制会话断开时，已托管的 worker 连接会在 `OfflineAfter` 之后被
回收——这正是「失租回收」在节点侧的正确语义，且**完全不需要数据库凭据**。
真实的 Redis/SQL 两库合取仍在控制面一侧（P4a）。

验收要求（用户指定）：**必须证明连接真的被回收**——即回收后对该连接发起的 RPC
确实失败，而不是只检查注册表条目数。

- 停机 → `Drain` 释放全部托管连接并屏障后续 adopt。
- **单实例回收不得影响其他实例，更不得触碰节点级控制连接**（控制流是节点级的，
  一个节点服务多个槽位）。

### 阶段 3 — 170 上的项目专用数据库与权威闭环

见第 4 节：SSH 被权限策略拦截，未绕过。预检脚本与建站步骤已就绪，待授权即执行。

- 只读预检：主机资源、现有 4 个业务容器元数据、端口、卷、网络。
  **不输出业务环境变量、凭据或日志**；只比对 ID/name/image/status/ports。
- 通过后只新增**项目专用**容器/网络/卷（独立命名前缀 `isthmus-p5-`，独立端口，
  不复用 Sub2 compose 的名称或数据目录）。
- 数据库只初始化仓库内嵌迁移与合成数据；**不复制线上数据库、Redis、账号或私钥**。
- 测试库不暴露公网：绑定回环 + SSH 隧道访问。
- 发现未知服务、资源不足、端口冲突或需要改宿主机配置 → 暂停汇报。
- 216.106.185.119 保持不动。

## 3. 每阶段流程

实现 → 独立 adversarial review → 修复 → 离线全量 `go test -race`/`go vet`/
linux-amd64 编译 + 关键包多轮 race + 变异验证 → `make -C recovery check` →
按明确文件 `git add`（**绝不 `git add .`**）→ 本地提交，不 push。

## 4. 受阻项：到 170 的 SSH 被权限策略拦截

用户已选定方案 3（使用 170.106.159.197）。但本会话中 `ssh 170.106.159.197 ...`
与 `colima start` **均被 Claude Code 的权限分类器拦截**，我**没有绕过**。

这不是授权问题而是本地工具权限问题。解除方式（任一）：

- 用户在 Claude Code 设置里为 `ssh` 增加 Bash 允许规则；或
- 用户用 `! <command>` 在会话中自行执行预检，把输出贴回来（我据此判断是否继续）。

在解除之前，阶段 2 不受影响，照常推进并交付；阶段 3 保持待命。

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
