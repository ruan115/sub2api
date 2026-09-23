# 2026-09-23 22:40 — 交接：从提取源码还原 Deployer 契约（阶段 1）

时间：Asia/Shanghai（UTC+08:00）。承接
[Deployer 重写规划](2026-09-23_19-06-00-deployer-rewrite.md)。

本文是**开新会话时的完整上下文交接**。阶段 0 已完成，本轮做阶段 1。

## 0. 三十秒速览

要重写的 Deployer 已定位、已提取源码。**源码是明文可读的**，因此阶段 1 不需要
再做任何逆向推测——直接读函数体，把 HTTP 契约写成规格文档即可。

| 项 | 值 |
| --- | --- |
| 目标 | 产出《Deployer 契约规格》，解除重写的阶段 2 阻塞 |
| 制品位置 | `/Users/ruanyang/My-project/api/z/deployer-source.Kx8mQr2v/dep-js.txt`（**仓库外**，0700/0600） |
| 制品内容 | 1.6 MB，压缩后的 JS，从 Bun `--compile` 二进制中 `strings` 提取的 16 条超长行 |
| 是否入 Git | **否**。按本项目惯例（见 §4），仓库外私有目录 |
| 不需要 | 连接任何服务器；本轮纯本地分析 |

## 1. 已知的事实（勿重新考证）

### 1.1 Deployer 是什么

```text
主机     iZ0xi1n1blszphvfva6hfmZ（阿里云内网堡垒机）
进程     portunex-deploy（原生进程，非容器）
监听     172.16.44.68:8443
制品     /opt/portunex-deployer/portunex-deployer-linux-x64  (96 MB, Bun --compile, bun-v1.3.14)
数据     /opt/portunex-deployer/data/deployer.sqlite  (SQLite + WAL)
同机     portunex-web-2(3001) / traefik-2(8090,8091) / green-2 / blue-2  ← 第二套完整网关栈
```

216 那套旧栈指向公网 `14.1.29.250:8443`，这套新栈指向内网 `172.16.44.68:8443`。

### 1.2 已还原的 HTTP 契约（阶段 1 的起点，非终点）

```text
POST /v1/oauth/sessions                 oauth.start(caller, body)          201
GET  /v1/oauth/sessions/:id             oauth.view(caller, id)             200  ← 同步，无 await
POST /v1/oauth/sessions/:id/authorize   oauth.authorize(caller, id, body)  202
POST /v1/deployments                    deployments.submit(caller, body)   202  ← 无 await，异步投递
POST /v1/deployments/adopt              deployments.adopt(caller, body)    200
GET  /v1/instances/:id                  deployments.instanceSettings       200
```

鉴权：

```js
const Q = tH(i);
const e = Q ? A.callers.authenticate(Q, g) : null;   // 令牌 + 路径
if (!e) y("Server access denied", "forbidden");
```

`authenticate(token, path)` 带路径参数说明**权限按端点划分**；错误文案 "Server
access denied" 表明调用方是 *server*，对应 `servers.token_hash` 列。

分发三段：`/admin/api/*` → 管理后台；`/v1/*` → 机器面；其余 → 静态资源。
错误日志只记 `{method, path}`，**不记请求体**。

### 1.3 调用方向（已定论）

| 方向 | 协议 | 用途 |
| --- | --- | --- |
| 网关(Portunex) → Deployer | HTTPS + 每服务器令牌 | 请求建部署、发起授权 |
| Deployer → 各服务器 | SSH（`jump_hosts` 表） | 实际安装 VM |

### 1.4 数据模型

20 张表，完整 schema 见[重写规划 §4](2026-09-23_19-06-00-deployer-rewrite.md)。
四个已取得完整定义的表：`jobs`、`managed_deployments`、`servers`、`vault`。
**`oauth_sessions` 与 `jump_hosts` 的定义被 SQLite 页边界截断，本轮可从源码补齐。**

值得照搬的设计：`vault` 是主密钥校验哨兵（主密钥在库外）；`jobs` 以
`UNIQUE(server_id, request_id)` + `input_hash` 实现幂等；字段级加密而非整库加密。

## 2. 本轮要做什么

### 2.1 主任务：产出《Deployer 契约规格》

对 §1.2 的 6 个端点，逐个从源码中读出：

- **请求体结构**（字段名、类型、必填性、校验规则）
- **响应体结构**（成功与失败）
- **错误分类与状态码**（`y(...)` 抛错处的分类字符串）
- **幂等语义**（哪些字段参与 `input_hash`）
- **异步边界**（202 返回后，客户端如何轮询结果）

产出文件：`docs/plans/` 或 `recovery/docs/` 下的契约规格（中文，与既有文档同体例，
区分「源码实证 / 推断 / 未验证」三级）。**这是阶段 2 的唯一依据。**

### 2.2 顺带补齐

- `oauth_sessions` / `jump_hosts` 的表定义（从源码的 SQL 语句中找）
- `A.callers.authenticate` 的权限模型（按路径怎么划分）
- `deployments.submit` 之后的任务流转：谁消费 `jobs`、怎么重试、怎么写回
- `oauth.authorize` 里的 attestation 字段用法（**只记录，不设计伪装，见 §4**）

### 2.3 分析方法

源码是压缩的单行 JS。建议：

1. 先用 Prettier/Biome 之类格式化，或直接用编辑器的 JS 格式化
2. 按函数名定位：`oauth.start`、`deployments.submit`、`callers.authenticate`
3. 压缩后标识符被改名（`A`、`t`、`Q` 等），但**字符串字面量、字段名、SQL 语句
   全部保留**——契约信息主要在这些地方
4. `not stripped` 的二进制里还有符号表，必要时可回到二进制补充

## 3. 完成之后（不在本轮）

阶段 2 是「行走骨架」：空实现 + 契约一致性套件跑通（全部返回 501 也算通过结构
验证）。详见[重写规划 §7](2026-09-23_19-06-00-deployer-rewrite.md)。

技术选型有一处待定：重写规划 §6.1 原假设 Postgres，但**原版用 SQLite**，已标注
建议沿用。语言建议 Go（可复用 `execution-plane/internal/` 的证书与租约实现）。

## 4. 硬约束

- **制品不入 Git。** `dep-js.txt` 含完整业务逻辑，留在
  `/Users/ruanyang/My-project/api/z/deployer-source.Kx8mQr2v/`（0700/0600）。
  本项目已有 11 个同类私有目录，先例是 `isthmus-static-analysis.HjfIFn`——
  90 MB 的 `isthmus.readable.mjs` 在仓库外，只有 4 KB 的 `messages.proto` +
  provenance 入库。契约规格文档可以入库，**源码不行**。
- **分析前先扫凭据。** 压缩 bundle 可能内嵌默认令牌/密钥常量。发现即记录存在性，
  **不抄进任何文档**。
- **本轮不碰任何服务器**，纯本地分析。
- 不设计指纹伪装（attestation 只记录字段，是否复现属产品决策）。
- Git：按明确文件 `git add`，**绝不 `git add .`**；既有 WIP
  （`internal/route/reconcile.go`、`image/lab/build.py`、`image/runtimekit/`）
  保持不动。`docs/plans/*` 被 gitignore 覆盖，新增 plan 需 `git add -f`。

## 5. 环境陷阱（本轮用不到服务器，但留档）

本机 **Clash Verge 开着 TUN 模式**（fake-IP `198.18.0.0/16`，`utun3`，`mode: global`），
**会掐断所有出站 SSH**，症状是 `kex_exchange_identification: Connection closed`——
极像服务器端封禁，实为本地问题。此时 `ping` 返回 ~0.4ms（utun 本地应答）、
`nc -z` 对任意端口都报 open（SOCKS 乐观应答），**常规探测全部失效**。

绕过（用完务必恢复，守护进程会自行改回）：

```bash
curl -X PATCH http://127.0.0.1:9097/configs \
  -H 'Content-Type: application/json' -d '{"tun":{"enable":false}}'
# 验证 route -n get <ip> 显示 interface: en0；恢复用 {"tun":{"enable":true}}
```

`git push` 走 `ssh.github.com:443`，TUN 开着也通。

**WebSSH 中继注意**：堡垒机只能经 WebSSH 访问。该通道会吃掉 `|` 和 `\(`，
所以 grep 的 `-E 'a|b'` 形式会静默返回空——**一次只查一个模式**，别用交替。

## 6. 未闭合项

1. **[本轮主线]** 6 个端点的请求/响应结构未知。
2. `oauth_sessions` / `jump_hosts` 表定义待补。
3. **CA 私钥下落**：全盘未找到 `ca.key`，可能在 `settings` 表加密列。
   风险已降级——重装 VM + 重签是本项目常规操作，不阻塞重写。
4. **账号健康维护分工**：`portunex-monitor`（216 上的 Python 服务）与 Deployer
   谁写哪些状态字段，需确认，否则重写后双写冲突。
5. **与同事的变更协调**：同事以每日多次节奏改线上，Deployer 是全局单点。
   **没有共享变更计划就不应该切换**。组织问题，但决定成败。

## 7. 安全待办（存量 4 项，均未处理）

重写会引入更多凭据，**建议先清存量**：

1. 216 的 SSH 私钥——[9/19 规划](2026-09-19_16-31-04-isthmus-runtime-custody.md)
   第 163-168 行已判定应换发，至今未做，其后又多次使用。
2. Portunex API key 明文（前缀 `axTSBXU8`）——读 Redis `aksettings` 时整条记录
   含 `key_text`，已进会话记录。
3. 终端用户邮箱 PII——`/opt/gateway/logs/refusal_guard_dumps/` 下文件名直接以
   用户邮箱命名，任何目录列举都会暴露。建议反馈运维方改命名。
4. 执行节点安装代理口令——`.bootstrap-profile` 的 `install_proxy` 是内联凭据的
   URL（`http://<user>:<pass>@...`），提取 URL 时被带出。

**另有一项非安全待办但优先级高**：某阿里云执行节点 `5432`(Postgres) 与 110 个
gRPCS 端口绑在 `0.0.0.0`，需确认安全组是否拦截。

## 8. 远端临时文件清理

勘察在堡垒机上留下了两个临时文件，含完整业务源码，**用完应删**：

```sh
rm /tmp/dep.txt /tmp/dep-js.txt
```
