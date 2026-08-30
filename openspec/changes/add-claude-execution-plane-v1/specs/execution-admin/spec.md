## ADDED Requirements

### Requirement: 管理端必须展示执行面真实状态
V1 SHALL 在 `ccmax-manager/web` 使用 React + TypeScript + Vite 与 shadcn/ui 提供中文节点、槽位、镜像、会话、队列、租约、代理、模式健康和异步生命周期页面。UI MUST 提供中性色纯白/暗黑双主题，MUST 展示 desired 与 actual state，不得把 provisioning 显示为 ready。生产构建 MUST 由 CCMAX Go 服务内嵌提供且不得要求独立 Node 运行时。

#### Scenario: 节点失联
- **WHEN** 节点心跳超过阈值
- **THEN** 页面 MUST 展示离线时间、受影响 slots 和 fencing/rebuild 进度

### Requirement: 管理端必须支持受控批量操作
系统 SHALL 支持批量 drain、recreate、migrate、archive、soft delete、restore 和 purge，并 MUST 返回逐项状态。破坏性操作 MUST 有明确确认边界。

#### Scenario: 批量操作部分失败
- **WHEN** 部分账号无法验证或重建
- **THEN** 成功项 MUST 可独立提交
- **THEN** 失败项 MUST 保留安全状态并显示脱敏原因

### Requirement: 脱敏正文必须加密存储并受独立权限控制
请求/响应正文 SHALL 脱敏、压缩、KMS 加密后存腾讯云 COS，默认保留 3 天且可配置 N 天，单侧默认 2 MiB。认证头、Token、Cookie、PKCE 和代理密码 MUST 永不落盘。

#### Scenario: 管理员查看正文
- **WHEN** 管理员拥有正文查看权限
- **THEN** 系统 MUST 通过受审计后端读取并解密对象
- **THEN** 每次查看、搜索或下载 MUST 写管理审计

#### Scenario: 正文超过上限
- **WHEN** 脱敏正文单侧超过 2 MiB 或配置上限
- **THEN** 系统 MUST 只保存结构摘要、大小和哈希

### Requirement: 执行面必须可观测
orchestrator、host-agent、worker 和 CCMAX SHALL 提供 Prometheus 指标、结构化日志和可选 OTLP，并预留 Webhook 告警。

#### Scenario: billing 400 激增
- **WHEN** cli_native billing_blocked 事件超过阈值
- **THEN** 系统 MUST 产生按模式聚合的告警而不包含账号凭证或正文

### Requirement: 镜像发布必须固定版本、canary 并可回滚
系统 SHALL 使用 TCR 不可变 digest，关闭 Claude CLI 自动更新，先 canary 再逐批 drain rollout，并支持回滚到上一个 approved digest。

#### Scenario: canary 协议测试失败
- **WHEN** 新 CLI 镜像的 stream-json、tool 或 usage 验证失败
- **THEN** rollout MUST 自动暂停
- **THEN** 未升级 slot MUST 保持旧 digest

### Requirement: 生产部署必须经过容量和安全门禁
系统 SHALL 使用 fake Claude/fake Anthropic 完成 1000 长连接、故障注入和 soak，再使用少量授权账号 canary。远端节点修改 MUST 在代码验收后获得单独批准。

#### Scenario: PRD 确认后尚未获得部署批准
- **WHEN** 本地开发和测试仍在进行
- **THEN** 自动化 MUST NOT 安装 Docker、WireGuard 或修改 `43.172.83.39` 防火墙
