## ADDED Requirements

### Requirement: 系统必须支持两种独立执行模式
账号 SHALL 配置 allowed/preferred modes，分组 SHALL 配置 `auto|cli_only|api_only`。普通客户端 MUST NOT 自由指定内部执行模式。

#### Scenario: auto 请求包含 CLI 无法无损表达的能力
- **WHEN** capability matrix 判定请求包含不受支持字段或历史结构
- **THEN** 系统 MUST 选择健康的 oauth_api 路径
- **THEN** 系统 MUST NOT 静默删除字段后继续 CLI

#### Scenario: 管理员强制 CLI
- **WHEN** 有权限的调试请求强制 cli_native 且能力不支持
- **THEN** 系统 MUST 返回 Anthropic 格式 400 `unsupported_feature`

### Requirement: 两种模式必须使用独立转换链路
`cli_native` MUST 不经过 CCMAX 的 Claude identity、billing 或 metadata 注入；`oauth_api` MUST 保留现有 CCMAX 转换和 raw passthrough 语义。

#### Scenario: cli_native 执行普通客户端请求
- **WHEN** CCMAX 选择 cli_native
- **THEN** 客户端 system MUST 作为经过校验的附加指令交给官方 CLI
- **THEN** CCMAX MUST NOT 再加入会与 CLI 默认内容重复的 identity/billing block

### Requirement: CLI 必须使用安全的每回合进程和可恢复会话
每个模型回合 SHALL 使用 `claude -p`，后续回合使用 `--resume`。CLI MUST 使用 safe mode、strict MCP、禁用全部 built-in tools、关闭自动更新，session MUST 存于 tmpfs 并在默认 15 分钟空闲后销毁。

#### Scenario: 客户端改变 max_tokens 或 thinking
- **WHEN** 新回合提供可映射的 max_tokens/thinking
- **THEN** worker MUST 为该回合设置对应官方 CLI 环境变量/effort
- **THEN** 不可精确映射的组合 MUST 进入 API 模式或返回 unsupported

### Requirement: MCP Bridge 必须把工具执行留给客户端
Bridge SHALL 支持一个回合多个并行 tool_use，将它们返回客户端并挂起 CLI/MCP 调用，直到同 session 的 tool_result 到达或 15 分钟超时。

#### Scenario: 客户端提交多个 tool_result
- **WHEN** tool_result ID 与当前挂起调用匹配且未消费
- **THEN** worker MUST 把结果交回对应 MCP 调用并继续 CLI
- **THEN** 重复、未知、过期结果 MUST 返回稳定 session conflict

### Requirement: 并发和排队必须全局有界且公平
默认限制 SHALL 为 cli=1、api=3、total=3。分组 queue 模式 MUST 按 API Key 公平轮转，默认等待 120 秒，全局 outstanding 最大 1000；reject 模式 MUST 立即拒绝。

#### Scenario: 单个 API Key 尝试占满队列
- **WHEN** 该 Key 达到独立队列上限
- **THEN** 新请求 MUST 被拒绝而不消耗其他 Key 的队列份额

#### Scenario: 队列满或超时
- **WHEN** 请求不能在限制内取得容量
- **THEN** 系统 MUST 返回 Anthropic 格式 529 和 Retry-After

### Requirement: 协议响应与错误必须保持兼容
系统 SHALL 支持 Messages stream/nonstream、count_tokens、models 和现有 Chat Completions 转换。usage MUST 来自真实 CLI/上游事件，不得伪造。

#### Scenario: 客户端在输出前断开
- **WHEN** 请求仍在排队或执行且未产生 tool_use 挂起状态
- **THEN** 系统 MUST 取消 Redis、gRPC、worker 和 CLI 工作

#### Scenario: 上游在 SSE 输出后失败
- **WHEN** 已经发送客户端可见事件
- **THEN** 系统 MUST 不自动换账号或模式重试

### Requirement: 模式健康必须隔离
cli_native 与 oauth_api SHALL 分别记录健康。Agent SDK billing 400 MUST 只阻断受影响模式，并 MUST 避免在账号池中反复重试同类全局错误。

#### Scenario: cli_native 返回 extra usage 错误
- **WHEN** 官方 CLI 返回第三方/Agent SDK 额度不足错误
- **THEN** 系统 MUST 标记 cli_native billing_blocked
- **THEN** 账号的健康 oauth_api 能力 MUST 不被无条件判死
