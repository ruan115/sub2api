# 2026-09-23 — Sub2API / Isthmus 执行面业务逻辑排查总结

时间：Asia/Shanghai（UTC+08:00）。本文档总结此前与 Claude Code 的排查会话中梳理
出的全部业务逻辑。该会话因上下文耗尽与 API 错误中断，由本文档补齐收尾。排查
范围：SSH 部署审计、二进制完整性核验、架构梳理，以及执行面各子系统的状态机与
合同。本文只做事实归纳，不新增验收分数，不改变任何既有边界。

> **2026-09-23 复核修订**：经对照当日线上只读勘察，本文初版有两处事实错误与
> 一处重大缺漏，已就地更正并标注：
>
> 1. **§2** 曾把账号健康维护归为"主平台既有机制"，实为独立子系统
>    `portunex-monitor`——这是复原工作的重点之一，不是可沿用的既有能力。
> 2. **§10** 曾称"本地提交未 push"，实测三个文档提交均已推送。
> 3. **§1** 原三层架构**缺失 Deployer 与 Portunex 两层**，而 Deployer
>    （`14.1.29.250`）才是账户授权、VM 部署与证书签发的中枢。已补 §1.2。
>
> 另新增 §10b 记录当日线上全量部署、opus-5-5 故障、对既有规划的影响与方法论
> 风险。§3–§8（仓库内子系统）经抽查与既有 plan/OpenSpec 一致，未作改动。

## 1. 系统全景

仓库：`/Users/ruanyang/My-project/api/z/sub2api`，分支
`codex/claude-execution-plane-v1`。系统由三层组成：

### 1.1 仓库内的三层

| 层 | 位置 | 职责 |
| --- | --- | --- |
| Sub2API 主平台 | `backend/`（Go）、`frontend/`（Vue） | 登录、权限、计价、模型价格、用户倍率、账务；AI API 网关与订阅配额分发 |
| 执行面（isthmus） | `execution-plane/` | Docker/runc 隔离容器（"VM"）编排、实例身份、mTLS、租约、出口隔离、host-agent 生命周期 |
| CCMAX 桥接 | `ccmax-manager/` 及执行面 worker 通道 | 既有 gateway dispatch 接入执行侧，HTTP/WS/gRPC 合同不变 |

### 1.2 线上实际还有两层（仓库内无对应代码）

2026-09-23 只读勘察确认，线上生产架构比上表多出两个组件。**它们不在本仓库内**，
却是账户授权与 VM 生命周期的真正中枢；复原工作若只覆盖 1.1，会缺掉系统心脏。

| 组件 | 位置 | 职责 |
| --- | --- | --- |
| **Deployer** | `https://14.1.29.250:8443/`（独立服务器） | `/oauth/sessions/` 账户授权、`/providers/isthmus/deployments/` VM 部署、gRPCS 证书签发源头 |
| **Portunex** | 216 上 `portunex-blue`/`green`（Rust）+ `portunex-redis` + `portunex-postgres` | 转发层：路由、粘性、并发、缓存、usage；`deployer.rs` 调用 Deployer |
| **portunex-monitor** | 216 上 `/opt/portunex-monitor/current`（Python，systemd） | **账号健康维护**：凭据检查、配额探测、状态标注、故障隔离、自动恢复 |

Portunex 侧 Deployer 接口配置（变量名，值不记录）：

```text
PORTUNEX__DEPLOYER__ENABLED=true
PORTUNEX__DEPLOYER__BASE_URL=https://14.1.29.250:8443/
PORTUNEX__DEPLOYER__API_TOKEN=<Bearer 令牌，未读取>
PORTUNEX__DEPLOYER__VERIFICATION_MODEL=claude-haiku-4-5-20251001
PORTUNEX__DEPLOYER__ALLOW_INSECURE_LOOPBACK=true
```

认证是 **Bearer Token，不是 mTLS**。`VERIFICATION_MODEL` 表明授权完成后会用 haiku
发一次廉价校验请求确认账号可用。

完整链路：

```text
Portunex UI「添加提供商」
    │ Bearer token, HTTPS:8443
    ▼
Deployer (14.1.29.250)
    ├── /oauth/sessions/   ──► 生成授权链接（PKCE，code_verifier）
    │        ▲                   ↓ 用户浏览器登录 Claude
    │        └── 回传 code + attestation 指纹包
    │                            ↓ haiku 校验账号可用
    └── /providers/isthmus/deployments/ ──► 创建 VM + 签发 gRPCS 证书
                                                │
                                                ▼
                     216: /opt/isthmus/grpcs-certs → deploy-vm.sh → 每 VM .isthmus-grpcs/
                                                │
                                                ▼
                     Portunex ──gRPCS:10765──► isthmus ──Claude Agent SDK──► claude CLI 子进程
                                                │
                                                ▼
                                       portunex-monitor 持续健康维护
```

**attestation 是浏览器指纹，不是机器指纹**。二进制内字段为
`attestation_locale` / `attestation_language` / `attestation_timezone` /
`attestation_screen_height` / `attestation_time` / `attestation_bundle`——
授权时打包浏览器环境特征一并提交。这与 §4 的 gRPCS 证书是**两套独立机制**，
不要合并理解。

部署侧有完整状态机：`deployment_id` / `deployment_status` / `deployment_tasks`
（含 `attempt`、`reason`）/ `deployment_reservations` / `deployment_sessions`
（带 `expiry`）/ `deployment_status_check` / `last_checked_at` / `rpm_limit`。
即带预留、重试与健康检查的调度系统，非"创建即不管"。

关键约束（既有事实，不得改变）：

- "VM" 是 Docker/runc 隔离容器，不开发虚拟机内核，不切换 KVM。
- 双层账务归属：Sub2 终端用户账与 CCMAX 已有服务账户/成本账分别保留；执行侧
  只回传 usage，不新增结算、不删除 CCMAX 现有扣减，防止同一请求重复计费。
- 不更改既有调用方 HTTP/WS/gRPC 合同，不恢复 Portunex 全业务，不重做控制台视觉。
- 唯一分数来源：`docs/plans/isthmus-container-delivery-v1.md` 验收台账。当前
  总体 **32%**（镜像 40%、运行服务 60%、出口隔离 40%、身份 20%、宿主 40%，
  CCMAX 桥接/完整生命周期/整体验收各 0%）。

## 2. 计费与用户管理（主平台既有，沿用）

- 额度、余额、API key 生命周期、用量归集属于主平台既有能力，执行面不重建。
- 执行面与主平台的唯一账务接口是 **usage 回传**；结算仍由主平台与 CCMAX 各自
  原有路径完成。
- 账号健康维护（凭据有效性检查、配额探测、故障隔离、自动恢复）**不在主平台，
  而在 `portunex-monitor`**（见 §1.2）——216 上一套独立的 Python 服务，模块为
  `credential_checks.py`、`account_status.py`、`recovery.py`、`quota_probe.py`、
  `concurrency.py`、`slots.py`、`emergency.py`、`background_auth.py`、
  `service_identity.py`。执行面不引入第二份账号状态权威。

  > **修正说明**：本节初版将账号健康维护归为"主平台既有机制"，据 2026-09-23
  > 线上勘察更正。这一归属很关键——该子系统是账户维护的实际权威，也是复原工作
  > 的重点之一，不是"沿用即可"的既有能力。Portunex 侧的账务/路由缓存键为
  > `ptx:v1:bal:*`、`auth:ak:*`、`aksettings:*`、`cred`/`credtomb`。

## 3. VM 编排与生命周期

### 3.1 模块地图（`execution-plane/internal/`）

| 子系统 | 目录 | 要点 |
| --- | --- | --- |
| Docker provider | `provider/docker/` | 严格镜像/资源/用户/独占 Internal bridge 接纳；Create/Inspect/START 全路径门禁 |
| 调度边界 | `nodepolicy/`、`placement/` | lifecycle-only 节点明确排除业务调度（含 sticky 与无约束请求） |
| 路由 reconcile | `route/`、`reconcile/` | DesiredReady 下 stopped→destroy、destroyed→release→place 取新 epoch |
| 控制面与状态 | `control/`、`runtime/store/`、`service/` | 现成 binding/命令/租约结构；不另起第二份状态权威 |
| 宿主启动 | `cmd/host-agent/`、`hostagent/daemon/`、`hostagent/lifecycle/` | 预签发 node 身份 → TLS 控制 → 严格既有 CID START；仅生命周期，无业务数据面 |
| 任务/预留 | `runtime/store/`（含 `proxy_reservation_grants` 迁移）、`onboarding/` | 预留（reservation）、就绪绑定（status_check 类语义）、退避重试在 store 层落地 |

### 3.2 已接通的生命周期路径

`cmd/host-agent → daemon.SelectRunner → 同一 Docker provider + lifecycle.New +
ControlClient → 严格既有物理 CID START → CSR → 认证签发 → 实例安装 → worker mTLS`

其中**没有** CLI 业务激活、数据面转发或活租约续期。START 必须准确物理 CID，
不能 Create/recreate/替换容器；普通 TCP/INSPECT 不能复活失败或身份漂移后的
认证状态。

### 3.3 已落地的 provider 安全策略

- **P1 / 禁止 swap**：创建请求显式 `MemorySwap = Memory`（两者为正）；Engine
  JSON 省略/null/0/-1/不等于 Memory 全部拒绝；共同只读接纳门禁覆盖
  Inspect/InspectSlot/Create-adoption/START/endpoint/ValidateExisting/bootstrap
  前后复核。这是请求与接纳策略，不证明宿主内核执行（cgroup 实证留 P2）。
- **P3d / 禁用自动重启**：创建请求显式序列化 `--restart no`，只接受显式 `"no"`
  且零重试；缺失/null/空 Name 一律拒绝。既有 `unless-stopped` 容器只被拒绝，
  不修复不重建（provider 没有 update 动词）。边界：创建预防 + 接纳检测，
  **不是持续强制**。
- **P3c / reconcile 停止路由**：`DesiredReady` 下 stopped→destroy→release→place
  取新 epoch；同时修复了由此暴露的 CRITICAL——`ActionPlace` 幂等键在无
  assignment 时恒等导致第二次放置静默失败，现以 `NextExecutionEpoch` 判别。
- **已知运维缺口**：`reconcile.NewController` 无非测试调用方，Docker 守护进程
  或宿主重启后容器保持停止且无自动恢复路径，归 P4 装配。

## 4. 证书签发与 mTLS

- **实例本地身份**（`runtimeidentity/`、`runtimebootstrap/`）：每实例独立私钥 +
  CSR + 原子公开证书安装；实例私钥不向宿主输出。"目录不同"不作为私钥独立的
  证据；TLS 配置与固定客户端版本匹配，不做随机 TLS 指纹伪装。
- **认证签发**（`runtimeenrollment/`、`service/runtimeenrollment/`）：认证 RPC、
  SQL receipt、独立 Redis 校验。
- **1b0db23 / 按租约两库合取签发**：签发以两个租约存储的合取为门禁，比线上
  参照的静态分发更正确，是真正的设计升级。
- 宿主 node 密钥与每实例 server 密钥是不同角色，不得混用。

## 5. 执行租约权威（P4a）

- 权威划分：**Redis 令牌+TTL 是围栏权威**，**SQL `execution_leases` 是持久归属
  与撤销事实**。route TTL 不是执行租约，签发回执不是租约权威。
- `Coordinator.Revoke` 先写 SQL 再删令牌；删令牌失败时只问后端的校验会继续
  授权已撤销租约——因此 `Coordinator.Validate` 改为**两库合取**，任一不可读
  即失败关闭。
- `Fencer`/`Renewer` 依赖放宽为 `Validator`/`Refresher` 接口（`*Coordinator`
  均满足），不再把"只有 Redis"固化进类型。
- Review 修掉的真缺陷：续期只刷 Redis 会让 SQL `expires_at` 陈旧（代理租约
  校验读的正是它，同一租约被两条路径判定相反）；`Revalidate` 的 N+1 加共享
  超时会级联关闭全部 tunnel。

## 6. 撤销传播与连接托管（P4b / P5e / P5f）

- **P4b / runtime registry**（`runtimeregistry/`）：只接管已建立连接，绝不
  创建/拨号/重启；同一 generation 换容器一律拒绝；四条回收路径含
  `lease.Fencer` 回调（复用 P4a 合取校验）；`Revoke` 是屏障而非仅关闭。
  控制流（orchestrator ↔ host-agent）是节点级的，按槽位租约去关它是错的。
- **P5e / custody 接进 daemon**：`composition.go` 构造 `SessionAuthority` +
  `Custody` 并传入 `lifecycle.New`；用 `nodeFacts` 后绑定打破循环依赖；未绑定
  窗口失败关闭（视为已撤销）。离线窗口复用 `health.Timings.NodeOffline`
  （默认 45s ≥ 3×心跳），不新增配置旋钮。
- **P5f / 双库回收链路验收测试**：`lifecycle/leasechain_integration_test.go`，
  真实 MySQL + 真实 Redis 双重门控、默认 SKIP。链路：双写 Grant → 权威校验 →
  custody 托管真实 gRPC 传输 → 仅撤销 MySQL → 断言权威拒绝、custody 回收、
  传输真的拆除；反方向（令牌消失、持久行活动）同样拒绝。**状态：已写已编译，
  从未对真实库跑过**——远端首跑失败在配置（SSH 隧道地址 127.0.0.1:33379 在
  远端无监听），未跑到任何断言，不得记为通过。
- 缺口：`hostagent.Controller.Start` 仍把连接直接返还调用方，没有任何生产代码
  import `runtimeregistry`；`Coordinator`/`FailoverController` 依旧无非测试
  调用方，生产权威 writer 与续期循环未接线。

## 7. 出口隔离（P3a）

- 出口 CONNECT **结构性拒绝矩阵**：metadata/链路本地/未指定/组播广播/IPv6
  字面量/解析端口，在每槽 allowlist 之前生效，注册期命中即整绑定失败。
- 数字编码 IPv4 规避已封堵（该绕过为 HIGH 级缺陷）。
- **在途 tunnel 回收**：绑定撤销/epoch 换代不再只拒下一次 CONNECT。每个拒绝
  带精确原因头，不用超时冒充隔离。
- 边界：这不是内核网络门；VM0b/N3 保持开放。

## 8. 实例身份生命周期合同（P3b）

- 纯规则集 `identitylifecycle/`（不存储、非第二份状态权威、当前无生产调用方）。
- 三个已核实事实：身份在 `/run` tmpfs 停止即毁；对端校验是 SPIFFE URI 精确
  匹配且无 CRL；证书回执唯一键是 `(slot_id, execution_epoch)` 并比对
  `PublicKeySHA256`。
- 因此凡丢失私钥的事件必须 **epoch 与 generation 同时前进**；generation 每槽
  只增不复位；`ValidateHistory` 用 floor 锚定，挡住销毁后从 generation 1 重生。
- 持久 home 采用**白名单**（`/home` 之下）而非黑名单（Debian 基础镜像
  `/var/run` 是 `/run` 的符号链接）；未加任何挂载。

## 9. 环境边界与禁令（原样保留）

- `216.106.185.119`：线上参照服务器。默认不连接，不改任何代码/配置/镜像/
  数据库/UI/容器/网络/路由/账号，不采集业务 Env/Cmd/logs。
  > **2026-09-23 显式豁免**：用户在当日会话中明确指派连接 216 进行只读勘察与
  > 故障排查，本轮多次连接均在此授权下进行，**全程只读**，未写入、未重启、
  > 未改配置。该豁免限于只读；后续任何写操作仍需重新取得授权。
- `14.1.29.250`：Deployer（部署/授权服务器）。**本仓库无访问凭据，未连接、
  未探测**。其 `API_TOKEN` 仅读取变量名，未读值。
- `170.106.159.197`：用户批准的测试机，但有 **4 个真实业务容器**。每次操作前
  重核安全元数据；数量/身份/资源不符即停止远端实验。
- `43.153.75.220`：SSH 中转，不自动当作可部署实验宿主。
- SSH 仅用用户已有本地受保护密钥；凭据不进聊天/文档/Git；不关闭 host-key
  验证；不搜索或输出私钥。
- 失败关闭边界：`EXECUTION_HOST_AGENT_RUNTIME_ENABLED`、
  `EXECUTION_RUNTIME_ENROLLMENT_ENABLED` 默认关闭；不打开 `execution_onboarding`，
  不把账号标为 `migrated`；daemon `/readyz` 保持 503、
  `production_ready=false`；Redis PING 只证明连接可用，缺准确授权租约时签发
  拒绝，不加 Memory fallback。
- 禁止手动 flush/改业务网络/开放公网端口/挂载 docker.sock 给工作负载；不使用
  prune、批量删容器或按模糊名称删除网络。

## 10. 当前工作区状态（2026-09-23）

- 最近提交：`2dd2b4d`（同步 216 文档）、`0089d9c`（交接文档 + 退役一份 plan）、
  `64c1f8f`（按 9/14 核查修正 216 勘察），均为文档提交。
  **三者已推送至 `origin/codex/claude-execution-plane-v1`**，`git log @{u}..HEAD`
  为 0 条。
  > **修正说明**：本节初版称"本地提交未 push，不等于异地备份"，与实际不符，据
  > 实测更正。代码类 WIP 仍未提交（见下），那部分确实没有异地备份。
- 未提交改动（保留，非垃圾文件）：
  - `execution-plane/internal/route/reconcile.go`：仅字段对齐格式化。
  - `execution-plane/isthmus-runtime/image/lab/build.py` 与
    `execution-plane/isthmus-runtime/image/runtimekit/`：**暂停且未提交**的通用
    镜像构建框架（fake-candidate 镜像上下文：`stage_context`/`verify_context`
    按仓库信任锚校验路径/权限/SHA-256，26 个 runtime 路径探针）。除非用户明确
    恢复该方向，否则绕开，不新增另一套通用镜像构建框架。

## 10b. 线上 2026-09-23 的变更（非本仓库操作）

当日同事对 216 执行了**整套运行时全量更换**并处理了一次线上故障。这些变更发生在
本仓库之外，但直接影响复原工作的参照基线。详见
[216 CLI 托管现状](../../execution-plane/isthmus-runtime/docs/production-216-cli-custody.md)。

### 10b.1 全量部署（UTC）

| 时间 | 操作 |
| --- | --- |
| 09-22 20:42 | 上传 `isthmus_exp26092302_encrypted.zip`（40,036,394 B） |
| 09-23 00:48 | 替换 `isthmus-supervisor.sh`（16,688 → 18,383 B）、`Dockerfile.vm`、`setup-env.sh`、`install-apparmor.sh`、apparmor profile |
| 09-23 02:03 | 替换 `deploy-vm.sh`（85,030 → 88,886 B） |
| 09-23 02:43 | 写入 Claude Code **2.1.280**，重指符号链接 |
| 09-23 04:00–04:01 | 重签 gRPCS 证书，重新下发 77 个 VM home，**批量重启全部 77 个容器**（2 分钟内，非滚动） |
| 09-23 04:19 | 更新 `/opt/isthmus/dist/isthmus.pkg` |

### 10b.2 关键结果

- **CLI 2.1.258 → 2.1.280**，经官方 manifest 核验为正版（sha256
  `1e08503d…22925b`，233,709,640 B，commit `80abbfe7d723`）。旧版保留，回滚可行。
- **57/20 构建分裂已消失**：全机群 `isthmus.pkg` 收敛为单一构建
  （sha256 `775a2f7a…0f1c`，39,585,577 B，76/76 一致）。原先列为头号阻塞的
  「摸清 57 个未分析核心」任务，**前提已不成立**。
- **`PORTUNEX__REDIS__ENABLED` 由 `false` 变 `true`**，新增 `portunex-redis`
  容器，`MODEL_ALIAS_TTL_SECS=600`。9/14 核查记录的"Redis 关闭"已过时。
- 结构健康：77 容器 Up，231 个 isthmus 进程（77×3）自重启持续存活，
  supervisor 崩溃重启逻辑未触发。

### 10b.3 opus-5-5 故障与修复

`claude-opus-5-5` 一度报错。逐层排除结论：**不在 CLI，也不在转发层**——
CLI 2.1.280 认识该模型（二进制内 15 处），2.1.258 不认识（0 处）；Portunex
不硬编码模型名（透传）。根因在账号侧，由 `portunex-monitor` 七连发修复：

```text
04:22 v3.4-service-auth        06:18 v3.5-credential-checks
06:52 v3.5.1-validation-status 07:41 v3.6-account-status
08:07 v3.6.1-status-label      08:24 v3.7-account-recovery
08:29 v3.7.1-recovery-isolation  ← 08:30:43 systemd 重启生效，故障解除
```

**这印证了 §1.2 的判断**：账号健康维护是独立子系统，且是线上故障的高频来源。

### 10b.4 对既有规划的影响

- `isthmus-container-delivery-v1.md` 第 9 行把 100 分锚定在"已保全的
  2026-09-14/15 材料"，**该参照物已失效**；第 11 行"线上服务、镜像、配置不变"
  的前提亦不再成立。
- I1 已得 3 分的证据（`vm-identity-tls-baseline-2026-09-16.md`）源自旧基线。
  按台账第 17 行"子门槛出现回归则撤回对应分"，**是否撤回需用户裁定**。
- 仓库内 5 处硬编码 `2.1.258` 与线上脱节，runtimekit 流程现在会直接失败：
  `image/artifacts/binaries.py:49`、`image/lab/acquire.py:104`、
  `image/lab/toolchain.py:61`、`image/lab/cli.py:51`、
  `image/locks/toolchain-linux-2026-09-17.json`。建议改为从 lockfile 单一读取。
- 权重存疑：L（凭据与完整生命周期）仅占 10 分且为 0%，但按 §1.2 的真实架构，
  凭据与账号生命周期是系统核心；I（镜像复建）占 15 分却是相对容易的部分。

### 10b.5 方法论风险

线上由他人以**每日多次**的节奏发布，而当前复原方法依赖**静态分析快照**。
今日部署即一次性作废了 9/14 的分析基线；分裂消失不等于分析债减少，而是
**清零重置**——新构建同样无人分析。按此方法，每次发版都会让分析工作作废。

建议把锚点从"匹配保全实现的字节行为"改为"**协议契约 + 差分对拍**"：
`contracts/grpc/messages.proto` 是逐字节保全的，协议变化速度远低于实现。
把线上当黑盒做响应对拍，既定位理解缺口，又天然构成替换的验收标准，且对发版免疫。

### 10b.6 待处理的安全事项

1. **SSH 私钥**：[托管规划](2026-09-19_16-31-04-isthmus-runtime-custody.md)
   第 163–168 行已判定对应 216 的私钥应视为已泄露并建议换发，**至今未执行**，
   且 2026-09-23 会话中又多次使用。
2. **API key 明文**：排查中读取 Redis `aksettings` 时，返回值含完整 `key_text`
   （前缀 `axTSBXU8`）。按项目边界应视为已泄露，建议轮换。
3. **终端用户 PII**：`/opt/gateway/logs/refusal_guard_dumps/` 下文件名**直接以
   用户邮箱命名**，任何一次目录列举都会暴露 PII。建议反馈给运维方调整命名。

## 11. 下一步（按既有规划顺序）

1. 把 `Runtime` 连接交由 `runtimeregistry` 托管（接上 `hostagent.Controller.Start`），
   接线生产权威 lease writer 与续期循环。
2. 在专用 Linux 环境（用户单独授权后）跑 P5f 双库回收链路验收与 S2b5b 双
   Internal 网 Create/START/mTLS + cgroup swap 实证；远端跑测试时从自建容器
   动态取私网 IP 注入 DSN/URL，不写死。
3. P4 收尾后接 P5：既有 gateway dispatch 接入，HTTP/WS/gRPC、usage、错误、
   取消与 legacy 默认行为组合测试；防重复计费。
4. 生产部署、真实账号借用、真实模型请求仍需单独明确安排，不随"继续开发"
   自动执行。

## 12. 本地验证命令（仅本地编译/测试）

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

测试环境明确不含真实数据源；缓存或工具缺失是验证缺口，不联网升级依赖来掩盖。
