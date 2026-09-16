# B2b1：worker 实际加载状态与控制面核对

日期：2026-09-16。继 B2a 主动宿主 INSPECT 后，先补业务签票依赖的 loaded-state 证明。本切片仍是本地库/合同验收，不打开生产入口、不签发真实执行票、不续租、不使用线上账号。

## 当前缺口

`SecureActivator` 在 vault commit ack 后保存凭据版本 ID/auth type/明文内存，但没有保留本次 activation 的 proxy lease。`ModeHealth` 仅检查版本 ID，和实际 `Ready` 对 credential 非空的条件不同。Health RPC 只返回 slot/epoch/image/modes，没有版本、代理或一次请求的 challenge。因此不能据它判定当前实际装载状态与控制面一致。

## 模块与合同

1. **worker 原子快照** — `worker/health_snapshot.go` 及现有 activator 适配。用同一个 RLock 读取完整 identity、active version ID、auth type、proxy lease ID、worker-local activation revision 和 modes，不调用 `ActiveCredential()` 复制密文或明文，不分两次读拼状态。revision 是本实例本地计数，绝不冒充 vault version number。
2. **激活/排空安全** — 成功 commit 后、发布前在同一 mu 内复查 context/draining；版本/代理/revision 原子切换。失败、无效 ack、取消不发布新状态。Drain 先使 active/健康快照不可用，再等待 operationMu 清理 pending；在途 commit 不能复活。Drain 的返回仍可能等待依赖清理，不宣称强杀在途请求/子进程或流级撤销已完成。
3. **兼容 Health 扩展** — `worker.proto` 仅增加 HealthRequest.challenge（32小写hex，可空以兼容旧客户端）、HealthResponse.challenge 和可选 LoadedRuntimeState。其字段为 account_binding、node_id、credential_version_id、auth_type、proxy_lease_id、activation_revision。Slot/epoch/image 保留原字段。旧/fake health source 保留原 modes 但没有 loaded_state；未激活或 draining 同样没有 loaded_state。带状态的 source 必须提供与 RPC server 完整 identity 相同的单次 snapshot；不得从其他来源补齐。
4. **离线可复现生成** — 只用本地缓存 buf/protoc-gen-go，增加可复用的离线 worker 消息生成/检查入口。只生成 worker.pb.go，既有 RPC 方法和 worker_grpc.pb.go 不变；不用远端插件，不删除其他生成文件，不下载依赖，不手改 protobuf 生成代码。
5. **secret-free 持久投影** — `runtime/store/worker_readiness_binding.go` 新类型嵌入 B1 ExecutionBinding，附 active credential version ID/number/auth type 与当前 proxy lease/reservation/binding ID/revision。SQL 单一致性精确 JOIN，Memory 单锁；不读 ciphertext/encrypted DEK/nonce/AAD/hint/KMS key/代理地址或密码。要求 B1 当前会话新鲜健康观察、durable live lease，以及精确 current active pointer/账号/epoch/generation/未撤销 proxy reservation 与租约。只读失败固定错误且不返回部分状态。
6. **核对模块** — `workerproof/` 仅依赖上述只读 repository、活动会话验证、独立 lease 验证和受限 HealthReader。Check 输入显式 account/slot/node/epoch/generation/mode，核对 authority 后生成每次新随机 challenge，在一个短 context 内主动调用 Health；核对回显、身份/镜像、版本/auth type、proxy lease、revision>0、请求模式及无重复/畸形 modes；随后重读 authority，并再次校验 session/独立 lease/期限，任何绑定或 active version/代理变化均拒绝。错误不暴露依赖原文或状态细节。
7. **结果不是执行许可** — 返回此次核对的只读 receipt，不是可缓存 route、票据或续租授权。只证明某次请求看见的 loaded metadata 与控制面一致，不保证上游账号额度、网络可达性或 CLI 实现；challenge 不是恶意宿主不可伪造的硬件证明。HealthReader 必须后续接入受认证的健康签票与 B3 existing-only runtime/私网连接，不能接受任意 worker URL。

## 防止循环依赖与停止线

新 WorkerReadinessBinding 要求 active/version/proxy，所以只能用于 loaded-state 核对；**不能**据它签发首次 health/activation 票，否则重新形成先 ready 才能检查 ready 的循环。后续受限 health 签票必须基于当前 session 的受控 pending 命令及 ProbeBinding/Redis；业务票另要求完整当前证明。worker 没有 vault 数值版本，使用全局 opaque version ID 精确对应控制面的版本记录，不新造数值。

本切片不改 ticket v1、Control wire 签票、路由发布、账号选择、CCMAX UI/计费、不提供 lease 写接口。生产 assembly、ticket scope/版本绑定与撤销、双层续租、B3 registry、B4恢复/结果保留、gateway/CLI、真实 DB/Redis/Docker/1000连接/24h仍开放。

## 本地验收

- 原子版本/代理切换；未提交、取消、failed ack、drain/在途 commit 竞争及秘密不进入 Health 响应。
- 真实本地 worker gRPC 的 SecureActivate→合成 vault ack→带 challenge Health→控制面 loaded-state 核对；legacy/fake/missing state/跨账号/旧版本/旧代理/错 challenge/不健康模式拒绝。
- 前后两次 authority 之间的版本/代理/session/epoch/generation/镜像变化与 lease 失效、依赖超时/取消均拒绝；不写数据库、不续租。
- SQL 只读语句/参数合同、Memory 一致性；离线生成重复无diff、全模块 race/vet、独立交叉review和阶段 Git 提交。sqlmock、Memory和合成票据不替代真实生产依赖证据。

## 实现中的边界细化

- credential version ID 沿用有界 opaque transport ID，而非额外限定 UUID；实际 Vault 通常生成 UUID，worker 合同不另造限制。只接受现有 oauth/setup_token/api_key 三类已归一化 auth type。
- 原始 authority 的 durable lease、观察及节点时间共同限制 Health 和后续读取的 deadline；后来的心跳不能延长本次核对。Receipt 的时间在 RPC 前冻结，revision 在回包后立即复制，不保留可变响应引用。
- Health 在入站及快照读取后检查取消；commit ack 失败或取消保留此前有效 active state，Drain 则立即隐藏所有 active state。pending 保留用于同一合成 lease 的安全重试，不使 worker 未经 ack 就加载新版本。
- 本地组合测试使用实际 Vault.Rotate/Fake KMS 的版本插入和切换，验证控制面 version number 与本地 activation revision 可以不同。临时测试签票器不冒充 B2b2 受认证签票服务；B1 初始宿主健康观察在该测试中是合成结果，不冒充真实 provider 检查。
