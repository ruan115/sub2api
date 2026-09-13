# 阶段进度、review 与 Git 交付

日期：2026-09-13。分支：`codex/claude-execution-plane-v1`。

## 结论与边界

当前可交付的是执行面 5.5c 收尾、恢复工具底座、接口发现清单，以及
Go/React 管理演示和 isthmus fake HTTP/WS。不是整套线上系统的兼容替代品。
总计划 R0/R1 尚未完成，R2 仅完成部分工程底座；不能按文件数推算完成比例。

用户要求现有调用方接口优先兼容，Portunex 与 isthmus 两者都恢复。
本次只整理本地成果、review、修复明确问题并分阶段提交；不推送、不部署、
不替换线上程序或系统 Bun，不启用 `execution_onboarding`，不标记账号 `migrated`。

## 已完成与未完成

| 模块 | 已完成的本地成果 | 主要缺口 |
| --- | --- | --- |
| 既有 execution-plane | 5.5c：有序 outbox、持久化 onboarding 协调、重放/blocked retry、重复身份 drain/archive、私网路由发布 | 5.6–5.7 凭据刷新/迁移，以及 Phase 6–11 网关、真实数据面、CLI/MCP、生命周期、审计/发布/验收 |
| `recovery/` | 按模块拆分的快照/保全/合同清单/source-anchored wire 校验工具；14 个模块、116 条发现记录、145 项未知 | 全量线上产物来源与哈希、完整请求响应/权限合同、独立异机备份与恢复演练 |
| Portunex Go/React | 独立恢复命名空间、合成会话/RBAC、用户/Key/Provider 只读列表；独立 React，不替换原 Vue | 旧 HTTP 路径兼容、真实 DB/Redis、密码/Key 生命周期、账务/价格/订单/订阅/支付/OIDC 等业务 |
| isthmus runtime | 保留来源的 proto、WS codec、fake turn、HTTP/WS 流式/取消/busy/背压与本地冒烟 | gRPC 服务、真实 CLI/进程池/MCP/凭据、VM/镜像复现、执行平台接线、三 transport 兼容验证 |
| 数据恢复 | 已知 36 表、387 列、28 外键和 28 条迁移的库存信息 | 完整 DDL/索引/约束/函数/触发器；一致性数据导出和余额/账务对账，库存信息不等于可建库 schema |

Go 演示使用专用 `/__recovery__/v1`，不是已兼容的旧 `/portunex/*`。
Fake runtime 不调用真实模型；源码锚定只证明观察关联了源字节，不证明业务兼容。

## 本次独立 review

分别审查执行面、恢复工具/运行时、Go/React 管理演示，并对会话修复进行交叉检查。
本次确认的三个 P2 已按以下边界修复；这是当前切片的代码审查，不是生产安全认证。

| 问题 | 修复与回归 |
| --- | --- |
| 已归档 duplicate-identity 历史行占满批次，后续账号无法处理 | 候选 SQL 排除已归档行；覆盖历史行多于分页大小、有待处理账号及全归档空操作，先红后绿 |
| logout 未完成即重新 login，迟到的清 cookie 响应使 UI 与服务端会话不一致 | 同步 ref 排他与 Promise 串行化；登出等待正在进行的登录、重复登出复用请求、UI 在 auth pending 时禁用登录、拒绝陈旧初始化响应 |
| Git 子进程一次性收集输出，事后限额不能防止内存失控 | 新增独立 `workspace/process.py`：stdout 64 MiB、stderr 1 MiB 流式限额，总 deadline 60 秒，额外清理最多 1 秒；只清理自身启动的独立进程组；新增 12 项回归 |

全量 race 另发现 route 测试替身的 map 存在数据竞争。已在替身增加读写锁，
使完整 publish/unpublish 模拟原子的 Redis Eval；未改生产路由语义、未禁用 race。

## 验证记录

- `execution-plane`：全量 `go test -race -count=1 ./...`、`go vet ./...` 通过；route 包另连续 20 次 race 通过。
- `ccmax-manager`：全量 `go test -count=1 ./...`、`go vet ./...` 通过；duplicate/onboarding/outbox 相关测试另启用 race 通过。
- `make -C recovery check-demo`：Python 97 项、Bun 111 项、React 42 项（本次新增 9 项）、Go 恢复域 race/vet、React 类型检查/构建及正常回环 HTTP/WS 冒烟全部通过；会话修复另经只读交叉 review，未发现新的明确 P1/P2。
- 合同校验仍返回 `business_verification=false`，未把 discovered 或 unknown 项升级为已兼容。
- 未配置真实 MySQL/Redis 集成 DSN；这些集成测试跳过，route Eval 模型测试不替代真实 Redis Lua/TTL 验证。未运行 Docker E2E、Linux 运行时矩阵、生产灰度或线上测试。

## SSH 与 Bun 状态

使用用户提供的 `BindInterface=en0`，同一密钥只读 SSH 验证已成功。
因此当前不应写为“服务器 SSH 不可用”；完整线上资产采集仍待继续。

Bun 1.4.2 已在仓库外隔离验证，完整 demo 和正常/1011 关闭各 5 次通过。
系统、项目和 CI 仍固定 1.3.9；其服务端主动 WS 关闭后的原生停机门槛仍失败。
候选 Mac 验证不等于已采纳升级或 Linux/生产验证，D4.1b 保持未完成。
详细记录见 [隔离验证](../../execution-plane/isthmus-runtime/docs/bun-stop-fix-validation.md)。

## Git 与留档

提交前已使用经过 review 的恢复工具另建 `workspace-pre-commits` 私有 WIP 快照，
并再次独立执行 `workspace verify` 通过：259 项（9 项 Git 状态材料和 250 个
untracked 文件）、1,515,203 字节，起点 HEAD 为 `cab5ef0`。原有私有快照保持不动。
该快照保存提交前代码和文档草稿；随后新增的交付哈希/台账入口由本次文档提交保存。

已按依赖顺序形成以下本地提交，未混入 `codex/onboarding-user-1688`：

| 提交 | 范围 | 文件数 |
| --- | --- | --- |
| `6bceb4bd0a32b751ebd6c591c2dd615b262ff8e8` | 既有 execution-plane 5.5c 收尾与 review 修复 | 96 |
| `acb2dd998d733cd0309f3fe43c12be2aebd30742` | 恢复规划、模块化证据工具、协议与 fake runtime、离线 CI | 130 |
| `7a33f2d3ce329c7e68d3f9e385ead486e1a7909b` | 独立 Go/React 管理演示、9 项新增会话回归、demo CI | 70 |

第四个文档提交保存本台账、README 入口及总计划最新记录。各切片先前的
“未提交”措辞是对应时点的历史记录，以本节的最新交付状态为准。

只纳入源码、合成测试、锁文件和不含秘密的文档/清单；不纳入私钥、生产数据、
原始二进制、隔离 Bun、私有证据、`node_modules`、`dist`。提交前检查暂存区路径、
文件类型/大小、可识别秘密模式和空白错误均通过；模式检查不等于绝对无秘密的证明。
本地 Git 提交与同机私有快照均不等于异机灾备；本次没有自动 push 或上传备份。

## 下一阶段顺序

1. 继续只读采集线上静态产物、迁移/schema 线索，按白名单保全并冻结旧 HTTP/WS/gRPC 合同；未知项不猜。
2. 在独立测试库和 Redis 命名空间实现兼容认证、用户/Key/Provider 的真实持久化切片，再逐项补账务等业务。
3. 完成 gRPC 与真实运行内核，接入现有执行面；并行补 5.6/5.7 依赖和 Linux 停机验证。
4. 完成合成对照、数据恢复/对账、异机备份与演练，再另行审批任何生产变更。

完整未完成项以 [总计划](../../docs/plans/online-stack-recovery-v1.md) 和各 OpenSpec task 为准，
不会因本次提交而批量勾选。
