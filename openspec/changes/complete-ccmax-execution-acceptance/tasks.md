# 验收收尾台账

总计划：[CCMAX execution acceptance v2](../../../docs/plans/ccmax-execution-acceptance-v2.md)。

## A：HTTP worker 增量转发

- [x] A0 先记录范围、目录、合同、验收与线上禁止操作边界。
- [x] A1 独立有界 HTTP 响应转发模块，实际 worker 不再整包缓冲 SSE。
- [x] A2 独立 JSON/SSE usage 白名单观察、分块/增量/错误完整性测试。
- [x] A3 实际 worker 执行器回环集成，覆盖首段、累计大流、背压、取消和无伪完成（背压为模块测试，非整链容量验收）。
- [x] A4 独立 review、主代理 race/vet 回归、阶段 Git 提交与实证。

## 后续（不自动视为 A 的完成内容）

- [ ] B 可信状态、签票、runtime registry、host-agent 生产级装配与安全失败测试。
  - [x] B1 [会话绑定的持久化观察与只读权威快照](b1-design.md) 本地库/合同验收、交叉review与阶段提交；真实DB迁移/并发仍属于后续整链门槛。
  - [ ] B2 主动健康验证/续期及受认证签票，不能由缓存心跳虚报新鲜度。
  - [ ] B3 仅连接现有 runtime 的 registry 与失效清理。
  - [ ] B4 host-agent 控制/数据/出口生产级装配与断连恢复证据。
- [ ] C CCMAX gateway 新数据面接线、协议/usage/错误/取消闭环，默认关闭。
- [ ] D CLI/isthmus/MCP/会话与可靠停机。
- [ ] E Token 刷新/版本切换/生命周期/脱敏运维。
- [ ] F 当前代码整链 Docker/依赖故障/兼容/1000连接/24h本地验收。
- [ ] G 本地制品与恢复验收，真实 canary 和部署需要另行授权。

不可在 A 完成后勾选上述 B–G 或原执行面 Phase 6–11。每一项以实际实现与运行证据为准。
