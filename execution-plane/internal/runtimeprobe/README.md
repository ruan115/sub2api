# Runtime probe

控制面主动读取已有宿主实例的状态，不能当作 worker 业务就绪证明。

`Runner.Step` → `ProbeBindingRepository` 有界页 → 活动 mTLS 会话 → 独立 lease → 重读绑定/复核 lease → `DispatchToSession(INSPECT)` → host-agent `InspectSlot` → B1 结果入库。只有最后一步更新观察时间。

- 默认一秒一页、最多 100 行候选、当前会话健康证明 15 秒内跳过；每次授权检查最多 2 秒，命令最多 10 秒且不超过 durable lease 剩余有效期。
- 非常驻 slot 缓存；Control 负责探测单飞与容量保留。每轮新随机 `probe-` ID 与普通命令命名空间隔离；节点不能调用控制面内部派发入口。页游标独立于有效候选数，空过滤页不会遮蔽后续候选。
- 不提供 create/start/renew/grant/ticket 接口；不能从探测失败自动复活旧 epoch。
- `Result` 只返回数量，依赖错误不透传；单节点拒绝不终止扫描，连续扫描失败默认 5 次后停止 Run。依赖必须遵守 context；不以逃逸 goroutine 包装阻塞依赖。
- 生产入口尚未启用。周期结果沿用 B1 落库，接入前必须补命令结果保留/清理策略。真实 MySQL/Redis、Docker/VM、容量覆盖周期与 worker 版本/模式/代理健康检查均不是本模块本地测试的结论。

设计及边界见 `openspec/changes/complete-ccmax-execution-acceptance/b2a-design.md`。
