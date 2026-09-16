# Worker loaded-state proof

一次主动 Health 的加载元数据核对，不是执行票、route、可缓存的 serving 状态或续租许可。

`Verifier.Check` 先读取 secret-free `WorkerReadinessBinding`，验证活动控制会话与独立 lease；然后以每次新随机 challenge 调用 worker Health，核对身份、镜像、active version ID、auth type、proxy lease、worker-local revision 和请求模式；最后重新读取权威元数据并复查会话、lease 与原始期限。任何绑定变化、旧回包、缺少 loaded_state、超时或取消均失败，且不透传依赖原始错误。

- 默认整次核对上限 2 秒，最大 10 秒；观察最大年龄 45 秒。Health 及后续 I/O 还受最初的 durable lease/观察/节点新鲜度期限限制，后来的心跳或续期不能延长本次核对权限。
- `CheckedAt` 在 worker 调用前冻结。`ActivationRevision` 是本实例成功发布次数，不是 Vault 的 version number；Receipt 没有 Ready 或有效期字段，不可用于直接签发业务票或续租。
- SQL 只读投影不读取 credential envelope、hint、KMS 信息、代理地址或密码；Memory 用同一个读锁核对当前指针。存储预期状态不等于 worker 真正加载状态。
- HealthReader 必须后续接入受认证 health-only 签票及 B3 existing-only 私网 registry；不能接受任意 worker URL 或创建实例。本模块没有生产连接实现、签票或 lease 写接口。
- 首次 health/activation 不能依赖已激活版本的这个投影，否则会形成循环依赖。业务 proof 与受限首次探测分开授权。
- challenge 防止无意复用旧响应，不是硬件远程证明，不能证明恶意宿主可信；也不能证明上游凭据有效、额度充足、网络可达或 CLI 已实现。

本地集成使用实际 TLS NodeControl、worker SecureActivate/Health gRPC、临时签票密钥、Fake KMS、Memory Vault/lease 和合成凭据。没有真实模型、数据库、Redis、Docker/VM 或生产接线验收。设计见 [B2b1](../../../openspec/changes/complete-ccmax-execution-acceptance/b2b1-design.md)。
