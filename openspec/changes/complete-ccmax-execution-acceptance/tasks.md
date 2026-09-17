# 验收收尾台账

总计划：[CCMAX execution acceptance v2](../../../docs/plans/ccmax-execution-acceptance-v2.md)。

用户要求的固定百分比见 [100分交付台账](../../../docs/plans/isthmus-container-delivery-v1.md)：当前执行链工程验收32%，镜像专项6/15=40%、运行服务12/20=60%、身份2/10=20%。R3和K2已关闭；S2b2组件闭环及双实例真实Docker证书管理通道通过，但正式host-agent装配、跨容器mTLS及K3/K4完整门槛仍开放，不提前加分。

沿用Sub2计价/倍率/产品权限，按 [S1–S6聚焦阶段](isthmus-focused-stages.md) 交付。S1a后，S1b已用真实9包锁、原生Linux构建和新context/空builder-cache重跑关闭I2；[实证](verification.md#s1b原生linux基础镜像构建)。后续C/L仍是桥接usage/执行生命周期，不重做用户计费。

## 当前优先门槛：VM 隔离与 TLS

按用户要求，先处理 [VM 隔离优先计划](vm-isolation-design.md)，暂停后续业务接线；未通过不得用 B2b2a 的局部 PASS 放行整链。

- [x] VM0a 已发现的 provider 接纳/隔离漂移与 worker 环境代理/明文上游缺口修复，独立 review、全模块离线 race/vet、关键三包十轮 race 和本地 CONNECT/TLS 测试通过；仅源码前置门槛，不含真实 VM 或每实例证书签发。
- [ ] VM0b Linux 实际出口防火墙/namespace、宿主/跨槽/metadata/DNS/IPv6 拒绝及失效回收。
  - 局部前置：`recoverykit lab inspect` 只读专用端点检查已实现并 review；45项合成用例/十轮重复及236项完整Python回归通过。没有连接实际Docker、启动VM或验证防火墙，不勾选VM0b/N3、不增加23%分数。见 [预检结果](verification.md#n3前置专用实验端点只读预检)。
- [ ] VM0c 每实例身份材料、生产 worker mTLS 与换代/重放拒绝；按明确客户端版本验证 TLS 特征。
  - [x] S2a 两实际实例本地独立私钥/CSR，K2通过。
  - [x] S2b1 原子证书安装、真实worker/Controller mTLS与票据负例，170单容器原生三遍及review/race/vet通过；[实证](verification.md#s2b1证书安装与实际组件mtls)。
  - [ ] S2b2 受认证签发/启动前证书投递、host-agent装配、双实例mTLS/lease失效组合；不以S2b1关闭VM0c。
    - [x] S2b2组件：分目录实现受认证签发、SQL公开receipt、实例CA pin安装、Controller启动接线；本地race/vet、独立review及170单容器9组×3遍通过；[实证](verification.md#s2b2受认证签发与启动前bootstrap)。
    - [x] S2b2-live：双实例真实Docker CSR/证书投递，独立key、交叉/错CA精确拒绝、幂等安装、lease撤销后拒绝签发；清理后原4业务容器不变；[实证](verification.md#s2b2-live双实例真实docker证书管理通道)。
    - [ ] 剩余：生产host-agent/provider装配、真实SQL幂等并发、双实例mTLS连接及lease失效整链；当前不宣称CLI整链/生产可用。
- [ ] VM0d 当前组件实际隔离整链，不复用旧 VM/旧凭据或局部 PASS 冒充完成。

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
        - [x] B2b2a [当前控制命令的只读诊断签票](b2b2a-design.md)：health/credential_key，实际本地 TLS ControlClient/worker RPC 组合、交叉 review/race/vet/离线生成通过；默认关闭，不包含 activation/业务票、续租或生产装配。
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
