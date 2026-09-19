# 验收收尾台账

总计划：[CCMAX execution acceptance v2](../../../docs/plans/ccmax-execution-acceptance-v2.md)。

用户要求的固定百分比见 [100分交付台账](../../../docs/plans/isthmus-container-delivery-v1.md)：当前执行链工程验收32%，镜像专项6/15=40%、运行服务12/20=60%、身份2/10=20%。R3和K2已关闭；双实例真实Docker证书管理通道、严格START与S2b4默认关闭服务入口通过限定测试，但完整业务host-agent装配、权威lease writer、跨容器mTLS及K3/K4门槛仍开放，不提前加分。

沿用Sub2计价/倍率/产品权限，按 [S1–S6聚焦阶段](isthmus-focused-stages.md) 交付。S1a后，S1b已用真实9包锁、原生Linux构建和新context/空builder-cache重跑关闭I2；[实证](verification.md#s1b原生linux基础镜像构建)。后续C/L仍是桥接usage/执行生命周期，不重做用户计费。

## 当前优先门槛：VM 隔离与 TLS

后续开发及Claude接手从[2026-09-18时间命名规划](../../../docs/plans/2026-09-18_13-17-37-isthmus-claude-handoff.md)继续：P1/S2b5a已提交；P2 已完成本地实验合同/真实provider双槽位Internal网与精确清理（无本机Docker socket，实跑skip）。下一步是专用Linux只读预检后的双实例mTLS/cgroup，不跳过停止线、不重做计费/UI。

按用户要求，先处理 [VM 隔离优先计划](vm-isolation-design.md)，暂停后续业务接线；未通过不得用 B2b2a 的局部 PASS 放行整链。

- [x] VM0a 已发现的 provider 接纳/隔离漂移与 worker 环境代理/明文上游缺口修复，独立 review、全模块离线 race/vet、关键三包十轮 race 和本地 CONNECT/TLS 测试通过；仅源码前置门槛，不含真实 VM 或每实例证书签发。
  - [x] S2b5a provider请求/接纳明确要求正Memory与MemorySwap等值；JSON投影和bootstrap前后漂移负例、review/race/vet通过；[实证](verification.md#s2b5a禁止swap策略与时间命名交接规划)。仅本地策略，真实kernel/cgroup仍归VM0b/N3，不重复加分。
- [ ] VM0b Linux 实际出口防火墙/namespace、宿主/跨槽/metadata/DNS/IPv6 拒绝及失效回收。
  - [x] P3a 应用层前置：CONNECT 结构性拒绝矩阵（metadata/链路本地/未指定/组播广播/IPv6字面量/解析端口/数字编码地址规避）在每槽 allowlist 之前生效，注册期命中即整绑定失败，每个拒绝带精确原因头而非超时；绑定撤销与 epoch 换代回收在途 tunnel。两轮独立 adversarial review（含一处 HIGH 绕过）、全仓离线 race/vet、`internal/hostagent` race×10 通过。[设计](p3a-egress-deny-matrix.md)，[实证](verification.md#p3a出口connect结构性拒绝矩阵与在途回收)。**不是内核门**：未做 netns/防火墙，未接 host-agent daemon，DNS rebinding/DoH/容器内直连 socket 未覆盖；不勾选 VM0b/N3、不加分。
  - 局部前置：`recoverykit lab inspect` 只读专用端点检查已实现并 review；45项合成用例/十轮重复及236项完整Python回归通过。没有连接实际Docker、启动VM或验证防火墙，不勾选VM0b/N3、不增加23%分数。见 [预检结果](verification.md#n3前置专用实验端点只读预检)。
- [ ] VM0c 每实例身份材料、生产 worker mTLS 与换代/重放拒绝；按明确客户端版本验证 TLS 特征。
  - [x] P3b 生命周期合同（设计，非实证）：`internal/identitylifecycle` 纯规则集，adopt/restart/upgrade/account-change/destroy 的换代要求、floor 锚定销毁后重生、持久 home 白名单边界。两轮独立 adversarial review 共 8 项缺陷已修，全仓 race/vet 通过。[设计](p3b-identity-lifecycle.md)，[实证](verification.md#p3b实例身份生命周期合同与持久-home-边界)。**无生产调用方、未实现轮换、未做撤销传播**；不勾选 VM0c、不加分。
  - [x] P3c 按用户选定方案 (a) 改 reconcile 路由：`DesiredReady` 下 `stopped→destroy`、`destroyed→release`，与既有过时代次分支一致，使被停止的容器走 destroy→release→place 取得新 epoch。同时修复本改动暴露的 CRITICAL——`ActionPlace` 幂等键在无 assignment 时恒等，而 Place 一经派发即 completed 且不可再 claim，会导致槽位永久无 assignment；改用 `NextExecutionEpoch` 作判别量并在缺失时拒绝放置。[实证](verification.md#p3creconcile-停止路由改为销毁释放重新放置)。
  - [x] P3d 按用户指示禁用 Docker 自动重启：创建请求改为 `RestartPolicy{Name:"no"}` 并显式序列化；共同只读接纳门禁新增该校验，只接受显式 `"no"` 且零重试，缺失/null/空 Name 一律拒绝；既有 `unless-stopped` 容器只被拒绝，不修复不重建（provider 无 update 动词）。消除了与 `test/dockerbootstrap/policy.go:50` 及 lab Python 原有 `--restart no` 要求的不一致。43 子测试含 bootstrap exec 前后漂移，变异验证非空跑。[实证](verification.md#p3d禁用-docker-自动重启策略)。**创建时预防 + 接纳时检测，非持续强制**；带外 `docker update --restart` 只能在下次 inspect 发现。
  - [ ] **运维缺口**：`reconcile.NewController` 无非测试调用方，故守护进程/宿主重启后 runtime 容器无自动恢复路径，须待 reconciler 接线（P4 装配）。原 `unless-stopped` 只是用身份已损坏的容器掩盖该缺口，不是恢复。
  - [x] S2a 两实际实例本地独立私钥/CSR，K2通过。
  - [x] S2b1 原子证书安装、真实worker/Controller mTLS与票据负例，170单容器原生三遍及review/race/vet通过；[实证](verification.md#s2b1证书安装与实际组件mtls)。
  - [ ] S2b2 受认证签发/启动前证书投递、host-agent装配、双实例mTLS/lease失效组合；不以S2b1关闭VM0c。
    - [x] S2b2组件：分目录实现受认证签发、SQL公开receipt、实例CA pin安装、Controller启动接线；本地race/vet、独立review及170单容器9组×3遍通过；[实证](verification.md#s2b2受认证签发与启动前bootstrap)。
    - [x] S2b2-live：双实例真实Docker CSR/证书投递，独立key、交叉/错CA精确拒绝、幂等安装、lease撤销后拒绝签发；清理后原4业务容器不变；[实证](verification.md#s2b2-live双实例真实docker证书管理通道)。
    - [ ] 剩余：生产host-agent/provider装配、真实SQL幂等并发、双实例mTLS连接及lease失效整链；当前不宣称CLI整链/生产可用。
      - [x] S2b5b 本地：实验资源/拒绝矩阵、真实 `provider/docker` 双槽位独占 Internal 网 Create/Inspect、交叉CID拒绝、仅清单清理、派生镜像配方；live Docker 默认 skip。[合同](s2b5b-provider-lifecycle.md)，[实证](verification.md#s2b5b真实provider双实例实验合同)。未关闭 K3/K4/N3。
  - [x] S2b3组件：严格START经准确物理CID核验→认证bootstrap→实际mTLS，失败/漂移不被普通INSPECT复活；真实ControlClient/Dispatch与worker回环组合通过；[实证](verification.md#s2b3正式start命令的已有实例认证装配)。仅组件装配，host-agent二进制与orchestrator签发入口仍未启用。
  - [x] S2b4入口：cmd/host-agent 默认关闭生命周期服务，专属node身份/控制TLS、退出收敛、业务placement排除；orchestrator显式SQL receipt + 独立Redis lease校验装配；[实证](verification.md#s2b4默认关闭的服务入口与持久化签发装配)。只接生命周期，不启业务/出口，缺权威lease writer时签发拒绝；K3/K4/H5保持开放。
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
        - [x] P4a 执行租约权威状态转换设计（[设计](p4a-lease-authority.md)，[实证](verification.md#p4a执行租约权威的状态转换设计与两库合取校验)）：确立 Redis 令牌=围栏权威 / SQL=持久归属与撤销事实，route TTL 与签发回执**均非**租约权威；`Coordinator.Validate` 改为两库合取，任一不可读即失败关闭；`Fencer`/`Renewer` 依赖放宽为 `Validator`/`Refresher` 以便接最强权威；`Revalidate` 按 claim 去重。Review 修掉两个真缺陷（续期只刷 Redis 导致与代理租约判定相反；Revalidate N+1 加共享超时级联关闭）。**生产 writer/续期循环仍未接线，registry 仍不存在，真实 Redis/MySQL 集成测试整体 skip**，不加分。
        - [ ] B2b2c 可信证明驱动的双层续租与持续流失效；不以诊断票替代。
  - [ ] B3 仅连接现有 runtime 的 registry 与失效清理。
    - [ ] P5f 双库回收链路验收：测试已写（`leasechain_integration_test.go`，双环境变量门控、默认 SKIP、只建只删自有资源）并已交叉编译，独立 review 核对真实 store 门禁后采纳两条硬化（TTL 5 分钟、`MaxRetries=-1`）。**但从未对真实 MySQL/Redis 执行**——`ssh`/`colima` 被本地权限分类器拦截且未绕过，故**第 3 步未验收、链路未实证**。[实证](verification.md#p5f双库回收链路验收测试已写已编译尚未对真实库跑过)
    - [x] P5e 托管与会话授权接进 daemon：用 `nodeFacts` 后绑定打破「执行器→custody→authority→执行器」循环，未绑定窗口**失败关闭**（变异验证改成 `false` 即 FAIL）；`composition.go` 构造 authority+custody 并传入 `lifecycle.New`，窗口复用既有 `Timings.NodeOffline`（不新增旋钮）；`run.go` 增加 `custody.Run` 协程与停机等待 drain。`NewSessionAuthority`/`NewCustody`/`Custody.Run` **均已有非测试调用方**。[实证](verification.md#p5e托管与会话授权接进-daemon不再是只有测试调用方的模块)。仍默认关闭，`/readyz` 503、`production_ready=false`。
    - [x] P5d 签发门禁改用两库合取：`runtimeenrollment` 的 `LeaseValidator` 由 **Redis 单库**改接 `lease.Coordinator`（令牌+持久记录）。修复的是真洞——`Revoke` 先写 SQL 再删令牌，删令牌失败时旧门禁会**给已撤销租约签出证书**；变异验证换回单库即打印 `issuance accepted a durably revoked lease: <nil>`。同时**更正**上一轮把节点侧 `revokedThrough` 重启空表说成 fail-open 的高估结论：那只是纵深防御，真正的门禁在控制面签发路径，重启后依然生效。[实证](verification.md#p5d签发门禁改用两库合取并更正上一轮被高估的结论)
    - [x] P5c 节点侧会话授权：`hostagent.SessionAuthority` 实现 `lease.Validator`，只用「控制会话是否打开」+「控制面推送的撤销水位线」，**节点不持有任何数据库凭据**；明确标注弱于真实租约、不得当作权威。Review 发现并修复最严重缺陷——基于 `Send` 成功判定新鲜度不成立（Send 只进本地写缓冲；且无 gRPC keepalive，黑洞分区约 15 分钟才被 TCP 发现，叠加后可多托管约 20 分钟）：控制拨号新增 keepalive（承重）、改为记录会话开合、关闭时刻用单调时钟。另修死公开面与过度宣称的测试名。[实证](verification.md#p5c节点侧会话授权不持有任何数据库凭据)。**仍未接线**（循环依赖需后绑定闭包）；**已知 fail-open**：`revokedThrough` 内存态，重启后水位线清空，接线前必须处理。
    - [x] P5b 显式撤销接入命令路径：`hostagent.SlotCustodian` 窄接口，`RevokeEpoch`／`stop`／`destroy` 三条真实结束路径回收托管连接（RevokeEpoch 在动容器前回收，provider 失败也已回收），`INSPECT` 不回收；严格按槽位+epoch，不触碰其他槽位与节点级控制连接。**关键证据**：对真实 gRPC 监听器证明回收后传输确实拆除（托管期 `Health` 得 `Unimplemented` 说明到达服务端；回收后连接 `Shutdown` 且错误不再是 `Unimplemented`），而非只看注册表计数。变异验证两处接线任一移除即 FAIL。**未给 host-agent 任何数据库凭据**。[实证](verification.md#p5b显式撤销接入命令路径并证明传输真的被拆除)
    - [x] P5a 托管进入真实 START 链路：`lifecycle.Custody` 用**命令携带的** `lease_owner_id` 构造 claim 并向权威求证后才接管（host-agent 不签发租约）；未配置 custodian 时 START 行为不变（默认关闭）；重复 START 保持幂等；`Registry.Drain()` 供停机并屏障后续 adopt。`TestAuthenticatedSTARTControlToWorkerAndRevokedLease` 参数化为 with/without custody，托管侧以**真实两库 Coordinator** 为权威，证明**仅持久层撤销**即可回收 worker 连接。Review 修掉 owner 伪造(HIGH)、Drain 未屏障、ctx 顺序漂移、重放 TOCTOU、take 无单元覆盖五项。[实证](verification.md#p5a连接托管进入真实-start-链路)。**daemon 尚未构造 Custody，周期校验未在真实 daemon 运行**；不加分。
    - [x] P4b `internal/runtimeregistry`：只接管已建立连接、绝不创建/拨号/重启（`Connection` 仅 `Close`，并钉住 `Registry` 导出方法集）；每槽一条记录，同一 generation 换容器一律拒绝**不论 epoch 是否前进**；回收四路（显式 Revoke／被更新代次顶替／Release／`lease.Fencer` 回调，复用 P4a 合取校验）；`Revoke` 为**屏障**（撤销水位线）而非仅关闭。[实证](verification.md#p4bruntime-registry-与撤销传播到-worker-连接)。Review 修掉跨 epoch 身份漂移、Revoke 非屏障、Release 虚假报错三项，并补掉三个存活变异体与一个名不副实的并发测试。**无生产调用方**：`hostagent.Controller.Start` 仍把连接直接返还调用方，未交由注册表托管；不加分。
  - [ ] B4 host-agent 控制/数据/出口生产级装配与断连恢复证据。
- [ ] C CCMAX gateway 新数据面接线、协议/usage/错误/取消闭环，默认关闭。
- [ ] D CLI/isthmus/MCP/会话与可靠停机。
- [ ] E Token 刷新/版本切换/生命周期/脱敏运维。
- [ ] F 当前代码整链 Docker/依赖故障/兼容/1000连接/24h本地验收。
- [ ] G 本地制品与恢复验收，真实 canary 和部署需要另行授权。

不可在 A 完成后勾选上述 B–G 或原执行面 Phase 6–11。每一项以实际实现与运行证据为准。
