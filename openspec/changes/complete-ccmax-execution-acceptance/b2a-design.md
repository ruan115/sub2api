# B2a：有界的主动宿主观察

日期：2026-09-16。依赖 B1；本切片只完成本地库/合同闭环，不等于整个 B2 或生产装配。

## 动机与边界

B1 观察只在实际命令结果入库时刷新，45 秒后失效；心跳只证明节点在线。`ReadExecutionBinding` 不能授权重连后的首次探测，因为它本身要求新会话已有健康观察。现有 reconcile 对健康运行实例返回无操作，不能承担周期验证；旧 pending 命令也没有探测专用到期回收。

本切片只发送现有 `INSPECT`，host-agent 调用 `provider.InspectSlot` 获取实际状态，不读缓存 Snapshot，不创建、启动、销毁实例。容器健康不是 worker 模式/凭据/代理已就绪，不签任何票、不续租或重新 Grant，不触发 onboarding，不连接线上。

## 文件与职责

- `runtime/store/probe_binding.go`：独立、无凭据的 ProbeBinding，单一致性 JOIN / Memory 单锁读取与 keyset 分页；不能转换为业务授权。
- `control/probe.go`：固定活动会话的 INSPECT 派发、探测单飞、保留控制容量、过期及生命周期冲突失效。既有 Control 通道和 B1 入库校验仍负责结果。
- `runtimeprobe/`：有界分页 Runner，只依赖只读候选、活动会话校验、独立 lease 校验、受限派发接口；不拥有 provider、凭据、密钥或租约写接口。
- 测试按模块放置；真实 TLS 合成控制流闭环放在 `control/`，复用已有临时证书 harness。生产 service/host 装配留到 B4，默认不自动启用 Runner。

## 合同

1. ProbeBinding 核对 desired ready、当前未释放 assignment 的 epoch/generation/image、非空已有 provider ref、已连接节点及新鲜心跳、准确 current session、未撤销且未到期 durable lease。允许观察缺失、过期、不健康、来自旧 session；未来时间/不一致状态拒绝。最大 node age 为 45 秒。
2. 候选分页按二进制 slot ID 严格递增，SQL 每页最多扫描 100 行；只返回无凭据字段。Page 独立返回 `NextAfterSlotID`，格式过滤后的空候选页仍按最后扫描的合法 key 推进；不能安全推进的 key 固定失败，不无界重扫。每次 Step 最多一页；扫描不足一页回卷。Memory 单锁扫描内存 map，但页缓冲最多 100 行并检查取消。大规模覆盖延迟必须另做容量验收，不因默认批次而保证所有槽位 45 秒内刷新。
3. Runner 默认每秒扫一页，同会话最近 15 秒有健康证明则跳过；失败/无证明仍受分页、短 deadline 和控制通道单飞约束。对候选验证实际认证 Control 会话、独立 Redis/Memory lease，随后重读绑定，发生账号/节点/session/epoch/generation/image/provider/owner 变化则拒绝，重读后再次验证独立 lease，调用前再检查新鲜度与 lease 截止时间。仅有 durable lease 不足以派发。
4. 每个探测生成新的随机命令 ID（`probe-` + 32 位小写 hex），不复用 provisioning job 幂等键。此命名空间只用于受限探测入口，普通 Dispatch 必须拒绝任何 `probe-` 前缀，避免过期回收后迟到结果被重新归类为普通命令。派发 API 仅为控制面内部可信接口，节点无派发权限；调用方必须每轮生成新随机 ID，不能重用历史 ID。deadline 默认/最多 10 秒且不得晚于 durable lease 截止时间；每个依赖调用均承接短 context。串行有界处理，不启动无界 goroutine，不持有无限 slot 缓存；并发 Step 拒绝。
5. `DispatchToSession` 只允许 INSPECT，必须校验并固定相同活动会话，不能查询 session A 后发到 B。探测与同 slot/epoch pending 操作冲突时拒绝；探测最多使用一半 pending/queue 容量，至少留一个队列位置，不阻塞等待入队。
6. 到期探测回收 pending；发送前跳过失效/过期探测。非探测控制命令使同槽位旧探测失效。迟到/未授权结果不得写库（未知结果仍按既有 fail-closed 策略可关闭控制流），不能用无界 tombstone 保存历史。不可误回收 credential commit 等非探测 pending。
7. 只有节点实际返回并通过 B1 session/image/deadline/transaction 检查的结果更新观察；Runner 的扫描、派发、heartbeat、失败、重试不刷新时间。不自动重新获取过期 lease，不宣称 snapshot 的 Ready 是业务就绪。
8. Runner 错误采用固定分类和计数，不输出账号/实例/凭据/原始依赖错误。单个节点拒绝不饿死后续分页；持续扫描故障有限重试后 Run 返回错误，取消干净退出。

## 验收与后续

本地离线测试：无健康 proof 仍可探测；当前 session/独立 lease 门槛；断连/替换/过期/跨槽位拒绝；重复探测与容量保留；迟到结果无更新；实际 Control TLS 发送 INSPECT、返回结果、重连后重新确认；分页、公平前进、取消、依赖延迟、race/vet。SQL 使用 sqlmock 仅证明语句/参数合同，不冒充真实 MySQL 并发/迁移证据。

以下不在 B2a 完成范围：worker 实际加载 credential version/proxy/mode 证明、health/activation/messages 分 scope 的受认证签票、可信证明驱动的 durable+Redis 续租、B3 现有实例 registry 和流级初始 session 固定、B4 默认关闭的生产装配、真实 Docker/DB/Redis 故障与容量测试。探测沿用 B1 命令结果落库，每轮都会增加 `node_command_results` 记录；生产接入前必须补保留/清理或紧凑存储策略及容量证据，不能无限保留周期结果。不得因为本切片通过而勾选整个 B2。
