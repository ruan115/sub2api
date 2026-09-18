# 2026-09-18 13:17:37 — isthmus 开发规划与 Claude 接手说明

时间：Asia/Shanghai（UTC+08:00）。用户要求先规划再开发、以时间命名，并准备交给
Claude 继续。本文件为本轮及后续的接手入口，不要求读取整段聊天记录。

## 1. 接手时先确认，不要从零重写

- 仓库：`/Users/ruanyang/My-project/api/z/sub2api`。
- 分支：`codex/claude-execution-plane-v1`。不要把 execution-plane 改动混到
  `codex/onboarding-user-1688`；未确认前不切分支、不丢弃工作区、不 force push。
- 规划基线：`d1e722a`。已提交的最近成果：
  - `82863b6`：严格既有实例 START/物理 CID/bootstrap/mTLS 验证记录。
  - `67e3b77`：控制面默认关闭的 SQL 收据与独立 Redis 租约签发依赖。
  - `4a540f9`：默认关闭的 host-agent 生命周期服务真实二进制入口。
  - `d1e722a`：S2b4 review、测试和剩余门槛。
- 本文件末尾的执行记录会补上本轮后续提交。以上是基线，不表示最新 HEAD；接手
  必须重新执行 `git status --short`、`git branch --show-current`、`git log -8 --oneline`。
- 以下是已有、暂停且未提交的工作，不是垃圾文件，不得覆盖/清理/偷偷提交：
  - `execution-plane/isthmus-runtime/image/lab/build.py`
  - `execution-plane/isthmus-runtime/image/runtimekit/`
  除非用户明确恢复该方向，否则绕开它们；不新增另一套通用镜像构建框架。

## 2. 产品范围与不能改变的事实

目标是恢复可维护的 **isthmus-vm-base + isthmus/CLI 执行容器 + host-agent + CCMAX
桥接**。这里的“VM”是 Docker/runc 隔离容器，不开发虚拟机内核，不换成 KVM。
Sub2API 已有登录、权限、计价、模型价格、用户倍率和账务继续沿用，只回传执行 usage。
不恢复 Portunex 全业务，不重做控制台视觉，不更改既有调用方 HTTP/WS/gRPC 合同。

基础镜像不等于完整运行服务。基础镜像构建通过、固定真实 CLI 单实例合成调用通过，
不能代表 app/session/pool/MCP、完整 runtime 镜像或线上等价已经完成。线上参照来自
已保全材料，不保证能还原原作者源代码或未知行为；未知项须明确记录，不能猜成已证实。
每实例独立机器标识、私钥、证书和生命周期要有证据；TLS 配置与固定客户端版本匹配，
不做随机 TLS 指纹伪装，不把“目录不同”当作私钥独立的证据。

唯一分数来源：[100 分验收台账](isthmus-container-delivery-v1.md)。本轮开始总体 **32%**：
镜像40%、运行服务60%、出口隔离40%、身份20%、宿主40%，CCMAX桥接/完整生命周期/
整体验收各0%。分数不是代码完成率或上线就绪度；只补源码、文档或 mock 不提前加分。

## 3. 已有调用路径、真实缺口与模块位置

| 功能 | 当前入口/目录（相对仓库根） | 已有能力 / 不得误认完成的部分 |
| --- | --- | --- |
| 基础镜像与制品锁 | `execution-plane/isthmus-runtime/image/` | 基础构建/固定部分制品；完整派生 runtime 与恢复冷启动未完成 |
| CLI 执行 | `execution-plane/isthmus-runtime/src/runtime/cli/`、`src/app/cli/` | 固定真实 CLI 单轮文本/JSON/SSE合成上游；非完整会话池 |
| Docker/runc 隔离 | `execution-plane/internal/provider/docker/` | 严格镜像/资源/用户/独占 Internal bridge 接纳；swap配置缺口为本轮首项 |
| 实例本地身份 | `execution-plane/internal/runtimeidentity/`、`runtimebootstrap/` | 实例内私钥/CSR与原子公开证书安装；不向宿主输出实例私钥 |
| 认证签发 | `execution-plane/internal/runtimeenrollment/`、`internal/service/runtimeenrollment/` | 认证RPC、SQL receipt、独立Redis校验；生产权威lease writer仍缺 |
| 宿主启动 | `execution-plane/cmd/host-agent/`、`internal/hostagent/daemon/`、`lifecycle/` | 预签发node身份→TLS控制→严格START；仅生命周期，无业务数据/出口服务 |
| 节点调度边界 | `execution-plane/internal/nodepolicy/`、`placement/` | lifecycle-only 节点明确排除业务调度，含sticky和无约束请求 |
| 控制面与任务状态 | `execution-plane/internal/control/`、`runtime/store/`、`service/` | 有现成binding/命令/租约结构；不另起第二份状态权威 |
| 实际管理通道实验 | `execution-plane/test/dockerbootstrap/`、`image/lab/livebootstrap/` | 已做真实Docker CSR/公开证书传递；不是实际跨容器mTLS验收 |

`cmd/host-agent → daemon.SelectRunner → 同一 Docker provider + lifecycle.New +
ControlClient → 严格已有 CID START → CSR → 认证签发 → 实例安装 → worker mTLS`
是现在接好的生命周期路径；**其中没有** CLI业务激活、数据面转发或活租约续期。

必须保留这些失败关闭边界：

- `EXECUTION_HOST_AGENT_RUNTIME_ENABLED`、`EXECUTION_RUNTIME_ENROLLMENT_ENABLED`
  默认关闭；不打开 `execution_onboarding`，不把账号标为 `migrated`。
- 当前 daemon `/readyz` 是503、`production_ready=false`；不能为了演示改成200。
- Redis PING 只证明连接可用。缺准确授权租约时签发拒绝，不加 Memory fallback，
  不在签发 handler 中临时 Acquire/Grant 来让测试过关。
- START 必须准确物理CID，不能Create/recreate/替换容器；普通TCP/INSPECT不能复活
  失败或身份漂移后的认证状态。宿主node密钥与每实例server密钥是不同角色。
- 当前 provider 不允许 host bind/named volume；旧实验的只读 worker bind 是独立
  实验例外，不能搬进 provider。不要直接解开旧 `docker_e2e_test.go` 的保护性失败。

## 4. 顺序规划与完成标准

### P1 / S2b5a — 先补 provider 的禁止 swap 策略（本轮先开发）

- [ ] 创建请求显式 `MemorySwap = Memory = spec.Resources.MemoryBytes`，两者为正。
- [ ] Engine JSON读写字段完整；省略/null/0/-1/不等于Memory全部拒绝。
- [ ] 共同只读接纳门禁覆盖 Inspect/InspectSlot/Create-adoption/START/endpoint/
  ValidateExisting/bootstrap 前后复核；失败不自动更新/重建/清理旧容器。
- [ ] 新测试放 `provider/docker/` 的独立 swap 文件；已有资源序列化/正向fixture
  作最小更新。不新增可关闭此安全策略的开关，不改 host swap/sysctl/daemon配置。
- [ ] 正向用例、各类漂移/缺字段拒绝、零写入、实际本地 HTTP 编解码、review、race/vet。

依据：[Docker官方资源限制](https://docs.docker.com/engine/containers/resource_constraints/#--memory-swap-details)，
`MemorySwap` 表示内存与swap总量，等于正Memory才禁止swap；0不是禁止，-1不是禁止。
这是请求和接纳策略，不证明宿主内核执行；Linux cgroup 实证留在 P2。现有不满足新
策略的容器只会被拒绝，不执行在线迁移或修复，不把旧实验PASS升级为新策略实测。

### P2 / S2b5b — 当前 provider 两实例真实管理与 mTLS

入口条件：P1完成、只读预检通过、实验镜像内容/摘要明确、仅本任务资源可证明归属。

1. 复用既有基础镜像/固定制品与认证组件，采用最小不可变实验派生镜像，公开程序
   放镜像内，不用host bind绕过provider。必要时只建聚焦 `test/providerlifecycle/`
   和 `image/lab/providerlifecycle/`，已有功能直接复用；不要提前建空目录。
2. 两个测试slot，各自独占 Internal bridge、独立tmpfs/实例身份；所有Docker操作
   固定实验CID和network ID。真实provider的Create/只读接纳/认证START走相同代码。
3. 从可信宿主分别连接真实worker端点完成mTLS；错node/CA/slot/epoch/generation、
   交叉安装、实例替换与撤销后重新签发/重验必须拒绝。记录公开key/证书摘要，不
   导出私钥、不读取真实账号home；不把TCP连接成功当成身份认证成功。
4. 核对真实Docker策略及cgroup swap限制有效；不制造宿主OOM、不关闭宿主swap来
   规避验证，不使用容器内 `free` 输出作为容器swap权限证明。
5. 收尾只清理本任务记录的准确资源，比较业务元数据基线；清理失败单独报告，
   不能输出完整PASS。不得使用prune、批量删容器或按模糊名称删除网络。

本阶段可使用明确标注的合成control/lease夹具检验管理流，但不能声称真实SQL/Redis
已经验收；N3/N4/K3/K4完整门槛仍须逐项证据，不因两实例握手自动全勾。

### P3 — 出口/实例隔离与身份生命周期

- 复用现有fixed transport，补实际跨槽、宿主、metadata、直连公网、旁路DNS与IPv6
  拒绝矩阵；每项含允许对照及准确拒绝原因，不把任意超时算作成功隔离。
- 只允许明确上游通过独立出口路径；代理撤销、规则缺失、在途tunnel取消应失败关闭。
- 梳理实例重启/升级/换账号/销毁/恢复时机器标识、home、私钥、证书保留与换代合同。
  对当前tmpfs-only provider与最终持久home需求做明确设计，不能临时加宿主目录挂载。
- 规则安装/清理必须可证明限定本任务；如果共享测试宿主无法安全做到，停在本地
  实现和设计，请用户指定真正专用环境，不把“已有测试授权”扩成修改业务网络授权。

### P4 — 权威租约 + existing-only runtime registry + CLI桥接

- 先写状态转换设计：复用 SQL assignment/execution lease，明确独立 Redis
  `execution:lease:v1:` writer/续期/撤销的归属，失败与竞争恢复不得并行授权旧代。
  不用route TTL充当执行lease，不把签发存储当作租约权威。
- 使用真实专用本地/实验 DB、Redis核对幂等/并发/超时；不读取或迁移线上数据库。
- 已有CID的runtime registry负责连接/当前generation/lease归属与回收，不隐式Create。
- 将已有真实CLI适配接入worker/host-agent的数据与出口通道；先一实例合成上游，再
  双实例。延长会话、session/pool/MCP/工具续接按R4/R5逐项实现，不做假成功兼容。
- 任何时刻失去身份/lease/代理版本，拒绝新请求并回收相应在途调用；不能仅拒绝
  下一次签发就声称撤销已经生效。

### P5 — CCMAX现有调用端接入与可恢复交付

- 最后接既有gateway dispatch；HTTP/WS/gRPC协议、usage、错误、取消与legacy默认
  行为都要组合测试。计费仍只有Sub2原权威，防双重记账。
- 完成实际运行制品/版本/摘要清单、空白环境重建与冷启动；无账号、凭据、私钥进入
  镜像或Git。现有base-only镜像或Go-only实验worker不能称作完整isthmus运行制品。
- 补故障/恢复与稳定性证据，然后按K/H/C/L/E门槛逐项加分。生产部署、真实账号
  借用、真实模型请求仍需单独明确安排，不随“继续开发”自动执行。

## 5. 环境和权限边界（接手必须原样遵守）

- `216.106.185.119`：线上参照服务器。本计划默认不连接，不改代码、配置、镜像、
  数据库、UI、容器、网络、路由或账号，不采集业务Env/Cmd/logs。
- `170.106.159.197`：用户批准用于测试，但此前发现 **4个真实业务容器**，不是空机。
  每次操作前重核安全元数据；数量/身份/资源不符就停止远端实验，不自动迁移业务。
- `43.153.75.220`：当前测试链的SSH中转，不把中转节点自动当作可部署实验宿主。
- SSH仅使用用户已有的本地受保护密钥，凭据不进聊天/文档/Git；不关闭host-key验证。
  连接资料缺失就问路径/配置名，不搜索或输出私钥，不猜密码，不擅自换生产机器。
- 本轮 P1 只做本地开发/合成测试，不SSH、下载工具、部署、请求模型或启动真实容器。
- 后续 Internal bridge 创建会让Docker维护其自身规则；不能承诺宿主规则字节完全
  不变。禁止手动flush/改业务网络/开放公网端口/挂载docker.sock给工作负载。
- 保留预检/资源预算/超时/准确资源归属/清理报告；不导出业务容器完整inspect。
  只比较 ID/name/image/start/restart/mount元数据，不读业务认证环境或日志正文。

## 6. 本地验证与Git约定

下面命令只做本地编译/测试；先确认目录与工具存在。不安装/替换宿主Bun，不运行
`make docker-e2e`，不移除专用实验保护。Go已安装在本机 `/opt/homebrew/bin/go`；换
电脑后可用等价本地Go路径，但保留offline/禁真实依赖环境变量的边界。

```sh
cd /Users/ruanyang/My-project/api/z/sub2api/execution-plane
env -u EXECUTION_REDIS_TEST_URL -u EXECUTION_CCMAX_MYSQL_TEST_DSN -u EXECUTION_MYSQL_TEST_DSN \
  GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local /opt/homebrew/bin/go test -race -count=1 -timeout=120s ./...
env -u EXECUTION_REDIS_TEST_URL -u EXECUTION_CCMAX_MYSQL_TEST_DSN -u EXECUTION_MYSQL_TEST_DSN \
  GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local /opt/homebrew/bin/go vet ./...
env GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  /opt/homebrew/bin/go build ./cmd/...
cd /Users/ruanyang/My-project/api/z/sub2api
make -C recovery check
git diff --check
```

先定向测试再全量。缓存或工具缺失是验证缺口，不联网升级依赖来掩盖；测试环境明确
不含真实数据源。上轮基线：全Go race/vet/Linux编译通过，恢复236 Python、150 Bun/
1027断言、镜像186 Python通过。本轮新提交后必须重新验证，不引用旧PASS冒充。

每阶段先规划/实现/独立review/测试，按明确文件 `git add`，禁止 `git add .` 纳入旧WIP。
逐步提交本地Git，不push、不自动开PR。每阶段更新本文件执行记录及
[verification](../../openspec/changes/complete-ccmax-execution-acceptance/verification.md)、
[tasks](../../openspec/changes/complete-ccmax-execution-acceptance/tasks.md)；固定台账只按实证加分。

## 7. Claude接手提示（可直接作为下一次任务）

> 阅读本规划、最新verification顶部、固定验收台账，检查当前branch/HEAD/dirty files。
> 保留暂停的build.py/runtimekit，不从零重写。先复核下面“执行记录”，从第一个未完成
> 步骤继续。每个功能模块放自己的目录，复用现有实现。严禁影响216线上及170现有业务，
> 不取真实凭据、不打开业务开关。先列本切片的目标与停止线，再开发、review、验证、
> 分阶段Git提交，明确哪些仅mock/哪些真实Linux通过；不要用测试数量增加验收分数。

## 8. 本轮执行记录（交接前更新）

- 规划创建：2026-09-18 13:17:37 +08:00，基线 `d1e722a`。
- P1/S2b5a：计划阶段，尚未把源码/实测计为通过。
- P2–P5：未在本轮执行，必须按入口条件逐项推进。
- 下一接手动作：先检查最新执行记录，避免重复已提交的P1；若P1通过，先做P2实验
  设计与安全预检，不直接运行旧Docker实验脚本。
