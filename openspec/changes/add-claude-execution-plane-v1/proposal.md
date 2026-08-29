## Why

CCMAX 已有账号池、代理、OAuth、调度、排队、会话粘性和计费，但账号凭证与上游请求仍由控制面直接处理。现有实现无法为每个账号提供独立 Linux/Claude Code 环境，也无法把登录、刷新和推理强制绑定到同一出口，更缺少容器状态机、多节点防双活和可灰度的 CLI/API 双执行模式。

将账号直接映射为云服务器不具备成本可行性。V1 需要把 UI 中的“VM”标准化为逻辑 `ExecutionSlot`：一个账号一个 Docker 槽位，多个槽位共享 Ubuntu worker 节点，同时保留未来 KVM/Firecracker provider 边界。

## What Changes

- 新增独立 Go module `execution-plane`，包含 `orchestrator`、`host-agent` 和 `worker` 三个命令。
- 新增 protobuf/gRPC 控制面与数据面，流量通过 WireGuard 私网和 mTLS。
- 新增 Docker `ExecutionProvider`；业务层不保存 Docker container ID。
- 新增 transactional Outbox、期望/实际状态 reconciliation、节点心跳、资源感知放置、drain 和镜像灰度。
- 新增 execution epoch、短租约和 egress fencing，阻止同一账号跨节点双活。
- 新增腾讯云 KMS 信封加密凭证保险库；CCMAX 不接触迁移账号的明文凭证。
- 新增 host-agent egress gateway，把 worker 的 HTTP CONNECT 固定转到账号绑定的 HTTP/HTTPS/SOCKS5 代理，并禁止 worker 直连公网。
- 新增 `cli_native` 与 `oauth_api` 双模式、能力矩阵、模式独立健康与三层并发限制。
- 新增 Claude Code stream-json adapter、15 分钟 tmpfs session、`--resume` 和 MCP Tool Bridge。
- 扩展现有 `/v1/messages`、`count_tokens`、models 和 `/v1/chat/completions` 链路，保持 Anthropic 错误 envelope。
- 新增节点、槽位、镜像、会话、队列、租约、正文审计和批量生命周期管理 UI；V1 页面只提供中文。
- 新增脱敏正文 COS 存储，KMS 加密、独立 RBAC、默认保留 3 天。
- 新增 TCR 精确镜像、canary、rollback、Ansible 节点安装和容量压测。

## Capabilities

### New Capabilities

- `execution-runtime`: 定义节点、槽位、provider、心跳、epoch、租约、Docker 安全、固定出口和故障转移。
- `credential-vault`: 定义 KMS 信封加密、一次性凭证租约、固定出口上号、Token 轮换和单向迁移。
- `execution-dispatch`: 定义双执行模式、能力矩阵、并发、公平队列、session、MCP 工具循环、协议和错误语义。
- `execution-lifecycle`: 定义上号、drain、归档、软删除、批量恢复、彻底清除、代理/凭证切换和 Outbox 一致性。
- `execution-admin`: 定义中文管理页面、RBAC 正文审计、可观测性、镜像发布、压测和灾备门禁。

### Modified Capabilities

- CCMAX 账号调度：新增 runtime ready 与模式健康约束。
- CCMAX 账号授权：迁移账号的交换和刷新从控制面移动到 worker。
- CCMAX capacity queue：从进程内等待状态扩展为 Redis 分布式公平队列。
- CCMAX 归档/删除：接入统一 drain、slot 销毁和可恢复回收站。

## Impact

- **代码**：新增 `execution-plane/`；CCMAX 增加执行客户端、能力路由、Outbox、生命周期、审计和 UI。
- **数据库**：CCMAX MySQL 增量字段和 Outbox；同一 MySQL 服务新增独立 `worker_runtime` 数据库，无跨库外键。
- **运行依赖**：生产 Redis、腾讯云 KMS/COS/TCR、WireGuard、Docker Engine、内部 CA。
- **外部 API**：保留现有入口；只有启用新执行面的账号进入新链路。强制 CLI 遇到不支持能力时新增明确 400。
- **安全**：worker 无宿主挂载/Docker Socket/公网直连；凭证只在 orchestrator 解密内存与 worker 内存/tmpfs 短暂存在。
- **迁移**：按账号 canary 单向迁移，不双写长期明文；未迁账号继续 legacy。
- **部署**：代码验收后仍需单独批准，才可修改 `43.172.83.39`。

## Non-Goals

- 邮箱/密码自动登录、Kubernetes、真实 VM/Firecracker、云资源自动扩容。
- OpenAI Responses/Batch、跨地域双活、公开转售订阅 OAuth。
- 用 UA、metadata 或 system 注入改变 Anthropic 计费归类。
- 让不能被 CLI 无损表达的请求静默降级或丢字段。
