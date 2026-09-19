# P4a：执行租约权威的状态转换设计

2026-09-19。承接时间命名规划
[2026-09-18_13-17-37-isthmus-claude-handoff.md](../../../docs/plans/2026-09-18_13-17-37-isthmus-claude-handoff.md)
第 4 节 P4 第一条「先写状态转换设计」。不重做 P3a–P3d，不打开业务开关，不接线
生产签发，暂停中的 `image/lab/build.py` 与 `image/runtimekit/` 继续绕开。

## 1. 现状核实（不是意图，是代码）

- Redis 后端 `internal/lease/redis.go` 已实现，键前缀 `execution:lease:v1:`
  （`internal/config/runtime_enrollment.go:16`），键为前缀 + `sha256(slotID)` 前
  16 字节的十六进制；值是 base64url 的 `{NodeID, ExecutionEpoch, OwnerID}`。
  acquire/renew/validate/revoke 全部是 Lua 脚本，比较令牌后再动作，**原子**。
- SQL `execution_leases`（`internal/runtime/store/leases.go`）是持久归属与撤销记录。
  `GrantExecutionLease` 会 `FOR UPDATE` 锁住 assignment，并要求该 assignment 处于
  活动状态且节点匹配。
- `internal/lease/coordinator.go` 的 `Coordinator` 与 `failover.go` 的
  `FailoverController` **都没有非测试调用方**。生产只在
  `internal/service/runtimeenrollment/runtime.go:63` 构造了 `RedisBackend`，而且
  **只用于校验**。这就是台账里「生产权威 lease writer 仍缺」的确切含义。
- 代理租约（`proxy_leases.go`）的有效性**依赖**执行租约：
  `ValidateCurrentProxyLease` 连接 `el.revoked_at IS NULL AND el.expires_at > now`。

## 2. 权威划分

| 角色 | 承担者 | 不承担 |
| --- | --- | --- |
| 当前代次的围栏（fencing） | Redis 令牌 + TTL | 不是持久归属记录 |
| 持久归属与撤销事实 | SQL `execution_leases` | 不是实时围栏 |
| 过期 | Redis TTL，**且续期必须同步推进 SQL `expires_at`** | 校验侧不由 SQL 时间戳再推导 |

过期这一行有个前提，不满足就会产生两库分歧：**续期必须同时延长两边**。
`ValidateCurrentProxyLease` 读的是 `el.expires_at > ?`，所以如果续期只刷新 Redis
令牌，SQL 的 `expires_at` 会逐渐过期，而令牌长生不老——执行租约校验说「有效」，
代理租约校验说「失效」。见第 5 节：`Renewer` 因此必须能被 `Coordinator` 驱动。

明确的**非**权威（计划书点名）：

- **route TTL 不是执行租约**。路由发布的 TTL 只说明路由条目新鲜，不授权执行。
- **签发存储不是租约权威**。`runtime_certificates` 回执证明某个 `(slot, epoch)`
  签发过证书，不证明此刻仍持有租约。

## 3. 两库皆可单独「过度授权」，所以判定取合取

`Coordinator.Revoke` 先写 SQL 再删 Redis 令牌。若删令牌失败，后端会继续把该 claim
报成 current 直到 TTL 耗尽；**只问后端的校验会继续授权一个已被撤销的租约**。反向
亦然：令牌已删但持久写入失败时，SQL 仍显示活动租约，而代理租约校验正是读 SQL。

因此 `Coordinator.Validate` 现在是**两库合取**：后端令牌匹配，**且**持久记录存在、
未撤销、node/owner 相符。

过期**刻意不**由持久行再推导：围栏 TTL 才是过期权威，拿数据库时间戳与本进程时钟
相比只会把时钟漂移变成误撤销。

### 分歧矩阵

| Redis | SQL | 成因 | 判定 |
| --- | --- | --- | --- |
| 令牌匹配 | 活动 | 正常 | **授权** |
| 令牌匹配 | 已撤销 | Revoke 的 Redis 步骤失败 | 拒绝（本轮新增） |
| 令牌匹配 | 不存在 | Grant 的 SQL 步骤失败且补偿也失败 | 拒绝（本轮新增） |
| 令牌匹配 | node/owner 不符 | 旧代残留或并发写入 | 拒绝（本轮新增） |
| 无令牌 | 活动 | Revoke 的 SQL 步骤失败，或 TTL 自然到期 | 拒绝（后端） |
| 无令牌 | 已撤销 | 正常撤销完成 | 拒绝（后端） |
| 令牌匹配 | 活动但 `expires_at` 已过 | **续期绕开了 Coordinator** | 授权——但这是缺陷状态，见下 |
| 不可达 | 任意 | 无法判定 | 拒绝（失败关闭，原有） |
| 任意 | 不可达 | 无法判定 | 拒绝（失败关闭，本轮新增） |

倒数第三行不是可接受的稳态：此时执行租约判定为有效，而代理租约校验因
`el.expires_at > ?` 判定为失效，同一租约被两条路径给出相反结论。**唯一正确的
处置是让它不可达**——续期必须走 `Coordinator`，同时推进两边。本轮把
`Renewer` 的依赖从 `Backend` 放宽为 `Refresher` 正是为此（第 5 节）。校验侧仍不
重新推导过期：那只会把时钟漂移变成误撤销，治标且引入新的失败模式。

规则：**状态不可验证或两库分歧时一律不授权。** 分歧靠重试撤销来收敛，收敛期间
读侧失败关闭。代价是持久库故障会牺牲可用性——这与后端不可达时既有的取舍一致；
优化路径是读缓存，不是放宽正确性。

## 4. 失败与竞争恢复：不得并行授权旧代

- `acquireScript` 是「不存在**或**令牌相同才写入」。因此**新代次无法夺取仍存活的
  旧令牌**，只会拿到 `ErrLeaseHeld`，不存在覆盖式抢占。
- 旧代必须先被围栏掉：`FailoverController.FenceAndRelease` 先检查后端可用，再
  确认旧租约**已不是 current**（不可用时失败关闭），然后撤销并
  `ForceReleaseAssignment`。只有 assignment 被释放后，新的放置才会取得新 epoch
  （见 [P3c](verification.md#p3creconcile-停止路由改为销毁释放重新放置)）。
- `Grant` 的顺序是先 Redis 后 SQL。若 SQL 失败则补偿性删除令牌；补偿再失败会留下
  一个「幽灵令牌」，它**阻塞**该槽位直到 TTL，而不会造成双重授权——失败方向是
  拒绝服务而非越权，这是刻意的选择。
- `Renew` 的顺序同样是先 Redis 后 SQL，SQL 失败则撤销令牌。由于
  `RenewExecutionLease` 要求 `revoked_at IS NULL` 且节点匹配活动 assignment，
  带外撤销会让续期的 SQL 步骤失败，从而**自动收敛**为令牌被删。

## 5. 围栏与续期都应接最强权威

两处都曾把依赖写死成 `Backend`，从而把「只有 Redis」固化进类型里：

- `Fencer` 只用到 `Validate`，参数类型改为新的 `Validator` 接口。
- `Renewer` 只用到 `Renew`，参数类型改为新的 `Refresher` 接口；`Coordinator.Renew`
  相应接收 TTL（此前它用自己的 `c.ttl`，而 `Renewer` 传的是 `timing.OfflineAfter`，
  两者本就可能不一致）。

`Backend` 与 `*Coordinator` 都满足这两个接口。生产必须接 `Coordinator`：否则
第 3 节新增的那些拒绝在围栏上不生效，且续期只会推进 Redis 一边。

另外 `Fencer.Revalidate` 原本逐连接校验。同一槽位上的所有连接共享同一个 `Claim`，
因此现在按 `Claim` 去重，每轮每个不同 claim 只发一次权威往返——校验可能要读持久库
之后，这一点从「无所谓」变成「必要」。

## 6. 本切片**没有**做的事

- **没有接线生产 writer**：`Coordinator`/`FailoverController` 仍无非测试调用方，
  也没有续期循环在跑。这是 P4 的主体工作，不因本设计而关闭。
- **没有 runtime registry**：仓库中不存在「已有 CID → 连接 / 当前代次 / 租约归属 /
  回收」的注册表。最接近的既有件是 `runtime/store/slots.go` 的 assignment 模型、
  `control/server.go` 的 node 会话表，以及 `hostagent/egress.go` 的 `EgressRegistry`
  模式。注册表须在这些之上建，且**不得隐式 Create**。
- **撤销传播仍不完整**：P3a 让出口 tunnel 在绑定撤销时被回收，`Fencer.Revalidate`
  会关闭失效 claim 的受保护连接。但 `control/server.go` 的会话表按 **NodeID** 索引，
  **不绑定 epoch 或租约**，所以撤销执行租约**不会**关闭 worker 的 mTLS 流。
  这条仍然开放。
- **没有真实 Redis/MySQL 实证**：`EXECUTION_REDIS_TEST_URL`、
  `EXECUTION_MYSQL_TEST_DSN`、`EXECUTION_CCMAX_MYSQL_TEST_DSN` 在本地一律不设置，
  相应集成测试整体 skip。幂等/并发/超时须在专用本地或实验实例上另行核对，
  **不读取、不迁移线上数据库**。
- 没有接 CLI 数据面与出口通道；没有 session/pool/MCP 续接。
- `/readyz` 保持 503、`production_ready=false`；缺权威 writer 时生产签发继续拒绝。

## 7. 验证

`internal/lease` 18 个子测试：分歧矩阵五行各自拒绝、两库任一不可达均失败关闭、
过期刻意不由持久行推导、畸形 claim 拒绝、`Fencer` 接受 `*Coordinator` 并在**仅
持久撤销**时关闭受保护连接、`Renewer` 可由 `Coordinator` 驱动且续期同时推进两边、
持久撤销后续期失败并连带删除令牌。变异验证非空跑：把合取改回只问后端即 FAIL。

两轮独立 adversarial review。第一轮核实了分歧矩阵每一行可达、`acquireScript`
确实无法被新代次夺取，并发现两个真缺陷：**跳过过期会造成两库分歧**（因为
`Renewer` 只能接 `Backend`，`Coordinator` 无法驱动它），以及 `Fencer.Revalidate`
的 N+1 加共享超时可能级联关闭全部连接。另有 `%v` 吞掉错误链、测试桩与真实 SQL
语义漂移（忽略 ownerID、重复撤销覆盖首次时间戳、行只按 epoch 索引）。全部已修。

本机离线：全量 `go test -race`、`go vet`、linux/amd64 `go build ./cmd/...` 通过，
`internal/lease` race ×10；`make -C recovery check` 236 / 150 / 186 与基线一致。
真实 Redis/MySQL 集成测试因环境变量未设置而整体 skip，**未跑**。

分数不变，仍 32%。设计与合取校验都不是实证，不加分。
