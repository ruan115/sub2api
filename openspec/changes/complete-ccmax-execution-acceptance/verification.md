# 验证记录

日期：2026-09-16。A 总规划提交 `c8380da` 先于实现 `fe9c3b8`；B1 细化规划 `e07bb7d` 先于其实现。A 与 B1 的本地库/合同验收通过；B 总项（B2–B4）、C–G 与整链/生产门槛仍开放。

计划复验：

- `cd execution-plane && GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local go test -race -count=1 -timeout=120s ./...`
- 同样离线依赖设置下执行 `go vet ./...`。
- 针对实际 worker upstream 的重复流式/取消/背压测试。
- 检查没有默认监听、没有改线上 UI/配置/数据或启用迁移标志。
- 独立 review 后修复，主代理复跑相关测试；Git 只含源码、合成测试及脱敏文档。

真实模型、生产数据库、SSH、canary、部署均不在本轮验证路径。

## A 实际结果

| 验收点 | 结果及边界 |
| --- | --- |
| 模块拆分 | `worker/upstream/` 管理 HTTP 响应资源/有界读取；`worker/upstreamusage/` 只观察计数；`upstream_adapter.go` 持有原凭据注入与 RPC 适配；从 `process.go` 移除整包执行逻辑 |
| 实际执行器首段 | 新 `TestHTTPWorkerRPCFirstChunkBeforeEndAndLargeStream` 使用实际 HTTP 服务、worker Executor 和带身份票据的 gRPC；上游被通道阻塞不能结束时，客户端已收到 headers/首段 |
| 大流字节保真 | 同一实际 HTTP→worker gRPC 测试累计超过 3 MiB，按 SHA-256 对比；每块 ≤32 KiB；不受旧 2 MiB 累计截断。独立 relay 模块另测 6 MiB+17 bytes |
| 背压 | 同步慢 Sink 测试断言下游返回前不继续 Read。不是完整 gateway 慢客户端或1000连接压测 |
| 取消/半关闭 | 实际 gRPC 客户端取消后，HTTP handler 收到 Context 取消；请求半关闭仍可完整收到 SSE。模块另验证阻塞 Read/Send、deadline、close once |
| usage | JSON及SSE开始/增量提取固定整数白名单，累计取最大不相加，保留缓存5m/1h和thinking分桶；缺失不补0；原始正文不改写 |
| SSE边界 | 覆盖任意字节切块、UTF-8、CR/LF、BOM、多data行、注释/未来事件；error、缺stop、非法JSON/重复键、异常状态、事件超限不产生可信Completed usage |
| HTTP错误 | 400/401/429/500状态及有界正文保留，无synthetic usage；此时Completed仅表示HTTP response-end。实际截断HTTP无Completed；不重试、不跟随302/307/308 |
| 大小/编码 | 请求/非流/错误体保留各自2MiB限制，不增加凭据上限；Go transport解码gzip可用，剩余未解码编码拒绝；非空畸形Content-Type输出前拒绝 |
| 凭据与入口 | 现有ActiveCredential注入和Destroy保留，入站Authorization/Cookie不透传；没有改gateway、UI、生产开关或账号迁移状态 |
| 全模块 | 主代理两轮 `GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local go test -race -count=1 -timeout=120s ./...` 及 `go vet ./...` 通过 |
| 重复回归 | 主代理最终10轮 `go test -race -count=10 -timeout=90s ./internal/worker -run 'TestHTTPWorker(RPC|Malformed|Incomplete|Actual)'` 通过 |
| Docker fixture | `go test -tags docker_e2e -run '^$' ./internal/hostagent` 仅编译通过，未运行Docker E2E，未启动VM |

新增 43 个顶层 Test 与 2 个 fuzz target（含子用例）。usage模块作者执行 SSE fuzz 10秒/177,959次和 JSON fuzz 5秒/49,627次均通过；主代理复跑其race与完整模块回归。这些是合成解析输入，不是真实模型token计算。

`EXECUTION_MYSQL_TEST_DSN`、`EXECUTION_REDIS_TEST_URL`、`CCMAX_EXECUTION_MYSQL_TEST_DSN` 均未设置，因此专用真实数据库/Redis集成没有执行，不能记作PASS。全部实际HTTP监听均为测试自建回环端口并由测试收尾；没有默认应用listener、真实凭据或原始线上正文。

## Review 与修复

两位代理交叉审查非本人模块，主代理审查及集成，发现并关闭2项P2：

1. Relay复用的读取buffer被RPC protobuf引用：适配器改为复制有界单块，并用不Clone、保留原proto引用的测试覆盖读取返回后的覆写。
2. adapter忽略MIME参数解析错误、Relay却要求解析成功，造成usage分支不一致：改为共享 `IsStreaming`，畸形非空Content-Type在读取/输出前失败；覆盖两个读取入口与实际worker无headers/body/completion输出。

Reviewer已复核关闭上述两项，未发现其他阻碍A提交的问题。另一位reviewer独立执行取消/deadline/背压/编码/失败清理定向10轮race通过。

## 本阶段没有宣称完成的内容

- host-agent生产装配、可信投影/执行签票、gateway接线、CLI/MCP、刷新/生命周期仍未实现本地全链闭环。
- 现有RPC只在Completed上有usage；中途失败的部分usage没有额外结算通道。没有通过伪成功传递它；错误后部分用量处理保留为后续C阶段协议门槛。
- usage观察的SSE行/事件1MiB、JSON深度64/节点数262144与非流2MiB都是明确的本地安全边界，不声称线上大小/未知变换完全兼容。
- 旧原件/官方ARM64 CLI→stub不曾接入本轮生产应用；真实模型请求仍0。当前修改未部署，线上未动。
- 原 `add-claude-execution-plane-v1` Phase6–11总项和PRD§31保持未通过。

## B1 实际结果

设计及停止线见 [B1设计](b1-design.md)，模块边界见 [executionauthority](../../../execution-plane/internal/executionauthority/README.md)。没有新增RPC或默认监听，也没有向host-agent分发数据库或签私钥。

| 验收点 | 证据与限制 |
| --- | --- |
| 会话来源 | NodeControl把服务端当前session.id写入CommandResult；没有在节点报文中新增可伪造字段。SQL与Memory均在写入前核当前connected会话 |
| 持久证明 | 迁移013只新增nullable observed_control_session_id；不回填历史值。旧无会话Observe/Apply、失败/不健康结果会清除证明；schema gate要求新列 |
| 当前镜像 | 健康成功结果需匹配pending image/deadline，store在同事务锁定active assignment并精确比较ExpectedImageDigest；无provisioning job也不能跳过。SQL/Memory错误不部分落命令、任务或证明 |
| 一致性读 | SQL单条只读JOIN及精确身份/会话/镜像比较；Memory同一RLock，拒绝不一致/缺失/重复active状态。纯值输出无可变仓库引用，不读取credential或route cache |
| 真实控制RPC组合 | `TestAuthenticatedCommandObservationSnapshotRequiresNewSessionConfirmation` 使用本地实际TLS1.3 NodeControl、注册证书、命令派发/结果、Memory仓库与Source。首次观察前拒绝、当前结果允许、断连拒绝、仅Hello/heartbeat重连拒绝、新受控结果恢复、旧连接证书撤销拒绝 |
| 负例层次 | 同一fixture的旧会话迟到结果及pending/result B但assignment A两个负例，直接调用recordCommandResult进行控制层→存储层组合，不是实际stream.Send负例；SQL锁/回滚另由sqlmock覆盖 |
| 只读活动会话 | 未accepted、弱/未验证TLS、证书失效/撤销、durable session不同、上下文取消和读取期间detach/替换均拒绝；验证存储时不持session-map锁 |
| 观察期限 | Source在读取/活动会话验证后再次检查观测/节点时间及durable lease有效期；不把查询或heartbeat当新观察。Control在observer前冻结ReceivedAt/ObservedAt，回调后复核deadline；observer/storage上下文受命令期限约束 |
| Resolver组合 | 实际FencedResolver+Source+内存租约验证跨账号/节点/旧epoch/generation与Redis替身故障拒绝；lookup期间重连，包含新会话已重新确认同实例的情况，由ControlSessionID前后比较拒绝 |
| 最终模块回归 | 主代理最新完整 `GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local go test -race -count=1 -timeout=120s ./...` 与 `go vet ./...` 通过 |
| 重复回归 | 主代理对control/executionauthority/store中的会话、观察、Source、binding、事务/取消关键用例执行 `go test -race -count=10 -timeout=90s ... -run 'TestCurrentSession\|TestAuthenticatedCommandObservation\|TestHealthyObservation\|TestSource\|TestReadExecutionBinding\|TestMemoryCommand\|TestMemorySession\|TestSQLCommand\|TestCommandResultRejects'` 通过。新增镜像专门测试在最终全模块运行，另一reviewer独立三轮镜像/取消/binding race通过 |
| Docker fixture | 最新 `go test -tags docker_e2e -run '^$' ./internal/hostagent` 编译通过；没有运行Docker、启VM或使用真实账户 |

两位代理交叉审查非本人模块，主代理复核并集成；已关闭：lookup前后漏比session、observer延迟放大期限/新鲜度、Memory等任务锁期间取消后仍写入，以及pending/result镜像未关联durable assignment的缺口。相关拒绝回归均保留；review范围内无剩余已知P1/P2阻碍本切片提交。

### B1 不能代替的门槛

- 没有实际MySQL迁移/并发事务/死锁或索引性能证据；013脚本只进入Git，不自动应用。新结果事务node→assignment锁顺序与现有release顺序可能形成死锁，需隔离数据库验证失败/重试行为，当前不可据此批准生产。
- 尚无主动fresh Inspect/Health调度；超过45秒后拒绝是预期安全行为，不得拿缓存心跳维持Ready。
- Source只在持有对应活动控制流的orchestrator内有效；其受认证RPC、跨控制面实例访问、签票、runtime registry和进程接线仍在B2–B4。
- `Snapshot.Ready`仅代表分配/会话/租约候选，不是worker模式、active credential/proxy版本或业务授权已经核完。
- Lookup会话比较不是已经打开的stream的原始session绑定；快速断连再确认可能发生在周期检查之间。持续流原会话撤销、连接回收仍待B3/B4，不能宣称即时中断已验收。
- 无真实模型调用（累计仍0）、无SSH/生产数据库/配置/UI/进程/路由操作，未push、未部署、未启迁移开关。整链Docker、CLI/gateway、刷新、1000连接与24小时稳定性、真实canary均不能由本阶段PASS替代。
