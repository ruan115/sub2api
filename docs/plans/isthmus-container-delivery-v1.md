# isthmus 执行容器交付与固定百分比台账

日期：2026-09-17；基线 `60324ed`。用户要求逐项完成，尤其 `isthmus-vm-base`，并按百分比持续汇报。本页是当前执行链的唯一百分比口径，替代此前只按 VM0a–d 数量得到的25%；不覆盖旧阶段的实际证据。

## 目标与边界

恢复的是 `isthmus-vm-base` 所承载的 **Docker/runc 执行容器体系**：可维护镜像与构建入口、明确的 app/Bun/Claude CLI/工具制品、每实例 home/身份/证书、isthmus 协议/进程生命周期及 CCMAX 调用。不是开发虚拟机内核，不改用 KVM/Firecracker。本机 Colima 只提供 Linux 实验宿主，不是产品交付物。

线上参照截止已保全的2026-09-14/15材料；基础镜像并不包含完整应用，[职责与依赖证据](../analysis/online-stack-recovery-2026-09-13.md#isthmus) 和 [身份证书差距](../../recovery/docs/vm-identity-tls-baseline-2026-09-16.md) 必须同时满足，不能仅重建一个同名镜像便称完成。线上证书独立目录不证明私钥唯一；按用户要求实现每实例独立私钥，不复制真实线上材料。

范围为 CCMAX/isthmus 执行链的**本地工程验收**；Sub2API 登录/产品权限/计费继续沿用，前端视觉恢复另有计划，暂停的 Portunex 整业务恢复不重新开启。生产部署、push、canary与真实账号请求不计入这100分，也没有因此获得授权。线上服务、数据、UI、镜像、配置和路由不变。

## 固定计分规则

- 总计100验收分，百分比=已通过分/100。每个子门槛只有通过或未通过，没有主观“写了八成”计分。
- 通过必须有该项范围的实现/正反例/必要实际运行、独立review和记录。只写设计、仅编译、局部mock、未复验历史PASS不能替代后续运行门槛。
- 同一成果不重复计分；子门槛出现回归则撤回对应分。发现额外风险仍须修复，不凭新增小任务稀释分母或涨分；确需变更范围/权重须另记版本和理由，不静默替换。
- 分数不是代码量、工时估计、生产准备度或“与线上百分之几相同”。即使99分，未满足安全停止线仍不能启用新执行面。
- 每次阶段交付固定汇报：总体百分比、镜像专项百分比、本轮关闭项、仍缺项、测试/review、commit。未完整关闭子门槛时分数可以不变。

## 当前总览：32%

| 模块 | 权重 | 已验收 | 模块完成率 |
| --- | ---: | ---: | ---: |
| I 可复建 isthmus-vm-base 及依赖 | 15 | 6 | 40% |
| R 实际 isthmus/CLI 运行服务 | 20 | 12 | 60% |
| N 出口与实例隔离 | 20 | 8 | 40% |
| K 独立机器标识/密钥/证书/mTLS | 10 | 2 | 20% |
| H 控制面与 host-agent | 10 | 4 | 40% |
| C CCMAX 调用链接通 | 10 | 0 | 0% |
| L 凭据与完整生命周期 | 10 | 0 | 0% |
| E 整体验收及可恢复交付 | 5 | 0 | 0% |
| **总计** | **100** | **32** | **32%** |

### I：可复建镜像（每项3分）

- [x] I1 镜像职责、外部卷、静态来源和未证实差异基线；[证据](../../recovery/docs/vm-identity-tls-baseline-2026-09-16.md)。只奖励基线，不声称镜像已重建。
- [x] I2 可审查 Dockerfile、隔离构建入口及构建失败/无隐式代理或秘密注入测试；[S1b实证](../../openspec/changes/complete-ccmax-execution-acceptance/verification.md#s1b原生linux基础镜像构建)。本次入口为共存测试宿主上的受限可信构建，官方包特权builder例外不代表不可信工作负载隔离、N3或完整runtime已通过。
- [ ] I3 app/Bun/CLI/工具版本与每项哈希固定，允许来源及双架构兼容边界明确；不使用 latest 或不审查原件。
  S1c已固定Bun/CLI双架构公开制品、23文件fake源码和amd64展开哈希，并在170完成
  原生CLI版本/两版Bun测试/关闭对照；[实证](../../openspec/changes/complete-ccmax-execution-acceptance/verification.md#s1c固定制品与原生linux工具验证)。
  已有真实CLI单实例probe，但默认app仍fake，完整运行工具依赖及不可变组合制品未完成，暂不关闭I3、不加分。
- [ ] I4 无秘密的 home/配置/卷初始化合同，所有权/权限、跨实例隔离、重建保留语义实际验证。
- [ ] I5 空白专用环境重建与冷启动，核对当前镜像和挂载制品；不能以旧缓存、Go-only worker 或同名 tag 替代。

### R：运行服务（每项4分）

- [x] R1 原 proto/WS codec 与 fake HTTP/WS 测试基础；[边界](../../execution-plane/isthmus-runtime/README.md)。不代表真实服务或原版可靠停机已恢复。
- [x] R2 实际 HTTP 增量转发、usage、背压和取消；[A验收](../../openspec/changes/complete-ccmax-execution-acceptance/verification.md)。
- [x] R3 固定版本真实 CLI 执行器，参数/环境白名单、stream-json、无凭据假上游实际验证；[原生五项实证](../../openspec/changes/complete-ccmax-execution-acceptance/verification.md#r3单实例真实cli双向stream-json闭环)。只含单轮文本、JSON/SSE与合成上游，不含会话池、真实模型或线上等价。
- [ ] R4 session/pool/MCP/工具续接与子进程生命周期、跨会话拒绝、超时终止。
- [ ] R5 实际 HTTP/WS/gRPC 兼容、连接回收和可靠停机；关闭既有 Bun 停机缺口。

### N：隔离（每项4分）

- [x] N1 provider 严格接纳与账号/镜像/网络/权限漂移拒绝；[VM0a](../../openspec/changes/complete-ccmax-execution-acceptance/verification.md#vm0a实例接纳与固定-tls-出口前置修补)。
- [x] N2 固定代理与验证型目标 TLS；同上，仅库/回环范围，不是任意 socket 的 kernel ACL。
- [ ] N3 专用 Linux 实验环境、规则安装与仅本任务资源清理的实际验证；仅配置或只读预检工具不足以得分。
- [ ] N4 当前代码的跨槽/宿主/公网/DNS/IPv6/metadata 实际拒绝矩阵。
- [ ] N5 规则丢失、代理撤销、在途 tunnel 取消/half-close 与恢复均失败关闭。

### K：独立身份（每项2分）

- [ ] K1 稳定机器/逻辑实例标识生命周期，重启/升级/恢复/换账号的保留与换代规则。
- [x] K2 每实例本地安全随机私钥/CSR，不能由模板、宿主Env/argv或线上私钥复制；[S2a双实例实证](../../openspec/changes/complete-ccmax-execution-acceptance/verification.md#s2a双实例本地身份与csr)。仅本地生成/保护/公开CSR，未安装证书或接通mTLS。
- [ ] K3 准确身份的受认证签发、安全存放和原子安装，控制面不下发同一server私钥。
- [ ] K4 实际 worker 与 host-agent 双向 TLS，准确SAN/用途/执行代与独立ticket/lease检查。
- [ ] K5 轮换、旧代、跨槽、撤销、恢复旧home拒绝及在途连接处理。

### H：控制与宿主（每项2分）

- [x] H1 B1 权威快照和 B2a 主动 INSPECT；[任务与验证](../../openspec/changes/complete-ccmax-execution-acceptance/tasks.md)。
- [x] H2 B2b1 原子加载状态核对与 B2b2a 命令绑定诊断票；同上，默认关闭且不授权业务执行。
- [ ] H3 activation/业务精确授权，当前版本/代理/模式绑定。
- [ ] H4 双层续租与持续流撤销，不借新会话复活旧流。
- [ ] H5 existing-only registry 和当前 host-agent 控制/数据/出口真实装配。

### C：CCMAX 接通（每项2分）

- [ ] C1 gateway 实际 dispatch 接线，默认关闭、legacy行为保留。
- [ ] C2 Messages/count_tokens/models/Chat 完整协议闭环。
- [ ] C3 usage 正确回传、计费仍只有原权威，不能双重记账。
- [ ] C4 错误、取消和 migrated 无明文回退的组合测试。
- [ ] C5 Sub2API→CCMAX→执行侧的当前组件合成请求闭环。

### L：凭据与生命周期（每项2分）

- [ ] L1 刷新单飞与失败回退规则。
- [ ] L2 原子版本切换和旧版本/旧票/代理处理的实际组合；已有 Vault 局部测试不足以关闭。
- [ ] L3 drain/recreate/archive/delete/restore 当前链路一致性。
- [ ] L4 重启恢复与幂等、不复活过期身份/任务。
- [ ] L5 管理操作权限、脱敏审计和有界保留；不重复实现Sub2产品权限。

### E：交付（每项1分）

- [ ] E1 当前组件完整隔离整链，非多份互不连接的局部PASS。
- [ ] E2 真实本地专用 DB/Redis 及故障组合。
- [ ] E3 1000连接与24h稳定性。
- [ ] E4 已冻结线上合同的兼容矩阵，未知差异明确不冒称相同。
- [ ] E5 制品校验、版本清单与空白环境恢复演练。

## 实施顺序与文件职责

2026-09-17用户确认[收敛顺序](../../openspec/changes/complete-ccmax-execution-acceptance/single-instance-cli-first.md)：先真实CLI单实例，再双实例隔离/身份，最后CCMAX桥接。该顺序覆盖下列原始排序，暂停S1d新增构建工具，保留其WIP；验收门槛和100分权重不变。R3和S2a的K2已通过，当前总体32%、运行服务60%、身份20%、镜像40%。

用户再次明确沿用Sub2价格/倍率/计费，CCMAX保留现有业务模板；[S1–S6收敛阶段及本轮S1a设计](../../openspec/changes/complete-ccmax-execution-acceptance/isthmus-focused-stages.md)规定分阶段交付。这里的C3仅回传usage和防重复归属，不实现第二套计价；L是执行凭据/实例生命周期，不是Sub2用户业务。权重不变；S1b后总体26%、镜像40%，S1c部分交付不重复加分。

1. **N3前置 → I2/I3**：先补专用实验环境安全入口，拒绝默认Docker context/环境代理；再审查并固定镜像/运行制品、离线可控构建。现有 `scripts/docker-e2e.sh` 不能未经预检直接运行。
2. **I4/I5 + N3/N4**：在专用 Linux 中完成卷/home初始化、当前镜像冷启动、实例及网络矩阵。镜像尚未获得隔离证据前不注入身份材料或真实账号。
3. **K1–K5 + N5**：独立标识/密钥/证书、实际双向认证、轮换与撤销。材料分生命周期，不随机制造TLS指纹。
4. **R3–R5**：接原CLI能力、session/pool/MCP及三种transport；仍只用合成凭据/假上游。
5. **H3–H5 → C1–C5 → L1–L5 → E1–E5**：完成控制台调用和最终验收，每个小切片先规划、review、回归、Git提交。

只在实施对应功能时建目录，不预建空壳：

```text
recovery/tooling/recoverykit/lab/        专用实验环境的非生产预检/证据工具
recovery/tests/lab/                     合成输入与本地Unix socket测试
execution-plane/isthmus-runtime/image/ 镜像构建、制品锁定与初始化（I2起）
execution-plane/isthmus-runtime/src/runtime/{cli,session,pool}/
execution-plane/isthmus-runtime/src/tools/mcp/
execution-plane/internal/provider/docker/  复用现有容器生命周期与接纳门禁
execution-plane/internal/worker/           只装配实际执行器；按需独立identity/TLS模块
```

本轮第一切片见 [专用实验环境只读预检](../../openspec/changes/complete-ccmax-execution-acceptance/vm-lab-preflight-design.md)。预检成功不自动运行镜像/改防火墙，也不自动增加N3的4分。

### 2026-09-17 切片记录：N3 前置工具

`recoverykit lab inspect` 已实现；模块、参数、策略与测试分别放入上述独立目录。只允许显式本地Unix socket的五个只读GET，检查预期身份/架构/能力和空白资源；成功仍输出 `isolation_verified=false`、`execution_permitted=false`。两处超时边界已先复现再修复并获独立review关闭。

45项lab合成测试及十轮重复通过；完整236项Python回归与116条合同清单结构检查通过，后者仍为 `business_verification=false`。[具体证据与边界](../../openspec/changes/complete-ccmax-execution-acceptance/verification.md#n3前置专用实验端点只读预检)。未启动真实Docker/VM、构建镜像或安装规则，所以总体仍 **23%**，镜像仍 **20%**。下一实现切片为I2/I3镜像构建/依赖固定，不跳过Linux实际验收，也不恢复暂停的业务接线。

### I2/I3 下一项的已确认缺口

本次重新读取已保全 `Dockerfile.vm` 前通过文本内容检查，SHA-256 为 `b51a59151f6b89f43a6afd4fe0cb6fcbc4894a78922b5b71ac730e62ef61d991`，与已有manifest一致；没有执行或复制原件进Git。这是历史静态证据，不代表刚检查线上。

- **系统层**：原件使用浮动 `debian:13` 和未锁版本APT；已有 `07133e…` 是运行镜像config ID，不可冒充可拉取的OCI manifest digest。下一步须固定基底、必要包及来源/摘要，拒绝缺项时动态安装或退回latest。
- **隔离差异**：原件给 `unshare` 设置 `CAP_SYS_ADMIN` 文件能力，并注释PID隔离失败会降级。本地不能为兼容旧脚本直接放宽NNP/dropALL/只读根门禁；需要验证新的进程隔离方案，失败必须停止，不声称现有Dockerfile已等价。
- **Bun/CLI**：已有[CLI 2.1.258双架构摘要与有限运行证据](../../recovery/docs/local-cli-cache-stub-2026-09-15.md)；amd64原生及整链仍缺。Bun 1.4.2仅有[macOS候选验证](../../execution-plane/isthmus-runtime/docs/bun-stop-fix-validation.md)，不能当Linux通过，也不能擅自替换现行版本；两Linux架构制品及停机验证待补。
- **应用与构建内容**：当前TS模块仍为fake，Go worker不是原镜像替代品。image模块只接显式锁定且审查的程序/源码，禁止复制整个仓库、旧bundle、账号home、证书或宿主配置。独立home属于I4，身份生成属于K，不能打进共享镜像。
- **实现与验收顺序**：`image/` 内先闭合制品清单与最小构建上下文，再交付可实际构建的Dockerfile和显式专用宿主入口。检查失败/版本或架构不符/代理和秘密注入负例后，在真正专用Linux环境验证构建与冷启动。仅staging/静态Dockerfile不关闭I2/I3/I5；`--network=none`本身也不能证明FROM阶段不会拉取镜像。

### 2026-09-17 S1a 子切片记录

按 [S1–S6阶段计划](../../openspec/changes/complete-ccmax-execution-acceptance/isthmus-focused-stages.md) 完成base-only Dockerfile模板、严格锁合同、离线私有上下文生成/复核、摘要CLI与独立测试模块。沿用Sub2计价/倍率，并记录CCMAX实际桥接断点与两层账务归属，不改业务代码。完整基础镜像阶段仍未完成。

最终 `make -C recovery check`：236项恢复工具Python、111项Bun1.3.9、37项镜像工程测试均通过；37项镜像测试另完整十轮370次通过。两位代理交叉review；自审/复审发现并关闭锁关联窗口、私有父目录配方及URL空分隔符问题。[完整证据](../../openspec/changes/complete-ccmax-execution-acceptance/verification.md#s1a基础镜像配方与离线构建上下文)。仅为合成输入测试，非Docker构建证据。

当前没有完整官方发布锁、实际Linux构建或冷启动，因此不勾选I2/I3/I5，总体仍23%、镜像仍20%。下一切片S1b：取得并验证官方base/package索引及依赖闭包，在通过实际宿主准入的专用Linux里构建base；然后固定Bun/CLI/app，不把上下文工具当成镜像交付。

### 2026-09-17 S1b 子阶段验收（更新上述S1a历史状态）

[S1b实证](../../openspec/changes/complete-ccmax-execution-acceptance/verification.md#s1b原生linux基础镜像构建)：
真实官方base/BuildKit固定digest、签名APT对应9包锁、原生amd64实际构建、新context与空
builder-cache完整重跑、全9包及dpkg audit、非特权无网络冒烟、失败关闭和精确所有权清理
通过。独立review收尾，完整236恢复Python/111Bun/101镜像测试PASS，镜像测试再十轮1010次。

只给I2增加3分，当前**26%/镜像40%**。这是一台共存测试机上的可信官方包受限构建，
特权builder不承载不可信代码/CLI/账号；没有关闭专用Linux网络隔离N3、完整runtime冷启动I5。
下一项I3补齐固定Bun/CLI/app与双架构边界；I4、I5、S2隔离和S3独立身份仍需实际验收。
