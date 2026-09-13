# 第一阶段设计

目录与依赖以 `recovery/docs/architecture.md` ADR-001 为准。用户已明确授权开始本阶段，生产变更不在授权内。

## Evidence

保全输入是有来源、SHA-256、大小和相对路径的显式manifest。校验全部目标后才进行复制；输出新建且owner-only，不覆盖。拒绝绝对/parent traversal/符号链接和敏感文件；不能遍历复制服务器public或home全目录。

工具只处理字节，不执行或import目标。缺少某个原件或hash错误必须失败，不伪造成功。元数据缺项与外部私有原件缺失分别报告。

## Workspace

读取Git基线、tracked diff、untracked内容，输出到明确的仓库外私有目录，禁止覆盖、递归包含自身或自动恢复。保留分支/HEAD/状态和每个文件hash。snapshot不是commit，也不是异机备份。校验和恢复流程独立，后续恢复需显式指定干净工作区。

## Contracts

每个业务模块独立文件夹；每条合同有稳定ID、owner、来源、已知信息和unknowns。状态至少区分discovered与specified；不能因validator通过将合同标成业务verified。路径模板不是完整API规范，不自动推断方法、鉴权、分页或响应结构。

本阶段验证结构、重复ID/路径冲突、来源完整、未决项与状态一致；未来再绑定golden fixture与实现证据。

## Runtime protocol

原Messages proto只有一个权威副本。WS frame codec是纯函数，带方向/tag/长度校验；合成测试覆盖客户端/服务器帧、未知tag、空帧、长度边界、payload保真。它不等于完整WS连接状态机。

## Verification

Python工具只用标准库，测试不需要生产、数据库、网络或外部secret。Bun只运行本轮新写的纯协议代码，不运行恢复bundle。现有execution-plane库执行独立回归。

## 与旧 execution-plane 的依赖

旧进度仍以 `openspec/changes/add-claude-execution-plane-v1/` 为准，不能用本change的通过项替代旧5.5c/5.6–Phase11的完成证据。

- 5.6（刷新/原子credential version）与5.7（明文迁移）仍是恢复核心接入真实账号的前置门禁。
- Phase6的gateway dispatch/实际gRPC数据面、Phase7的CLI/MCP、Phase8的统一生命周期仍需单独实现和验证。
- Phase9的审计/UI、Phase10的基础设施/发布、Phase11的验证证据需与恢复总计划R7–R10汇合。
- 当前pure codec不会启动执行槽、不更新账号状态，不使 `execution_onboarding` 或 `migrated` 变得可启用。
