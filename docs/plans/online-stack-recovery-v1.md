# 整套恢复实施清单与验收台账

日期：2026-09-13。状态：恢复底座和本地管理演示已交付；执行端异常停机门槛、完整R0/R1与旧业务兼容尚未完成。
PRD：`docs/prd/online-stack-recovery-v1.md`。

## 已完成的规划输入

- [x] 用户确认 Portunex 后端/网页/数据库 + isthmus 全套范围。
- [x] 用户确认现有调用方接口兼容优先。
- [x] 只读核实 Rust/React/Bun 技术栈和现有 Go/Vue 差异。
- [x] 只读核实 36 表、387 列、28 外键、28 条迁移元数据。
- [x] 复核 isthmus 产物和旧 execution-plane 库测试基线。
- [x] 形成新分析、PRD、阶段清单。
- [x] 用户要求开始第一阶段，明确开发前规划结构、每个模块独立目录；按新 PRD 推进，具体兼容差异仍逐项确认。

上面的勾选只表示分析已做，不代表下列恢复阶段已完成。

## R0：保全与可恢复基线

- [x] 对当前完整tracked/untracked WIP生成仓库外私有快照并校验，保留既有5.5c；先前91项为旧时点计数，本次快照另含第一阶段新增文件；不自动stash/reset/切分支。
- [ ] 为Rust运行包、React资源、isthmus原JS/ELF/proto/脚本和镜像记录来源、大小、SHA、版本证据和采集时间。
- [x] 按本阶段显式白名单保全9份文本参考材料并独立校验；完整bundle因筛查命中暂排除，原件不动。秘密、PG数据、日志正文不进入Git；这不是全部运行物备份。
- [ ] 查证运行程序、磁盘发布物、依赖与已启用配置之间的对应关系；77实例不能仅抽一个即全体假定相同。
- [ ] 取得全部schema对象/SQL迁移线索并交叉验证；目前列清单不够创建数据库。
- [ ] 单独批准一致性备份、加密存放目的地、密钥保管与恢复演练；未经批准不导出真实数据。
- [ ] 独立处理静态根gateway.tar风险：先检查目标/同盘目的地，获准才移动并验证可恢复；未经允许不删除或改代理配置。
- [ ] 新机器校验恢复材料哈希；尚未完成异机/离线副本时不得称“已灾备”。

## R1：完整合同与新 OpenSpec

- [ ] 逐个冻结 Portunex method/path/query/body/envelope/status/header/cookie/role/分页/幂等合同，不能只登记路径。
- [ ] 冻结23条前端route记录与页面动作；从数据库/后端补齐未在菜单出现的订单、订阅、兑换等业务。
- [ ] 查明完整模型入口与Provider种类，含前端未显式调用的路径；接口不存在时不虚构兼容项。
- [ ] 确认身份/Key/session/OIDC旧值兼容策略，不读取或输出线上秘密来造fixture。
- [ ] 冻结NUMERIC单位、价格优先级、舍入、usage/points/订阅结算与支付回调不变量。
- [ ] 冻结isthmus proto/WS帧/SSE聚合/错误/取消/64MiB边界；注明request大小与流累计大小是不同限制。
- [ ] 建立source namespace/ID映射、账务唯一owner、provider→runtime控制接口ADR。
- [ ] 建立MITM/消息变换/会话策略与旧PRD冲突ADR；未决项禁止默认启用。
- [ ] 确认旧域名、页面/API同名路由与虚拟主机映射；保持旧客户端URL不变。
- [x] 创建独立OpenSpec恢复change并链接旧5.6/6/7/8/9/10/11依赖，不改写旧完成历史：`restore-online-stack-foundation`。
- [ ] 为每项登记 evidence/source、owner、验收fixture、风险、状态、获准差异。

## R2：可构建的工程与 fake 测试底座

- [ ] `backend/internal/portunex/` 业务域和受保护route注册；独立恢复DB与只操作该库的migration命令。
- [ ] Redis强制namespace封装，隔离key/pubsub/锁/幂等/配额/账务缓存；后台任务独立owner与锁域，无法强制时用独立Redis实例。
- [ ] 跨域同值ID测试：认证、缓存失效、账务/限流、幂等、订阅任务不会相互污染或抑制。
- [ ] `portunex-web/` React/TS工程与旧页面路由；保留`frontend/`现有Vue。
- [ ] `execution-plane/isthmus-runtime/` Bun/TS工程、TurnEngine/child/credential/pool接口与fake driver。
- [ ] 独立旧Messages proto与新执行协议，生成器/依赖版本固定，不复用错误message来隐藏原始错误。
- [ ] fake mail/OAuth/OIDC/payment/upstream/Claude，全部本地且无真实秘密。
- [ ] CI包含secret扫描、锁文件、typecheck、unit、Go测试、proto drift；禁用依赖自动升级。
- [ ] 无生产依赖也能跑“登录/列表”与“三transport→fake turn”两条本地竖向测试。

## R3：Portunex 管理业务基础

- [ ] users/password/session/RBAC；匿名、普通用户、管理员、过期/撤销会话均有测试。
- [ ] 用户/管理API Key全生命周期及旧Key兼容，独立ID空间。
- [ ] Provider/credential、模型alias/目录、基础健康/状态与批量命令。
- [ ] 读写和分页/filter/sort/date/error合同一致；禁止越权查询别人的Key/usage/trace。
- [ ] React首页、登录、总览、用户/Key/Provider及统计页面；空态/错误态/权限/大列表完整。

## R4：账务、窗口与身份扩展

- [ ] 多层价格、生效区间、优先级、cache Token价格与NUMERIC精度回归。
- [ ] provider窗口/RPM/RPS/并发/冷却/sticky边界、超时与故障恢复。
- [ ] 积分、订单、支付渠道/回调、套餐、订阅窗口、消费snapshot、兑换与防重复结算。
- [ ] 重复callback/乱序状态/重启/并发扣费/失败补偿测试；绝不实际付款。
- [ ] magic-link/第三方OAuth/OIDC完整授权与撤销流程；PKCE/state/nonce/redirect严格验证。
- [ ] 补齐价格/窗口/账单等页面和数据库存在但原manifest未列出的必需操作入口。

## R5：消息与三种传输

- [ ] 模型Gateway接口按已冻结矩阵实现，保留模式/Provider与计费归属。
- [ ] HTTP/WS/gRPC共用turn core，不复制三份生命周期。
- [ ] SSE逐块转发及非流式聚合，覆盖UTF-8/JSON跨chunk、工具参数、usage与异常结束。
- [ ] 2xx/non-2xx/local transport failure区分；status text/允许headers/body保真，秘密与hop-by-hop header过滤有明确合同。
- [ ] WS单in-flight、busy=409、cancel后复用连接、下一轮参数清理、keepalive和drain。
- [ ] gRPC oneof/presence、半关闭、deadline、TLS/错CA/错leaf/无证书。
- [ ] 客户端首chunk在上游完成前收到；慢消费者内存有界；合法大响应不受现有2MiB整包buffer误截断。

## R6：真实运行内核与凭据

- [ ] 固定CLI版本、最小启动权限、stream-json控制ACK关联、子进程退出/超时/取消。
- [ ] pool reservation/commit/cancel、预热、上下文隔离、idle/turn上限、旧epoch退役和drain。
- [ ] session/one-shot/reset与已批准entrypoint；PTY只在必要且安全能力验证后实现。
- [ ] MCP工具schema、并行call、tool_result续接、幂等、pending清理及悬挂会话TTL。
- [ ] credential refresh单飞、Vault原子version/ACK、旧进程处理、固定出口与401一次恢复。
- [ ] 独立OAuth task创建/提交/查询/取消/过期/重启，若实际合同要求则全部覆盖。
- [ ] 消息变换只实现获准且规范化的字段语义，拒绝/未知行为不得悄悄当兼容成功。

## R7：现有执行平台与可观测接入

- [ ] Go worker executor桥与slot内IPC、ticket/epoch/generation绑定。
- [ ] host-agent生产bootstrap、DataPlaneService server、逐事件RuntimeClient。
- [ ] CCMAX gateway显式dispatch、legacy隔离、无明文回退、mode健康与模型/count_tokens。
- [ ] Portunex provider映射与唯一控制owner；跨DB只通过幂等应用接口/事件。
- [ ] 一次请求只结算一次；应用业务/CLI/上游usage与trace可关联。
- [ ] 登录/发布/权限/Provider/slot操作审计，脱敏、RBAC、保留策略和有界日志存储。
- [ ] Docker E2E包含进程/网络/凭据/租约故障、禁止直连、无双活与graceful shutdown。

## R8：整套兼容及迁移演练

- [ ] 参考二进制仅在隔离环境用合成账户/fake依赖运行；无法安全重定向时不强行运行。
- [ ] 每一API/页面/账务/协议合同有对照结果；动态输出不做伪精确比较。
- [ ] 恢复独立数据副本；校验扩展/索引/约束/sequence/函数/触发器/时区。
- [ ] 用户/Key/provider/订单/订阅/usage引用、NUMERIC余额与逐笔账务对账。
- [ ] 客户端不改接口的冒烟/集成，包括旧网页路径、旧Key/Session策略与SDK调用。
- [ ] 并发/慢消费/流中断/刷新/支付重放/重启等故障回归；真实账号不用于压力测试。
- [ ] 子系统测试、race/vet、前端typecheck/组件/浏览器E2E、数据库迁移均通过。

## R9/R10：发布、备份、审批与切换

- [ ] 构建签名/哈希、immutable镜像、SBOM、变更说明、依赖/CLI/schema版本矩阵。
- [ ] release/health/pause/rollback/state恢复工具，不复用清空共享app目录的更新方式。
- [ ] PG一致性备份/PITR、独立密钥保管、源码/制品异机恢复；明确并演练RPO/RTO。
- [ ] 处理gateway.tar风险并确认站点不再公开备份；归档保留位置和权限可追踪。
- [ ] 明确新旧DB单写权威、停写/增量同步、回滚边界；禁止无定义双写。
- [ ] 另行批准测试账户/费用范围和生产变更；少量canary后再逐批替换。
- [ ] 旧服务保留可验证回退路径，最后才讨论退役；不自动删除任何生产历史数据。

## 阶段记录模板

每个切片交付必须记录：目标、影响文件、既有WIP隔离、合同ID、运行命令、结果、未通过项、是否触及真实系统、可恢复方式、下一步。

禁止把[ ]直接批量改为[x]：实现、测试、集成、迁移、真实canary和上线是不同证据等级。

## 分阶段交付记录

规划轮：新增本清单、新PRD及分析基线，并在`.gitignore`中精确放行这三份文件。未提交/推送。

第一阶段：用户确认后先编写 `recovery/docs/architecture.md`（ADR-001），再按模块开发 `recovery/tooling/recoverykit/`、`recovery/contracts/`、`execution-plane/isthmus-runtime/` 和独立离线 CI。详细验收项及实际运行结果见 `openspec/changes/restore-online-stack-foundation/{tasks,verification}.md`。

验收结果：55项Python单测、57项Bun协议单测通过；4个execution-plane包无缓存回归通过。14个模块清单/116条discovered记录/145项未知；9份白名单文本和当前WIP已有本机私有校验副本。未提交、未推送、未部署，未做异机灾备。

本切片只覆盖 R0/R1 的可复核基础和少量 R2 协议工程，不代表 R0/R1 全部完成。暂不创建空的业务目录，也不虚构完整 HTTP 方法/请求/响应、数据库DDL或生产服务。

第二切片：用户要求“继续”，先写`restore-online-stack-demo/design.md`（ADR-002）及React/runtime子模块结构，再建立Go独立恢复域、内存会话/RBAC/三类只读列表、React页面和fake HTTP/WS。Go入口仅在`backend/cmd/portunex-demo/`；React不替换现有Vue；`src/runtime/turn/`与`src/transport/{http,websocket}/`分离。每个业务模块单独目录，未挂入生产wire。

旧Portunex资源本轮读取受阻：SSH在密钥认证前关闭，静态HTTP/HTTPS也未返回资源；记录在`recovery/contracts-wire/portunex/README.md`。因此演示使用专用`/__recovery__/v1`合同，不能据此补全旧method/body/envelope。DB、Redis、真实密码/Key生命周期、账务、gRPC、CLI、旧接口对照及生产部署均未完成，上方相关总项继续保持未勾选。

本地业务测试与Go/React实际浏览器联调通过；fake正常HTTP/WS路径也已用真实回环端口验证。另测出Bun 1.3.9服务端主动关闭后的原生停机缺陷：已有明确超时报错及独立非0退出的回归门槛，未伪装修复。`restore-online-stack-demo`的D4总项仍开，详细运行结果和安全留档见其`verification.md`。

第三切片：先写`restore-online-stack-evidence/design.md`（ADR-003），再将静态wire观察校验独立实现于`recovery/tooling/recoverykit/wire/`，测试单独置于`recovery/tests/wire/`。观察文档必须关联已审阅文件清单、完整文件哈希、UTF-8字节片段哈希及已有API记录；只读返回`source_anchored`，始终保留`business_verification=false`。不自动生成或提升旧合同，不据合成演示创建数据库DDL。

本轮SSH复核仍在认证前关闭，因此没有新增生产接口观察。Bun停机问题已查到上游修复与包含修复的1.4.2发布版本，但隔离下载/实测尚待确认；项目继续固定1.3.9，D4.1b保持未勾选。调查事实及实测前置条件见`execution-plane/isthmus-runtime/docs/bun-stop-fix-validation.md`。第三切片验收见`openspec/changes/restore-online-stack-evidence/verification.md`；不替代R1/R8完整兼容验收。

第三切片还修复独立审查发现的catalog祖先软链接/检查后换链和无界读取问题。最终`check-demo`通过：85项Python、111项Bun、33项React、Go演示race/vet和正常回环smoke；旧execution-plane四包回归通过。新的私有WIP快照已独立校验，原5.5c字节未变；未提交、推送或部署。独立Bun异常停机门槛仍失败，不能与这些通过项合并成“全部完成”。

随后同日更新：用户提供`BindInterface=en0`后，使用同一密钥的只读SSH检查成功；本机默认路由为`utun3`，先前未绑定连接的失败不能再表述为服务器SSH不可用。此轮未采集新的生产资产，旧wire未知项仍待补齐。用户另明确批准仅隔离下载/运行Bun、不替换、不部署；仓库外1.4.2候选完整check-demo及正常/1011关闭各5次均通过，系统1.3.9对照仍失败。系统/项目/CI版本未动，Linux和生产未验证，不自动关闭当前运行版本的D4门槛。完整证据及隔离路径见runtime的`docs/bun-stop-fix-validation.md`。

阶段提交更新：按用户要求完成三路独立 review，修复归档批次饥饿、会话登录/登出并发、Git 输出收集无界三个 P2，并修复 route 测试替身的数据竞争。最新回归为 Python 97 项、Bun 111 项、React 42 项及 Go race/vet/正常回环冒烟通过；真实 DB/Redis、Bun 1.3.9 异常停机与生产验收仍不算通过。新增私有 WIP 快照校验后，已将旧 5.5c、恢复底座和 Go/React 演示分成三个本地提交，再用文档提交记录交付。详细哈希、review、未完成项和下一阶段顺序见 [进度台账](../../recovery/docs/status-and-review-2026-09-13.md)。未推送、未部署，未替换 Bun，尚未完成异机灾备。

第四切片：`restore-portunex-wire-contracts` 先写 ADR-004，再只读采集 12 份指定静态文件；11 份共 163,689 字节完成白名单私有保全，1 份 Provider 页面包因凭据形状 URL 命中而排除。7 个 API 记录形成 27 条观察/60 个来源锚点，全部 `source_anchored`，未清除 catalog unknown。新证据显示旧认证采用 token/user 响应、localStorage 和 Bearer，而不是演示 Cookie session。另用明确 READ ONLY catalog SQL 补到三表 30 列及对象计数，不读业务行/默认表达式，不生成 DDL。[验收与边界](../../openspec/changes/restore-portunex-wire-contracts/verification.md) 单独记录；[下一认证方案](../../recovery/docs/portunex-identity-next-slice.md) 待确认后才进入旧认证代码还原。总 R0/R1、真实业务和上线门槛保持未完成。
