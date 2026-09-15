# B1：会话绑定的只读执行快照

先规划，后实现。B1 是 B 的前置安全切片，不是 host-agent 已完成生产装配。

## 已确认的缺口

- `slot_assignments.last_observed_at` 没有控制会话身份；仅 JOIN 当前 Node，重连后的新 Hello 可使旧观察再次符合健康条件。
- NodeControl 校验结果属于当前 pending command，但入库时丢失 session ID。
- heartbeat 中的 slots 虽被校验，却没有写入 assignment；宿主 Snapshot 又来自缓存，不能把心跳时间冒充实际 runtime 新观察。

## 模块和合同

1. `runtime/store/` 增加可逆迁移 013，为 assignment 增加 nullable `observed_control_session_id`。历史数据保持 NULL，不回填、不改线上。`CommandResult.ControlSessionID` 从认证控制会话传递，不能来自节点报文。新认证结果必须在同一事务内锁定并校验当前 connected 节点会话，再更新 observation；旧无会话的内部观察路径只能清空证明，不能继承。MemoryRepository 同步语义。
2. `runtime/store/execution_binding.go` 用一次只读 JOIN（Memory 使用同一锁）读取 slot、active assignment、node、durable execution lease。核对账号/槽位/节点、epoch/generation、镜像、ready状态、provider ref、当前 observation session、节点/观察时间和租约有效性。不读凭据列、不依赖 route cache、不将查询时间写成 ObservedAt。
3. 新 `internal/executionauthority/` 负责把只读结果转为 `dataplane.Snapshot`。它仅运行于控制面侧，仍需当前活动控制流和证书校验；数据库残留 connected 不能代表活跃连接。延迟读取后重新检查 freshness/租约有效期。现有 FencedResolver 继续独立验证 Redis lease，并在 runtime lookup 前后检查绑定。
4. `control/` 提供只读当前会话校验，断连、上下文取消、证书撤销、会话替换均拒绝；不要持锁执行数据库 I/O。host-agent 不拿数据库、CA私钥或签票私钥。B1 尚不新增RPC、不打开listener、不注册默认生产路由。

实现/review补充：Snapshot携带ControlSessionID，Resolver在lookup前后精确比对，连“途中重连且已重新确认同一实例”也拒绝返回旧查找结果。健康成功结果需匹配已发命令image/deadline，并在持久化同事务锁定核对assignment的实际image；没有provisioning job也必须通过。接收时间在observer前冻结，observer后重查期限，observer/storage都受命令context期限约束。已经打开的流尚不绑定最初session，这不是B1宣称关闭的门槛。

## 验收与边界

- 单元、SQL合同和内存模型：无会话历史观察拒绝，当前会话允许，断连拒绝，只有 Hello/heartbeat 的重连拒绝，旧会话迟到结果拒绝且不落部分写入，新的受控结果可重建证明。
- desired generation、epoch、account/node/ref、镜像、状态、新鲜度、租约任何不一致均失败；查询阻塞跨越有效期后不得放行。
- 组合真实 FencedResolver 验证旧账号/节点/代、Redis故障、lookup期间重连和控制面故障不会返回 runtime。合成依赖不是实际MySQL/Redis生产验证。
- 独立 review、race/vet、脱敏扫描后提交。实际MySQL迁移与并发锁验证须另列未执行，不把 sqlmock 当真实数据库证据。
- B1 不刷新 observation；超过45秒未有实际受控确认必须失败。主动健康检查/续期调度、签票RPC、runtime registry、host-agent装配留在 B2–B4，不允许用缓存心跳延长健康。
- Snapshot 的 Ready 只说明当前分配/会话/租约具备调度候选资格；worker mode readiness、active credential/proxy version、业务授权仍是后续签票/执行必要门槛。
