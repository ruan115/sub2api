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
