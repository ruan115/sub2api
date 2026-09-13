# 本地演示切片验收记录

日期：2026-09-13。范围：新增代码的合成业务与fake transport；**不是旧系统兼容验收或上线证明**。

## 实际实现与模块边界

- Go：`backend/internal/portunex/{identity,users,apikeys,providers,platform,app}/`，独立`cmd/portunex-demo`。专用`/__recovery__/v1`登录/登出/会话/只读列表；随机session摘要、TTL/撤销、原子换新、角色及Key owner过滤、输入与本地访问限制。
- React：`portunex-web/src/modules/{identity,users,apikeys,providers}/`，独立app/mock/adapters/shared。默认mock；显式Go模式才走固定回环代理。运行时校验响应模型，不把TypeScript断言当验证。保留现有Vue。
- isthmus：`src/runtime/turn/`拥有fake核心；`src/transport/{http,websocket,shared}/`拥有适配；`src/app/`只装配fake。覆盖逐块输出、非流式有界收集、取消、busy、背压与关闭；proto/codec仍是单一来源。
- 结构在实现前分别记录于ADR-002、`portunex-web/docs/structure.md`及runtime的`docs/fake-transport-slice.md`。没有把新增业务塞进原有大服务文件，也没有加入生产wire。

## 本机运行结果

工具链：backend Go 1.26.6、Bun 1.3.9、Node 20.20.2；React依赖使用固定版本和`package-lock.json`。

| 实际运行 | 结果 |
| --- | --- |
| `portunex-web`: `npm ci --ignore-scripts --no-audit --no-fund` | 成功按lockfile重装114个包；随后构建通过。未声称依赖漏洞审计完成 |
| 仓库根：`make -C recovery check-demo` | 最终exit 0：离线验证、Go/React、正常路径smoke通过。中间发现的正常关闭竞态已修复；独立异常停机门槛仍失败，不能互相替代 |
| Python恢复工具 | 55项通过 |
| 合同清单校验 | 14模块、116条发现记录；145项未知，`business_verification=false` |
| Bun离线协议/turn/HTTP/WS/app | 最终111项通过，12个测试文件，含停机deadline补丁；默认单测不监听端口 |
| Go演示域 | 7个包`go test -race -count=1`及`go vet`通过，独立命令编译通过 |
| React | `typecheck`、33项测试、`build`通过；默认mock制品输出在被忽略的`dist/` |
| 显式`bun run test/loopback.smoke.ts` | 实际临时loopback HTTP/WS通过；首chunk早于完成，取消、START/CHUNK/END、Host/Origin拒绝、停止后active/sessions均为0 |
| 显式`bun run test/shutdown-regression.smoke.ts` | **未通过，exit 1**：服务端1011关闭后的原生stop触发超时；保留为独立发布门槛，不能被正常smoke覆盖 |
| 旧execution-plane回归 | `go test -count=1 ./internal/worker ./internal/hostagent ./internal/service ./internal/route`四包通过 |
| 辅助检查 | 新CI YAML语法、tracked diff及167个新增文件空白检查通过；GitHub Actions未推送/运行，不冒充Linux CI已通过 |

本地`check`仍只运行离线验证；`check-demo`新增的smoke只创建自己的回环临时端口，结束时关闭，不连接线上、模型或数据库。

## 实际浏览器联调

以`go run ./cmd/portunex-demo`和`VITE_RECOVERY_BACKEND=go npm run dev`启动自己的本地进程，使用合成账号，在本地浏览器确认：

1. 管理员登录后展示“Go内存服务”，用户、Key、Provider各3条记录均解析显示；Provider状态枚举与Go一致。
2. 搜索无匹配返回0条与空态，不残留上一批记录；恢复搜索后可切换列表。
3. 退出返回登录页；普通用户仅显示工作台，直接打开`/dashboard/users`显示无权限。
4. 已人工查看用户列表截图，默认视口布局无明显溢出或遮挡；浏览器error/warn日志为空。
5. 验证后登出，关闭临时页面、Go与Vite进程。没有保留后台预览或真实会话。

组件测试另覆盖loading/empty/error/unauthorized、会话过期与畸形响应；本次没有做移动端、多浏览器或旧页面像素级对照。

## 审查与WIP隔离

独立审查和联调已促成修复：Provider枚举漂移；会话满额下的原子换新；HTTP响应运行时校验；fake监听器Host/Origin限制和body读取期限；底层HTTP/WS接收预算；停机必须等待而不是丢弃原生Promise。正常client-close与stop的竞态已通过调整native-stop先后顺序修复，不使用sleep掩盖。

### 仍未通过：Bun服务端主动关闭后的原生停机

增加真实server-initiated `1011`关闭检查后，本机Bun 1.3.9的`server.stop(true)`不结束，尽管fake turn/session已清理。独立Node+ws客户端收到1011并正常退出后仍复现；纯Bun最小程序的1000/terminate也出现该模式。默认离线mock不能发现这个原生问题。

该现象与[Bun上游问题 #36223](https://github.com/oven-sh/bun/issues/36223)一致；该报告测试的是1.3.14，不能根据issue的closed标签断言本机1.3.9已修复。本轮不改变wire关闭码、不偷偷升级工具链，也不把Promise超时当成功。演示stop增加有界失败保护；正常路径仍须等待真正结束。异常停机门槛保留未完成，需要后续隔离验证修复版本或经确认的传输实现方案。

默认`shutdownTimeoutMs=3000`，超时抛`FakeShutdownTimeoutError / FAKE_SHUTDOWN_TIMEOUT`，`stats().shutdown=failed`而非`stopped`。超时保护不会替Bun释放卡住的原生句柄；显式probe在失败后仅退出自己的临时进程，应用库不自行退出主进程。最终probe按预期揭示失败并非0退出，不能改成跳过或期待失败即视为停机验收通过。

与上一阶段`workspace-final`快照逐段/逐字节对照：HEAD、分支、staged patch未变；已有tracked patch除本轮`.gitignore`放行前端测试外保持一致；原CCMAX及execution-plane内部的50个未跟踪WIP文件未变。本轮未commit/push/stash/reset/切分支。

已用`recoverykit workspace snapshot`生成仓库外私有`/Users/ruanyang/My-project/api/z/sub2api-recovery-private.kbZovy/workspace-demo`，再独立`workspace verify`通过：236项（9份Git元数据/补丁、227个未跟踪文件），1,394,718字节。快照前后Git状态各组成部分及index字节SHA-256一致。验收台账更新后的交付副本另存同父目录的`workspace-demo-final`；清单文件为`manifest.json`，可用相同`workspace verify --directory`复验。

留档只含Git状态、补丁和未忽略文件，不包含node_modules/dist、Git完整历史、生产数据或凭据；本机副本不等于异机灾备。未上传任何恢复资料，已有留档代次不覆盖或删除。

## 尚未完成与下一步

- SSH在密钥认证前断开；静态HTTP/HTTPS没有返回资源。旧Portunex method/body/envelope/cookie合同仍未知，详见`recovery/contracts-wire/portunex/README.md`。需要确认SSH端口/白名单或提供已授权的脱敏静态资料；无需发送密码。
- 没有真实DB/Redis接入或DDL恢复，没有真实密码/Key/Provider生命周期、账务、OIDC/OAuth、数据迁移。
- isthmus没有真实CLI/凭据/上游、gRPC绑定/服务、SSE聚合、MCP、keepalive/compression全面对照；fake的资源与本地准入策略不是原版合同。
- 未执行恢复的JS/ELF/脚本，未请求真实模型，未导出生产数据，未修改服务器服务、流量、防火墙或`gateway.tar`；未启用execution_onboarding或标记migrated。
- 下一切片先补旧wire/schema证据，再做兼容适配和独立持久化；不能把demo DTO改名后当作旧接口发布。

技能影响：`web-reverse-master`使本轮保持静态证据与新fake代码隔离，原材料不执行；其离线自测7项通过。`frontend-design`用于独立模块化的本地管理UI，不作为原页面视觉一致性证明。
