# B2b2a：当前控制命令的只读诊断签票

日期：2026-09-16。先关闭 B2b1 提交后 review 的两个取消问题，再实施此切片。依旧只做本地开发/合成验证，不连接线上、不借用账号，不启用生产配置。

## 范围与分工

- `internal/control/probe_tickets.go`：控制面持有 Ed25519 signer，认证与命令实例固定、只读权威核对、限时诊断签票。既有 `server.go`/`probe.go` 仅作少量会话事件/派发状态适配。
- `internal/hostagent/probe_ticket_client.go`：仅在当前 command context 内可用的诊断票请求/响应桥，队列和等待表有界，断连/取消/迟到回包清理；`controlclient.go` 仅接线，`runtimeclient.go` 可选择当前命令绑定的来源。
- `api/proto/execution/v1/control.proto`：Control 流兼容新增请求/响应 oneof，不新增 gRPC 方法。离线生成脚本只新增 control.pb.go 的生成/检查入口；不使用远程插件或下载依赖。
- `internal/control/probe_ticket_integration_test.go`：实际本地 TLS ControlServer/ControlClient、命令请求、控制面签票、worker Health/公钥 RPC 的合成闭环。

## 授权合同

1. **默认关闭且显式 capability。** 控制面仅在可注入 `ProbeTicketConfig` 完整有效时启用；host-agent 需显式启用并广告 `probe_tickets`。未配置/未协商不得签票或回退到本地 signer；不修改生产 bootstrap 或开启默认监听。
2. **只签两种只读 scope。** `health` 仅对应 INSPECT SlotCommand；`credential_key` 仅对应 CredentialKeyCommand。拒绝 activate、secure_activate、messages、count_tokens、混合 scope 及未知值。Health/public key 不携带已有凭据明文，但返回的票仍是 bearer secret，不写日志或数据库。
3. **请求不携带授权身份。** `ControlProbeTicketRequest` 只有 command_id/scope；账号、slot、node、epoch、image、generation 从当前已认证 TLS 控制会话、控制面已派发命令和 ProbeBinding 推导并精确对比。SlotCommand 使用已有严格规范正整数 `metadata.desired_generation`；CredentialKeyCommand 添加 typed desired_generation，来自现有 provisioning generation。旧缺 generation 命令仍按旧合同执行，但不能换新诊断票。
4. **pending 不等于已派发。** 记录原 outbound 对象及开始交给 stream.Send 的授权点；只在该点之后允许换票，而非 reserve/queued 时。此点不是接收确认；发送失败与会话结束后不能继续签票。每次 pending 实例/允许的 scope 最多一次签票尝试，前置 claim 用实例指针固定，失败/丢票也不重复签新 nonce，只能由控制面派发新命令重试。无跨命令无限历史/票据缓存。
5. **双读与当前会话固定。** 单次检查默认2秒且有上限；先固定 session/pending，再核活动 TLS1.3 身份、ProbeBinding/current durable lease 和独立 lease，随后重读并复查。account/slot/node/epoch/generation/image/providerRef/owner/session 变化、过期、取消、依赖失败均拒绝。最终签名/入队仍绑定原 session/pending，不把已签结果送到新会话；出队前再次拒绝失效 attempt、已完成命令和过期票。
6. **最短期限。** 票默认5秒、最大10秒，受原始命令 deadline、durable lease、节点新鲜度、控制证书有效期及前后读取期限限制；秒精度 exp 向下取整，不为补足一秒越过任一上限。独立 lease 目前只有 Validate、没有原子剩余 TTL：本切片仅证明核对当时仍有效，不宣称票有效期被 Redis PTTL 截断或撤销可即时生效。已发出的诊断票可残留至配置 TTL；因此不能用于业务或续租。
7. **桥接有界且只使用同一控制流。** 每 session 限制请求队列/等待项，不另开无界 goroutine。取消与断连立即使等待失败；迟到且已经无人等待的合法回包可丢弃，不用无限 tombstone。command scope 不匹配、响应字段矛盾、过大票和无当前 command context 均拒绝。启用命令绑定来源后不得偷偷回退旧 TicketSource 获取其他权限。

## 验收与停止线

- 先复跑并提交两个 review 修复，再提交本规划；实现后独立交叉 review、主代理离线 race/vet、重复边界回归、协议重复生成零 diff、文本/evidence policy 检查和阶段提交。
- 覆盖缺 capability/默认关闭/无 pending/仅排队/错 scope/同命令并发重复/旧实例 ABA、跨节点/旧会话/旧代/错镜像/版本世代变化、校验中撤销/取消/到期、队列压力与迟到响应。真实 TLS 控制流签出的票交由实际 worker Guard 验签并限制健康/公钥用途，验证 nonce 不能重放且不能用于模型执行。
- 使用 Memory/sqlmock/内存 lease、临时测试 CA/签票密钥、bufconn worker/合成 Onboarder；不调用真实模型，不读线上凭据，不自动启用 DB/Redis 集成。
- ProbeBinding 要求已存在 providerRef，本切片不解决新建实例的首轮观察循环，也不实现 B3 existing-only runtime registry；默认 host INSPECT 仍可只检查 provider，本地组合的诊断 executor 不冒充生产装配。
- **B2b2 仍开放：** secure activation 的 payload/lease 精确授权、业务票的版本/代理/模式绑定与 worker 使用时复核、可信证明驱动的双层续租、原流撤销，以及真实 MySQL/Redis/VM/CLI/gateway/容量/24h 验收另做。不把 B2b1 receipt 或本切片短诊断票用作执行许可。
