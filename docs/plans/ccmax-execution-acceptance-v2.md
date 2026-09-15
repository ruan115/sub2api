# CCMAX 转发与隔离验收收尾计划

日期：2026-09-16。用户要求先规划，再继续完善至验收。本计划以当前 `codex/claude-execution-plane-v1` 为基线，不重写已完成的历史验收。

## 范围与停止线

- 维护 CCMAX / execution-plane / isthmus 执行侧；登录、产品权限、计费仍由既有 Sub2API 链路负责。不重启 Portunex 整套业务恢复。
- 所有开发、合成请求、故障注入、压测均在本地专用环境。不得修改线上数据库、配置、UI、进程、镜像或路由，不从线上借号发真实请求。
- 本轮授权包含本地实现、测试、review 和阶段 Git 提交；不包含 push、部署或开启 `execution_onboarding` / 改账号为 `migrated`。
- “本地工程验收”“线上兼容证据”“生产上线许可”分开记账。真实 canary 会消耗账号额度、产生记录，当前边界下不执行；到该门槛停下说明缺项，不伪造通过。
- 线上参照以已保全的 2026-09-14/15 证据为准，不宣称与此后线上状态持续一致。兼容的是已确认的接口与行为；未知 pipeline 分支必须显式标注。

## 当前差距

DP1 的真实回环 RPC / mTLS / fencing / 中继已通过，Docker 基础网络隔离有历史 fake E2E；但 gateway 新路由、host-agent 生产装配/签票/可信快照、真实 CLI/MCP、刷新、完整故障与容量验收仍缺。实际 HTTP worker 只支持 oauth_api，先聚合最多 2 MiB 响应后发送，不能把中继测试当作真实流式闭环。

## 模块与开发顺序

只在阶段实施时创建目录，不预建空模块。尽量复用既有权威/协议，不复制第二套 credential、billing 或 runtime 状态源。

| 阶段 | 实现范围 / 模块 | 放行证据 |
| --- | --- | --- |
| A：实际 HTTP 执行器 | `execution-plane/internal/worker/upstream/` 有界响应转发；`upstreamusage/` 独立 usage 观察；`worker/process.go` 仅适配装配 | 首段早于上游结束、超过 2 MiB 累计 SSE、慢读背压、取消、出错后无伪完成/重试、非流/错误体上限、合成 usage 对照、race/vet |
| B：宿主控制与权威装配 | 既有 `control/`、`runtime/store/`、`ticket/`、`lease/`、`hostagent/`；按需新增职责明确的 snapshot/runtime registry 模块；`service/` 只装配 | 受认证签票、跨账号/节点/旧代拒绝、断连后拒绝服务、重连不复活旧实例、只查找已存在 runtime、ready 不虚报；签私钥仅控制面 |
| C：CCMAX 网关接通 | 优先新增独立 `ccmax-manager/internal/executiongateway/`，旧 `gateway.go` 仅接线；保留既有变换/usage所有权 | legacy 行为不变，migrated 不回退明文，Messages/count_tokens/models/Chat Completions、错误与取消端到端、开关默认关闭 |
| D：CLI / isthmus | `execution-plane/isthmus-runtime/src/runtime/{cli,session,pool}/`、`src/tools/mcp/` 与 worker 内部适配 | 固定 CLI、无 builtins/严格 MCP、增量协议、工具续接、会话隔离、超时杀子进程、fake 与官方同版本 CLI 的无外联合成测试；Bun 停机缺口单独关闭 |
| E：凭据与生命周期 | 复用 `credential/`、onboarding、outbox/lease；CCMAX 运维 API 不另建权限权威 | 刷新单飞与原子版本切换、旧票据/出口撤销、drain/recreate/archive/delete/restore、重启恢复、脱敏审计；合成凭据扫描 |
| F：整链隔离与兼容 | 按职责组织 E2E / replay / load 测试目录，独立 Docker/DB/Redis/fake KMS/COS | Sub2API/CCMAX入口→host-agent→worker→假上游；HTTP/WS/gRPC合同矩阵；固定出口、跨槽位/宿主/公网绕过、分区/重入/取消/刷新/慢消费；1000连接与24h稳定性 |
| G：发布准备及另行授权 | 版本/制品矩阵、恢复演练、canary/rollback运行手册 | 本地恢复演练通过；真实 canary/线上操作保持待授权，不以 fake 结果替代 |

B/C/D/E 内部还应按接口和依赖拆小提交。A 可以先独立完成，不等待生产装配；但 A 完成不会勾选 Phase 6 总项。F 只能在所用实际组件接通后验收，不把几段互不相连的 PASS 拼成全链 PASS。

## 第一批 A 的明确合同

1. 不改变请求正文、已有凭据来源、账号选择、代理选择或生产开关；不执行原始 JS/ELF。
2. HTTP 请求大小、单帧大小、累计流大小、需聚合的 JSON 大小分开处理。保留当前请求/非流及错误响应的 2 MiB 边界，本阶段不声称其与线上全部大小限制一致；SSE 不按 2 MiB 累计截断，按固定小块发送且无整流队列。
3. 只在实际 HTTP 响应头为 SSE 时走流式，原始字节顺序不变。headers 先于正文，缓慢下游直接阻塞下一次读取。取消应关闭响应体并传到 HTTP 请求。
4. 输出开始后读取/发送错误不得返回成功 Completed，也不自动重试。传输 EOF 与语义消息完整性区分；不把截断的 SSE 记为完整模型结果。
5. usage 解析为旁路观察，不重写正文；只提取白名单数值字段，不混入文本、token 或未知键，不将缺失值伪造为 0。支持 JSON 及分块 SSE 起始/增量 usage；超大/非法输入必须有界且不能伪称计数可靠。
6. gzip 由现有 Go transport 解码时可使用解码后响应；仍带未处理 Content-Encoding 的响应显式失败，不把压缩字节伪装成明文 SSE/JSON。
7. 测试使用 httptest / 自建 transport / 合成事件，禁用依赖网络下载，不启线上连接或默认应用监听。

实现/review 补充：HTTP 传输和 usage 观察共用同一 SSE 分类；非空但畸形的 Content-Type 在读取/输出前拒绝，避免分支不一致。原始 read buffer 借用给传输 Sink，RPC 适配器在交给 protobuf 前复制单块，避免后续读取覆盖已发送消息。SSE 事件/行观察上限 1 MiB，未知事件在活跃消息内可忽略，但不产生 usage 或完成；这个旁路限制及错误后的部分 usage 尚不能等同于全线上合同。

## 质量与进度规则

- 每阶段：先补设计与合同 → 实现 → 正向/拒绝/故障测试 → 独立 review → 修复 → 主代理复跑 → 文档与 Git 提交。
- 证据记录实际命令、当前提交、测试依赖、未执行项。早期 Docker PASS 与最新代码的真实 E2E 分开。
- 不为得到绿色结果删除失败门槛、降级证书校验、跳过凭据检查或降低验收要求。
- 发现必须用真实账号、改线上或扩大 UI 范围才能继续的步骤，停在该步骤；仍可先做不依赖它的本地工作。

详细任务与本轮结果见 [OpenSpec 台账](../../openspec/changes/complete-ccmax-execution-acceptance/tasks.md)。原 [§31 上线门禁](../prd/claude-execution-plane-v1.md#31-上线门禁) 仍有效。
