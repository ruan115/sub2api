# 验收收尾台账

总计划：[CCMAX execution acceptance v2](../../../docs/plans/ccmax-execution-acceptance-v2.md)。

## A：HTTP worker 增量转发

- [x] A0 先记录范围、目录、合同、验收与线上禁止操作边界。
- [ ] A1 独立有界 HTTP 响应转发模块，实际 worker 不再整包缓冲 SSE。
- [ ] A2 独立 JSON/SSE usage 白名单观察、分块/增量/错误完整性测试。
- [ ] A3 实际 worker 执行器回环集成，覆盖首段、累计大流、背压、取消和无伪完成。
- [ ] A4 独立 review、主代理 race/vet 回归、阶段 Git 提交与实证。

## 后续（不自动视为 A 的完成内容）

- [ ] B 可信状态、签票、runtime registry、host-agent 生产级装配与安全失败测试。
- [ ] C CCMAX gateway 新数据面接线、协议/usage/错误/取消闭环，默认关闭。
- [ ] D CLI/isthmus/MCP/会话与可靠停机。
- [ ] E Token 刷新/版本切换/生命周期/脱敏运维。
- [ ] F 当前代码整链 Docker/依赖故障/兼容/1000连接/24h本地验收。
- [ ] G 本地制品与恢复验收，真实 canary 和部署需要另行授权。

不可在 A 完成后勾选上述 B–G 或原执行面 Phase 6–11。每一项以实际实现与运行证据为准。
