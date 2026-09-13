# Portunex + isthmus 整套线上系统恢复 V1

编写日期：2026-09-13。状态：**恢复底座与本地管理演示已交付；执行端异常停机、完整R0/R1与旧业务兼容尚未完成**。

目标仓库/分支：当前 `sub2api` / `codex/claude-execution-plane-v1`。
依据：`docs/analysis/online-stack-recovery-2026-09-13.md`。
任务与验收：`docs/plans/online-stack-recovery-v1.md`。

## 1. 已确认的用户目标

1. 旧电脑没有留档，现需要在当前仓库恢复可维护、可构建、可发布的完整线上系统。
2. 范围包括 **Portunex 业务后端、React 管理网页、数据库业务、isthmus 执行服务、发布运营与灾备**，不是只恢复执行容器。
3. 第一版优先保证现有调用方不改 HTTP/WebSocket/gRPC 接口即可接入。
4. 先分析，写新规划，确认后分阶段开发；未经另行批准不改线上流量、服务、数据库或防火墙。
5. 用户已要求开始第一阶段，并明确实现前规划文件结构、每个模块使用独立文件夹。模块边界见 `recovery/docs/architecture.md`（ADR-001）。

“完整”的含义是按冻结的功能/数据/协议/运营清单逐项恢复并验证，不是恢复原 Git 历史，也不是复制所有旧故障。任何省略、受限或安全修正必须进入差异台账，不得静默降低验收范围。

## 2. 完成标准

- 干净机器从私有源码仓库和锁定工具链可构建全部自研服务与网页，不依赖旧电脑或仓库外的临时目录。
- 现有管理 API、模型 API、isthmus HTTP/WS/gRPC、页面 URL、认证与客户端状态契约有完整 manifest 和测试证据。
- 用户、API Key、Provider、价格、积分/订阅/订单、用量与 OIDC 等业务可在隔离环境完成对应流程。
- 已批准导入的历史数据保持稳定 ID、引用和数值精度；用户/Key 无需无故重建；重复支付通知、重试、迁移不重复入账。
- 实时流式、错误、取消、背压、工具续接、Token 轮换和 drain 均可测，不能把缓冲完的响应称为 streaming。
- 构建物固定 digest，发布有记录、有健康门禁、有可验证回滚；源码、数据库、运行物、密钥分别有备份与异机恢复演练。
- 所有兼容项均为 PASS 或用户批准的明确差异；不能用“编译成功/首页打开/假上游成功”替代整套验收。

## 3. 采用的恢复路线

### 3.1 Portunex 后端：在现有 Go backend 中重建独立业务域

线上 Rust 已 stripped，无法像 Bun 一样直接取出可维护源码。恢复策略是用已核实的数据库、前端合同、部署物及受控行为样本重建，而不是把本轮变成全面 Rust 反编译工程。

- 承载于 `backend/internal/portunex/`，先按 identity/users/providers/billing 等业务域分目录，再在各域内部按需分 transport/application/domain/repository；沿用当前进程基础设施。不将所有 handler/service 放在一个全局文件夹。
- 保留旧 `/portunex/*`、`/oidc/*` 及查证后的模型入口，挂载独立路由组与鉴权策略，不把旧 body/envelope 强改成 Sub2API 风格。
- 初期使用**独立 PostgreSQL 数据库和 DB role**承接 Portunex 恢复模型；如后续合并为 schema，必须单独 ADR/迁移。禁止碰同名的现有 users/api_keys/subscription_plans。
- user/provider/key 等 ID 使用 source namespace 与显式映射，不因数值碰巧一致而合并。
- Redis 同样隔离：恢复域使用强制 `portunex:v1:` namespace 的客户端封装，覆盖 key、pubsub、分布式锁、幂等、限流/配额与账务缓存；后台任务独立注册、独立锁域。不得直接复用现有全局失效频道或订阅任务锁。若无法保证所有访问都带 namespace，则使用独立 Redis 实例；逻辑 DB 号不能隔离 pubsub。
- Portunex 恢复业务由自己的账务状态负责，不能同时在 CCMAX 和 backend 重复扣费。计算库可以复用，事实记录必须只有一处权威。
- 保持现有 Sub2API、CCMAX 业务可用，不顺手重构它们的全部用户/订单/代理模型。

这不是新增第四套控制面进程；是在现有业务进程中增加隔离的恢复域。恢复兼容优先于过早统一数据库。

选 Go 是为复用当前项目的维护与测试体系，不是认为换语言天然等价。独立 Rust 兼容服务是保守备选：若 R1 发现内部 Tonic、OIDC、SQL 事务或其他合同无法在所定成本内保持，必须先提交 ADR 比较 Rust 独立恢复与 Go 重建，再确认落点；不得为坚持 Go 而删掉兼容行为，也不得未经确认增开一套实现。

### 3.2 网页：当前仓库内保留独立 React 子应用

- 建议新增 `portunex-web/`，恢复原页面、样式资源和交互；不替换现有 Vue `frontend/`，不把 React chunk 粘进 Vue。
- 第一版以旧 React 网页的页面/接口合同为参照重建可维护 TSX，并以静态产物作视觉参考；生产 chunk 仅是受控基线，不是最终源工程。
- 旧域名下 `/dashboard`、`/auth`、`/oidc/authorize` 等 URL 保持；在虚拟主机/反向代理层隔离与当前 Vue 的同名路径。不能为了目录方便擅自给客户端加 URL 前缀。
- 登录、RBAC、数据来源与 backend 恢复域一致；前端隐藏按钮不是授权控制。
- 开发前提取完整 request/response、分页、筛选、日期与错误格式；23 条路由记录只是最初清单，数据库/后端发现的遗漏业务也必须纳入。

### 3.3 isthmus：Bun/TypeScript 运行核心 + 现有 Go 执行治理

- 建议新增 `execution-plane/isthmus-runtime/`，重建 transport、turn、CLI、pool、session、MCP、credential 生命周期模块。
- 保留 `isthmus.v1.Messages` wire contract，HTTP/WS/gRPC 共用同一个 TurnEngine；不能将其直接改名为 execution.v1。
- Go worker 通过 slot 内受控 IPC/私有连接接入运行核心；host-agent 仍负责票据、epoch、节点与固定出口，不能把 Docker Socket、KMS 或代理密码交给运行核心。
- IPC 只在 slot 内开放，具有进程身份/版本绑定、长度边界、取消及背压；秘密不能放命令行或日志。
- 现有 oauth_api 与恢复核心保留明确模式边界。不能把“走直接 API”静默声称为“重现 CLI 原行为”。
- Bun/CLI 精确版本先通过部署元数据和基线验证固定；不能用内嵌 MCP/SDK 的版本字符串冒充实际 CLI 版本。

### 3.4 CCMAX 与多 Provider 的职责

- Portunex 的 Provider 不等于 CCMAX Claude account：OpenAI/Gemini/其他已发现入口分别冻结能力清单与授权方式。
- Claude 隔离执行可复用 CCMAX → execution-plane 的受信任控制/投影边界，但必须通过幂等应用接口和稳定映射接入；禁止 backend 直接写 CCMAX MySQL。
- 对每个对象明确唯一 owner：Portunex 用户/订单/积分在 backend 恢复域；执行槽位/租约/Vault 在 execution-plane；CCMAX 既有账号/代理保持其现有权威。
- 新旧两条请求链不得同时为同一调用结算，也不得对同一账号形成两个有效执行所有者。

## 4. 目标装配关系

```text
旧网页/客户端/SDK（原 URL 与协议）
  ├─ Portunex React UI → Go backend 的 Portunex 兼容域
  │                       ├─ 独立 Portunex 恢复数据库
  │                       ├─ 认证/OIDC/用户/Key/价格/账务/用量
  │                       └─ Provider 路由与明确的执行适配器
  └─ isthmus 旧协议 → 受认证的兼容入口 → 绑定的账号 slot
                                             ↓
                                  Go worker + Bun runtime
                                             ↓
                                    固定出口与授权上游

CCMAX / orchestrator / host-agent：复用既有控制职责
现有 Vue 站点、Sub2API 与 legacy 请求：保持原路径，独立回归
```

旧 isthmus 请求没有 slot/account 字段，必须从受认证 listener/客户端身份和服务端绑定获取，不能任由 body 指定账号。外部旧端口可由受控入口兼容，不要求每个 worker 端口继续公网发布。

## 5. 必须恢复的功能范围

### A. Portunex 身份与权限

- 密码登录、注册、logout、session 管理、magic link、已观察的第三方 OAuth。
- 用户/管理员角色、用户自助及管理端 API Key 创建/更新/删除/轮换/设置/统计。
- OIDC client、consent、authorization code、token、刷新/撤销及 discovery/JWKS/userinfo 等实际存在的 endpoint。
- 冻结 cookie/header 名、token 传递方式、重定向、PKCE/state/nonce、密码哈希验证与错误合同。
- 使用本地模拟 OAuth/邮件/OIDC 测试，不发送真实邮件、真实登录或新建客户应用。

### B. Provider、路由与限额

- Provider CRUD、credential 管理、导入/批量动作、健康/错误分类/冷却/可用时段。
- 模型目录、alias、alias/provider/model/type 多层定价及有效时间/优先级。
- weighted/sticky routing、RPM/RPS/并发、窗口配置/状态/reset；算法要以实证恢复，不能只实现相同字段。
- 各 Provider 的模型 API、流式与非流式、用量与错误映射；未查证的 OpenAI/Gemini 等路径列为发现任务，不虚构支持。

### C. 积分、订阅、订单与兑换

- points、充值控制、订单创建/支付状态/结算/过期/重试、支付渠道配置。
- 套餐、用户订阅、日/周/月窗口、消费明细、价格快照、兑换码和防重复兑换。
- PostgreSQL NUMERIC/定点运算，不使用二进制 float 处理账务；明确货币/积分单位、舍入时机、价格优先级、缓存 Token 价格。
- 支付回调验签、幂等键、业务事务与审计；测试只能用本地 stub/供应商沙箱，不能产生真实订单或付款。

### D. 用量、请求追踪和运营

- 用户/Key/Provider 统计、usage、trace 查询、分页/筛选/权限、价格明细。
- 登录与权限变更、Provider 变更、发布/回滚/drain 等操作审计；请求 ID 可关联，但不得落密钥或完整敏感参数。
- 明确线上 request-log 实际存储介质及保留策略；PG 表清单并不能证明完整日志后端。
- 日志/账务写入失败不能伪装成功，不得对管理员开放无审计的秘密/正文导出。

### E. isthmus 外部合同与执行

- HTTP `/v1/messages`、WS 同路径、Messages Create/CreateStream、TLS/mTLS/client pin、健康与受保护的管理信息。
- WS 1-byte tag 帧、单连接单 in-flight、busy/cancel/下一轮参数清理、keepalive/背压。
- gRPC bytes/oneof/presence、start/chunk/error/half-close；非流式 SSE→Message 聚合器，不能直接拼接 SSE 当 JSON。
- status/status_text/合法 headers/原始错误 body、跨 chunk UTF-8/SSE/JSON、首字节前后取消与重试界线。
- CLI 控制 ACK、stream-json/PTY、进程池、预热、reservation commit/cancel、session reset/one-shot/工具循环、MCP pending-call 与 tool-result 续接。
- credential lease/epoch、刷新 single-flight、Vault commit→version ACK→新请求生效、旧进程退役、至多一次条件成立的 401 恢复。
- 独立 OAuth task API：确认实际消费者后恢复创建/查询/提交 code/取消/过期/重启语义。

`count_tokens`、本地 models 汇总等属于当前平台目标或 Portunex 待验证能力，不得标成已经从 isthmus 恢复的代码。

### F. 构建、部署与灾备

- 可复现源码构建、固定依赖/镜像 digest、SBOM、私有制品、版本与迁移清单。
- 安装/建实例/启动/停止/重启/升级/回滚/健康/状态/日志；重要旧 CLI 参数若有消费者则提供兼容 façade。
- 原加密 `.pkg` 是旧分发格式，不是应用协议；第一版优先自主构建的新制品。需要接收旧 pkg 必须另列验收，不猜解密算法。
- 镜像自含只读代码与固定工具，状态/会话与秘密分开；不复制共享 `/app` 清空后解 tar 的更新机制。
- 四类备份：源码与文档、二进制/镜像、数据库/PITR、密钥/证书。数据备份必须加密，恢复所需密钥不能只存在同一份备份里。
- 防止再次换机丢档：新机器仅凭私有仓库/制品/受控秘密可跑测试与构建；恢复演练有时间、对象和结果记录。

## 6. 与旧 PRD 的关系和不能静默复制的行为

- 旧 execution-plane 的 slot/provider、隔离、Vault、epoch、Outbox、固定出口及上线安全门仍有效；5.5c WIP 保留。
- 新规划覆盖整套恢复的功能范围与优先级，不把旧 5.6–11 直接改成完成，也不自动废除旧安全原则。
- 原 PRD 的每回合 CLI 与线上池化/session 策略不同；原 PRD 排除生产 MITM，线上却包含本地 relay/消息改写。必须建立字段级变换合同和 ADR，未经确认不能把两者声称为同一实现。
- 恢复外部协议不等于启用身份伪装、计费归类规避、反风控或绕过权限。相关遗留行为只做事实记录，不作为重建这些规避机制的授权。
- refusal/内容终止修饰、遥测/设备身份及非标准客户端模式不得混入通用流处理器。若它们影响兼容，台账明确未实现/不纳入/待评审；不能隐藏兼容缺口。
- `GET /` 的身份/Token 返回只能在经授权的旧管理边界兼容，不能原样暴露在普通公网消息入口。
- 禁止 SYS_ADMIN/无限额/隔离检查失败继续等默认降级；需要的进程能力先验证当前 provider 能否安全支持。

## 7. 数据恢复和迁移规则

1. 本轮仅做结构发现，没有承诺恢复所有历史数据。
2. 经批准取得一致性逻辑备份/必要 WAL 与配置清单，放入访问受控且加密的存储。含秘密的备份不进入 Git 或网页目录。
3. 在独立 PostgreSQL 18 恢复库演练，确认 extensions（已见 citext）、collation、sequence、索引、函数、触发器、约束、时区和所有业务表。
4. 从 schema/SQL线索/fixture 重建 migrations；不把二进制 strings 直接执行为 SQL，不把旧运行中 tar 视为一致性备份。
5. 用匿名或合成数据完成日常开发；受控真实备份仅用于授权的迁移验证，不在会话输出任何行内容。
6. 检查用户/Key/provider ID、软删除状态、订单/订阅引用、NUMERIC 总额及逐笔 hash/统计对账；未知规则先阻止写迁移。
7. 新旧生产写入只有一个权威；切换前明确短暂停写/增量同步/回滚策略，禁止未定义双写。
8. 切流失败只回到已证明可用的原服务/数据边界；已迁凭据不能自动回落到明文 legacy。

## 8. 开发顺序

| 阶段 | 交付 | 通过门槛 |
| --- | --- | --- |
| R0 保全 | 全组件哈希、私有恢复材料、脱敏配置/结构快照、备份/恢复方案 | 不含秘密的开发基线可在新机器校验；生产备份须单独获准 |
| R1 合同 | API/页面/DB/协议矩阵、差异台账、OpenSpec、fake fixtures | 每个模块有证据/owner/测试/未知项，完整性可追踪 |
| R2 基础 | Portunex Go 域/独立DB、React工程、Bun runtime契约、CI | 干净环境构建；旧系统回归不受影响；mock不冒充完成 |
| R3 业务骨架 | 用户/Key/session/RBAC、Provider/模型、基础查询与页面 | 假数据下管理闭环，越权/重复写/分页/旧envelope测试通过 |
| R4 账务与身份扩展 | 价格/窗口/积分/订单/订阅/兑换、OIDC/OAuth | 幂等结算/精度/权限/过期与回放通过，支付/外部身份仅沙箱 |
| R5 协议与路由 | 各模型 API、isthmus三入口、SSE聚合、错误/取消/背压 | 全 fake transport parity；首chunk早于上游结束，无全响应buffer |
| R6 执行与凭据 | CLI/pool/session/MCP、Token刷新、Vault、独立OAuth任务 | fake进程/刷新竞态/工具续接/旧epoch/drain全部通过 |
| R7 平台接入 | worker桥、生产host-agent数据面、gateway dispatch、日志/审计 | 本地Docker端到端、无双活/无直连/不重复计费 |
| R8 整套回归 | 完整页面业务流程、数据迁移演练、协议对照/故障/容量 | 所有兼容项PASS或获准差异；不是只跑isthmus |
| R9 发布与恢复 | 自主制品、灰度/回滚、备份与异机恢复演练 | 没有旧电脑也能恢复；部署仍等待单独授权 |
| R10 受控切换 | 授权canary、观察、逐步替换旧服务 | 安全/账务/可观测门禁满足，审批后才切生产 |

R3/R4 与 R5/R6 可以在 R1/R2 后并行开发，但 R7/R8 必须汇合。**不是先做完数月的 UI 再发现执行器不兼容，也不是只做好执行器就宣布整套完成。**

首个开发切片：R0/R1 的可复核基线及合同测试框架，随后用假数据打通“Portunex 登录→用户/Key/Provider 列表”和“isthmus 三入口→fake turn”两个独立闭环。它们的交付名称分别为业务骨架和传输兼容，不是整套复刻。

## 9. 验收原则

- 三类差异：必须一致、已批准安全/实现差异、未知待验证；未知项不能算通过。
- 本地参考版本只能在无真实凭据、隔离网络、测试存储、固定 fake child/upstream 下启动；不得导入整包 `.env` 或生产目录后直接运行。
- 不承诺模型自然语言输出逐字一致；比较请求语义、原始协议字节边界、确定性 fixture、工具/用量/错误与资源释放。
- 账务、权限、Token不变量必须严格成立，不能因“看起来功能一样”放宽。
- 同值用户/Key/订阅 ID 的跨域测试必须证明：认证与账务缓存不混用、失效消息不越域、锁和后台任务不互相抑制、配额/幂等记录不碰撞。
- 当前四个 Go 包测试通过只是库基线；完整验收另含 backend/CCMAX/UI、Docker、DB、迁移与跨组件测试。
- 每阶段结束更新证据、失败项与下一步；只有真正完成才能勾选。性能容量以实际基准定义，不凭生产容器数量推算。

## 10. 开发授权与生产边界

用户在确认“全套范围”和“调用方接口兼容”后，已明确要求开始第一阶段开发。当前按以下落点推进：

- **Go backend 中重建 Portunex 业务域 + 独立恢复数据库**；
- **保留独立 React 子应用，现有 Vue 不替换**；
- **Bun/TS 恢复 isthmus 核心，Go execution-plane 负责治理**；
- 先建立合同/恢复基线，逐阶段开发，不将不透明二进制直接当新项目上线。

`web-reverse-master` 的 Phase 3 规划确认门已由本次开发指令满足。第一阶段先实施 `restore-online-stack-foundation`：模块化结构 ADR、文本证据/WIP 留档、合同发现目录与校验器、独立 proto/WS codec 和离线测试。旧 execution-plane 的 5.5c WIP 不覆盖，不启用 execution_onboarding，不迁 migrated；后续业务开发按合同证据逐项推进，不因底座完成而宣称业务已兼容。

后续“继续”指令对应`restore-online-stack-demo`本地演示切片（ADR-002）：Go合成会话/列表与独立React连通，isthmus只使用fake turn实现HTTP/WS。它不等待线上可连接才能验证本地模块协作，但也不替代旧合同取证；数据库业务、gRPC和真实执行内核等仍按上述阶段门槛逐项推进。

服务器风险归档的移出、真实数据备份、测试账号/支付沙箱、私有备份目的地及生产切流分别需要明确范围和授权，不由确认开发规划自动代替。
