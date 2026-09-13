# Online stack recovery

本模块是 Portunex + isthmus 恢复工程的开发工具和证据入口，不是第四个生产服务。

- [最新：隔离 PostgreSQL 与认证存储基础](docs/postgres-foundation-2026-09-14.md)
- [此前认证证据/密码模块切片](docs/identity-foundation-2026-09-13.md)
- [此前演示与执行面阶段进度（2026-09-13）](docs/status-and-review-2026-09-13.md)
- [首批旧 HTTP 调用证据](contracts-wire/portunex/README.md)
- [已确认、实施中：旧 Bearer 认证兼容规划](docs/portunex-identity-next-slice.md)
- [目录与模块边界](docs/architecture.md)
- [恢复工具命令与安全边界](tooling/README.md)
- [合同清单维护规则](docs/catalog.md)
- [静态wire观察校验](docs/wire.md)
- [产品范围](../docs/prd/online-stack-recovery-v1.md)
- [实施清单](../docs/plans/online-stack-recovery-v1.md)
- [第一阶段验收](../openspec/changes/restore-online-stack-foundation/verification.md)
- [本地演示切片结构](../openspec/changes/restore-online-stack-demo/design.md)
- [本地演示验收与未通过项](../openspec/changes/restore-online-stack-demo/verification.md)
- [源码证据切片结构与范围](../openspec/changes/restore-online-stack-evidence/design.md)
- [源码证据切片验收](../openspec/changes/restore-online-stack-evidence/verification.md)
- [Portunex Go 演示](../backend/internal/portunex/README.md)
- [独立 React 演示](../portunex-web/README.md)
- [isthmus 协议与 fake runtime](../execution-plane/isthmus-runtime/README.md)

代码按职责拆在 `tooling/recoverykit/` 下；测试放在对应模块测试目录；不含秘密的基线和合同分别放在 `baselines/`、`contracts/`。原始二进制、生产静态包、工作区快照和真实数据不纳入 Git，外部私有目录由命令参数指定。

第一阶段只提供恢复基线、合同登记/校验和协议测试。它不启动旧二进制，不连接生产数据库，不宣称业务兼容已完成，也不自动上传任何文件。

从仓库根目录运行 `make -C recovery check`，执行合成单测、合同目录校验及 Bun 协议测试。需要 Python 3.9+、Git 和 Bun 1.3.9；本模块无第三方运行依赖。

第二切片新增独立 Go/React 演示和 isthmus fake HTTP/WS。先在`portunex-web/`执行`npm ci --ignore-scripts`，再从仓库根目录运行`make -C recovery check-demo`，会额外执行Go race/vet、React类型/组件测试和构建，以及临时loopback端口的fake HTTP/WS冒烟并自动关闭。需要backend指定的Go工具链和Node/npm；不启动线上程序。默认`check`仍不监听端口。演示不使用旧`/portunex/*`合同，不连接DB/Redis或真实模型，不等于整套恢复完成。

另有独立`make -C recovery runtime-shutdown-gate`：检查服务端主动WS关闭后的原生停机。本机Bun 1.3.9未通过，脚本保持非0退出；正常路径`check-demo`通过不能替代此门槛。生产与完整transport验收保持关闭。

认证数据库另有显式 `postgres-integration` 门槛：按 [隔离运行时说明](runtime/postgres/README.md) 提供 `PORTUNEX_TEST_PG_BIN`，
只启动自己的私有 socket 合成 PostgreSQL。普通 `check`/`check-demo` 不启动数据库；缺失运行时不算跳过成功。
