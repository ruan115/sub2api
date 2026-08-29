## ADDED Requirements

### Requirement: 每个有效账号必须对应一个稳定隔离槽位
系统 SHALL 为每个启用新执行面的账号维护一个稳定 `ExecutionSlot`。V1 MUST 使用 Docker provider 实现槽位，但 CCMAX 和业务数据库 MUST NOT 以 container ID 作为业务标识。

#### Scenario: 新账号完成上号
- **WHEN** 凭证与固定代理验证成功且账号进入 provisioning
- **THEN** orchestrator MUST 创建或复用该账号唯一的 slot ID
- **THEN** 只有 worker 探活成功后账号才能变为 schedulable

#### Scenario: Docker 容器被人工删除
- **WHEN** reconciliation 发现期望为 ready 的 slot 缺少容器
- **THEN** orchestrator MUST 使用同一 slot ID 和当前期望配置重建容器
- **THEN** CCMAX MUST NOT 因 container ID 变化创建新账号或丢失历史

### Requirement: 节点调度必须资源感知且保持账号稳定
系统 SHALL 按节点可用 CPU、内存、slot/CLI/API 容量、标签、区域、代理约束和镜像能力执行 least-loaded + spread 放置。账号 MUST 保持在原节点，除非故障、drain、迁移或资源不足。

#### Scenario: 首台节点达到容量
- **WHEN** `43.172.83.39` 已达到 20 slots、4 CLI 或 12 API 的相应上限
- **THEN** orchestrator MUST 不再向该节点分配超出限制的工作
- **THEN** 请求 MUST 排队、拒绝或落到其他合格节点，而不是突破硬限制

### Requirement: execution epoch 必须防止账号双活
系统 SHALL 为每次 slot assignment 维护单调递增 epoch，并通过短执行租约和 egress fencing 阻止旧 epoch 发起或维持上游连接。

#### Scenario: 节点失联后账号重建
- **WHEN** host-agent 45 秒未续租且达到 90 秒重建窗口
- **THEN** orchestrator MAY 在其他节点以更高 epoch 重建 slot
- **THEN** 旧节点恢复后 MUST 无法通过 egress 使用旧 epoch

#### Scenario: Redis 或控制面不可用
- **WHEN** host-agent 无法续租
- **THEN** 节点 MUST 停止接受新执行并在租约到期后关闭受保护连接
- **THEN** 系统 MUST NOT fail-open 形成双活

### Requirement: worker 网络必须强制固定代理出口
worker 和 Claude CLI MUST 只能连接 host-agent egress gateway。gateway MUST 根据 slot 与有效 epoch 选择账号唯一固定代理，并支持 HTTP、HTTPS 和 SOCKS5。

#### Scenario: worker 尝试直连公网
- **WHEN** worker 进程尝试绕过本机 egress 访问公网
- **THEN** Docker 网络策略 MUST 拒绝连接

#### Scenario: 账号使用 SOCKS5 代理
- **WHEN** CLI 只支持 HTTP/HTTPS proxy 而账号绑定 SOCKS5
- **THEN** worker MUST 使用本机 HTTP CONNECT
- **THEN** host-agent MUST 把该连接转换并发送到固定 SOCKS5 代理

### Requirement: Docker worker 必须使用最小权限基线
worker MUST 非 root、只读根文件系统、capabilities 全部移除、启用 no-new-privileges/seccomp/AppArmor、使用 tmpfs，且 MUST NOT 挂载 Docker Socket 或宿主目录。

#### Scenario: 检查已创建容器
- **WHEN** host-agent 完成 slot 创建
- **THEN** provider health check MUST 验证安全参数和资源限制
- **THEN** 不满足策略的容器 MUST 不得进入 ready
