## Why

用户要求在第一阶段恢复底座后继续开发。本切片将目录规则落到可运行的本地管理演示与fake执行传输，仍不改已有5.5c或生产服务。

## What Changes

- 按业务模块创建独立Go恢复域与仅loopback演示命令，提供合成身份、会话、权限和三类列表。
- 创建独立React子应用，按identity/users/apikeys/providers拆模块，支持mock与本地Go演示适配器。
- isthmus添加fake TurnEngine及HTTP/WS适配，保留单一核心、流式、取消和有界资源。
- 显式记录来源缺口，增加集成测试、模块说明与离线验收入口。

## Non-Goals

远端本轮SSH在KEX阶段关闭，HTTP/TLS静态资源读取亦失败，因此不得把内部演示DTO当作旧HTTP契约。旧契约继续discovered；完整DB/Redis接入、React视觉精确复刻、gRPC、CLI、OAuth、账务、迁移和部署均未完成。不会创建真实账户、调用模型/支付或更改SSH/防火墙。
