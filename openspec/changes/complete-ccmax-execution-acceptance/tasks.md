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
    - [x] B2a [有界主动宿主 INSPECT](b2a-design.md)：独立探测投影、会话固定、单飞/超时回收、命令命名空间隔离与离线 TLS 闭环，本地 review/race/vet 通过；未启生产，不代表 worker/凭据 ready。
    - [ ] B2b worker 实际加载版本/代理/模式的主动证明、分 scope 受认证签票，以及可信证明驱动的双层续租。
      - [x] B2b1 [原子 loaded-state 与控制面核对](b2b1-design.md)：兼容 Health 扩展、secret-free metadata、主动核对与真实本地 worker RPC/Vault 合同验证；交叉 review/race/vet/离线生成通过。没有生产签票、续租或真实依赖验收。
      - [ ] B2b2 受认证 health/activation/业务分 scope 签票、当前版本约束与双层 lease 续期；不能用 B2b1 receipt 直接授权执行。
        - [ ] B2b2a [当前控制命令的只读诊断签票](b2b2a-design.md)：health/credential_key，默认关闭；不包含 activation/业务票或续租。
        - [ ] B2b2b activation payload/lease 精确授权、业务票版本/代理/模式绑定与使用时复核。
        - [ ] B2b2c 可信证明驱动的双层续租与持续流失效；不以诊断票替代。
  - [ ] B3 仅连接现有 runtime 的 registry 与失效清理。
  - [ ] B4 host-agent 控制/数据/出口生产级装配与断连恢复证据。
- [ ] C CCMAX gateway 新数据面接线、协议/usage/错误/取消闭环，默认关闭。
- [ ] D CLI/isthmus/MCP/会话与可靠停机。
- [ ] E Token 刷新/版本切换/生命周期/脱敏运维。
- [ ] F 当前代码整链 Docker/依赖故障/兼容/1000连接/24h本地验收。
- [ ] G 本地制品与恢复验收，真实 canary 和部署需要另行授权。

不可在 A 完成后勾选上述 B–G 或原执行面 Phase 6–11。每一项以实际实现与运行证据为准。
