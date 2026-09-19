# 验证记录

## P5b：显式撤销接入命令路径，并证明传输真的被拆除

2026-09-19。规划
[2026-09-19_16-31-04-isthmus-runtime-custody.md](../../../docs/plans/2026-09-19_16-31-04-isthmus-runtime-custody.md)
阶段 2a。**硬约束**：不得只给 host-agent 加 MySQL/Redis 凭据；租约权威与数据库
访问留在控制面，节点侧授权只经已认证的控制通道。本切片**完全没有**给 host-agent
任何数据库凭据。

### 本轮实现

- `hostagent` 内新增窄接口 `SlotCustodian{Revoke(slotID, epoch)}`，`*lifecycle.Custody`
  满足它（在 `hostagent` 一侧声明以避免 import 环）。默认 nil，行为与此前一致。
- 三条真实结束路径接上回收：
  - `RevokeEpoch`——控制面推送的显式撤销，**在动容器之前**回收，且即使随后的
    provider 操作失败也已回收；
  - `stop`、`destroy`——容器即将结束，其 tmpfs 身份与认证传输一并结束。
- `INSPECT` 等不结束实例的命令**不**回收。
- 回收严格按槽位与 epoch 作用，不触碰其他槽位，更不触碰**节点级控制连接**。

### 关键验收证据：传输真的死了

用户明确要求不得只检查注册表条目数。`TestReclaimTearsDownTheRealTransportNotJustTheBookkeeping`
对**真实 gRPC 监听器**建立**真实连接**：

1. 托管期间调用 `Health`，得到 `Unimplemented`——证明请求确实到达了服务端；
2. `Revoke` 之后连接状态为 `Shutdown`，再次调用 `Health` 失败且**不再是**
   `Unimplemented`——证明请求根本没能到达服务端，传输已被拆除。

变异验证：移除 `RevokeEpoch` 的回收、移除 `stop`/`destroy` 的回收，两者任一即 FAIL。

### 验证与剩余

全仓离线 `go test -race`、`go vet`、linux/amd64 编译通过；hostagent 全家 +
registry race ×5；Redis 真实集成 **PASS**；`make -C recovery check` 236/150/186
与基线一致。

**仍未做（阶段 2b）**：`Custody.Run` 的周期校验仍未在真实 daemon 中运行——
`daemon.Config` 没有权威依赖字段，daemon 仍构造不出 `Custody`。按硬约束，2b 的
校验器应由**控制会话新鲜度 + 控制面推送的撤销水位线**构成，而非数据库凭据。
**阶段 3（170 上的项目专用数据库）受阻**：本会话 `ssh` 与 `colima start` 均被
Claude Code 权限分类器拦截，未绕过。分数仍 32%，`/readyz` 保持 503、
`production_ready=false`。

## P5a：连接托管进入真实 START 链路

2026-09-19。规划入口
[2026-09-19_16-31-04-isthmus-runtime-custody.md](../../../docs/plans/2026-09-19_16-31-04-isthmus-runtime-custody.md)
阶段 1。把 P4b 的注册表从「有能力、无调用方」接进认证 START 路径。

### 真实依赖基线（PASS / FAIL / SKIP）

- 全仓 44 包 **PASS**、0 **FAIL**。
- **Redis 真实依赖 PASS**：本轮新起项目专用实例 `127.0.0.1:63799`，独立目录、
  `--save ''`、`--appendonly no`，起始 DBSIZE=0。`TestRedisBackendIntegration`
  由 SKIP 转 **PASS**。未对任何共享 Redis 执行 FLUSHDB。
- **MySQL 仍 SKIP**：本机无 `mysqld`，`colima start` 被权限策略拦截且**未绕过**。
  `EXECUTION_MYSQL_TEST_DSN`/`EXECUTION_CCMAX_MYSQL_TEST_DSN` 未设置。

### 本轮实现

- `internal/hostagent/lifecycle/custody.go`：`Custody` 持有 `lease.Fencer` +
  `runtimeregistry.Registry`。**host-agent 不签发租约**——它用命令里的
  `(slot, epoch, owner)` 构造 claim，经 `Adopt → Fencer.Admit → Validator`
  向权威求证；权威不确认就不托管，不存在可绕过的本地租约路径。
- `startup.Start` 先校验实例绑定，再移交所有权。**未配置 custodian 时行为不变**
  （仍是短命连接并关闭），因此默认关闭。
- 所有权与关闭顺序：任何拒绝都由调用方关闭，托管**从不关闭它未接受的连接**；
  被更新代次顶替时关闭旧连接；先摘除并释放 fencer 名额再关闭，全程锁外关闭。
- **重复 START 保持幂等**：同一实例的重放命中 `ErrAlreadyHeld`，比对确认是同一
  incumbent 后返回「未移交」，由调用方关闭本次多余连接，命令仍然成功。
- `Registry.Drain()` 供停机释放，并**屏障后续 adopt**。

### Review 发现并已修的缺陷

1. **租约 owner 被伪造（HIGH）**：原实现把 owner 写成 `CustodyConfig` 的节点级
   常量，而 `LeaseOwnerID` 在仓库中一贯是**按绑定**取值
   （`runtimeprobe/runner.go:170`、`probe_binding.go:192`）。同一节点上不同槽位
   的 owner 可以不同，写死会让 `Coordinator.Validate` 拒绝、START 反而失败。
   已改为经**已认证的命令元数据** `lease_owner_id` 逐次传入
   （`SlotStartup` 接口增加该参数）；该值不被单独信任，仍须通过权威校验。
2. **`Drain` 未屏障在途 adopt（MEDIUM）**：先过 fencer、后 `store` 的适配会写进
   已排空的注册表，而此时轮询已停止，连接将永不回收——正是 Drain 要避免的悬挂。
   已加 `drained` 标志。
3. **默认路径 ctx 检查顺序漂移（LOW）**：原实现在 `Close()` **之后**检查
   `ctx.Err()`，新实现移到之前，会让「关闭期间被取消」由失败变成成功。已恢复。
4. **重放 TOCTOU**：`Current()` 是第二次查询，若期间被回收则重放失败，与「重放
   不得失败」矛盾。已改为一次有界重试。
5. **`take` 无单元覆盖**：原签名吃 `*hostagent.Runtime`，包外无法构造。已改为接
   `provider.Instance` + `Connection`，claim 构造与各重放分支现已直接覆盖。

### 验证与剩余

`TestAuthenticatedSTARTControlToWorkerAndRevokedLease` 现参数化为
**without custody / with custody** 两条子测试，共用同一条真实链路（真实 TLS
NodeControl + 签发 broker + `worker.RunProcess`）。托管子测试的权威是**真实的
两库 `lease.Coordinator`**，并证明了本轮核心命题：**仅在持久层撤销**（Redis 令牌
故意保持存活、`backend.Validate` 仍通过）即可回收 worker 连接。

变异验证：托管不接管、owner 写死、Drain 不屏障——三者任一即 FAIL。全仓离线
`go test -race`、`go vet`、linux/amd64 编译通过；hostagent 全家 + registry +
lease race ×5；`make -C recovery check` 236 / 150 / 186 与基线一致。

**仍未做**：`daemon` 尚未构造 `Custody`（`daemon.Config` 没有 Redis/持久库字段），
因此 `Custody.Run` 的**周期校验在真实 daemon 中尚未运行**，显式撤销也未接命令路径
——这是阶段 2。真实 MySQL 闭环是阶段 3，受阻于 MySQL 访问。分数仍 32%。

## P4b：runtime registry 与撤销传播到 worker 连接

2026-09-19。用户选定先做撤销传播。任务 1（撤销传播）与任务 2（registry）本是同一
件事：能回收 worker 连接的机制**就是**注册表。

### 核实到的缺口

每实例 worker mTLS 连接在 `internal/hostagent/runtimeclient.go` 的 `Runtime`
（持有 `*grpc.ClientConn`，TLS 绑定 `runtimeidentity.Binding{slot,epoch,generation}`），
由 `Controller.Start`/`StartExisting` 创建后**直接交给调用方，无人持有**，因此撤销
执行租约根本无从关闭它们。`internal/hostagent/lifecycle/startup.go` 自己就写着
「START 只拥有短命的认证连接，**未来的 runtime registry 必须取得自己的授权生命期**」
——当前 START 用完立即 Close，所以生产路径today没有长活连接，缺口是前瞻性的。

注意区分：orchestrator ↔ host-agent 的**控制流是节点级**的，一个节点服务多个槽位，
按槽位租约去关它是错的。要回收的是 host-agent → worker 容器的每实例连接。

### 本轮实现

`internal/runtimeregistry`：**只接管已建立的连接，绝不创建/拨号/重启**。

- `Connection` 接口只有 `Close`；测试同时钉住 `Registry` 的导出方法集，使新增
  create/dial 动词无法悄悄混入。
- 每槽一条记录，按 `(epoch, generation, RuntimeID)` 排序与匹配。同一 generation
  下换成另一个容器一律拒绝——**不论 epoch 是否前进**，因为这正是 URI 精确匹配的
  对端校验看不出来的复用。
- 回收路径四条：显式 `Revoke`、被更新代次顶替、`Release`、以及租约不再校验通过时
  `lease.Fencer` 的回调。后者直接复用 [P4a](p4a-lease-authority.md) 的合取校验。
- `Revoke` 是**屏障而非仅关闭**：同一临界区内抬高每槽撤销水位线，使「撤销前已通过
  admit、撤销后才 store」的竞态无法把已撤销的 epoch 重新装回。
- `Release` 在条目已被回收时返回 `ErrNotHeld` 并明确标注为良性，避免撤销风暴中
  `defer Release` 产生虚假失败。

### Review 发现并已修的缺陷

1. **跨 epoch 的身份漂移未拦**：原实现仅在 epoch 相同时比对 RuntimeID，另一个容器
   可以在更新的 epoch 下继承同一 generation——与包头声明的规则直接矛盾。
2. **`Revoke` 只是关闭而非屏障**：admit 在前、store 在后的适配可在撤销返回后重新
   装回该 epoch，直到下一次 revalidate 才被清掉。
3. **`Release` 在正常失租路径上返回错误**，会让调用方的清理路径虚假报错。

另有三个存活变异体（拒绝路径漏 `release()` 造成 admit 泄漏、`closeSlot` 去掉
generation 守卫、`Revoke` 的 `<=` 改成 `==`）以及一个名不副实的并发测试。现已全部
补测：五个守卫逐一做变异验证，去掉任一即 FAIL；并发测试改为 Adopt/Release/Revoke/
Revalidate 四路真并发，断言任何连接至多关闭一次且结束时注册表与 fencer 均为空。
为使 admit 泄漏可观测，`lease.Fencer` 增加 `Len()`。

### 验证与剩余

`internal/runtimeregistry` 与 `internal/lease` race ×10 通过；全仓离线
`go test -race`、`go vet`、linux/amd64 编译通过；`make -C recovery check`
236 / 150 / 186 与基线一致。

**仍未做**：**没有任何生产代码 import 本包**——`hostagent` 的 `Runtime` 尚未交由
注册表托管，`Controller.Start` 仍把连接直接返还调用方。权威 writer 与续期循环
依旧未接线（`Coordinator`/`FailoverController` 无非测试调用方）。真实 Redis/MySQL
集成测试仍整体 skip。分数仍 32%。

## P4a：执行租约权威的状态转换设计与两库合取校验

2026-09-19。P4 第一条「先写状态转换设计」。设计与边界
[p4a-lease-authority.md](p4a-lease-authority.md)。

### 本轮实现

- 设计文档确立权威划分：Redis 令牌 + TTL 是围栏权威，SQL `execution_leases` 是
  持久归属与撤销事实；**route TTL 不是执行租约，签发回执不是租约权威**。含完整
  分歧矩阵与失败/竞争恢复规则。
- `Coordinator.Validate` 由「只问后端」改为**两库合取**。`Revoke` 先写 SQL 再删
  令牌，删令牌失败时后端会继续把该 claim 报成 current 直到 TTL——只问后端的校验
  会继续授权一个已撤销的租约。任一库不可读即失败关闭。过期刻意不由持久行再推导，
  以免时钟漂移变成误撤销。
- `Fencer` 与 `Renewer` 都曾把依赖写死为 `Backend`，等于把「只有 Redis」固化进
  类型。现分别放宽为 `Validator` 与 `Refresher`，`*Coordinator` 均满足；
  `Coordinator.Renew` 相应接收 TTL（此前它用 `c.ttl`，而 `Renewer` 传
  `timing.OfflineAfter`，两者本就可能不一致）。
- `Fencer.Revalidate` 按 `Claim` 去重：同槽位所有连接共享同一 claim，每轮每个
  不同 claim 只发一次权威往返。

### Review 发现并已修的两个真缺陷

1. **跳过过期会造成两库分歧**：`Renewer` 只能接 `Backend`，`Coordinator` 无法
   驱动它，因此续期只刷新 Redis 令牌、永不更新 SQL `expires_at`；而
   `ValidateCurrentProxyLease` 读的正是 `el.expires_at > ?`。同一租约会被执行侧
   判为有效、被代理侧判为失效。修法是让续期能走 `Coordinator`（`Refresher`），
   使该状态不可达，而不是在校验侧补一个带时钟偏差预算的过期判断。
2. **`Revalidate` 的 N+1 加共享超时**：逐连接串行校验且不按 claim 去重，加入持久
   库读取后单项成本上升约一个数量级，慢库会让整轮超出自身 deadline，进而关闭
   其余**全部** tunnel，自我放大。按 claim 去重后消除。

另修：`%v` 吞掉错误链改为 `%w`；测试桩与真实 SQL 语义漂移（忽略 ownerID、重复
撤销覆盖首次时间戳、行只按 epoch 索引而未覆盖 slot）。

### 验证与剩余

18 个子测试通过；变异验证非空跑（把合取改回只问后端即 FAIL）。全仓离线
`go test -race`、`go vet`、linux/amd64 编译通过，`internal/lease` race ×10；
`make -C recovery check` 236 / 150 / 186 与基线一致。

**仍未做**：`Coordinator`/`FailoverController` **依旧没有非测试调用方**，生产
权威 writer 与续期循环未接线——这是 P4 主体，未因本设计关闭。**runtime registry
不存在**。撤销传播仍不完整：`control/server.go` 的会话表按 **NodeID** 索引，不绑定
epoch 或租约，撤销执行租约**不会**关闭 worker 的 mTLS 流。真实 Redis/MySQL 集成
测试因环境变量未设置而整体 skip，幂等/并发/超时**未实证**。分数仍 32%。

## P3d：禁用 Docker 自动重启策略

2026-09-19。用户就 P3c 发现的相邻缺陷指示改为 `"no"`。

### 本轮实现

- `provider.go` 创建请求由 `RestartPolicy{Name: "unless-stopped"}` 改为
  `{Name: "no"}`，并在 HTTP 线上显式序列化（不省略字段，免得未来引擎默认值说了算）。
- 共同只读接纳门禁 `validateSandbox` 新增该字段校验（与 P1 的 no-swap 同一处）：
  只接受 `Name == "no"` 且 `MaximumRetryCount == 0`。**缺失/null/空对象/空 Name
  一律拒绝**——与「缺失证据不是安全默认」保持一致，正向 fixture 相应补上真实引擎
  会报的 `{"Name":"no","MaximumRetryCount":0}`。
- 不满足的既有容器只被**拒绝**，不修复、不重建：provider 没有 update 动词。

理由：身份在 tmpfs，Docker 自行重启会绕过 host-agent——`bootstrap.Prepare` 不重跑，
容器带着新私钥且无证书回来，永远不健康；而且崩溃不再表现为 `stopped`，P3c 新路由
收不到信号。`test/dockerbootstrap/policy.go:50` 与 lab Python（`build.py`、
`toolchain.py`、`livebootstrap/policy.py`）**原本就要求 `--restart no`**，生产
provider 是唯一的例外，本轮消除该不一致。

### 验证与剩余

43 个子测试：创建请求的类型化与线上 JSON、7 类会被 Docker 执行的策略在**全部接纳
入口**被拒、缺失证据 4 例被拒、bootstrap exec **前后**漂移各自拒绝且不把已发生的
exec 说成可重试、拒绝时零写入、不静默修复。变异验证非空跑：禁用门禁即 FAIL，
把 Create 改回 `unless-stopped` 亦 FAIL。

独立 adversarial review 确认：`Engine` 接口**没有** update 动词，provider 无法修复；
每个 `readSandbox` 调用方都会复检，bootstrap 前后各一次；既有 `unless-stopped`
容器在 Create 的冲突接纳路径与 `ValidateExisting` 都被拒；Python lab 无冲突。

全仓离线 `go test -race`、`go vet`、linux/amd64 编译通过，`internal/provider/docker`
race ×10；`make -C recovery check` 236 / 150 / 186 与基线一致。

**边界**：这是**创建时预防 + 接纳时检测**，不是持续强制。带外
`docker update --restart=always` 对运行中的容器立即生效，只能在下一次 inspect 被
发现，这是时间点核验而非持续监控。

**已知运维缺口**：`reconcile.NewController` **目前没有非测试调用方**。因此 Docker
守护进程重启或宿主重启后，runtime 容器会保持停止且**没有自动恢复路径**，直到
reconciler 被接线。原先的 `unless-stopped` 只是用一个身份已损坏的容器掩盖了这一点，
不是真正的恢复。此项属 P4 装配范围。

## P3c：reconcile 停止路由改为销毁→释放→重新放置

2026-09-19。用户就 P3b 发现的缺陷选择方案 (a)「改 reconcile 路由」。

### 本轮实现

`Plan()` 在 `DesiredReady` 下的路由：

| 实际状态 | 原 | 现 | 理由 |
| --- | --- | --- | --- |
| `missing` | create | create（不变） | 该 epoch 从未创建过容器，仍可签发 |
| `created` | start | start（不变） | 创建但从未启动，尚未签发 |
| `stopped` | **start** | **destroy** | tmpfs 身份已毁，无法在该 epoch 恢复 |
| `destroyed` | **create** | **release** | 该 epoch 已被用过，重建会撞上钉住旧公钥的回执 |

这让常规分支与既有的**过时代次分支**（原本就是 stopped→destroy、destroyed→release）
一致。`controlAction` 不包含 Place/Release，二者是本地动作。

### 同时修复的 CRITICAL（本改动使其在常规路径可达）

`ActionPlace` 在 `Assignment == nil` 时 epoch 与 actualGeneration 都是 0，幂等键
退化为 `slot/<S>/generation/<G>/epoch/0/actual/0/place`，对同一槽位同一 desired
generation **恒等**。而 Place 一经派发即 `completed`，`ClaimProvisioningJob` 只接受
`pending | failed | running | dispatched`（jobs.go:63-64），**completed 永不可再
claim**；`RuntimeExecutor` 又把该 job ID 当作 `AssignmentReservation.ID`，即
`slot_assignments` 主键。因此第二次 Place 会静默失败，槽位永久没有 assignment。

改动前该死锁基本不可达（DesiredReady 下的 Release 只来自过时代次分支，代次通常
已变而产生新键）；改动后每次 destroy→release 都会落到同一个烧毁的 Place 键上。

修复：`reconcile.Slot` 增加 `NextExecutionEpoch`（store 在 `ReserveAssignment` 内
递增，SQL 与内存实现一致），Place 的幂等键改用它；缺失时 `Plan` 直接报错而不是
发出必然碰撞的键。

### 验证与剩余

新增：停止→销毁→释放→放置整序列、两次连续 placement 的幂等键与 job id 必须不同、
缺 next epoch 时拒绝放置。改动前**没有任何测试**覆盖 `DesiredReady + ActualStopped`
或 `+ ActualDestroyed`——这正是缺陷得以存活的原因。

独立 adversarial review 确认：前提四条成立（身份在 tmpfs、签发发生在 START 而非
CREATE、回执唯一键钉住公钥、旧路由必然 `ErrRejected`）；无回归（`controlAction`
无法下发 STOP，不存在 idle-suspend/scale-to-zero 特性）；状态可达性正确；幂等键
与旧路由无跨部署碰撞。

全仓离线 `go test -race`、`go vet`、linux/amd64 编译通过，`internal/reconcile`
race ×5；`make -C recovery check` 236 / 150 / 186 与基线一致。

**已知保守性**：`dockerState` 把所有非 running 非 dead 容器映射为 stopped，因此
被 INSPECT 观测到的「已创建但从未启动」容器现在会被替换而非启动。多花一次放置，
仍收敛，不造成身份复用。

**待用户决定的相邻缺陷**：`provider.go:257` 的 `RestartPolicy: unless-stopped`
与 tmpfs 身份模型矛盾——Docker 会绕过 host-agent 自动重启容器，`bootstrap.Prepare`
不会重跑，容器换了新私钥却没有证书，永远不健康，而崩溃也就不会表现为 `stopped`，
本轮新路由因此收不到信号。`sandbox.go` 未校验该字段，也无测试依赖它。**未擅自
修改**（涉及宿主重启后的运维行为）。

## P3b：实例身份生命周期合同与持久 home 边界

2026-09-19。规划入口
[2026-09-18_13-17-37-isthmus-claude-handoff.md](../../../docs/plans/2026-09-18_13-17-37-isthmus-claude-handoff.md)
P3 后两条；设计与边界 [p3b-identity-lifecycle.md](p3b-identity-lifecycle.md)。

### 本轮实现

`internal/identitylifecycle` 纯规则集（不存储、不是第二份状态权威，当前无生产调用方）：

- 事件 `adopt`/`restart`/`upgrade`/`account-change`/`destroy`。只有 adopt（接管仍在
  运行的同一容器）保留私钥并要求绑定完全相同；其余丢失私钥的事件必须 **epoch 与
  generation 同时严格前进**。generation 每槽只增不复位，换回旧账号也不回退。
  `SlotID`/`NodeID` 非 destroy 事件不得变更。事件由调用方给出，不从绑定反推。
- `ValidateHistory(floor, steps)` 要求首步不低于 floor（该槽历史最高 epoch/generation，
  含已销毁者），否则销毁后可从 generation 1 重生，而旧证书仍在有效期内。
- `ValidatePersistentPaths` 为**白名单**（`/home` 之下），因为 Debian 基础镜像
  `/var/run` 是 `/run` 的符号链接，黑名单会放行 `/var/run/execution/identity`；
  身份重叠检查先于白名单执行以给出精确原因。不挂载任何东西。

三条依据均已核实：身份在 `/run` tmpfs、停止即毁；对端校验是 URI 精确匹配无 CRL；
回执唯一键 `(slot_id, execution_epoch)` 且比对 `PublicKeySHA256`。

### 发现的既有缺陷（未修复）

**被停止的 runtime 容器目前无法重新 bootstrap。** `reconcile.go:218` 把
`ActualStopped + DesiredReady` 路由为 `ActionStart`，`control_executor.go:36-41`
用原 epoch 下发且 generation 不变，`bootstrap/coordinator.go:52-63` 按同一 binding
再签发；但停止已抹掉 tmpfs 身份，实例呈递新公钥，而 `(slot, epoch)` 回执钉住旧的
`PublicKeySHA256`，`SameReceiptIdentity` 比对失败即 `ErrRejected`。关闭它要把
stopped 路由成 destroy→release→place 以分配新 epoch，属控制面行为变更，本切片
刻意不做，仅以 `TestResumeAtTheSameBindingStaysRejected` 固化冲突。

### 验证与剩余

每个拒绝断言具体原因串而非仅 `errors.Is`；覆盖 floor 半回退（epoch 高于 floor 但
generation 低于 floor）、历史链接、destroy 终结性、`/var/run` 别名、`/home` 本身、
`/homework` 段边界、控制字符与超长路径。两轮独立 adversarial review 共 8 项缺陷
全部已修（含与回执唯一键矛盾、销毁后重生未锚定、测试空跑、别名绕过、分层污染、
身份重叠检查被遮蔽成死代码）。`go list -deps` 确认生产包只依赖 `runtimeidentity`。

本机离线全量 `go test -race`、`go vet`、linux/amd64 编译通过，本包 race ×5 通过；
`make -C recovery check` 为 236 Python / 150 Bun / 186 镜像 Python，与基线一致。

**仍未做**：无生产调用方；未实现轮换（`InstallCertificate` 仍拒绝替换，仓库中
`Epoch`/`Generation` 依旧无任何代码递增）；撤销传播未做——lease 撤销仍只标记 DB，
不拆除身份或已建立的 worker mTLS 连接（P3a 只回收了出口 tunnel，那是代理绑定），
归 P4。未 SSH、未连 Docker、未请求模型、未部署。分数仍 32%，VM0b/VM0c/N3 开放。

## P3a：出口 CONNECT 结构性拒绝矩阵与在途回收

2026-09-19。规划入口
[2026-09-18_13-17-37-isthmus-claude-handoff.md](../../../docs/plans/2026-09-18_13-17-37-isthmus-claude-handoff.md)
P3 前两条；设计与边界 [p3a-egress-deny-matrix.md](p3a-egress-deny-matrix.md)。
不重做 P1/P2，不改 provider，不纳入暂停的 `build.py`/`runtimekit` WIP。

### 本轮实现

- `internal/hostagent/egress_deny.go`：`classifyEgressTarget` 在每槽 allowlist
  **之前**拒绝 metadata（IMDS/ECS/阿里云/`metadata.*` 主机名）、链路本地、未指定、
  组播广播、IPv6 字面量（含 `::ffff:` 映射形）与名称解析端口 53/853/5353。
  注册时命中即整个 binding 失败（槽位无出口，而非部分规则生效），请求时再复判。
  无可关闭该策略的开关。
- `validTargetHost` 要求非 IP 字面量主机的最右标签以字母开头，堵住
  `2852039166`/`0251.0376.0251.0376`/`0xa9fea9fe` 这类经上游代理解析回
  169.254.169.254 的编码规避。
- 每个拒绝返回 403 + `X-Execution-Egress-Deny` 原因头，不是静默挂起。
- `EgressRegistry` 发布撤销事件，`EgressGateway` 关闭同槽且代次不高于撤销代的
  在途 tunnel：`Unregister` 与 epoch 换代都真正回收 relay，不再只拒下一次 CONNECT。
  撤销只在成功路径发布；`Serve` 返回（含 nil listener 早退）时注销订阅。

### 验证与剩余

`internal/hostagent` 全量通过，race ×10 通过；全仓离线 `go test -race`、`go vet`、
linux/amd64 `go build ./cmd/...` 通过；`make -C recovery check` 为 236 Python /
150 Bun / 186 镜像 Python，与基线一致。两轮独立 adversarial review：先发现数字
编码 IPv4 绕过（HIGH）、失败注册误发撤销、监听器泄漏；复核后再发现 nil-listener
早退漏注销；四项均已修并补回归用例。

**这不是内核网络门**：容器内进程直接建 socket、DNS rebinding 到 metadata/私网、
DoH 共用 443 都不在本层覆盖；宿主与跨槽可达性仍须 VM0b 在专用 Linux 上做 netns/
防火墙实证。loopback/私网目标刻意仍由 allowlist 管辖，不等于宿主隔离已完成。
本切片尚未接进 host-agent daemon（`AllowedTargets` 无非测试生产者）。未 SSH、未
连 Docker、未请求模型、未部署，未打开业务开关。分数仍 32%，VM0b/N3 保持开放。

## S2b5b：真实 provider 双实例实验合同

2026-09-18。规划入口
[2026-09-18_13-17-37-isthmus-claude-handoff.md](../../../docs/plans/2026-09-18_13-17-37-isthmus-claude-handoff.md)
P2；合同 [s2b5b-provider-lifecycle.md](s2b5b-provider-lifecycle.md)。不重做 P1
swap 策略，不纳入暂停的 `build.py`/`runtimekit` WIP。

### 本轮实现

- 固定槽位 `slot-p2-a`/`slot-p2-b`、独立账号 hash、派生镜像 digest 合同，以及
  host bind/非 Internal/MemorySwap 漂移/交叉 CID/公网 endpoint 拒绝矩阵。
- `internal/providerlifecycle` 通过真实 `provider/docker` Create/Inspect：两槽位
  各自独占 Internal=true bridge，创建请求 `MemorySwap=Memory`、无 bind/volume、
  监听 `0.0.0.0:8093`、环境不含原始账号；交叉 slot 与替换物理 CID 的
  `ValidateExisting` 失败。清理只 Destroy 清单内 ProviderRef，未跟踪 ID 事先拒绝。
- 实验派生 Dockerfile 只 `FROM` 已有 digest 并 `COPY /worker`；live Docker 仅在
  `EXECUTION_PROVIDER_LIFECYCLE_DOCKER=1` 下运行，默认 skip。

### 验证与剩余

本机 `go test`/`go test -race`/`go vet` 覆盖 `internal/providerlifecycle` 与
`test/providerlifecycle` 通过。只读预检：本机 `/var/run/docker.sock` **不存在**，
因此未创建网络/容器，未声称 cgroup swap 或跨容器 mTLS。未 SSH 216/170，未打开
业务开关，分数仍 32%；K3/K4/N3 保持开放。下一步：在专用 Linux 上先重核业务容器
基线，再按合同实跑双 Internal 网 START/mTLS 与 cgroup。

## S2b5a：禁止swap策略与时间命名交接规划

2026-09-18 13:26 +08:00。用户要求先按时间命名规划，再继续开发并准备交给Claude。
[接手入口](../../../docs/plans/2026-09-18_13-17-37-isthmus-claude-handoff.md) 规划提交
`4cbb6df`；本地策略切片 `2b41e90`。文档包含分支/基线、暂停WIP、完整模块位置、
P1–P5顺序/停止线、离线验证命令和逐阶段执行记录；保留CCMAX原服务账户/成本账，
不把Sub2终端用户计费权威误解为删除CCMAX现有扣减。未创建/启动新的Claude任务。

### 本轮实现

- Docker Engine `HostConfig.MemorySwap` 完整投影/序列化；创建时显式等于正Memory。
- shared sandbox只读门禁拒绝missing/null/0/-1/-2及不等于Memory，覆盖既有实例
  接纳、Inspect/InspectSlot、Start、RuntimeEndpoint、ValidateExisting及bootstrap
  执行前后核验。不更新、重建或清理不满足新规则的旧容器；无安全策略豁免开关。
- 原Docker管理实验的Go协调器也核验相同swap字段，Python外层原有规则不变；
  只读worker bind仍是原实验的特定例外，不允许到生产provider。
- 直接provider Stop/Drain/Destroy原本不是接纳门禁，本轮保持其清理语义不变，
  不声称所有底层操作都通过此规则；新的创建响应仍不是内核执行证明。

依据：[Docker官方文档](https://docs.docker.com/engine/containers/resource_constraints/#--memory-swap-details)。
只限制Memory不能代替显式禁止swap；内核能力/cgroup有效值仍须专用Linux实测。
不以“宿主没配置swap”、容器内free、旧network-none实验或本次HTTP夹具代替该证据。

### Review、测试、剩余

独立 `provider/docker/memory_swap_test.go` 用真实本地HTTP Engine编解码检查请求字段
和返回投影；每个负例先验证原正向fixture。覆盖缺失/null/默认/无限/负值/上下漂移、
错误JSON类型/溢出、非正Memory、所有接纳入口及Request/Install前后漂移。预检失败
时不exec/修改Docker；exec期间漂移时准确记录已exec一次，返回拒绝而非可重试就绪。
不能撤销已发生的exec/安装或已签证书，仍是point-in-time，不是持续监控。

三位代理分工和交叉review，限定改动无未处理P1/P2；root实验协调器也有独立review。
最终全execution-plane离线race/vet、Linux/amd64 CGO=0命令编译通过；provider/
hostagent/control/worker相关race三遍，swap定向race二十遍通过，实验协调器race
三遍通过。`make -C recovery check`：236恢复Python、150 Bun/1027断言、186镜像
Python通过。未SSH、真实Docker运行、下载/替换工具、模型请求、线上配置/UI/数据
写入、迁移、部署或push；原build.py/runtimekit WIP保留且未提交。

本轮只完成P1源码/Engine策略，不关闭N3/K3/K4/H5或镜像完整交付；总体32%、镜像40%
不变。Claude下一步按时间命名规划P2，完成实验设计/只读预检后，用当前provider验证
两独占Internal网络实例、准确mTLS与真实cgroup策略；不要重写已完成的P1。

## S2b4：默认关闭的服务入口与持久化签发装配

2026-09-18，规划 `4e3826b`，持久化签发装配 `67e3b77`，host-agent 入口
`4a540f9`。本轮补上 S2b3 的两个实际入口断点；不是镜像新版本或业务整链上线。

### 已交付

- `cmd/host-agent` 明确注入 `daemon.SelectRunner`；关闭时仅读开关，保留旧健康
  服务，不加载节点文件/连接 Docker/控制端。错误不回退。共享 service 不反向依赖
  host-agent，避免原 secure_activation 组件测试的依赖循环。
- 独立 `hostagent/daemon/` 目录按 entry/config/identity/composition/executor/run/
  health 分文件。预签发专属 node 证书由物理路径安全加载，逐级 no-follow、所有者/
  权限/大小/硬链接/读后替换核验；严格 exact node SPIFFE/P-256/keypair/CA/EKU。
  控制连接保留 Go 标准主机名与链验证，TLS1.3，仅拨配置私网/回环 literal IP，
  不使用环境代理。宿主不生成 CA、不持有票据签名私钥、不复制实例私钥。
- 同一真实 Docker HTTP adapter/provider 接 RPC enrollment→lifecycle.New→严格
  START；固定安全策略和每实例 CA pin。无 activation、业务/Health票据、runtime
  registry 或出口代理服务。Hello 仅生命周期模式，placement 对其标签或能力显式
  拒绝，包括无约束、sticky 和主动要求 lifecycle marker 的业务请求。
- 回环健康接口 `/readyz` 始终 503/production_ready=false。过期结束本次运行；
  停机先封闭命令、取消控制流、有界等待，再关闭连接。跨重连旧排队命令不能落到
  provider；不响应 context 的已进入操作超时为 ErrShutdown/非零退出，不声称强停。
- 独立 `service/runtimeenrollment/`：显式默认关闭、SQL receipts/assignment store、
  单独 Redis lease validator 与固定 `execution:lease:v1:` 前缀；只读 PING，完整
  失败/退出清理；不应用迁移、不自动授租、不使用 Memory fallback。启用时证书
  TTL 与 Authority 对齐，短 TTL 缩小续期窗口，关闭时保留原配置行为。

### 验证与 review

实际 `prepare`→ControlClient 使用合成专属节点证书、真实 loopback mTLS NodeControl
和 Unix socket HTTP Docker **夹具**（非真实 Docker）。收到 lifecycle-only Hello，
下发缺失实例 START 得到失败，至少执行 PING/version/实例检查且 Docker writes=0；
错 node 在 Docker 前拒绝，错控制主机名无法建立 Control stream。该测试不代表
真实 provider 容器创建/网络/跨容器 mTLS；已有 S2b3 worker 组合测试保持独立证据。

Redis 用真实 go-redis 客户端连接本地 RESP 夹具：只见 HELLO/PING 和只读 GET Lua
EVAL，固定前缀，空库不能授权；并非真实 Redis 集群或 MySQL 持久化并发验收。

三个代理交叉 review；修复默认 ControlConfig 覆盖签发配置、启用时短 TTL 不匹配、
启动日志期间取消被随机归类为运行故障，新增回归。额外修正全量测试发现的入口
循环依赖。最终本轮范围未发现未处理 P1/P2；不是整个仓库/隔离方案的安全认证。

最终全 execution-plane 离线 `go test -race -count=1 -timeout=120s ./...` 与
`go vet ./...` 通过；Linux/amd64、CGO=0 的 `go build ./cmd/...` 通过（只编译不运行）。
daemon/config/service/placement 定向 race 三遍，真实入口 TLS/拒绝/取消回归十遍，
executor 跨会话/封闭竞态定向二十遍通过。恢复回归两次通过：236 Python、150 Bun /
1027 断言、186 镜像 Python。未 SSH、部署、push、真实模型请求或读取账号凭据，
216/170 的业务容器、UI、数据均未操作；暂停 build.py/runtimekit WIP 未纳入提交。

### 剩余项与分数

**生产 execution lease writer 尚未实现**：Redis 能连接不代表 slot 有租约；缺租约
签发必拒绝。完整 host-agent 数据/出口服务、existing-only runtime registry、真实
SQL 并发、独立 Internal 网络双实例 mTLS、MemorySwap/ACL、在途 lease 失效与 CLI
桥接仍开放。两个入口默认关闭，不能启用生产 onboarding 或把账号标记 migrated。
K3/K4/H5 未完整关闭，总体仍 **32%**（镜像40%、运行服务60%、身份20%、宿主40%）。
下一切片应聚焦真实 provider 双实例管理/mTLS 与 swap/网络门禁，随后补权威租约
写入/续租并接现有 CLI，不再另起无关构建工具或扩展 Sub2 业务模块。

## S2b3：正式START命令的已有实例认证装配

2026-09-18，先规划`3b41be9`；物理实例锁定`277406d`，认证START组件与控制流
组合测试`b43fd69`。发现并修复原START仅调用provider.Start/Inspect、未走证书
bootstrap的接线缺口；worker原本等待证书才监听，单纯启动容器不能完成这个流程。

### 实现与文件职责

- `provider.Instance.RuntimeID`来自真实Docker Container.ID，保留ProviderRef原有
  逻辑名称，避免破坏旧Destroy/网络名称语义；不改proto或持久化结构。
- `provider/docker/existing.go`按确切物理CID只读核完整slot/account/epoch/gen/
  image/资源/用户/出口/sandbox，无Create、修复或重新发现替代容器。
- `hostagent/runtime_existing.go`在启动前、bootstrap后和TLS握手后重核完整spec；
  Start、管理投递、ready检查与endpoint查找使用同一物理CID。禁止Create/recreate，
  失败关闭连接但不盲目Stop/Delete。只做真实TLS1.3/HTTP2握手，不消耗业务票。
- `hostagent/lifecycle/`将同一provider、原Coordinator、Controller和命令执行器
  装配为严格START入口；要求认证enrollment、CA trust及node certificate，禁止
  自定义替代Startup，使用拒绝所有业务ticket的source，返回后关闭临时连接。
- `command_start_proof.go`保留进程内准确实例的成功启动记录，失败、漂移、健康
  丢失、drain/stop/destroy/revoke后清除；普通TCP/INSPECT不能复活认证失败状态。
  它不是持续mTLS检查、业务runtime registry、有效lease或持久化授权。

### 实际控制流组合测试

`lifecycle/integration_test.go`使用实际loopback TLS NodeControl服务与原ControlClient：
EnrollNode→活Control session→Server.Dispatch START→认证EnrollRuntimeCertificate→
原worker.RunProcess安装及监听→Controller mTLS→命令结果写MemoryRepository。
只有物理container provider是fake；控制RPC、TLS和worker不是fake调用，未使用Docker。

首次及重复START均成功；撤销独立lease后的第三次START拒绝，即使既有worker的
TCP还存活，随后INSPECT和Snapshot也保持不健康。断言Create=0、物理CID Start=3、
成功安装=2、认证签发RPC=3、Control业务票/credential提交=0。没有账号或模型请求。
这只证明重启/重验被拒，不声称撤销既有业务流或关闭已有worker监听。

另有真实loopback mTLS正负例：错slot/account/node/epoch/gen/CA、明文对端；
无完整只读provider能力、无bootstrap、无物理CID及spec漂移启动前拒绝；每段
取消与TLS后置验证失败均失败关闭并释放连接。START过程中不调用Create/Stop/Delete，
不请求Health/业务RPC或ticket。Docker只读核验有真实ID来源及资源/身份漂移回归，
但本轮未新增实际Docker验收。

### Review、回归与边界

三位代理交叉review，关闭三处P2：失败START可被普通INSPECT恢复健康；取消发生在
Inspect返回时仍能回成功；epoch/gen/image等实际漂移后恢复旧metadata可复用proof。
新增确定回归，延迟旧命令不会清掉当前新代proof。最终限定改动未见未处理P1/P2。

全execution-plane离线race/vet通过；provider及hostagent相关包race三遍，实际控制流
组合测试另做race十遍通过。`make -C recovery check`通过：236恢复Python、150 Bun/
1027断言、186镜像Python。测试使用合成密钥/临时目录，未访问216生产或170测试机，
无部署、push、真实数据库迁移、宿主Bun/网络/防火墙/UI/业务数据改动。原有暂停
`image/lab/build.py`及`image/runtimekit/`WIP继续保留、未纳入本轮提交。

**本轮是可装配组件，不是host-agent二进制已启用**：`cmd/host-agent`仍为健康HTTP
骨架；orchestrator生产装配尚未注入RuntimeEnrollment，不能将组件测试作为已上线。
下一步先完成这两个入口的显式默认关闭配置/装配，再做真实provider两独占Internal
网络实例mTLS实验（同步收紧MemorySwap门禁），之后CLI桥接和在途lease失效/出口矩阵。
完整K3/K4/H5未关闭，固定总分仍32%，镜像40%、运行服务60%、身份20%。

## S2b2-live：双实例真实Docker证书管理通道

2026-09-18，规划`09e3b99`，精确拒绝信号/真实HTTP回归`477925c`，按模块实现
实验协调器与双容器实验`2cf0fe9`。本轮补上此前mock的Docker CSR/证书传输，不是
完整host-agent、跨容器mTLS或CLI桥接验收；固定总分仍32%、镜像40%、身份20%。

### 实现及review

- `test/dockerbootstrap/`按输入协议、容器策略、控制面夹具、验证流程拆分；
  `image/lab/livebootstrap/`独立管理制品校验、profile、有界IPC与创建/清理。
- 实际Unix socket `HTTPEngine`执行固定UID1000 CSR导出/公开证书安装。控制面使用
  真实bufconn TLS、Node enrollment及活Control session，签发调用实际RPC；
  assignment、双lease和公开receipt仍为Memory，并非SQL/Redis实证。
- 修复验收误报风险：只有准确exec/CID、已完成、exit2、空stdout、固定拒绝stderr
  才算 `ErrInstallRejected`。Docker错误、输出错误、取消、身份不符不能算负例PASS。
  Request语义不变；inspect返回后再次核取消，busy75仍只允许限时重试。
- 独立review还关闭迟到IPC仍被接受、清理回执覆盖旧阶段文件两个P2；增加
  capacity/select/read/parse后超时复核、独立live回执和未知B创建只清已知A的回归。
  两位代理审查与root复验后，无未处理P1/P2。

### 原生Linux实跑

仅170测试机，43为跳板；未访问216生产。root实验根
`/var/tmp/isthmus-s1b.ZZoFXYUK`，使用原S1b固定base：
`sha256:3f7a9a38c6ae0a779eb563ceb98cd70db6ae62243bf19ffbaa35587fa15ab807`。
预检MemAvailable约4.30GiB，入口要求至少3GiB；两个同时存在的worker各UID1000、
1CPU/1GiB/swap0/pids128/core0、cap0/NNP、只读根、network-none、私有tmpfs。
实际容器内回读cgroup、仅lo、挂载只读/noexec、UID/权限和worker SHA全部通过。

明确实验例外：唯一host bind是当前实验根的root所有、0555公开worker文件，只读
挂载`/worker`；无目录/socket/home/凭据挂载，不放松生产provider禁bind规则。
可信宿主管理协调器有Docker管理员权限，不冒称安全沙箱；CA/node/ticket私钥仅在
该进程RAM，worker私钥仅各自tmpfs；没有私钥通过管理通道或证据输出。

Go1.26.0、linux/amd64、CGO0、trimpath/buildvcs=false；345项Go/proto/module
源码摘要、24项公开helper/锁和两ELF制品保全，上传后逐项校验：

| 制品 | 字节 | SHA256 |
| --- | ---: | --- |
| worker | 18867914 | `35fed251c55fad10e83396c40bcdef2f079adf0ec0d6128a3588237d81f6b4f9` |
| bootstrap-driver | 20182744 | `4ef25361e31e8ea67f11e1c727666bcec0c6a86663cb1b0f4d34d6d9be7b52fb` |

一次双实例真实Docker流程2.01秒，包含创建、核查、实际管理exec及清理，exit0：

- A/B公钥不同；同key新ECDSA CSR重试返回原叶证，不另发新证。
- 安装前不监听；A证书装B、错CA装A均收到精确worker拒绝，仍不监听。
- 正确证书安装、相同公开bundle重装均成功，既有UID65532固定TCP healthcheck通过。
  这里只证明TCP就绪，不冒称TLS握手通过。
- A撤销独立lease、B撤销DB侧Memory lease后，新的签发RPC均PermissionDenied；
  不代表已经建立的TLS/业务连接已被撤销。
- `real_model_requests=0`、`private_keys_exported=false`；
  `cross_container_mtls/production_provider_adoption/cli_bridge/production_ready=false`。

已删除且仅删除本轮两个已核owner的准确CID，tmpfs/home/测试私钥随之销毁：
`f39c9b2df5ffb618b59da91dae8bca56d7925a6ef3a02ddc8faab0a988355d44`、
`af1b47608c065158629c00a7a6e9ccafff7af80a08de799764853f9bfc59a76f`。
cleanup_complete=true、unresolved_create=false，原4业务容器ID/image/start/restarts/
mount元数据相同，未读其Env/Cmd/logs。保留公开制品/实验记录，不删除已有镜像、卷、
网络；生产、UI、业务DB、防火墙、宿主Bun均未改，无push/部署。

### 回归、证据及剩余门槛

全execution-plane离线race/vet通过（清除真实DB/Redis测试变量）；新driver与
provider/bootstrap另有race三遍。最终`make -C recovery check`：236恢复Python、
150 Bun/1027断言、186镜像Python全部通过。新lab8项测试包含真实本地pipe，
但其余Docker操作mock；不能用这些本地测试代替上面的真实Docker实跑。

仓库外0700保全目录：
`/Users/ruanyang/My-project/api/z/isthmus-live-bootstrap.Cc5UqoAH`。
上传包SHA256 `9e16c7b16ec74ec2bc7077a5c58f2f99d677ec30072b63a8e5a09730075973ef`；
Go源码清单 `147e7b250d614f045c16dd3acaacb53e6716d7afe3d2e514cd9512a5a57b4fb8`；
实跑结果 `d846d7b4eea69c728d23c791d9b29aeda575a5f7a12c1b923c7775c63785866d`；
清理回执 `5a2047f72db894492aa6fa930d27aeb973d22c7256d6c0f43ae269b2d0b62609`。

下一门槛：正式host-agent/provider与实际双实例mTLS/在途lease失效组合，再桥接
isthmus/真实CLI。仍需专用internal网络与受限出口拒绝矩阵、真实SQL并发、
持久home/恢复/轮换和最终不可变runtime镜像。仅完成Docker管理通道不能关闭K3/K4。

## S2b2：受认证签发与启动前bootstrap

2026-09-18，先规划`6b228c0`；签发及公开receipt提交`a347da8`，实例启动接线提交
`adecb20`。按功能拆为`runtimeenrollment/{contracts,storage}`、`runtimebootstrap`、
`hostagent/bootstrap`、`provider/docker/bootstrap`；原控制/worker/provider仅保留适配。
本轮关闭的是**组件启动闭环**，不提前关闭完整K3/K4；总体32%、镜像40%、身份20%不变。

### 本轮实现与review

- 原NodeControl新增默认关闭的受认证签发RPC，不新开明文端口。严格TLS1.3/node单叶
  身份，RPC必须使用当前活Control stream同张证书。实例account/slot/node/epoch/gen/
  image来自权威DB；当前session、DB及独立lease在存储前后重新核验，不能用worker
  ready或provider_ref当首次签发前提。签证书不等于签业务ticket。
- 首次公开证书持久化后才返回；assignment及slot/epoch唯一约束，固定完整binding、
  SPKI、CA摘要和有效期。同key的不同ECDSA CSR仍返回原证书，换key/过期receipt拒绝，
  不把重试变成轮换。新增014迁移，**未运行真实迁移**；SQLmock不是MySQL并发实证。
- 正常worker启动先在实例内建独占目录和私钥，等待公开证书，默认最多45秒。
  宿主只有固定CSR导出/公开bundle安装通道，配置钉住确切CA PEM的SHA256。实例验证
  本地key、绑定、CA与首次安装规则后原子保存，安装前不监听。私钥不通过Env/argv/
  RPC/数据库传送；host node key和控制CA key不送入实例。
- 原Controller按Create→Start→Bootstrap→waitReady→实际mTLS Ready接线；宿主也核对
  返回SPKI、CA、身份，再由固定非root Docker exec投递。管理调用前后核准sandbox/
  准确CID/UID/image/代，exec inspect必须具有准确exec ID/ContainerID/状态/退出码。
  Docker通道目前是mock合同验证，不冒称已实跑Docker自动投递。
- 两位独立代理交叉review，修复正常Wait/Install锁竞争导致随机启动失败（只对busy
  限时重试同bundle，不重签），以及最后文件I/O后取消仍可能返回成功/启动监听的窗口。
  固定命令短输出、空exec状态等边界补回归。最终未见未处理P1/P2。

### 原生Linux运行及清理

仅170测试机、43只作SSH跳板，未访问216生产。使用原S1b精确base ID
`sha256:3f7a9a38c6ae0a779eb563ceb98cd70db6ae62243bf19ffbaa35587fa15ab807`，
一个network-none/仅lo测试容器，UID1000、只读根、cap0/NNP、1CPU/1GiB/swap0/
pids128、独立tmpfs、无host bind/端口/新网络。预检MemAvailable约4.29GiB；入口
要求至少2GiB以留宿主余量。root仅capless系统tar上传，新测试程序全部非root执行。
CA、node及实例测试私钥在容器临时目录中生成，从未导出或写入Git。

Go1.26.0、linux/amd64、CGO0、trimpath/buildvcs=false，336项Go/proto/module源码
摘要及19项公开helper/锁有记录；二进制按确切大小/SHA检查后运行：

| 制品 | 字节 | SHA256 |
| --- | ---: | --- |
| runtimebootstrap.test | 7577396 | `82ddc8ad5b4f94d2614717a09a62db15fb2643c22dfe6d58fa2881398d674b1d` |
| worker-command.test | 17804234 | `11c27f63cab386932a42fe6e3e6270f30094533d0ee2e19fa5d28b7cf9e0d59a` |
| control.test | 23121824 | `edd52e8f74a264925f36e5a1e18cb1c51171570e1fa4397072eca01d37241343` |
| docker.test | 11508992 | `37f6a994c45c291073cbcce5410cdebc249e167e16b59aae31019908d717f56a` |
| docker-bootstrap.test | 7101971 | `6d59b6a80bcf93b3bc4eda21df976f5c600085b1a4b91efee558c3ffda468160` |
| enrollment.test | 19563120 | `9b9c4d940ac6bf83b9b9e386ccd1765bdab552cb41d22db317f3f0797b327d8e` |
| receipts.test | 19681944 | `927b208d77dd2b1031866d6c5437e3f2cb56930b8770464f19623656f7642776` |
| host-bootstrap.test | 17551664 | `b7ea8377f3892759f00ff70b54f4aa69d618ede6ac24519a9255d2d7f021d93b` |
| worker.test | 22696343 | `ec5cdcc2cb61657e132a52052f7dfdac8beafc6589ac1d44735e043698576930` |

九组各3遍，共90个顶层PASS，6.41秒exit0。真实控制TLS RPC→受权签发→实例本地
安装→原worker TCP监听→原Controller mTLS→独立票据Health通过；provider/exec为
fake，证书receipt为Memory（SQL另为mock）。覆盖无会话/断连、另一张同节点证书
借会话、错槽/代/key/CA、lease不可用、并发首次key固定、存取期间权威变化、超时/
取消、未安装不监听、错误/重放ticket等反例。没有真实账号/模型请求。

实验根`/var/tmp/isthmus-s1b.CqSmGWCn`，准确CID
`d33c271d3de72b93832c8b2501cf232d3b986e57a600a201742021a79d6788fd`已删除；
临时home/密钥随tmpfs销毁，cleanup_complete=true、unresolved_create=false。
原4业务容器ID/image/StartedAt/restarts/mount元数据一致，未读其Env/Cmd/logs。
没有生产、UI、业务数据库、宿主防火墙、宿主Bun改动或push/部署。

### 回归、保全和下一步

最终全execution-plane离线race/vet通过（移除真实DB/Redis测试变量）；控制集成和
两位代理各自相关模块另做race三遍。固定buf1.72.0/固定插件生成及lint通过。
`make -C recovery check`通过：236恢复Python、150 Bun/1027断言、178镜像Python。
lab另验证空匹配测试拒绝、独立profile、payload绑定与失败清理；这不替代业务验证。

公开源码摘要、测试制品及证据保存在仓库外0700目录
`/Users/ruanyang/My-project/api/z/isthmus-enrollment-artifacts.du7Nqk0y`；无私钥。
上传包SHA256 `f3ebb87cbddf3e9ff34e01b827612b529023a552c286534c1df205bdb48ad30e`；
native日志SHA256 `6ca037e92f7f4ed2f33141472e2fb3bd259566644ad965d42d47d16896358295`；
清理回执SHA256 `3ad18713a605fae9e026b74f775115a58f33e6dfffe51187b3a1d4a4891d93a1`。
运行后仅完善helper docstring说明，不改变执行逻辑。

仍缺：实际Docker投递与双实例mTLS/lease撤销组合、真实SQL幂等并发、host-agent
二进制完整装配、Go控制桥与isthmus/真实CLI接通、持久home/恢复/轮换/撤销，以及
内核受限出口拒绝矩阵。默认生产开关不变，不宣称与线上完全一致或已可上线。

## S2b1：证书安装与实际组件mTLS

2026-09-17，先提交规划`18d16e9`，本地证书模块提交`c766b5d`。
本轮交付安装及原组件接线，**不是S2b自动发证/跨容器整链完成**。
固定台账仍为总体32%、镜像40%、运行服务60%、身份20%；K3/K4完整门槛不提前勾选。

### 实现、review与修复

- 在原`runtimeidentity`独占目录内安装单叶证书；外部配置固定CA，验证本地公钥、
  accountHash/slot/node/epoch/runtime generation、时间/用途/SAN。原子替换同一0600
  状态文件，不更换私钥/机器标识。首次安装、同证幂等；不同证书替换及残留半成品拒绝。
  CLI新增`install`，仅有界公有证书stdin和受信CA文件路径，无私钥导出或CA发现逻辑。
- CSR仍URI-only，签发端派生唯一`.execution.invalid` DNS SAN。TLS1.3默认Go验链、
  时间、用途、hostname保留，再精确校验URI和原始SAN。服务端要求本node的clientAuth
  证书；没有InsecureSkipVerify、系统根/明文回退、可注入验证回调或共享server私钥。
- 原`worker.RunProcess`加载预安装身份后才监听；fake activation也无明文例外。
  原`Controller.Start`使用mTLS并等待gRPC Ready，不以lazy client冒充握手，也不为
  readiness额外消费业务scope票。Health仍要求独立有效ticket，错scope/账号/重放拒绝。
- 显式贯通desired/runtime generation，新增SecureActivationCommand字段并按固定版本
  重新生成proto。Docker label/env/adoption和activation严格核对generation与image。
  review发现旧实例清理不能用新意图代/新image：明确区分当前desired generation与
  target runtime generation，cleanup准确绑定旧assignment的epoch/generation/image；
  create/start不能复活旧代，延迟旧cleanup不能作用于替代实例。相应回归通过。
- 两位代理对非本人安装/CLI/worker/native lab部分交叉review，主代理审TLS与控制目标
  语义。缺证书负例补为有效CA+无身份/无已装叶证/错绑定；错误TLS不拿deadline充数，
  正向Health在负例后仍成功；另直接测试Controller错误代/node不能返回lazy成功。

### 原生Linux最终运行

仅170测试机，43仅SSH跳板；没有访问216生产。沿用已验证的S1b精确image ID
`sha256:3f7a9a38c6ae0a779eb563ceb98cd70db6ae62243bf19ffbaa35587fa15ab807`。
一个受限容器承载真实Go worker和Controller的回环TCP连接，provider创建使用fake，
没有Docker自动证书bootstrap或真实CLI桥接。网络none/仅lo、UID1000、rootfs只读、
cap0/NNP、1CPU/1GiB/swap0/pids128、私有tmpfs、无宿主bind/端口/新增网络；先读回
内核限制与代码SHA。预检MemAvailable约4.29GiB，入口要求至少2GiB以保留余量。
root仅系统tar上传，所有新测试二进制UID1000运行，合成CA/节点/实例密钥不导出。

Go1.26.0、CGO0、linux/amd64、trimpath/buildvcs=false交叉编译三个测试二进制：

| 制品 | 字节 | SHA256 |
| --- | ---: | --- |
| identity.test | 9510801 | `6311166dd35268eb951cc905450361163bd04b451e87f483835dc60ed162081c` |
| identity-command.test | 7530318 | `019d0506e890eb2d7cc676b28e28efa025b8228c488d411c65cae5de2888a64f` |
| worker.test | 22640507 | `5860d405c49854c6332e586099d46212a818bce5958049488cad60c658cf620d` |

三组各3遍（worker仅`^TestProcessMTLS`），共66个顶层测试PASS；两个Controller错误
身份子例各3次PASS；最终2.23秒exit0。正确链/票据通过；错槽/代/node/CA、明文、
TLS1.2、错误用途/隐藏SAN、过期证书、无票/错票/重放、缺启动材料与错误安装均拒绝。
TLS1.2/用途/过期/SAN矩阵在identity组，业务票在实际worker组，不混称为全业务链。

初轮根`/var/tmp/isthmus-s1b.La13G3UH`；加强Controller直接负例后从新根
`/var/tmp/isthmus-s1b.31yVdVI2`重跑全部三组。最终准确CID
`6e831ff8fded76c644e7e1f2d081106e295a0033e185201762e15074225a61d6`
已删除，私有tmpfs与测试私钥随容器销毁；cleanup_complete=true、unresolved_create=false。
两轮原4业务容器ID/image/StartedAt/restarts/mount元数据均未改变，未读取其Env/logs。
没有宿主防火墙改动、生产/UI/数据库操作、真实模型调用、push或部署。

### 回归与保全、尚缺

最终全execution-plane离线race/vet通过（显式移除三个真实DB/Redis测试变量）；
ProcessMTLS定向race另3遍；固定buf1.72.0/仓库固定plugins生成及lint通过。
`make -C recovery check`通过：236恢复Python、150 Bun/1027断言、177镜像Python；
14manifest/116条合同仍只是结构检查，business_verification=false。
旧Docker E2E build-tag编译通过，但实际入口**预期失败**且准确提示缺证书bootstrap，
在任何Docker或0.0.0.0监听副作用之前停止；不是Skip/PASS，不代表容器整链通过。

21项公开helper/锁/测试二进制逐项大小与SHA核对后上传。304项Go/proto/module源码
摘要、二进制、两轮公开证据和回归日志保存在仓库外0700目录
`/Users/ruanyang/My-project/api/z/isthmus-mtls-artifacts.KL3Qq4RI`，不含实例私钥。
最终native日志SHA256 `344af3140ebb264e005f976b0dd6beb801c136ea15d817ce0de1121b06e226a2`；
清理回执SHA256 `f8fcd3ba24da03ed65e9efd8df8d4f988bd93326c72a5ae0ce3596e230037060`。

下一步S2b2：受认证CSR授权/投递必须在worker readiness前完成，生产host-agent真正
装配、双实例实际连接与lease失效验证。仍缺CLI桥接、持久home恢复、轮换/撤销、
内核受限出口与跨槽拒绝矩阵。证书标识不是CLI内部device-ID兼容性证据。

## S2a：双实例本地身份与CSR

2026-09-17，规划`d55d0cd`及账号hash对齐`224114b`先于实现。
只关闭K2（+2分）：**总体32%、身份2/10=20%、运行服务60%、镜像40%**。
使用170测试机，43仅SSH中转；不连接216生产，不用真实账号/模型、不改Sub2或UI。

### 实现与边界

- `internal/runtimeidentity/`：绑定与CSR、私有状态分别成文件；账号hash沿用现有
  `provider.RuntimeAccountID`的32位hex，不新增账号身份算法。独立P256私钥和逻辑
  机器标识在实例内由OS安全随机源生成，0700独占目录、0600文件、文件锁及no-replace
  原子发布。唯一公开出口为身份摘要/CSR，无私钥导入或导出命令。
- `cmd/instance-identity/`：仅Linux UID1000，绑定从有界stdin传入，拒重复/额外字段；
  固定错误。相同绑定重开保留；五字段任何不符均拒绝，不静默覆盖或轮换。
  损坏/未知文件、初始化半成品、软硬链接、FIFO、错误权限/所有者和超限均拒绝。
- `pki.Authority.IssueRuntime`复用现有CA，只接准确URI、有效签名的P256 CSR；用途
  仅serverAuth，并检查CA生效/到期。调用方仍需在外层认证并授权绑定。
  这里只完成签发库及测试，没有向实际容器安装证书或接通worker/host-agent mTLS。
- 应用逻辑机器标识不等于OS `/etc/machine-id` 或线上CLI内部device-id；其兼容性仍
  未证实。临时home销毁后身份销毁；没有持久卷recreate/restore、轮换/撤销或防回滚
  证据。信任宿主/内核和同UID文件所有者，权限不是抵御恶意宿主的安全边界。

### 最终双容器原生验证

两容器同时创建/启动，使用同一S1b固定base和同一代码/Bun1.4.2/CLI2.1.258，
home各自独立tmpfs。UID1000、只读根、cap0、NNP、networknone/仅lo，无hostbind/
端口/新网络/卷。每实例1GiB、swap0、1CPU、pids128，内核读回与inspect均匹配。
预检约4.29GiB可用，启动门槛3GiB以保留业务余量；未放宽权限或资源使测试通过。
旧单实例默认2GiB不变；root仅运行系统tar上传，所有新二进制/CLI均UID1000。

| 验收点 | 实际结果 |
| --- | --- |
| 本地生成 | 两个逻辑机器标识和公钥SHA256均不同，未传入任何私钥 |
| 相同绑定 | init/init/show/request分别启动新身份进程，公开标识与公钥保持一致 |
| 错绑定 | account hash、slot、node、epoch、generation共5类×2实例全部拒绝；之后show仍等于原件 |
| 文件隔离 | 各实例有自己的home标记，看不到对方标记；不是完整网络拒绝矩阵 |
| CSR | 仅导出公开CSR，本机OpenSSL独立验签，两份SPKI SHA256与实例报告分别匹配 |
| 实际CLI | 两实例分别JSON、SSE、取消、超时、上游503全通过；4.85秒/4.82秒，真实模型调用0 |
| 清理 | 两确切CID均删除，原4业务容器ID/image/StartedAt/restarts/mount元数据一致；unresolved_create=false |

最终容器ID为`2d7dd5c0ef399780fd3e33161ad908944358989f028499ea80637ecdbe7329af`、
`bc772b1fc14a22a10edbc8364ec3ba828faa666a7af037a6cb0507894adbc7fd`；临时home及
其中新生成的测试私钥随容器移除。没有导出私有状态，只有公开CSR/摘要与固定测试日志。
按CCMAX测试规范用回环随机路径和合成凭据/usage，不把1/1 fixture当推理或计费结果。

### Review、回归与证据保全

独立review关闭两项P2：初始化半成品被忽略后生成另一身份；Docker创建成功但客户端
未收到CID时误报完整清理。前者严格检查独占目录、不覆盖/删除残留；后者在发起create
前置unresolved标记，只有合法完整CID入清单后解除，不确定时明确cleanup_complete=false。
模拟“daemon已创建后超时”和“非法ID响应”均不再误报成功，也不按前缀扩大删除范围。
两轮原生运行均成功；最后一轮在上述最终修复后重新全跑，不用早期PASS替代最终源码。

全execution-plane离线`go test -race -count=1 -timeout=120s ./...`和`go vet ./...`通过；
三个真实DB/Redis测试变量明确移除。identity/command/pki定向race再5轮通过；独立
reviewer另跑并发/半成品25轮、CLI race及identity/toolchain最终25项mock通过。
`make -C recovery check`：236 recovery Python、150 Bun/1027断言、173 image通过；
最后新增创建不确定性负例后image全量174项及29项定向测试再次通过。
14份/116条合同结构检查仍business_verification=false。

新助手二进制由当前3源码+go.mod/go.sum、Go1.26.0、CGO0、Linuxamd64、trimpath/
buildvcs=false生成；5,756,637字节，SHA256
`9ffc0b47921e1dd588f100c5c4447b74f94204293fc13d469c7386897d523326`。
最终远端根`/var/tmp/isthmus-s1b.mLeicj3w`，两轮公开证据备份在仓库外0700目录
`/Users/ruanyang/My-project/api/z/isthmus-identity-artifacts.7QKZpqSH`；不含实例私钥。
最终32项输入源码/锁/助手清单SHA256为
`46d3992b50eb54cf429e4d45f0c65de0686f9cd964f4a825b439544a3ae8de70`，下载后逐项
大小/摘要与工作区一致；二进制及其构建清单保留，不进入Git。

I4/I5、K1/K3–K5、N3–N5及完整调用链继续开放；S2a不是已完成VM/TLS/出口防泄漏验收。
下一项是S2b受认证签发、安装与实际TLS，再验证可用出口；不借此启用生产新执行面。

## R3：单实例真实CLI双向stream-json闭环

2026-09-17；先提交收敛规划`dad891e`，暂停S1d构建WIP，再实现真实执行器。
只关闭R3（+4分）：**总体30%、运行服务12/20=60%、镜像6/15=40%**。
生产216.106.185.119未连接；170.106.159.197仅测试，43.153.75.220仅SSH中转。
没有真实凭据、模型调用、宿主Bun替换、生产配置/数据/UI变更、push或部署。

实际测试链为HTTP调用方→新isthmus CLI probe→官方CLI2.1.258→同容器回环合成
上游→原路JSON/SSE。stdin和stdout均stream-json；stdin为一个用户记录加EOF。
按CCMAX测试技能使用随机回环capture路径、显式synthetic凭据/usage、固定计数诊断，
不保留认证头、正文或CLI原始stdout/stderr。不是模型推理或计费准确性证据。

### 范围与原生结果

- `src/runtime/cli/`分别管理参数/环境、进程与有界事件解码；`src/app/cli/`为显式
  loopback工厂；`test/cli/`独立合成上游与实际冒烟；`image/lab/cli.py`只复用既有
  宿主门禁和容器，不新增构建框架。默认fake入口和Sub2业务代码均未改。
- 固定模型、max_tokens128、单轮文本、单并发；其他请求字段明确拒绝。CLI工具、MCP、
  用户/项目配置、会话持久化及非必要外联关闭。显式env，不继承宿主代理或凭据。
- 使用S1b精确base image ID及已锁定CLI2.1.258/Bun1.4.2；10源码+2二进制在容器内
  逐项复核SHA/版本。Bun1.4.2仅实验，项目/CI仍1.3.9。不是新不可变组合镜像。
- UID1000、rootfs只读、cap0、NNP、networknone、只lo、无宿主bind/端口；私有空白
  tmpfs home700/noexec，代码树UID1000不可写；内核回读1CPU/2GiB/swap0/pids128/core0。
  CLI/服务/stub共用一个隔离容器，不是已通过双实例或内核可用出口验收。

| 最终真实CLI测试 | 实际结果 |
| --- | --- |
| 非流式JSON | 固定合成文本、usage1/1；恰好1个CLI/上游请求，进程回收 |
| SSE | 文本/usage增量返回；完整stream+成功result+exit0+清理后才发message_stop |
| 客户端取消 | 已到达stub才取消；无完成响应，active=0 |
| 执行超时 | 已到达stub，HTTP504；active=0 |
| 上游503 | HTTP502；恰好1请求，无重试或伪成功；错误映射不冒称线上保真 |

最终冻结源码版本在原生Linux amd64运行exit0、4.85秒；真实模型调用0。
最终容器`435519d57c1767a7c6299bbd1e57a0406dd5edace4baf614e8960b2c2c4b6d2b`
已删除；原4业务容器ID/image/StartedAt/restarts/mount元数据前后相同。
只清理各轮明确自有容器；没有新卷/网络/镜像。磁盘上的任务输入与证据保留。

### Review、回归与保全

独立review及主代理关闭：遗漏shutdown依赖、成功marker核对、同一stdout块取消后
继续输出、已reaped后向旧数字PGID发信号、leader已退出但stderr未EOF使取消挂起。
取消同时唤醒两条pipe；只对仍活跃的工具禁用probe进程组发一次KILL，再等待退出。
这不是任意工具后代监督合同，R4仍未通过。最后两项均有回归，reviewer独立9项
进程测试/27断言PASS并确认关闭；最终原生五项在修复后重新全跑通过。

前三次原生失败保留记录：真实CLI输出`system/status`被严格解码器拒绝；仅补上该已
观察到的非内容事件，不放行未知事件或忽略错误。之后文本stdin、双向stream-json、
最终cleanup修复三个版本分别五项通过，不把较早版本当最终版本证据。
六轮均清理自有容器并通过业务基线对照。

最终`make -C recovery check`通过：236 recovery Python、150 Bun1.3.9测试
（1027断言）、158 image Python；14份合同/116条entry仍business_verification=false。
普通Bun测试的进程/HTTP使用内存fixtures；与上面的真实Linux五项分别记录。

六轮源码/助手/清单/运行及清理记录已保全至仓库外0700目录
`/Users/ruanyang/My-project/api/z/isthmus-cli-artifacts.NzTfeRIr`。
最终远端根`/var/tmp/isthmus-s1b.QyuYs1Aa`；其10项`cli-source.json` SHA256为
`28b58145f54740fb5cf1248fecdb18d64d85d921dde0ace991ed6d6106dd154a`。
传输后六轮各10源码逐项核大小/SHA；最终10源码与当前工作区完全一致。
二进制沿用S1c已保全的公开制品，不重复进入Git。

### 调用流程与线上仍不等价

线上参照为2026-09-14/15已保全材料，不是本轮重新观察生产。
[历史调用配置](../../../recovery/docs/online-cli-forwarder-config-2026-09-14.md)是
Portunex→WS/gRPC(S)→isthmus→持久CLI池/双向stream-json→上游，带会话/工具循环。
当前只是HTTP单请求→一次CLI进程→合成上游；两端stream-json已对齐，但会话池/
工具/MCP、实际gRPCS及独立证书、受限出口、5m/1h请求改写/透传合同、真实凭据刷新、
CCMAX/Sub2整链接线仍未验收。host-agent是本地桥接设计，不冒称线上原拓扑。
I3/R4/R5/N3–N5/K/H3–H5/C/L/E均不随R3关闭；不能启用生产新执行面。

## S1c 后续 review：ZIP64 解析前预算

2026-09-17，按用户要求先review、汇报26%进度，再修复和规划S1d。
新增1项P2：普通EOCD目录上限8KiB/最多2项检查后，Python ZipFile仍可读取
ZIP64覆盖值。200个合成条目的11,800字节目录被解析后才拒绝，违反预解析预算。
修复在构造ZipFile前拒绝其固定位置ZIP64 locator；允许的官方制品不需要ZIP64。
回归覆盖零长度和65,535字节comment，断言解析器零调用；普通ZIP最大comment
中仅出现签名的正例仍成功。作者和主代理复验binary19项通过，source16项通过。

另一独立review核对上传、timeout、cleanup及私有证据，未发现新P1/P2；
41项定向测试通过，18个代码/锁摘要与实际运行记录一致。修复前主代理复跑
镜像工程152项通过，但这些旧用例没有覆盖新缺口。未连接生产或使用真实账号。
缺陷属于未关闭的I3；已验收26%、I=40%不变，继续[S1d规划](s1d-immutable-candidate-image.md)。

## S1c：固定制品与原生Linux工具验证

2026-09-17；先规划提交`a318eda`，再开发和隔离验证。**部分交付，不关闭I3：
总进度仍26%，镜像6/15=40%**。本轮没有生产连接、账号凭据、真实模型请求、
宿主Bun替换、镜像发布或业务部署。170用于测试，43.153.75.220只转发SSH。

| 验证项 | 实际结果及限制 |
| --- | --- |
| 源码冻结 | 固定Git `e2715b6e7f968e638c2f4fd68467c56fa0151c72`，23普通文件/47,550字节；proto/package/相对imports，无第三方安装；明确fake-only |
| 二进制来源 | Bun1.4.2候选、1.3.9对照及CLI2.1.258各自锁amd64/arm64官方URL、size/SHA；Linuxamd64实际下载/展开复验。BunPGP未验，不冒称已验签 |
| CLI签名 | 官方manifest1778字节，SHA256 `ec53b2ce3d9f95d2c94c07aacf9202e6df9f79c695b71a1af2b4491081fba60f`；独立GPG目录，VALIDSIG指纹 `31DDDE24DDFAB679F42D7BD2BAA929FF1A7ECACE`；两架构签名清单hash/size均与锁相符 |
| CLI原生启动 | S1b精确image ID上，UID1000真实运行`--version`为`2.1.258 (Claude Code)`；不是QEMU，也不是假CLI。未运行真实CLI转发/工具调用/登录 |
| Bun正常合同 | 两版均110 PASS/0 FAIL/499 expect，11测试文件；本地全仓另有1项proto provenance测试，因其引用带绝对恢复路径的metadata未上传。proto的4433字节和固定哈希已在40文件检查内验证，不伪称Linux111项 |
| Bun关闭 | 1.4.2正常loopback与server1011异常关闭各5次PASS；1.3.9正常loopback通过、异常关闭返回精确单行FAIL及exit1。只复现指定gate失败，不从genericcatch推断唯一底层原因；项目/CI仍1.3.9，R5未关闭 |
| 运行限制 | 精确S1b image ID、UID1000、cap0、NNP、networknone、无宿主/业务挂载和端口；内核cgroup读回memory.max=2147483648、swap.max=0、cpu.max=100000/100000、pids128、core0 |
| 文件边界 | 根实际ro；home和/tmp独立rw/noexec；512MiB `/opt/isthmus-probe` 为root-owned rw/exec tmpfs。40个固定普通文件、UID1000整树不可写、内层哈希先检查再执行。它不是不可变组合镜像 |
| 上传root例外 | Docker29的cp只读rootfs检查也拒绝tmpfs，改用自制固定USTAR397,486,080字节通过stdin给容器内系统shell/tar。只有该系统准备步骤UID0，实测cap0/NNP/networknone/2GiB；新工具一律UID1000。不是外部tar/特权解包 |
| 清理与共存 | 最终probe命令exit0/7.36s；只删除本轮确切容器 `2226d3e7f61c931ef0ced0cc7c777255435df85fae0d1d9e807dd02e54122070`。原4业务容器ID/image/StartedAt/restarts/mount元数据前后相同；成功receipt在清理和基线比较后发布 |

公开锁在`image/locks/`：toolchain外层、amd64-expanded内层、app-fake-source、
probe-tests-source分别管理。实际ELF解释器均`/lib64/ld-linux-x86-64.so.2`；readelf
静态NEEDED为glibc家族（libc/loader/pthread/dl/m，CLI另librt），未用ldd执行原件、
未复制旧rootfs库。不意味着所有CLI子工具/git/curl/rg或真实执行能力已验收。

私有证据：最终重新下载/验签在170 `/var/tmp/isthmus-s1b.C4hJQZtN`；
最终运行证据在`/var/tmp/isthmus-s1b.6d4oItCu`。本机备份在仓库外
`/Users/ruanyang/My-project/api/z/isthmus-s1c-artifacts.UqaLLThy`，包含官方原始下载、
签名/public key、源锁、运行/清理记录；不含真实账号、密码或token，不入Git。
早期macOS UID501目录上传被权限校验拒绝，已修正任务文件所有权；两次Docker cp
方案失败均在新工具运行前，失败容器已清理。没有跳过安全检查使旧方案通过。

Review关闭项：成功记录提前发布、把任意exit1当对照、载荷自证未锚定公开锁、
复制累计上限、HTTPS慢读硬超时、chmod后字节变化导致旧摘要。均有合成负例；
源/二进制模块交叉review，上传失败不会执行工具。实际证据与离线PASS分开记账。

最终`make -C recovery check`通过：236项recovery Python、111项本地Bun测试、
152项image Python；14份合同/116条目录项仍`business_verification=false`。
二进制下载备份在本机用最终stager再次核外层+内层哈希通过；26个模块/锁/测试文件
内容扫描及`git diff --check`通过。三个试验run label均无残留容器，原4容器基线
一致，测试机磁盘余41,208,025,088字节。只保留私有公开制品/日志，没有新卷/镜像发布。

下一项：固定真实CLI必需工具闭包与不可变组合运行制品，随后I4双实例私有home
持久化/重建语义。ARM64本轮仅元数据边界；真实CLI转发、身份/独立密钥证书、
出口拒绝矩阵和控制面桥接均未因此验收。不上线、不借用真实凭据。

日期：2026-09-16。A 总规划提交 `c8380da` 先于实现 `fe9c3b8`；B1 细化规划 `e07bb7d` 先于实现 `c3dae96`；B2a 规划 `f32aa47` 先于实现 `d6a12a6`；B2b1 规划 `02b0f88` 先于本切片实现。B 总项（B2–B4）、C–G 与整链/生产门槛仍开放，细分切片结果如下。

计划复验：

- `cd execution-plane && GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local go test -race -count=1 -timeout=120s ./...`
- 同样离线依赖设置下执行 `go vet ./...`。
- 针对实际 worker upstream 的重复流式/取消/背压测试。
- 检查没有默认监听、没有改线上 UI/配置/数据或启用迁移标志。
- 独立 review 后修复，主代理复跑相关测试；Git 只含源码、合成测试及脱敏文档。

真实模型、生产数据库、SSH、canary、部署均不在本轮验证路径。

## A 实际结果

| 验收点 | 结果及边界 |
| --- | --- |
| 模块拆分 | `worker/upstream/` 管理 HTTP 响应资源/有界读取；`worker/upstreamusage/` 只观察计数；`upstream_adapter.go` 持有原凭据注入与 RPC 适配；从 `process.go` 移除整包执行逻辑 |
| 实际执行器首段 | 新 `TestHTTPWorkerRPCFirstChunkBeforeEndAndLargeStream` 使用实际 HTTP 服务、worker Executor 和带身份票据的 gRPC；上游被通道阻塞不能结束时，客户端已收到 headers/首段 |
| 大流字节保真 | 同一实际 HTTP→worker gRPC 测试累计超过 3 MiB，按 SHA-256 对比；每块 ≤32 KiB；不受旧 2 MiB 累计截断。独立 relay 模块另测 6 MiB+17 bytes |
| 背压 | 同步慢 Sink 测试断言下游返回前不继续 Read。不是完整 gateway 慢客户端或1000连接压测 |
| 取消/半关闭 | 实际 gRPC 客户端取消后，HTTP handler 收到 Context 取消；请求半关闭仍可完整收到 SSE。模块另验证阻塞 Read/Send、deadline、close once |
| usage | JSON及SSE开始/增量提取固定整数白名单，累计取最大不相加，保留缓存5m/1h和thinking分桶；缺失不补0；原始正文不改写 |
| SSE边界 | 覆盖任意字节切块、UTF-8、CR/LF、BOM、多data行、注释/未来事件；error、缺stop、非法JSON/重复键、异常状态、事件超限不产生可信Completed usage |
| HTTP错误 | 400/401/429/500状态及有界正文保留，无synthetic usage；此时Completed仅表示HTTP response-end。实际截断HTTP无Completed；不重试、不跟随302/307/308 |
| 大小/编码 | 请求/非流/错误体保留各自2MiB限制，不增加凭据上限；Go transport解码gzip可用，剩余未解码编码拒绝；非空畸形Content-Type输出前拒绝 |
| 凭据与入口 | 现有ActiveCredential注入和Destroy保留，入站Authorization/Cookie不透传；没有改gateway、UI、生产开关或账号迁移状态 |
| 全模块 | 主代理两轮 `GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local go test -race -count=1 -timeout=120s ./...` 及 `go vet ./...` 通过 |
| 重复回归 | 主代理最终10轮 `go test -race -count=10 -timeout=90s ./internal/worker -run 'TestHTTPWorker(RPC|Malformed|Incomplete|Actual)'` 通过 |
| Docker fixture | `go test -tags docker_e2e -run '^$' ./internal/hostagent` 仅编译通过，未运行Docker E2E，未启动VM |

新增 43 个顶层 Test 与 2 个 fuzz target（含子用例）。usage模块作者执行 SSE fuzz 10秒/177,959次和 JSON fuzz 5秒/49,627次均通过；主代理复跑其race与完整模块回归。这些是合成解析输入，不是真实模型token计算。

`EXECUTION_MYSQL_TEST_DSN`、`EXECUTION_REDIS_TEST_URL`、`CCMAX_EXECUTION_MYSQL_TEST_DSN` 均未设置，因此专用真实数据库/Redis集成没有执行，不能记作PASS。全部实际HTTP监听均为测试自建回环端口并由测试收尾；没有默认应用listener、真实凭据或原始线上正文。

## Review 与修复

两位代理交叉审查非本人模块，主代理审查及集成，发现并关闭2项P2：

1. Relay复用的读取buffer被RPC protobuf引用：适配器改为复制有界单块，并用不Clone、保留原proto引用的测试覆盖读取返回后的覆写。
2. adapter忽略MIME参数解析错误、Relay却要求解析成功，造成usage分支不一致：改为共享 `IsStreaming`，畸形非空Content-Type在读取/输出前失败；覆盖两个读取入口与实际worker无headers/body/completion输出。

Reviewer已复核关闭上述两项，未发现其他阻碍A提交的问题。另一位reviewer独立执行取消/deadline/背压/编码/失败清理定向10轮race通过。

## 本阶段没有宣称完成的内容

- host-agent生产装配、可信投影/执行签票、gateway接线、CLI/MCP、刷新/生命周期仍未实现本地全链闭环。
- 现有RPC只在Completed上有usage；中途失败的部分usage没有额外结算通道。没有通过伪成功传递它；错误后部分用量处理保留为后续C阶段协议门槛。
- usage观察的SSE行/事件1MiB、JSON深度64/节点数262144与非流2MiB都是明确的本地安全边界，不声称线上大小/未知变换完全兼容。
- 旧原件/官方ARM64 CLI→stub不曾接入本轮生产应用；真实模型请求仍0。当前修改未部署，线上未动。
- 原 `add-claude-execution-plane-v1` Phase6–11总项和PRD§31保持未通过。

## B1 实际结果

设计及停止线见 [B1设计](b1-design.md)，模块边界见 [executionauthority](../../../execution-plane/internal/executionauthority/README.md)。没有新增RPC或默认监听，也没有向host-agent分发数据库或签私钥。

| 验收点 | 证据与限制 |
| --- | --- |
| 会话来源 | NodeControl把服务端当前session.id写入CommandResult；没有在节点报文中新增可伪造字段。SQL与Memory均在写入前核当前connected会话 |
| 持久证明 | 迁移013只新增nullable observed_control_session_id；不回填历史值。旧无会话Observe/Apply、失败/不健康结果会清除证明；schema gate要求新列 |
| 当前镜像 | 健康成功结果需匹配pending image/deadline，store在同事务锁定active assignment并精确比较ExpectedImageDigest；无provisioning job也不能跳过。SQL/Memory错误不部分落命令、任务或证明 |
| 一致性读 | SQL单条只读JOIN及精确身份/会话/镜像比较；Memory同一RLock，拒绝不一致/缺失/重复active状态。纯值输出无可变仓库引用，不读取credential或route cache |
| 真实控制RPC组合 | `TestAuthenticatedCommandObservationSnapshotRequiresNewSessionConfirmation` 使用本地实际TLS1.3 NodeControl、注册证书、命令派发/结果、Memory仓库与Source。首次观察前拒绝、当前结果允许、断连拒绝、仅Hello/heartbeat重连拒绝、新受控结果恢复、旧连接证书撤销拒绝 |
| 负例层次 | 同一fixture的旧会话迟到结果及pending/result B但assignment A两个负例，直接调用recordCommandResult进行控制层→存储层组合，不是实际stream.Send负例；SQL锁/回滚另由sqlmock覆盖 |
| 只读活动会话 | 未accepted、弱/未验证TLS、证书失效/撤销、durable session不同、上下文取消和读取期间detach/替换均拒绝；验证存储时不持session-map锁 |
| 观察期限 | Source在读取/活动会话验证后再次检查观测/节点时间及durable lease有效期；不把查询或heartbeat当新观察。Control在observer前冻结ReceivedAt/ObservedAt，回调后复核deadline；observer/storage上下文受命令期限约束 |
| Resolver组合 | 实际FencedResolver+Source+内存租约验证跨账号/节点/旧epoch/generation与Redis替身故障拒绝；lookup期间重连，包含新会话已重新确认同实例的情况，由ControlSessionID前后比较拒绝 |
| 最终模块回归 | 主代理最新完整 `GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local go test -race -count=1 -timeout=120s ./...` 与 `go vet ./...` 通过 |
| 重复回归 | 主代理对control/executionauthority/store中的会话、观察、Source、binding、事务/取消关键用例执行 `go test -race -count=10 -timeout=90s ... -run 'TestCurrentSession\|TestAuthenticatedCommandObservation\|TestHealthyObservation\|TestSource\|TestReadExecutionBinding\|TestMemoryCommand\|TestMemorySession\|TestSQLCommand\|TestCommandResultRejects'` 通过。新增镜像专门测试在最终全模块运行，另一reviewer独立三轮镜像/取消/binding race通过 |
| Docker fixture | 最新 `go test -tags docker_e2e -run '^$' ./internal/hostagent` 编译通过；没有运行Docker、启VM或使用真实账户 |

两位代理交叉审查非本人模块，主代理复核并集成；已关闭：lookup前后漏比session、observer延迟放大期限/新鲜度、Memory等任务锁期间取消后仍写入，以及pending/result镜像未关联durable assignment的缺口。相关拒绝回归均保留；review范围内无剩余已知P1/P2阻碍本切片提交。

### B1 不能代替的门槛

- 没有实际MySQL迁移/并发事务/死锁或索引性能证据；013脚本只进入Git，不自动应用。新结果事务node→assignment锁顺序与现有release顺序可能形成死锁，需隔离数据库验证失败/重试行为，当前不可据此批准生产。
- B1 本身没有主动fresh Inspect/Health调度；下述 B2a 新增 opt-in INSPECT 库，但未接生产入口，worker Health/版本证明仍缺。超过45秒后拒绝是预期安全行为，不得拿缓存心跳维持Ready。
- Source只在持有对应活动控制流的orchestrator内有效；其受认证RPC、跨控制面实例访问、签票、runtime registry和进程接线仍在B2–B4。
- `Snapshot.Ready`仅代表分配/会话/租约候选，不是worker模式、active credential/proxy版本或业务授权已经核完。
- Lookup会话比较不是已经打开的stream的原始session绑定；快速断连再确认可能发生在周期检查之间。持续流原会话撤销、连接回收仍待B3/B4，不能宣称即时中断已验收。
- 无真实模型调用（累计仍0）、无SSH/生产数据库/配置/UI/进程/路由操作，未push、未部署、未启迁移开关。整链Docker、CLI/gateway、刷新、1000连接与24小时稳定性、真实canary均不能由本阶段PASS替代。

## B2a 实际结果

设计：[B2a](b2a-design.md)；模块：[runtimeprobe](../../../execution-plane/internal/runtimeprobe/README.md)。只增加本地库及测试，没有启用生产 runner、新增服务 listener 或修改 wire proto。

| 验收点 | 证据与限制 |
| --- | --- |
| 探测与业务分权 | 新 ProbeBinding 不读凭据/route cache，不要求已有健康 proof，允许重连后的首次探测；ReadExecutionBinding 仍拒绝这些未证明状态。核对 current slot/assignment/image/generation/node/session/durable lease |
| 有界分页 | SQL 单一致性 JOIN + 二进制 keyset + LIMIT ≤100；格式过滤掉整页时也返回扫描游标。Memory 单锁、最多100行页缓冲；读/分页观察时间深拷贝，取消/错误无部分返回。SQL 是 sqlmock 合同，不是真实 MySQL 验收 |
| 探测授权 | Runner 双读取精确绑定、两次独立 lease 验证、I/O 后复核时间；不续租、不重新 Grant。每次授权默认2秒，命令≤10秒且不超过 durable lease 期限 |
| 固定会话 | DispatchToSession 仅 INSPECT，验证活动 TLS1.3/证书/current durable session 后仍固定原 session 指针；认证 I/O 中替换、断连、取消或过期时拒绝 |
| 容量与失效 | 每 slot/epoch/session 单飞，probe 使用 pending/queue 至多一半并保留普通队列位置；非阻塞入队；过期/被同 slot 控制命令取代的 probe 取消并回收，出队跳过失效项，不误清 credential commit。队列标记只存到出队，无无限 tombstone |
| 真实 TLS 组合 | `TestRuntimeProbeTLSInspectRefreshReconnectAndUnhealthy`：真实 TLS NodeControl + Runner + Memory 仓库/lease + 实际 hostagent SlotCommandExecutor + 仅实现 InspectSlot 的合成 provider + B1 Source。初始无 proof 可探测；心跳/重连不能恢复；新的实际 Inspect 才恢复；provider 不健康覆盖仍健康的宿主缓存并撤销 proof |
| 负例层次 | 控制侧容量、命令碰撞、迟到健康/失败结果与 Apply 期间 mutation/detach/stream cancel 是直接控制方法/合成 repository 检查；不是实际 MySQL 锁或所有实际 wire 故障已验收 |
| 调度与错误 | 一页一步、无常驻 slot map、并发 Step 拒绝；坏节点不阻塞后续页，连续扫描失败有限重试，取消退出；错误不透传依赖原文。未证明大规模45秒覆盖或长时间稳定性 |

Review 由两位代理交叉审查非本人模块，主代理集成/复验。发现一个 P2：旧 probe command ID 可被普通命令重新分类；仅提前检查当前 pending 不足以覆盖已 reap 的历史 ID。修复方案为专用 `probe-` + 32小写hex命名空间，普通 Dispatch 禁止任何 `probe-` 前缀，并保留活动 ID 碰撞拒绝。内部可信 Runner 每轮随机新 ID；节点没有命令派发权限，不通过无界历史表防重复。

独立 reviewer 已复核关闭上述 P2，并独立通过 namespace/已回收 ID/格式拒绝/真实 TLS/32并发单飞五轮 race；另一 reviewer 对 store/runner 未发现新增阻碍。主代理最终复验：

- 完整 execution-plane：`go test -race -count=1 -timeout=120s ./...` 和 `go vet ./...` 通过。
- 定向重复：`go test -race -count=10 -timeout=120s ./internal/runtimeprobe ./internal/runtime/store ./internal/control -run 'Test(Probe|RuntimeProbe|Ordinary|ExpiredOrInvalidated|Reaped)'` 通过，包含最终 namespace 修复。
- `go test -tags docker_e2e -run '^$' ./internal/hostagent` 仅编译通过，没有运行 Docker 测试。
- 上述命令使用 `GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local`，并通过 `env -u` 显式移除 `EXECUTION_MYSQL_TEST_DSN`、`EXECUTION_REDIS_TEST_URL`、`EXECUTION_CCMAX_MYSQL_TEST_DSN`，不让环境中的外部数据库地址进入测试路径。真实依赖集成属于未执行项，而非 PASS。
- `git diff --check` 与 recoverykit 文本/evidence policy 扫描通过；仅源码、合成测试与脱敏文档提交，无凭据或制品。

### B2a 不能代替的门槛

- 没有 worker 实际加载 credential version/proxy lease/mode 的证明；provider 健康不等于模型 ready。没有签 health/activation/messages 票，没有延长 durable/Redis lease。
- 没有生产接线、跨 orchestrator 路由、B3 existing-only registry 或已打开流的原会话撤销。
- 周期结果仍写 `node_command_results`，生产启用前须补保留/清理或紧凑存储策略。页缓冲/队列有界不等于结果表增长、SQL 索引开销和覆盖周期已验收。
- 未运行真实 MySQL/Redis/Docker/VM、上游模型、1000连接/24h验收；B1真实事务死锁/提交取消竞态门槛仍有效。
- 当前修改仅本地；SSH/线上数据/UI/配置/服务/路由/账号均未触碰，真实模型请求仍0，未 push 或部署，未开启 execution_onboarding 或标记 migrated。

## B2b1 实际结果

设计：[B2b1](b2b1-design.md)；模块：[workerproof](../../../execution-plane/internal/workerproof/README.md)。只新增本地库/合同、兼容协议字段和测试，不启用生产入口。

| 验收点 | 证据与限制 |
| --- | --- |
| 原子加载状态 | 同一 RLock 返回 modes 与完整 identity/version ID/auth type/proxy lease/local revision；不通过 ActiveCredential 复制秘密。成功 ack 后同锁切换全部字段并递增 revision；失败、取消或空凭据不能产生新 loaded proof |
| 并发与 Drain | 32 轮并发观察不拼出新旧混合元数据；commit 失败/取消保留此前版本、同 lease 可安全重试。Drain 先隐藏并清空 active，再等待 pending 清理；阻塞 commit 返回后不能复活，revision 不溢出 |
| Health 兼容 | 仅新增 challenge 和可选 loaded_state，无 RPC 方法变化。空 challenge 保留旧调用；fake/legacy/未激活/Drain 不产生 loaded_state。非法 challenge、跨完整 identity、ctx 取消拒绝；序列化响应无 credential bytes |
| 持久投影 | 单条只读 SQL / Memory 单锁，沿 account active pointer 取版本，沿当前 slot/epoch 取未撤销 proxy/reservation；保留 B1 current session/image/generation/新鲜观察/live lease 条件。不读 envelope、hint、KMS 或代理地址/密码；SQL 仅 sqlmock 合同 |
| 主动核对 | 每次新随机 challenge；精确比对身份/镜像/version/auth/proxy/revision/mode，前后两次元数据和活动会话/独立 lease 核验。原 authority 截止时间限制 RPC 及后续 I/O；旧回包、重复模式、缺字段、版本/代理/会话漂移、失效及超时统一拒绝 |
| 本地 RPC/Vault 组合 | `TestWorkerLoadedProofRealRPCVaultAckRotationAndDrain` 真实经过 SecureActivate、worker 加密凭据返回、rotation recipient 解密、Vault.Rotate/Fake KMS/Memory active pointer 更新、同流 ack、Health RPC、Verifier 双读。ack 前拒绝；ack 后通过；版本轮换未加载、缓存 challenge 重放、核对中再轮换与 Drain 均拒绝；proxy 撤销后投影不可用 |
| 版本语义 | 同一组合中控制面 version number=3，而 worker 成功 activation revision=2，仍按准确 version ID 对齐；不把两种计数混为一谈 |
| 结果用途 | Receipt 的 CheckedAt 在 Health 前冻结、revision 回包后立即复制，没有 Ready/有效期/续租接口。它是一次对照记录，不是业务授权或上游可用证明 |

两位代理交叉 review 非本人模块，主代理集成复查；worker 原子发布/Drain/取消、store active pointer/proxy 投影、Verifier 期限/双读和实际组合证据均已审查，没有剩余已知可复现 P1/P2 阻碍本切片提交。review 时明确并保留：取消 ack 不发布、RPC 快照读取后的取消检查、旧响应可变引用不进入 receipt，以及 opaque version ID 与本地 revision 的语义边界。

主代理最终验证（工作目录 `execution-plane`）：

- 全模块 `go test -race -count=1 -timeout=120s ./...` 及 `go vet ./...` 通过。
- `go test -race -count=10 -timeout=120s ./internal/workerproof ./internal/worker ./internal/runtime/store ./internal/control -run 'Test(LoadedProof|WorkerLoadedProof|Health|DrainHides|ActivationCancelled|InvalidOnboarding|ReadWorkerReadinessBinding)'` 通过，四个包均实际执行用例。独立 reviewer 另行 worker/control 定向 race 三轮通过。
- 上述 Go 命令均使用 `GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local`，并用 `env -u` 屏蔽 `EXECUTION_MYSQL_TEST_DSN`、`EXECUTION_REDIS_TEST_URL`、`EXECUTION_CCMAX_MYSQL_TEST_DSN`；真实数据库/Redis集成未执行，不记作 PASS。
- `sh scripts/worker-proto-offline.sh check` 连续两次通过且零 diff，只用本地缓存 buf v1.72.0 / protoc-gen-go v1.36.11；`worker_grpc.pb.go` 与 CCMAX 生成文件无改动。仅新增 worker 消息生成/检查脚本，不运行会访问远端插件的整库生成目标。
- `go test -tags docker_e2e -run '^$' ./internal/hostagent` 仅编译通过，没有运行 Docker E2E 或启动 VM。
- `git diff --check` 与 recoverykit 文本/evidence policy 对本切片 20 个文件（包括被全局 scripts 规则忽略、定向纳入的离线生成脚本）扫描通过；只提交源码、合成测试和脱敏文档。

### B2b1 不能代替的门槛

- worker RPC 是 bufconn + 临时测试 Ed25519 票；Control 是实际本地 TLS，但初始 provider observation 在本测试中由合成结果建立，非本切片真实 Inspect。B2a 另有真实 executor/合成 provider 检查，不能把两份证据拼成生产整链。
- Vault 为实际加密/版本切换库，KMS 和存储为 Fake KMS/Memory；未经过完整生产 rotation handler、host-agent 转发及一次性 credential lease 授权，也没有真实凭据或模型调用。
- HealthReader 的生产受认证分 scope 签票、私网 existing-only registry 装配仍待 B2b2/B3；没有给宿主签私钥、延长任何 durable/Redis lease 或启用新的业务入口。challenge 不是恶意宿主不可伪造的硬件证明。
- Drain 立即撤销可见 loaded state，不表示强制取消所有在途 Onboard/Commit/执行流；不遵从 context 的依赖仍可能延迟 Drain 返回，持续流撤销属于后续 B3/B4。
- 真实 MySQL 单语句/时钟偏差/事务死锁与索引性能、Redis、代理连通性、Docker/VM、CLI/gateway、1000连接/24h与 canary 未验。B 总项、B2b2–G 与 PRD §31 仍开放。
- 无 SSH/生产数据/UI/配置/服务/路由/账号操作；真实模型调用仍为 0。未 push、未部署，未开启 execution_onboarding 或标记 migrated。

## B2b1 提交后复审（用户要求先 review 再继续）

基线 `b3a548a`。重新由两位代理独立审查，主代理复跑全模块 race，发现两项取消边界；不是将已声明的生产接线缺项重新记成代码 bug。

1. **P2：核对完成前漏检取消。** 最后一次活动会话检查之后仍调用 `Now`，该回调期间发生取消时，原实现会返回成功 Receipt。新增 `TestLoadedProofCancellationDuringFinalClockReadIsDenied` 在最后一次时间读取中同步取消父 context，不用 sleep 或不服从 context 的外部依赖；旧实现确定性失败。返回前增加最终 context 检查。
2. **P2：等待激活串行锁无法及时取消。** 已有激活卡在 commit 时，第二个激活通过前置检查后等待 `sync.Mutex.Lock`；取消第二个请求不能使它返回，直到第一个 commit 结束。这是此前即存在、B2b1 仅验证“取消不发布”而未验证“取消等待及时结束”的缺口。现改为零值可用的 context-aware channel gate，无等待 goroutine；选中获取后复查取消并归还 token。提交回归以包装 context.Done 信号确定已经进入等待，不依赖 sleep、runtime.Stack 或调度符号；第二个请求可在第一个 commit 释放前退出，且不运行 onboarding、不改变第一次发布。Drain 等待在途清理的既定边界不变。

两项均先复现旧实现失败，再修复；两位 reviewer 已交叉复核。主代理修复后完整 `go test -race -count=1 -timeout=120s ./...`、`go vet ./...` 和 worker/workerproof 的 `Test(ActivationWaiter|ActivationGate|ActivationCancelled|DrainHides|HealthSnapshot|LoadedProof)` 定向 race 10 轮通过。命令沿用上述离线依赖/屏蔽外部 DSN 设置，无真实依赖或线上操作。原“review 无发现”仅代表当时范围，本次新增发现与修复按事实补记，不宣称 review 能证明无缺陷。

## B2b2a 实际结果

先提交上述 review 修复 `18d5f52`，再提交规划 `46b2d3f`，随后实现本切片。设计：[B2b2a](b2b2a-design.md)；模块边界：[只读诊断签票](../../../execution-plane/internal/control/probe_tickets.md)。生产 bootstrap、默认配置及监听均未改变。

| 验收点 | 证据与限制 |
| --- | --- |
| 协议兼容与代数来源 | 既有 TLS Control 流新增诊断票请求/响应 oneof，无新 gRPC 方法；CredentialKeyCommand 新增 desired_generation，由已有 SecureOnboardingPlan 单一来源填入。缺代数的旧命令不能换新票，原执行路径保留 |
| 显式开启与最小权限 | 控制面必须注入完整 ProbeTicketConfig，宿主必须同时设置 EnableProbeTickets 和 probe_tickets capability；仅 INSPECT→health、CredentialKey→credential_key。未配置、错 scope、无 command context、仅排队或无 pending 均拒绝；启用的 command context 不回退旧签票源 |
| 权威与会话固定 | 请求不提供授权身份/地址；控制面从当前已认证会话及已交给 stream.Send 的原命令推导身份，前后各读一次 ProbeBinding、核独立 lease/活动会话；account/slot/node/epoch/generation/image/providerRef/owner/session 必须一致。发送交接点不是节点收讫证明 |
| 一次尝试与关联隔离 | pending 实例固定且只有一次尝试；失败/丢票需要新命令。每次宿主请求新随机 request_id，仅作关联；与 command_id/scope 一起核对，覆盖相同命令 ID 重用时的旧回包 ABA，不建立无限历史表 |
| 期限与取消 | 默认 TTL 5秒、最大10秒，授权检查默认2秒、最大5秒；向下截到原命令、证书、前后 durable lease/节点新鲜度期限。后读的延长期限不能放大第一次上限。取消、元数据/lease 超时、会话替换、完成/过期、最终时钟回调取消均有拒绝回归 |
| 队列与秘密边界 | 宿主每会话请求队列/等待项各最多64，等待不超过10秒/命令期限；复用单一 stream.Send 循环，取消/断连回收，合法迟到关联 ID 丢弃；控制面出队复核命令实例/期限。签私钥仅在控制面，票不写日志/DB；非诊断 pending 不保留大密文 |
| 实际本地组合 | TestProbeTicketTLSControlClientToActualWorker 经过实际 TLS ControlServer/ControlClient、控制面真实 Ed25519 签票、实际 worker Guard 及 Health/公钥 RPC；使用 Memory 权威/lease、临时测试密钥及 bufconn worker。默认关闭分支拒绝，显式开启分支成功 |
| 权限负例 | 同一组合拒绝跨账号、scope 升级、重复换票、nonce 重放、独立 lease 不可用；用诊断票调用实际 CountTokens 在执行器之前 PermissionDenied。worker 未激活，Health 不伪造 loaded_state，模型执行计数始终0 |

两位代理交叉 review 非本人模块，主代理复核集成。实现中复审发现原 outbound 指针如果无差别保留，会让 secure activation pending 在任务结束前一直持有最大2MiB密文，并扩大普通命令的出队语义变更；已缩小到 INSPECT/公钥两种诊断命令，其他命令不保留响应指针，增加 TestProbeTicketsDoNotRetainNonDiagnosticControlPayloads。request_id 关联同时明确防止宿主等待表的命令 ID 重用误投递。最终交叉 review 无剩余已知 P1/P2 阻碍本切片提交，未将此结论扩大为生产整链无缺陷。

### B2b2a 验证与一次既有测试失败

主代理最终验证工作目录为 `execution-plane`。所有 Go 测试/vet 均使用 `GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local`，并以 `env -u` 屏蔽 `EXECUTION_MYSQL_TEST_DSN`、`EXECUTION_REDIS_TEST_URL`、`EXECUTION_CCMAX_MYSQL_TEST_DSN`：

- 最终完整 `go test -race -count=1 -timeout=120s ./...` 与 `go vet ./...` 通过；此前主代理多轮完整 race 也通过。
- `go test -race -count=10 -timeout=120s ./internal/control ./internal/hostagent ./internal/service -run 'Test(ProbeTicket|RuntimeProbeSource|DiagnosticKeyCommand|ControlClient.*Probe)'` 通过。控制面作者对最后新增证书、第一次 lease/node 期限上限及实际 TLS 等用例另执行 race 5轮通过；两位 reviewer 独立相关 race/vet 通过。
- `sh scripts/control-proto-offline.sh check` 连续两次零 diff，`sh scripts/worker-proto-offline.sh check` 通过；均仅使用缓存的固定版本工具。生成修改仅 control.pb.go；control_grpc.pb.go、worker 及 CCMAX 生成文件未改。
- `git diff --check` 与 recoverykit 文本/evidence policy 对本切片22个文件扫描通过，包含精确纳入的离线生成脚本；提交仅含源码、合成测试与脱敏文档，不含票据、凭据或构建制品。
- `go test -tags docker_e2e -run '^$' ./internal/hostagent` 仅编译通过，未运行 Docker E2E、未启动容器/VM。真实 MySQL/Redis 集成未执行，不记为 PASS。
- reviewer 的一次全 hostagent race 中，既有 TestDataPlaneWorkerLoopbackDoesNotAggregateResponse 在13块后以 `FailedPrecondition: execution binding is no longer active` 失败；该 fixture 将 FenceInterval 设为10ms，watchFence 同时把它用作单次 Validate 硬超时，存在调度预算风险。相关 fixture/stream 文件本轮未修改，也未使用 ControlClient/新诊断来源。随后 reviewer 单独 race 10轮、主代理单独 race 20轮及最终全模块复跑通过。**未在旧提交独立复现，所以不宣称已证明是基线偶发问题**；未放宽超时、未隐去失败，数据面稳定性保留为 B4/F 门槛。

### B2b2a 不能代替的门槛

- 独立 lease.Validate 不返回 Redis 原子剩余 PTTL；已发诊断票在 lease 撤销后可能直到配置 TTL 才失效。本切片没有即时撤销保证，也不产生 activation/业务/续租权限。
- ProbeBinding 需要已有 providerRef；本地组合的 diagnostic executor 是显式合成实现，默认 INSPECT 仍只检查 provider，未完成首次实例 onboarding、生产 worker 诊断装配或 B3 existing-only registry。
- worker RPC 为 bufconn，权威/lease 为 Memory，不是实际数据库/Redis/私网 runtime 通道、真实 Docker/VM、凭据/代理或模型可用性证据；没有把 B2b1、B2a 和本切片的局部测试拼成生产整链通过。
- B2b2b activation payload/lease 授权、业务票版本/代理/模式约束及使用时复核、B2b2c 双层续租/持续流撤销、B3–G 和 PRD §31 仍开放；本切片只勾选 B2b2a。
- 当前阶段无 SSH/线上数据/UI/配置/服务/路由/账号操作，真实模型调用仍为0；没有 push、部署、开启 execution_onboarding 或标记 migrated。

## VM0a：实例接纳与固定 TLS 出口前置修补

先提交计划 `3ffc759`，再按用户明确的“每 VM 独立机器标识/密钥/证书”收紧范围。设计：[VM 优先门槛](vm-isolation-design.md)；模块：[Docker sandbox](../../../execution-plane/internal/provider/docker/README.md)、[固定 transport](../../../execution-plane/internal/worker/fixedtransport/README.md)；[线上静态身份对照](../../../recovery/docs/vm-identity-tls-baseline-2026-09-16.md)。仅 VM0a 源码门槛，不是每实例身份签发已完成。

### 已修复与证据层次

| 项目 | 本轮实现及实际验证 |
| --- | --- |
| 账号与请求规格一致 | 旧实现会复用同 slot/epoch、不同 account_hash 的实例；现校验 account、image、proxy、UID、资源和指定 seccomp/AppArmor/tmpfs。复用差异只拒绝，不重建或清理线上对象 |
| 共用接纳门禁 | Create 的旧实例复用、Inspect、InspectSlot、Start、RuntimeEndpoint 共用只读 gate；核对实际 Engine image ID/不可变引用、唯一专属 internal bridge/成员/IP/gateway、namespace、能力、挂载、NNP、profile 与 bootstrap 元数据。Start 使用已检查的 ID，停止态不广告 endpoint |
| profile 配置防降级 | Config 不再接受空白、畸形、控制字符或 unconfined（包括大小写变体）；保留 builtin/custom 支持，复制允许列表防调用方构造后修改。不能以 allowlist 名义关闭隔离保护 |
| Docker 合同证据 | 显式 typed JSON fixture 包含旧 decoder 丢弃的危险字段；每个漂移负例先验证同一正例，覆盖五入口拒绝且无变更调用；区分 registry manifest digest 与本地 image config ID，支持 tmpfs 投影/不投影及未首次启动网络形态。这些是合成 Engine 响应，不是真实 Docker 验收 |
| 启动前配置 | 新 EXECUTION_EGRESS_PROXY_URL 由 SlotSpec 传入；缺失、不规范或含凭据即失败，不先监听。生产执行/上号地址必须 HTTPS，只有显式 fake activation 可用 HTTP。原 HTTP/gRPC 消息合同未改，但旧手动启动配置必须补此内部字段 |
| 固定路径 | worker 执行与上号共用显式 transport；HTTP_PROXY/HTTPS_PROXY/ALL_PROXY/NO_PROXY/localhost 不参与选路，固定代理失败不直接拨目标。共享 provider/worker URL 校验合同，保留两处既有 no-redirect 和进程退出连接池关闭 |
| 实际 TLS 测试 | TestFixedProxyActualTLSProtectsOriginAndRejectsCertificateFaults 以实际 Go transport 经本地 CONNECT 到临时 TLS server；正常 TLS1.3/SNI 成功，未知 CA、错 SAN、过期和 TLS1.1 均在 HTTP handler 前拒绝。合成认证只在 TLS 内到达目标、不出现在 CONNECT；固定拨号适配器禁止任何其他地址，无目标 DNS/公网请求 |
| 材料边界 | 临时测试 CA/私钥仅内存生成；线上只读取已保全且经过内容扫描的选定脚本文本，未读真实证书/私钥/账号。独立 home 证书路径不等于独立密钥来源，允许同一发布证书目录的脚本不能证明线上每实例唯一；没有复制真实材料或生成任意 ClientHello 伪装 |

### Review 与回归

- 账号错误复用、缺/多/host 网络和 NNP=false 先取得旧 FAIL；worker 的缺显式 proxy、生产明文 origin、明文 loopback onboarding 三项也先取得旧 FAIL。
- 独立交叉 review 发现“允许列表内但不符合本次 spec 的 profile/tmpfs 仍能复用”，已补精确比较和八维度 TestSandboxReuseMatchesRequestedSecurityAndResources；reviewer 单独 race 三轮复核关闭。
- 主代理最终检查新增 Config 允许 unconfined 的问题；先取得14个旧 FAIL，再补构造期拒绝、20个负例、builtin/custom 正例与 slice 别名回归。另一 reviewer 独立只读复核并对三个配置测试执行离线 race 三轮，通过并关闭该项。该项不能由前述 inspect 负例代替。
- Provider 作者只读复核 worker 实际 process 接线、原 no-redirect 和主代理的 CONNECT/TLS/跨模块 URL 合同测试；worker 作者交叉复核 provider。模块文件独立维护，没有将安全检查堆进业务处理文件。

### 主代理最终验证

工作目录 `execution-plane`。下面全部 Go 命令均使用 `GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local`，并以 `env -u` 显式屏蔽 `EXECUTION_MYSQL_TEST_DSN`、`EXECUTION_REDIS_TEST_URL`、`EXECUTION_CCMAX_MYSQL_TEST_DSN`，没有安装依赖或连接外部数据库：

- 最终完整 `go test -race -count=1 -timeout=120s ./...` 与 `go vet ./...` 通过；包括最后的 Config 防降级修复。
- `go test -race -count=10 -timeout=120s ./internal/provider/docker ./internal/worker/fixedtransport ./internal/worker` 三个包全部用例通过，不仅选择新测试；此前同包定向十轮也通过。
- `go test -tags docker_e2e -run '^$' ./internal/hostagent` 仅编译通过，未运行 Docker 测试或访问 Engine socket。
- `git diff --check`、暂存区 `git diff --cached --check` 及 recoverykit 文本/evidence policy 对本切片22个文件扫描通过；提交只含源码、合成测试和脱敏文档，无私钥、证书制品或真实凭据。
- web-reverse-master 的本地脚本 selftest 为7/7通过，仅用于核查所用证据工具自身；不是 VM/TLS/生产验收证据。该技能的证据分层使本轮没有把独立存储目录或既有局部 PASS 视为身份唯一/整链通过。

### 当前不能放行的内容

- **VM0b–VM0d 未完成。** 未启动容器或 VM、未运行 make docker-e2e；现有 Colima profile 均停机。旧脚本会沿用 context、代理配置与共享目录，尚不能直接作为本次安全 Linux 验收入口。真实 kernel/Engine 行为、host-port/DNS/IPv6/metadata/跨槽 ACL、规则丢失/daemon 重启与运行中漂移均待验证。
- 每 VM 稳定机器标识、服务私钥/CSR/证书签发、保护存放/恢复/轮换、旧执行代与跨槽拒绝及 worker RPC 生产 mTLS 仍缺。进程 X25519 接收密钥和本轮目标 HTTPS 都不能替代服务端身份及双向认证。先维持无真实凭据的链路，再交付身份材料。
- Docker/runc 是共享受信内核；读取实际配置只是一个时点的接纳检查，不防恶意 host/daemon，也不是网络防火墙。库中固定 transport 不能拦截任意程序使用其他 socket API。
- host egress 在途 tunnel 的现有 fence 只观察 execution lease，proxy binding unregister/ProxyLeaseID 撤销的即时回收、CONNECT 握手取消与 half-close 清理仍待补；远程 HTTP/SOCKS 代理认证的传输保护未验。不得扩大为“没有泄漏风险”。
- B2b2b/C 等业务接线继续暂停；没有当前控制台→控制面→host-agent→隔离 worker→出口→TLS 假上游的一次实际组合证据。仍无真实 MySQL/Redis、真实凭据/代理/模型、CLI/1000连接/24h/canary 证据。
- 本轮无 SSH/线上数据/UI/配置/服务/路由/账号操作，真实模型调用为0；不 push、不部署、不启 execution_onboarding、不标 migrated。

## N3前置：专用实验端点只读预检

2026-09-17，先提交固定计分计划和设计 `db92daa`、`74c8491`，再实现独立 `recoverykit/lab` 模块及CLI接线；未运行旧 `docker-e2e.sh`。本切片属于N3前置工具，不是完成的Linux实验环境或镜像；总体23/100，镜像3/15，均未增加。

### 实际实现

- 只接受预先确认的Engine ID/name/架构及显式本地Unix socket；拒绝远程/默认端点、环境Docker context/proxy/SSH agent，不读取Docker客户端配置或调用Docker/Colima/SSH/shell。
- 逐级无symlink/可信所有权检查、直接父目录0700、socket当前用户所有；每次请求前后及结束时复核inode等身份。信任本机用户/OS/daemon，不声称能抵御同UID恶意替换或伪造元数据。
- 固定Engine API1.43，验证服务器显式min/max版本后仅五次GET；精确身份/架构、Linux/runc/AppArmor/seccomp、无Swarm/daemon代理、空容器/卷、三默认网络白名单。版本缺字段和未知能力失败关闭。
- 单请求3秒/总12秒预算，shutdown计时器处理header/body slow-drip；每body上限2MiB，严格HTTP framing与JSON重复键/数字/类型检查。CPU解析不会被计时器强制抢占，但解析跨过期限绝不返回成功；stdlib固定header行数/行长上限保留。
- CLI只输出固定字段或错误类型；成功仍为 `docker_endpoint_checked`、`isolation_verified=false`、`execution_permitted=false`、`created_resources=0`，不自动授权旧脚本/构建/清理或后续运行。

### Review 与验证

独立review确定性复现两项P2：connect跨过单次期限仍发GET，以及JSON解析跨过期限仍返回成功。主代理新增回归先确认两项FAIL，再统一在connect/headers/解析之后检查期限，修复后reviewer复核关闭。reviewer交叉审元数据策略、CLI、Unix传输与新测试，未发现其他可复现P1/P2；该结论不扩展为真实隔离通过。

以下全部只用合成文档和当前测试创建的临时本地Unix HTTP server，无Docker daemon/VM/线上账号：

```sh
cd recovery
PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=tooling:.. python3 -m unittest discover -s tests/lab -t . -v
# 45项通过；reviewer独立同范围也45项通过
make test contracts
# 全部236项Python测试通过；14份manifest、116条entry结构校验通过
```

主代理另将上述四个lab测试模块完整重复10轮，450次测试通过，未只挑选超时两例。合同检查仍明确 `business_verification=false`，不是在线接口兼容验收。git diff检查与文本/evidence内容检查通过，仅提交源码、合成测试、脱敏文档。

### 仍缺与禁止扩大结论

N3/VM0b尚未关闭：实际外层VM的共享目录/端口转发、防火墙安装及仅本任务资源清理、当前镜像/卷、N4/N5网络矩阵均未运行。此工具没有检查image/build cache，也不批准使用旧缓存。Docker元数据不能证明外层宿主隔离或以后不会漂移。

I2–I5镜像构建/固定制品/home初始化/空白冷启动、K1–K5独立身份与mTLS、真实CLI、控制台完整调用及最终验收仍开放。本轮无SSH/线上数据/UI/配置/服务/路由/账号操作，真实模型调用为0；不push、不部署、不开启功能标志。

## S1a：基础镜像配方与离线构建上下文

2026-09-17。先提交聚焦阶段规划 `21d77ee`，保留Sub2价格/用户倍率/产品业务以及CCMAX模板；随后实现 [image模块](../../../execution-plane/isthmus-runtime/image/README.md)。当前只完成S1a子切片，不是S1整个阶段，固定百分比仍23%、镜像20%。

### 实际交付

- base-only Dockerfile模板由精确platform和OCI digest渲染，拒绝浮动tag；只有显式包目录和SHA清单被COPY，无仓库根/账号home/旧bundle。离线dpkg安装没有网络修复兜底，固定非交互；UID/GID1000，home和必要子目录0700，不恢复SYS_ADMIN文件能力，默认/bin/false而非伪装成已启动服务。
- 独立锁schema固定来源、文件名、包名/版本/架构、大小/hash，限制数量/字节和核心包；拒绝未知字段、重复、路径/命令字符、非官方来源、URL认证/query/fragment和架构混用。来源URL只记录，不访问。
- staging只复制清单中的普通无硬链接文件，1MiB分块hash/写，前后核大小/inode/元数据/当前路径；输出为仓库外新目录0700、文件0600，不覆盖。独立verify核精确文件/目录、私有权限、锁/配方/checksum/receipt及最初请求的绑定。
- CLI只输出固定计数和 `image_built=false`、`execution_permitted=false`；没有download/build/run/cleanup接口，也不读取Docker环境。CLI整链测试在存在合成代理/DOCKER_HOST时mock禁止socket与subprocess，仍完成本地生成/复核，无外联。
- receipt在复制和初次复核后写入；最后写入/复核可能失败并留文件，但命令必须失败。新增确定性最终IO错误测试，要求后续独立verify，不把receipt存在当完成标志，不自动删除失败产物。

### Review 与先失败后修复

作者自身检查取得两个初版FAIL：解析锁后到inventory之间替换文件可逃逸、复制后改成另一套自洽lock/recipe可脱离原始请求。修复分别固定读取前身份和前后receipt与原始输入的比较，交叉reviewer复核通过。

另一交叉review指出模板没有显式设置.cache/.local中间目录权限，以及URL尾裸?/#与禁止query/fragment合同不一致。主代理先新增回归取得三项断言FAIL，再明确所有目录owner/mode并在解析前拒绝分隔符；reviewer独立37项通过并关闭。目录用例仅核配方，不宣称已经在Linux核验实际所有权。

主代理还检查真实CCMAX账务代码，修正规划中“一个计费权威”的业务边界为Sub2终端用户账；CCMAX已有服务账户/成本扣减路径不能误删。此次没有修改计费/网关源码。

### 最终验证

```sh
make -C recovery check
# 236项恢复工具Python通过；14份合同/116条entry结构校验通过（business_verification=false）
# Bun 1.3.9：111 pass、0 fail；镜像工程Python：37项通过
make -C recovery image
# 37项通过；reviewer独立同范围通过
```

主代理对image全部4个测试模块最终重复10轮，370次通过。`git diff --check`及文本/evidence内容扫描通过；安全扫描曾拒绝测试中直写的合成userinfo URL，已改为运行时组装明确合成字段，保留拒绝测试且没有放宽政策。初次make image遇到测试package尚无__init__.py的导入失败，补齐后最终全套通过；未把该中间失败隐去或当环境验收通过。

### 仍缺与停止线

没有完整官方base/package发布锁，没有校验本轮真实APT签名/包control metadata/依赖闭包/Pre-Depends顺序、目标架构匹配，没有Docker构建或Linux冷启动。合成ar头不是可安装软件包；hash只绑定审阅输入，不认证发布者。RUN无网络不证明FROM/builder无外联。实际builder准入、I2/I3/I4/I5、N3–N5、K/R/H/C/L/E原门槛仍开放。

本轮仅联网查阅官方Docker语法说明；无SSH/生产数据/UI/配置/服务/路由/账号操作，不读真实home/密钥，不下载或执行线上二进制，不启动VM，不改本机/项目Bun1.3.9。真实模型调用0次；无push、部署、execution_onboarding或migrated变更。下一切片是官方依赖固定与专用Linux实际构建，不再用增加静态工具代替这项运行证据。
# S1b原生Linux基础镜像构建

2026-09-17，先提交执行计划`7210db9`、review权限修订`4c09103`再执行。
阶段实现提交`3d7c43b`；本文为对应最终实测记录。
仅170.106.159.197测试机；43.153.75.220仅SSH中转，未访问216.106.185.119生产。
用户确认闲置，但保留原Portunex/CCMAX等业务；这不是空白专用宿主，不关闭N3/I5。

## 输入与真实构建

- 官方Docker registry通过HTTPS读取并核sha256，锁定Debian13-slim amd64 manifest
  `abc9cb88a5587630d7f915f47b23b0668fe250fbfc6457aa4d52b534c1bbf73f`。
  BuildKit v0.33.0 amd64 manifest
  `a461e7f0ce921972028acfbed628d45663d83e67ac1230722c2b34cf72760a0d`。
  它们是registry manifest digest，不是image config ID；不额外宣称验证了OCI发行者签名。
- [真实base锁](../../../execution-plane/isthmus-runtime/image/locks/base-linux-amd64-2026-09-17.json)：
  9包、7,993,428字节，ca-certificates20250419、passwd1:4.17.4-2、procps2:4.0.4-9、
  util-linux2.41.5-0+deb13u1及其基底外依赖。APT严格update、官方Signed-By且未禁验签，
  保留原始InRelease/Packages、每包index/control/URI；HTTP签名APT获取与HTTPS来源地址
  如实区分。实际keyring.pgp SHA256
  `506b815cbb32d9b6066b4a2aa524071e071761e7e7f68c3ac74f3061ba852017`。
- 最终上下文canonical lock SHA256
  `308dd8cb28ca7e359dabb376908c6070d9ab57d15d2b2346d85b9d0946c2c575`；
  渲染Dockerfile SHA256
  `e971191030efca4b350c28dd0dcecce434213e37afbc75c6ba59cc6b2653b2c3`。
  staging仍不授业务执行权，只把严格白名单包送入构建上下文。
- 实际离线dpkg安装成功。builder官方可信输入特权例外，独立缓存卷、无host bind/socket/
  port/secret/SSH/build-arg/环境代理，rootful不是不可信代码沙箱。真实cgroup根核对
  2CPU、2GiB、swap0、pids512；采样到dpkg/证书更新等进程在准确容器scope的子组内。
  builder watchdog900秒，客户端预算失败停止准确ID；磁盘8GiB增量/20GiB剩余是监控阈值。

## 最终代码的完整重跑

第一轮定位问题并修复后，使用**新0700目录、新上下文、新空BuildKit缓存**，重用同次
已验签索引所对应的确切9包字节，不重新浮动解析APT。直接执行当前build→smoke→cleanup
模块，全部返回成功；没有手工补状态通过最终这一轮。

- 构建exit0；缺失target负例exit1且错误准确匹配，失败输出不存在或仅0字节占位，不加载。
  构建器停止成功后才写built-image完成记录，非零传输错误不能冒充负例PASS。
- UID/GID1000、home及子目录0700且空、machine-id空；无网络（仅lo）、dropALL/NNP、
  readonly根、tmpfs、128MiB/0.5CPU/pids64；写根及unshare mount被拒。
  全9包版本/架构/install ok installed逐项匹配，dpkg --audit为空，默认false按预期exit1。
  注入额外合成context文件被拒，恢复后再verify；镜像Env仅PATH/HOME/USER/LOGNAME。
- 最终镜像tag `isthmus-vm-base-lab:isthmus-s1b-8g2apoxg`；image ID
  `sha256:3f7a9a38c6ae0a779eb563ceb98cd70db6ae62243bf19ffbaa35587fa15ab807`。
  Docker导出42,482,688字节，SHA256
  `193ee2984814e4c7f900ae7a7f88109e600cd9b07e56aef1592a63d845eb05a8`。
  两次构建验证输入锁和功能重建，不主张逐字节相同输出（时间元数据尚未归一化）。
- 原4个业务容器的ID/image/StartedAt/RestartCount/挂载元数据前后相同。没有部署或操作
  原服务、数据库、UI，也未手动修改宿主路由/防火墙配置（Docker自己的测试网络资源由
  daemon管理）。只移除本轮记录并核所有权的测试容器及缓存卷；
  原镜像和本轮导出/输入/证据都保留。没有全局prune，不声称删除事务可自动回滚。
  最后按两轮准确label再次查询：剩余测试容器0、测试缓存卷0；业务容器仍4且基线相同。

## Review与修复

两名独立代理审查，关闭：全量inspect读取业务Env、输出缓冲/超时末尾窗口、异常后漏停、
builder的image/卷/身份/namespace绑定、提前发布完成记录、APT缓存epoch与URI转义、
keyring软链接、用户创建时复制skel。真实运行另外修正local日志轮转参数及APT最小cap。
scope检查的初次误报也保留失败记录：BuildKit将daemon移至/init，不改变上级Docker资源
根；改为核exactCID systemd scope，不忽略任何越界进程。
参见[BuildKit v0.33.0实际初始化源码](https://github.com/moby/buildkit/blob/v0.33.0/executor/resources/monitor.go#L236)。

`make -C recovery check`：236恢复Python、111Bun1.3.9、101镜像测试PASS；
14manifest/116条合同结构校验仍`business_verification=false`。这些单测与真实运行证据
分别记录，不把合成测试当真实账号/生产兼容性证据。
101项镜像测试另连续十轮1010次PASS。原始包上下文、6份APT索引/Release、keyring及
42,482,688字节镜像导出已保存在本机仓库外0700目录
`/Users/ruanyang/My-project/api/z/isthmus-s1b-artifacts.VSdfTzYv`；传输后全部大小/哈希和
S1a context独立复核通过。原始二进制不入Git；Git保存源码、真实锁与本证据摘要，未push。

**只关闭I2（+3分）：总体26%，镜像6/15=40%。** I3完整Bun/CLI/app及双架构、I4独立
持久home、I5空白环境runtime冷启动、N3–N5内核隔离与出口、K每实例密钥证书、R3–R5真实
运行服务、控制面桥接均未因此通过。没有借用账号、真实模型请求、生产配置开关或Git push。
