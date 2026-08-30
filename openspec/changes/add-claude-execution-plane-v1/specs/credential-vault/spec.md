## ADDED Requirements

### Requirement: 账号凭证必须使用 KMS 信封加密
系统 SHALL 为每个 credential version 生成独立 AES-256-GCM DEK，使用绑定 account/version/auth type 的 AAD 加密数据，并使用腾讯云 KMS CMK 加密 DEK。worker MUST NOT 拥有 KMS 权限。

#### Scenario: 保存新凭证版本
- **WHEN** 上号或刷新产生新 AT/RT/API credential
- **THEN** orchestrator MUST 在数据库只保存 ciphertext、encrypted DEK、AAD metadata 和 hint
- **THEN** 日志、错误和管理 API MUST 不包含明文

### Requirement: worker 只能通过一次性凭证租约取得明文
凭证租约 SHALL 绑定 account、slot、epoch、credential version 和短有效期，并 MUST 只能成功领取一次。明文只允许存在于 orchestrator 解密内存与 worker 内存/tmpfs。

#### Scenario: 重放凭证租约
- **WHEN** 同一个租约 token 被第二次提交或 epoch 已失效
- **THEN** orchestrator MUST 拒绝请求
- **THEN** 系统 MUST 记录不含凭证内容的安全事件

### Requirement: 临时上号材料必须使用短期 KMS intent
CCMAX SHALL 通过内部认证的 onboarding intake 提交临时上号材料。orchestrator MUST 立即将材料封装为默认 30 分钟有效的 KMS envelope，并以 AAD 绑定 intent ID、account、目标 generation、source type 和 auth type。CCMAX 账号事务、runtime outbox、日志、错误和审计 MUST 只引用 opaque `onboarding_intent_id`，不得保存明文或可用于猜测明文的摘要。

#### Scenario: 幂等创建和 generation 隔离
- **WHEN** CCMAX 使用相同幂等键重试完全相同的 account、目标 generation、source type 和 auth type
- **THEN** orchestrator MUST 返回同一个 opaque intent ID
- **WHEN** 绑定字段不同或账号 generation 已推进
- **THEN** 系统 MUST 拒绝复用该 intent 且不得推进账号/outbox 状态

#### Scenario: claim、重放和到期
- **WHEN** provisioning owner claim 未过期 intent
- **THEN** 只有相同 owner 可在 claim 有效期内幂等重新解密
- **THEN** 其他 owner、错误 account/generation 或已过期 intent MUST fail closed
- **WHEN** claim 到期但 intent 未到期且尚未 consumed
- **THEN** 新 owner MAY 重新 claim 以恢复 provisioning

#### Scenario: 激活完成后消费 intent
- **WHEN** worker 返回的 Vault version ACK、activation command 结果和 slot/epoch/image 健康状态全部精确匹配
- **THEN** orchestrator MUST 幂等标记 intent consumed
- **WHEN** 完成响应丢失后相同 owner 重试 complete
- **THEN** 系统 MUST 返回成功且不得重复产生 credential version
- **WHEN** 激活失败、超时或绑定不匹配
- **THEN** intent MUST NOT 被提前标记 consumed

#### Scenario: CCMAX 通过独立 mTLS 身份提交材料
- **WHEN** CCMAX 创建或重新上号 execution account
- **THEN** intake MUST 只接受内部 CA 签发的预期 CCMAX service identity，且 MUST NOT 接受 host-agent node identity
- **THEN** CCMAX 和 orchestrator MUST 在 RPC 完成、拒绝或失败后清零各自持有的 Session Key/code/PKCE/API Key/Cookie bytes
- **THEN** receipt 的 account、generation、source、auth type、expiry 和 opaque intent ID 任一不匹配时 CCMAX MUST 不提交账号/outbox 事务

#### Scenario: 显式单账号 execution onboarding
- **WHEN** 管理员开启 `execution_onboarding` 并提供固定代理与稳定 `Idempotency-Key`
- **THEN** CCMAX MUST 创建不可调度的 pending 账号且保持 `credentials_json={}`
- **THEN** intake 成功后 CCMAX MUST 只把 opaque intent ID 写入 generation-fenced outbox
- **WHEN** intake 失败
- **THEN** pending 账号和代理预约 MUST 保留供相同幂等键重试

#### Scenario: intake 响应丢失与精确 receipt 恢复
- **WHEN** Create 已提交 intent 但 CCMAX 未收到响应
- **THEN** CCMAX MUST 使用原内部 intake key、account、generation、source 和 auth type 执行 lookup-only Recover
- **THEN** Recover MUST NOT 解密 intent 或返回凭证材料
- **THEN** NotFound、Aborted、FailedPrecondition 和 Unavailable MUST 分别表示未创建、精确 intent 过期、身份/生命周期冲突和持久层不可用
- **WHEN** 旧 attempt 响应在新 attempt 建立后晚到
- **THEN** exact CAS 和 immutable identity/fingerprint fence MUST 阻止旧 receipt 写入新 attempt
- **THEN** Create MUST 在返回前清零材料，且系统 MUST NOT 因晚到响应二次 Create 已消费的材料

#### Scenario: 创建 fingerprint 与按账号恢复 canonical submission
- **WHEN** 相同外部幂等键重放账号创建
- **THEN** CCMAX MUST 校验只覆盖规范化非秘密配置的版本化 canonical fingerprint
- **THEN** 凭证、OAuth/PKCE、代理文本/密码、名称、备注和自由文本 MUST NOT 进入 fingerprint
- **WHEN** 外部 key 丢失但 pending 账号已持久化
- **THEN** 失败响应 MUST 返回 pending account ID 和 resume URL
- **THEN** status/resume MUST 由服务端按账号找到原 canonical external/internal key，MUST NOT 接受客户端用新 key 接管
- **THEN** resume MUST 在 RPC 前校验 account、generation、proxy、migration、runtime lifecycle 和 create fingerprint，并在事务结束后才调用 intake

#### Scenario: orchestrator 重启后继续安全上号
- **WHEN** orchestrator 在 activation 已下发后重启
- **THEN** 新进程 MUST 从 KMS 或受保护持久层恢复同一个 rotation recipient 私钥，且 MUST 清零恢复输入缓冲区
- **THEN** durable runner MUST 有界扫描非终态 workflow，并以稳定 command ID 每轮最多推进一步
- **THEN** 单个 workflow 失败 MUST NOT 阻塞同批其他 workflow
- **THEN** 缺少 KMS、recipient、Vault、observer、credential sink、NodeControl 或 intake 任一依赖时 orchestrator MUST NOT 注册部分可用的 credential RPC

#### Scenario: production runtime 加载长期信任材料
- **WHEN** 管理员显式启用 orchestrator production runtime
- **THEN** MySQL MUST 使用 TCP、UTC 时间解析和服务端证书校验，配置日志/JSON MUST NOT 输出 DSN
- **THEN** CA/server 私钥和 rotation envelope MUST 来自绝对路径、非 symlink、owner-only regular file，且 server certificate MUST 匹配同一 CA 与显式 server name
- **THEN** rotation 私钥 MUST 使用独立于账号 credential 的 service-key AAD，经 KMS envelope 解封，并 MUST NOT 支持 raw key 环境变量或文件
- **THEN** 腾讯 KMS 凭证 MUST 仅来自 CVM instance role；任一校验或依赖失败时内部 credential RPC MUST NOT 开始监听
- **THEN** runtime MUST 在监听前连接 verified-TLS MySQL 并只读确认所有必需 schema，且 MUST NOT 在进程启动时自动迁移
- **THEN** health readiness 与 TLS 1.3 credential RPC MUST 只在完整组件图构造成功后共同启动，并共享关闭信号

#### Scenario: worker 结果安全回填 CCMAX
- **WHEN** worker 的 normalized credential 已由 Vault 提交为精确 version
- **THEN** orchestrator MAY 投影邮箱、organization/account ID、scope、订阅、rate-limit tier 和过期时间，但 MUST NOT 投影 AT/RT、API Key、Cookie、Session Key 或源材料
- **THEN** 投影 MUST 绑定 workflow、intent、account、generation、slot/epoch、credential/proxy lease 和 credential version 幂等持久化
- **WHEN** 投影写入失败或 workflow 尚未 completed
- **THEN** worker MUST NOT 获得假成功 ACK，且 CCMAX MUST NOT 读取结果或把账号标记 ready
- **WHEN** CCMAX 使用错误 intent、account 或 generation 查询
- **THEN** mTLS result RPC MUST fail closed 且不得返回其他账号的投影

### Requirement: 上号和刷新必须使用账号固定出口
Session Key 交换、OAuth code、Setup Token、Cookie/API Key 验证、配额查询与 Token 刷新 MUST 在账号 worker 中通过固定代理执行。

#### Scenario: Session Key 上号
- **WHEN** 管理员提交有效 Claude Session Key
- **THEN** 系统 MUST 先预约代理并创建临时 slot
- **THEN** worker MUST 经该代理完成组织查询、授权码申请和 AT/RT 交换
- **THEN** Session Key 与临时 OAuth/PKCE 数据 MUST 在成功、失败或到期后擦除

#### Scenario: 普通 Anthropic API Key
- **WHEN** 输入为 API Key 而不是 Session Key
- **THEN** 系统 MUST 不尝试交换 AT/RT
- **THEN** worker MUST 经固定代理验证并只允许 API 执行模式

### Requirement: Token 轮换必须原子切换版本
worker 刷新后 MUST 先把新版本信封加密写入 vault，验证提交成功后才切换 active version。并发 Token 操作 MUST 串行化。

#### Scenario: Vault 提交后响应丢失或 orchestrator 崩溃
- **WHEN** 相同 credential lease 的 worker commit 在 version 已提交后重试
- **THEN** Vault MUST 通过稳定 rotation operation 返回原 credential version
- **THEN** version 插入、active 切换、旧租约撤销和 operation 映射 MUST 原子提交
- **THEN** operation 映射 MUST NOT 保存凭证明文或明文派生摘要

#### Scenario: rotation authorizer 重启恢复与代理撤销
- **WHEN** worker 首次提交认证的 canonical credential frame
- **THEN** authorizer MUST 在调用 Vault 前按 credential lease 持久化材料摘要并校验精确 workflow、健康 slot/epoch/generation 和当前 execution/proxy lease
- **WHEN** 相同 credential lease 使用不同材料摘要重放，或 opaque proxy lease 已撤销
- **THEN** 系统 MUST 在调用 Vault 前拒绝
- **WHEN** version 已完成记账后 ACK 丢失
- **THEN** 只读重放 MUST 返回原 version，且 MUST NOT 重新调用代理校验或 Vault

### Requirement: 固定代理预约必须通过 trusted authority 投影
CCMAX MUST 在 onboarding generation 事务内先持久化固定代理预约并发出 secret-free grant，
execution-plane MUST 仅依据该 grant 为精确健康 slot epoch 建立 proxy lease。

#### Scenario: grant 与 onboarding 原子排队
- **WHEN** CCMAX 接受一个带固定代理的 execution onboarding receipt
- **THEN** proxy reservation grant 与账号 generation、onboarding event MUST 在同一事务提交
- **THEN** grant event MUST 排在 onboarding event 之前，且 payload MUST 只包含 opaque reservation、binding ID 和 revision
- **THEN** 任一后续 CAS 或事务步骤失败时，账号 generation、reservation 与两个 event MUST 全部回滚

#### Scenario: 旧 generation 撤销与新预约隔离
- **WHEN** 账号已存在更新 generation 的 active reservation，随后重放旧 reservation revoke
- **THEN** 系统 MUST 只撤销精确旧 reservation 并返回原 revoke event
- **THEN** 更新 generation 的 reservation MUST 保持 active

#### Scenario: provenance 或 assignment generation 缺失
- **WHEN** 升级前 proxy lease 缺少 trusted reservation provenance，或 assignment 未固化 desired generation
- **THEN** proxy lease grant/validation MUST fail closed
- **THEN** migration MUST NOT 删除旧 lease 或推测性回填授权

#### Scenario: runtime proxy 已占用或正在被测试
- **WHEN** proxy 存在 active reservation，或已由 nonlegacy onboarding account 引用
- **THEN** allocator、proxy/pool identity mutation 和旧 lifecycle API MUST fail closed
- **THEN** 管理端 proxy test MUST 在任何外部网络 I/O 前完成数据库占用检查并与 grant 串行

#### Scenario: healthy slot 原子启动 workflow
- **WHEN** pending intent、ready slot、同 generation/image 的 fresh healthy assignment、live execution lease 与 trusted reservation 全部匹配
- **THEN** 系统 MUST 在一个事务内创建精确 proxy lease 与至多一个 intent workflow
- **THEN** exact replay MUST 返回原 binding，且 MUST NOT 因后来 health 变化创建第二个 workflow
- **WHEN** 任何 authority 在 intent claim/decrypt 前或打开后 dispatch 前失效
- **THEN** 系统 MUST 禁止 activation dispatch，并 MUST 擦除已经打开的输入

#### Scenario: 新版本保存失败
- **WHEN** KMS 或数据库在轮换提交期间失败
- **THEN** active credential version MUST 保持旧值
- **THEN** CCMAX MUST 不接收伪成功状态

### Requirement: 现有明文凭证必须按账号单向迁移
系统 SHALL 支持 canary 和批量迁移。迁移账号在密文可解密验证成功后 MUST 清空原 `credentials_json`，且 MUST NOT 长期双写明文与密文。

#### Scenario: canary 迁移失败
- **WHEN** KMS、字段校验或 worker 验证失败
- **THEN** 该账号 MUST 保持 legacy 且不可标记迁移完成
- **THEN** 其他账号迁移 MUST 可暂停而不受影响
