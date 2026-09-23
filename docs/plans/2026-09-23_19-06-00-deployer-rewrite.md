# 2026-09-23 19:06 — Deployer 完整重写规划

时间：Asia/Shanghai（UTC+08:00）。承接
[业务逻辑总结 §1.2](2026-09-23_18-05-00-isthmus-business-logic-summary.md)。
范围由用户指定：**完整重写 Deployer 的全部职责**。

**进度：阶段 0 已完成**（2026-09-23 晚，经 WebSSH 中继勘察）。Deployer 已定位、
形态已判定、真实数据模型已取得。详见 §13 执行记录。

**结论提要**：制品是 **Bun `--compile` 二进制，JS 源码内嵌可提取**——阶段 0 原本
最坏的假设（啃裸二进制）没有发生，路线是「读源码」。真实 schema 已取得，
本文 §4 的数据模型已据此重写，原先基于字符串推测的内容作废。

## 0. 先纠正一个认知：我们还没有契约

必须先说清楚当前证据的强度，否则整个计划会建在沙子上。

关于 Deployer 的全部信息来自**两个间接来源**：Portunex 容器的环境变量，以及从
`portunex-server` 二进制里 grep 出的字符串。后者**不可直接当作接口清单**：

```text
/oauth/sessions/providers/isthmus/deployments/    ← 两个常量在字符串表里相邻，被一起 grep 出来
/oauth/sessions/providers/isthmus/oauth/sessions/
```

此前把它拆成 `/oauth/sessions/` 与 `/providers/isthmus/deployments/` 是**推断**。

**更要紧的问题**：提取到的路径里混杂了三类完全不同的东西，而字符串提取**无法区分**：

| 类别 | 例子 | 说明 |
| --- | --- | --- |
| Portunex 自身路由 | `/sessions/me`、`/accounts`、`/authorize` | 它自己也是 Web 服务，供管理界面调用 |
| **上游**提供商路径 | `/oauth/token` 旁挨着 `anthropic-ratelimit-requests`、`/oauth2/token` 旁挨着 `openai_oauth` | 是 Portunex 调 Anthropic/OpenAI，与 Deployer 无关 |
| **Deployer 路径** | `/providers/isthmus/deployments/` | 唯一较强的信号，因为有 `deployer.rs` 佐证 |

**因此：Deployer 的接口清单目前是未知的，归属判定是阶段 1 的首要任务。**
本计划的任何「接口设计」在阶段 1 完成前都只是占位。

> **§0 的时效性**：本节写于阶段 0 之前，描述的是「当时证据有多弱」。阶段 0 已取得
> 真实 schema（§13.4），接口路径的推测问题仍然成立——**schema 不等于 HTTP 契约**，
> 端点清单仍待阶段 1 从调用点确认。本节保留作为证据强度的记录。

### 已确证的事实（仅此而已）

```text
PORTUNEX__DEPLOYER__ENABLED=true
PORTUNEX__DEPLOYER__BASE_URL=https://14.1.29.250:8443/   ← 旧栈（216）指向
PORTUNEX__DEPLOYER__API_TOKEN=<Bearer 令牌，未读取>
PORTUNEX__DEPLOYER__VERIFICATION_MODEL=claude-haiku-4-5-20251001
PORTUNEX__DEPLOYER__ALLOW_INSECURE_LOOPBACK=true
```

- 认证是 **Bearer Token，不是 mTLS**。
- 授权完成后会用 haiku 发一次**廉价校验请求**确认账号可用。
- `deploy-vm.sh` 中 `openssl|x509|csr|fingerprint` **零命中**——它只在 185-194 行
  拷贝 `ca.crt`/`server.key`/`portunex-client.crt`，**不签发**。故签发在 216 之外，
  Deployer 是最可能的签发方，但**尚未证实**。

## 1. 阶段 0：从部署制品中榨取一切（源码已确认不可得）

**2026-09-23 核实结论：旧服务器上只有部署制品，没有源代码。**
因此本计划确定为「重写」而非「补完」。但制品本身仍是**信息量最大的单一资产**，
远胜于从 Portunex 侧间接反推。

### 0.1 先判定制品形态——这决定后续一切

不同形态的可还原性差了一个数量级，**第一件事就是查清它是什么**：

| 形态 | 可还原性 | 路径 |
| --- | --- | --- |
| Python（`.py`/`.pyc`） | **源码即在手** | 直接读；`.pyc` 可反编译回接近源码 |
| JS/TS bundle | **高** | 同 isthmus 的 `readable.mjs` 路线，格式化后可读 |
| Go 二进制 | 中 | 符号表通常保留；函数名、结构体、路由字符串可提取 |
| Rust 二进制 | 中低 | 符号残留少于 Go，但 serde 结构名常在 |
| 容器镜像 | 看层内容 | 逐层解包后按上述分类 |

参考：`portunex-monitor` 就是 Python 明文部署（`/opt/portunex-monitor/current/*.py`），
若 Deployer 同出一源，**很可能源码等价物直接可读**。

### 0.2 一并抢救的非代码资产

制品之外，这些同样关键，而且往往比代码更难重建：

- **数据库 schema**（表结构、索引、约束、迁移历史）——状态机的真实定义在这里。
- **配置文件**（含变量名与默认值；凭据只记存在性，不读值）。
- **CA 材料的存在形式**——根证书在哪、以什么形式存储、有无中间 CA。
  **此项直接决定 §5.1 的可行性与迁移难度。**
- **systemd unit / 启动参数**——暴露运行期开关。
- **依赖清单**（`requirements.txt`/`go.mod`/`Cargo.lock`）——反推技术栈与外部服务。

### 0.3 判定分叉

确认旧服务器制品与线上 `14.1.29.250` 是**同一版本**还是**已分叉**：
比对版本号、文件哈希、配置差异。若已分叉，**以线上为契约基准**
（Portunex 现在对接的是线上那个），旧制品作为设计参考。

### 0.4 产出

《Deployer 制品还原报告》：形态判定、可读出的接口与数据模型、
CA 材料形态、与线上的差异、以及**明确列出仍然缺失的部分**。

**这一步没结论之前，不要写任何 Deployer 实现代码。**

## 2. 阶段 1：从 Portunex 侧还原契约（不需要碰 Deployer）

**关键洞察：Portunex 是 Deployer 的唯一客户端**，因此 Deployer 的契约完全由
Portunex 的调用点决定。而 Portunex 在 216 上，可只读访问。

### 1.1 定位调用点

- 在 `portunex-server` 中定位 `deployer.rs` 相关的函数符号与调用点。
- 目标是把三类路径**区分开**：Portunex 自身路由 / 上游提供商 / Deployer。
- 判据：与 `PORTUNEX__DEPLOYER__BASE_URL` 拼接的才是 Deployer 路径。
  优先找 URL 拼接点与 `Authorization: Bearer` 注入点。

### 1.2 还原请求/响应结构

Rust 的 serde 结构体名通常保留在二进制里（已见 `deployment_id struct`、
`fingerprint bigrams struct` 等）。可据此还原字段集合，但**类型与可选性需另行确认**。

### 1.3 实时契约验证（条件性）

用户对 `14.1.29.250` **有部署权限**。若确认该机归属明确，可对**只读接口**
（如查询 deployment 状态）发请求以验证契约。

边界：**只读、不创建、不授权、不触发计费**；每次请求前说明将发什么。

### 1.4 产出

《Deployer 契约规格》：端点、方法、请求/响应 schema、状态码、错误语义、
幂等性、超时与重试。**这是后续所有实现的唯一依据。**

若阶段 0 找回了源码，本阶段降为交叉验证，但**不跳过**——契约文档本身是交付物。

## 3. 职责边界（重写什么，不重写什么）

| 职责 | 归属 | 本计划 |
| --- | --- | --- |
| OAuth 会话（PKCE 授权码流） | Deployer | ✅ 重写 |
| 账号凭据存储与刷新 | Deployer | ✅ 重写 |
| VM 部署编排（预留/任务/重试/状态检查） | Deployer | ✅ 重写 |
| gRPCS 证书签发（CA） | Deployer（待证实） | ✅ 重写 |
| 授权后 haiku 校验 | Deployer | ✅ 重写 |
| **账号健康维护**（凭据检查/配额探测/隔离/恢复） | **portunex-monitor** | ❌ 不属于 Deployer |
| 路由/粘性/并发/计费 usage | **Portunex** | ❌ 不动 |
| VM 内运行时 | **isthmus** | ❌ 不动 |

**注意**：账号健康维护常被误并入 Deployer，实际在 216 的 `portunex-monitor`。
两者的分工边界需在阶段 1 一并确认（谁写 `deployment_status`？谁写 `account_status`？）。

## 4. 数据模型（真实 schema，2026-09-23 取得）

> 原先本节是从 `portunex-server` 二进制提取的字段名推测。阶段 0 取得了
> **Deployer 自己的 SQLite schema**，以下为实测结果，推测版本作废。
> 取法：`strings deployer.sqlite | grep -i 'CREATE TABLE <名>'`（SQLite 以明文
> 保存表定义），**只读表结构，未读任何数据行**。

### 4.1 全部 20 张表

```text
管理后台    admins / sessions / settings
账户授权    oauth_sessions
拓扑        servers / jump_hosts / targets / target_folders / server_targets
部署        managed_deployments / managed_deployment_history
任务        jobs / job_receipts
传输分阶段  access_stages / access_stage_chunks
VM 迁移     target_move_stages / target_move_stage_items / target_move_stage_chunks
控制        service_control
密钥        vault
```

### 4.2 已取得完整定义的表

```sql
CREATE TABLE jobs (
  id TEXT PRIMARY KEY, request_id TEXT NOT NULL, server_id TEXT NOT NULL,
  target_id TEXT NOT NULL, input_hash TEXT NOT NULL, status TEXT NOT NULL,
  error TEXT, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
  result_encrypted TEXT,
  operation TEXT NOT NULL DEFAULT 'create',
  deployment_id TEXT NOT NULL DEFAULT '',
  UNIQUE(server_id, request_id)
);

CREATE TABLE managed_deployments (
  id TEXT PRIMARY KEY, server_id TEXT NOT NULL, provider_id TEXT,
  target_id TEXT NOT NULL, backend TEXT NOT NULL, instance_id INTEGER NOT NULL,
  revision INTEGER NOT NULL, metadata_encrypted TEXT NOT NULL,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);

CREATE TABLE servers (
  id TEXT PRIMARY KEY, name TEXT NOT NULL, ip TEXT NOT NULL,
  enabled INTEGER NOT NULL, token_hash TEXT UNIQUE NOT NULL,
  access_revision INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE vault (id INTEGER PRIMARY KEY CHECK(id=1), marker TEXT NOT NULL);

CREATE INDEX idx_oauth_sessions_expiry
  ON oauth_sessions(COALESCE(retain_until, expires_at));
```

`oauth_sessions`、`jump_hosts` 的完整定义跨 SQLite 页边界被 `strings` 截断，
仅知其存在且 `oauth_sessions` 含 `retain_until` 与 `expires_at` 两个时间字段。
**待补**（见 §11）。

### 4.3 从 schema 读出的五个设计决定

这些是重写时**应当照搬**的，原版在这几点上做对了：

1. **`vault` 不是密钥库，是主密钥校验哨兵。**
   `CHECK(id=1)` 强制单行，只有一个 `marker` 字段。语义是：启动时用外部提供的主
   密钥解 `marker`，解得开才算密钥正确。**主密钥不在库内**，因此库被整体拖走也解
   不开 `metadata_encrypted` / `result_encrypted`。这个模式直接沿用。

2. **`jobs` 幂等靠 `UNIQUE(server_id, request_id)`。**
   同一请求重投不会重复执行；`input_hash` 用于识别「同一 request_id 但参数不同」
   的冲突——这是幂等实现里最容易漏掉的一环，原版做了。

3. **字段级加密，不是整库加密。**
   `metadata_encrypted`、`result_encrypted` 是 TEXT 密文列，其余字段明文可索引。
   兼顾了查询能力与静态保护。

4. **`revision` / `access_revision` 双版本号。**
   部署有 `revision`，服务器有 `access_revision`——后者配合 `access_stages`，
   说明访问凭据/配置是**分阶段下发并带版本**的，可增量同步而非全量重推。

5. **双时效字段 `COALESCE(retain_until, expires_at)` 建索引。**
   授权会话有「过期」和「保留至」两个概念，清理任务按合并值扫描。
   说明会话过期后仍可能需要保留一段时间（审计或重试），不是到期即删。

### 4.4 schema 演进痕迹

文件中同时存在两个版本的 `jobs` 与 `servers` 定义（旧页未被回收）：

```text
jobs     旧：无 operation / deployment_id
         新：+ operation TEXT DEFAULT 'create', + deployment_id TEXT DEFAULT ''
servers  旧：无 access_revision
         新：+ access_revision INTEGER DEFAULT 0
```

说明这套系统是**演进出来的**，不是一次成型；且迁移方式是「加列 + 给默认值」，
未做破坏性变更。重写时的迁移策略可参照。

## 5. 安全设计（重写的最大风险面）

这是本计划中**最需要慎重**的部分。Deployer 同时是 **CA** 和**多账号凭据库**，
一旦实现有缺陷，影响面覆盖全部 VM 与全部接入账号。

### 5.1 CA

**现状**（阶段 0 实测）：自签私有根，非外部 CA 派生。

```text
subject = CN=Isthmus private grpcs CA, O=Isthmus
issuer  = 同上（自签）
有效期  = 2026-08-28 → 2036-08-28（10 年）
分发    = /opt/isthmus/grpcs-certs/{ca.crt, server.key, portunex-client.crt}
          由 deploy-vm.sh 拷入每个 VM 的 .isthmus-grpcs/
```

全盘（`/opt`、`/etc`，深度 3）只找到 `ca.crt`，**未找到 `ca.key`**。可能在
`settings` 表加密列中，或在更深路径。

> **风险已降级（用户 2026-09-23 裁定）**：此前本文把「根私钥不可得」列为最高风险，
> 理由是需给全部 VM 换信任根。该评估**有误**——本项目的目标本就是重写 VM 体系，
> **重装 VM + 重签证书是常规操作而非灾难**；`targets` / `target_move_stages` 等表
> 的存在也印证了批量重建是设计内的能力。因此根私钥是否可得**不构成阻塞**：
> 可得则平滑延续，不可得则新建根并随重装分发。

重写时的要求（与私钥是否可得无关）：

- 根密钥**离线生成**，不落在应用进程可读路径；建议独立加密存储。
- 签发必须有**授权门禁**——参考 `1b0db23` 的教训：签发门禁曾只校验 Redis 单库，
  会给已撤销租约签出证书，后改为两库合取。**新 CA 不得重蹈**。
- 证书**短期化 + 可轮换**，而非一次签发长期有效（现状是 10 年根 + 未知期限叶证书）。
- 必须有**撤销路径**，且撤销要能真正阻断（不能只删记录）。
- 签发全程审计日志（签给谁、何时、依据什么授权）。
- **换根演练**：既然重装重签是常规操作，就应把「换根」做成可重复、可验证的流程，
  而不是一次性手工操作。这同时也是灾难恢复能力。

### 5.2 凭据存储

- OAuth token **静态加密**，密钥与数据分离。
- 应用层不应有「导出全部凭据」的能力。
- 刷新失败要有退避与告警，不静默丢弃。
- 日志与错误信息**绝不**包含 token 片段。

### 5.3 服务间认证

- 当前是 Bearer Token。重写时评估是否升级为 mTLS——**但不要在切换期同时改协议
  与改实现**，那会让故障归因变得不可能。建议先契约兼容，再单独升级。
- Token 必须可轮换，且支持双 token 并存以实现无中断轮换。
- `ALLOW_INSECURE_LOOPBACK=true` 的语义需在阶段 1 查清：它放宽了什么？
  新实现的默认值应为 **false**，显式开启才生效。

### 5.4 attestation（浏览器指纹）

已观测字段：`attestation_enabled` / `attestation_bundle` / `attestation_locale` /
`attestation_language` / `attestation_timezone` / `attestation_screen_height` /
`attestation_time`。

这些是**浏览器环境特征**，在授权时随请求提交。**本计划将其记录为观测到的字段集合，
但不设计指纹生成或伪装逻辑。** 是否复现该行为、以何种方式复现，属于产品与合规
决策，需你自行判断并承担；技术上我可以实现「透传用户真实浏览器提交的 attestation」，
但不会构造用于规避自动化检测的合成指纹。

若选择透传真实浏览器数据，需注意这属于**终端用户环境信息**，应纳入隐私处理范围。

## 6. 架构与技术选型

### 6.1 建议形态

单体服务 + 关系数据库 + 后台任务队列。**不要上微服务**——职责虽多但耦合紧，
拆开只会让事务边界变难。

```text
┌─ HTTP API（Bearer 鉴权）
│    ├─ /oauth/sessions     授权会话：创建、取码、换 token、haiku 校验
│    ├─ /deployments        部署：预留、创建、状态查询、销毁
│    └─ /certificates       证书：签发、轮换、撤销
├─ 后台工作器
│    ├─ 部署任务（带 attempt/reason 的重试）
│    ├─ 会话过期清理
│    ├─ 凭据刷新
│    └─ status_check 轮询
├─ CA 模块（密钥隔离）
└─ 存储：Postgres（事务 + 状态机）
```

> **存储选型需重新评估**：原版用的是 **SQLite（WAL 模式）**，不是 Postgres。
> 对单实例、内网、以 SSH 外呼为主的编排器来说，SQLite 是合理选择——无需额外
> 进程、事务简单、备份就是拷文件。上表的 Postgres 是本文初稿的默认假设。
> 除非确有多写入实例的需求，**建议沿用 SQLite**，把复杂度留给业务逻辑。
> 注意原版主库 401 KB 而 WAL 4.1 MB，说明 checkpoint 不频繁，写入量不小。

### 6.2 语言选型

| 选项 | 理由 |
| --- | --- |
| **Go** | 与 `execution-plane/` 同语言，可复用既有 lease/身份/证书代码与测试habits |
| Rust | 与 Portunex 同语言，但仓库内无现成基础设施 |
| Python | 与 `portunex-monitor` 同语言，适合任务编排，但 CA 与并发较弱 |

**建议 Go**：`execution-plane/internal/` 里已有 `runtimeidentity`、
`runtimeenrollment`、`lease` 等直接相关的成熟实现，重写 Deployer 时可大量复用，
而不是第四次从零写证书与租约逻辑。

### 6.3 状态机优先

部署生命周期是本系统的核心，应当**先把状态机写死**（显式状态 + 允许的转移 +
每次转移的前置条件），再填业务逻辑。已观测字段表明原版就是这么设计的。

## 7. 分阶段实施

每阶段都有可运行产物，**不允许出现"写了三周还不能跑"的阶段**。

| 阶段 | 内容 | 出口标准 |
| --- | --- | --- |
| **0** | 制品还原（源码不可得） | 《Deployer 制品还原报告》，含 CA 材料形态判定 |
| **1** | 契约还原 | 《Deployer 契约规格》 |
| **2** | 行走骨架 | 空实现 + 契约一致性套件跑通（全部返回 501 也算通过结构验证） |
| **3** | 部署编排 | 状态机 + 预留 + 重试，用假 VM 后端跑通全生命周期 |
| **4** | CA | 签发/轮换/撤销，撤销要证明**真的阻断**，不只是删记录 |
| **5** | OAuth 会话 | PKCE 全流程 + haiku 校验，用合成上游 |
| **6** | 凭据存储与刷新 | 加密存储 + 刷新退避 + 无 token 泄漏的日志 |
| **7** | 影子运行 | 与现有 Deployer 并行接收流量，只比对不生效 |
| **8** | 切换 | 蓝绿切换 + 回滚预案 |

**阶段 2 是关键节点**：契约一致性套件一旦跑通，后续每个阶段都有客观的完成判据，
不靠主观「觉得差不多了」。

## 8. 验证策略

沿用[契约对拍方案](2026-09-23_18-23-00-isthmus-contract-differential.md)的思路：

- **契约一致性套件**：从阶段 1 的规格生成，新旧实现都要通过。
- **影子运行**（阶段 7）：真实请求同时打到新旧两侧，**新侧结果丢弃只做比对**。
  这是唯一能在不冒险的前提下验证等价性的方法。
- **状态机穷举测试**：非法转移必须被拒绝，不能靠调用方自觉。
- **失败注入**：上游超时、数据库断连、证书签发失败、token 刷新失败，
  每条路径都要有确定行为，不能是未定义。

## 9. 切换与回滚

- Portunex 已有 blue/green，Deployer 切换可复用同样模式。
- `PORTUNEX__DEPLOYER__BASE_URL` 是单一切换点——**这既是便利也是风险**，
  切换瞬间影响全部账号授权与 VM 部署。
- **回滚预案必须先于切换存在**：旧 Deployer 保持可用，数据双写或可回灌。
- 切换窗口需与同事协调（见 §11）。

## 10. 边界

- 阶段 0–6 **不碰生产**：不改 216、不改现有 Deployer、不触发真实模型调用
  （haiku 校验在阶段 5 用合成上游）。
- 凭据不进聊天/文档/Git；`API_TOKEN` 至今只读变量名，未读值，**保持如此**。
- 不设计指纹伪装（§5.4）。
- Git：按明确文件 `git add`，**绝不 `git add .`**；既有 WIP
  （`internal/route/reconcile.go`、`image/lab/build.py`、`image/runtimekit/`）
  保持不动。

## 11. 未决项与风险

1. ~~原版可得性未知~~ —— **已闭合**：无源码，但制品是 Bun 二进制，JS 可提取。
2. ~~[高优先] CA 材料可迁移性~~ —— **已降级**，见 §5.1 用户裁定：重装重签是常规操作。
3. **[阻塞阶段 2] HTTP 端点清单仍未知。**
   阶段 0 拿到的是**数据库 schema**，不是 HTTP 契约——两者不能互相替代。
   端点、请求/响应结构、状态码仍需从 Portunex 调用点或提取出的 JS 中确认。
   这是全部实现的前提。
4. **[待查] 服务器侧认证方向存疑。**
   `servers.token_hash` 表明每台服务器持有独立令牌（哈希存储），暗示**存在从服务器
   到 Deployer 的反向调用**；但 `jump_hosts` 又表明 Deployer 会主动 SSH 外呼。
   究竟是单向推送、单向拉取还是双向，**尚未判定**。
   > 记录一次修正：勘察中曾据 `jump_hosts` 断言「SSH 推式，节点不知道 Deployer
   > 存在」，随后 `servers.token_hash` 推翻了该断言。此处保持存疑，不写成结论。
5. **[待补] `oauth_sessions` 与 `jump_hosts` 完整定义**——被 SQLite 页边界截断。
   `oauth_sessions` 是账户授权的核心表，**优先补齐**。
6. **与同事的变更协调**：同事以每日多次的节奏改线上。Deployer 切换是全局单点，
   **没有共享变更计划就不应该切**。这是组织问题，不是技术问题，但会决定成败。
7. **账号健康维护的分工**：`portunex-monitor` 与 Deployer 谁写哪些状态字段，
   阶段 1 须一并查清，否则重写后会出现双写冲突。
8. **安全待办仍未处理**（存量已达 4 项）：216 的 SSH 私钥（9/19 判定应换发）、
   Portunex API key 明文、终端用户邮箱 PII、执行节点安装代理口令。
   重写 Deployer 会引入更多凭据，**建议先把存量清干净**。
9. **[非本计划但需处置] 阿里云执行节点 Postgres 暴露**：
   勘察中发现某执行节点 `5432` 绑定 `0.0.0.0`，另有 110 个 gRPCS 端口同样绑 `0.0.0.0`。
   是否被安全组拦住需确认。**这与重写无关，但优先级高于重写。**

## 12. 不做什么

- 不在契约未还原前写实现代码。
- 不同时改实现与改协议（先契约兼容，再单独升级 mTLS）。
- 不把账号健康维护并进 Deployer——那是 `portunex-monitor` 的职责。
- 不做指纹伪装。
- 不在没有回滚预案、没有与同事协调的情况下切换。

## 13. 执行记录

### 13.1 阶段 0 完成（2026-09-23 晚）

**方式**：用户经 WebSSH 中继执行命令并回贴输出；本会话无该机直连权限。
全程只读，未修改任何文件、未重启任何服务。

### 13.2 Deployer 定位

```text
主机     iZ0xi1n1blszphvfva6hfmZ（内网堡垒机）
进程     portunex-deploy（原生进程，非容器）
监听     172.16.44.68:8443   ← 绑内网 IP，非 0.0.0.0
制品     /opt/portunex-deployer/portunex-deployer-linux-x64
工作目录 /opt/portunex-deployer
```

**同机还跑着整套网关**，均带 `-2` 后缀，是与 216 并行的第二套部署：

```text
portunex-web-2      caddy      3001 → 80     ← 管理前端
portunex-traefik-2  traefik    8090 / 8091
portunex-green-2    转发层蓝
portunex-blue-2     转发层绿
```

这验证了用户的判断：**前端与授权服务同机**。也解释了此前在执行节点上遍寻
Deployer 配置而不得——**执行节点不持有该配置**，方向本就错了。

新旧栈的 Deployer 地址不同：216 那套指向公网 `14.1.29.250:8443`，这套指向内网
`172.16.44.68:8443`。

### 13.3 制品形态：Bun 编译二进制（关键结论）

```text
file   ELF 64-bit LSB executable, x86-64, dynamically linked, not stripped
大小   96,446,592 字节
特征   bun-v1.3.14 / /$bunfs/root/
```

`/$bunfs/root/` 是 Bun `--compile` 的内嵌虚拟文件系统路径——**JS 源码内嵌在二进制
里，可提取**。96 MB 的体积即 Bun 运行时（~90 MB）+ 应用代码；旁证是同生态的
isthmus 核心也在 90 MB 量级。`not stripped` 意味着符号表完整，可读性更好。

**因此阶段 0 的最坏假设（啃裸二进制）没有发生**，路线是「读源码」，
与 `isthmus.readable.mjs` 同路。

### 13.4 数据层

```text
/opt/portunex-deployer/data/   (0700)
  deployer.sqlite       401 KB
  deployer.sqlite-wal   4.1 MB   ← 活跃写入
  deployer.sqlite-shm    32 KB
```

**只有 SQLite，无任何密钥文件。** 20 张表的 schema 见 §4，五个可照搬的设计决定
见 §4.3。存储选型据此重新评估（§6.1）。

### 13.5 CA

自签私有根 `CN=Isthmus private grpcs CA, O=Isthmus`，2026-08-28 起 10 年期。
签发时间早于 216 的部署（9 月），说明该 CA 建立更早。
全盘未找到 `ca.key`。风险评级经用户裁定后降级，见 §5.1。

### 13.6 勘察中的两次自我修正

如实记录，避免后续误引：

1. **接口清单**：曾把 `portunex-server` 中 grep 出的
   `/oauth/sessions/providers/isthmus/deployments/` 拆为两个端点。该串实为字符串表
   中相邻常量被一并提取，**拆分无依据**，已在 §0 更正。
2. **认证方向**：曾据 `jump_hosts` 断言「SSH 推式，节点不知道 Deployer 存在」，
   随后 `servers.token_hash` 显示每台服务器持有独立令牌，**推翻该断言**。
   现列为待查项（§11.4），不写成结论。

### 13.7 勘察中的凭据暴露

`grep -ohE 'https?://...'` 在执行节点上命中了一条内联凭据的代理 URL
（`http://<user>:<pass>@...`，来自 `.bootstrap-profile` 的 `install_proxy`），
该口令已进入会话记录，**应视为泄露并轮换**。

教训：提取 URL 时应排除 userinfo 部分。后续命令改用只出键名或只出主机名的写法。

### 13.8 下一步

按依赖顺序：

1. **提取内嵌 JS**（§13.3）——这是阶段 1 的最短路径。拿到源码后，HTTP 端点清单、
   请求/响应结构、状态机转移条件可一次性取得，无需再从 Portunex 侧反推。
2. 补齐 `oauth_sessions` / `jump_hosts` 定义（§11.5）。
3. 判定认证方向（§11.4）。
4. 产出《Deployer 契约规格》，进入阶段 2。

提取方法待定：Bun 内嵌内容位于二进制尾部，可在目标机上就地提取，也可传回本地
分析。**制品含业务逻辑，传输与存放按仓库外私有目录处理，不入 Git。**
