# add-claude-execution-plane-v1

为 CCMAX 增加一个账号一个 Docker `ExecutionSlot` 的隔离执行面，支持 `cli_native` 与 `oauth_api` 双模式、多节点调度、固定代理出口、KMS 凭证保险库、MCP Tool Bridge 和完整运行管理。

权威产品范围见 [`docs/prd/claude-execution-plane-v1.md`](../../../docs/prd/claude-execution-plane-v1.md)。

阅读顺序：`proposal.md` → `source-baseline.md` → `design.md` → `specs/*/spec.md` → `tasks.md` → `verification.md`。
