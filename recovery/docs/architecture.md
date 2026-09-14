# 目录与模块边界 ADR-001

> 2026-09-14：下面的 Portunex 业务/网页目录是此前范围的历史记录，现暂停扩展。用户明确的 CCMAX 与原站前端恢复边界、拟定模块目录见 [新计划](../../docs/plans/ccmax-frontend-recovery-v1.md)。不覆盖现有 CCMAX web，也不把生产 JS 当源码执行。

状态：2026-09-13 用户确认启动第一阶段，要求各模块独立目录。本 ADR 在实现前记录。

## 1. 布局原则

1. 先按产品/业务模块分目录，再在模块内按职责分文件，不创建全局大 `handlers/`、`services/`、`utils.py`。
2. `app` 仅装配，不放业务规则；共享目录只收纳至少两个模块需要且职责明确的基础能力。
3. 不创建几十个只有 `.keep` 的空模块。第一阶段只创建真实交付所需目录；其他目录在对应切片落地。
4. 既有 Vue/CCMAX/execution-plane 不搬家，不覆盖现有 5.5c WIP，不把恢复代码塞进已有大文件。
5. source-of-truth 唯一：合同、原件、审阅副本、测试产物分开；原件不作为可执行代码导入。

## 2. 全项目目标结构

```text
backend/internal/portunex/            # R2起创建，Go业务恢复域
  app/                               # 配置/依赖装配/独立路由组
  platform/                          # DB、Redis namespace、时钟、ID等有限基础
  identity/                          # password/session/magic-link/OAuth
  oidc/                              # authorization/token/consent/client
  users/                             # 用户与角色
  apikeys/                           # Key生命周期与权限
  providers/                         # Provider/credential/健康/导入
  catalog/                           # 模型与alias
  pricing/                           # 精确价格/优先级/快照
  quotas/                            # 窗口/RPM/RPS/并发
  billing/                           # points/账本/结算
  orders/                            # 订单与支付适配
  subscriptions/                     # 套餐/订阅/消费窗口
  redemption/                        # 兑换与幂等
  routing/                           # Provider选择与执行适配
  observability/                     # usage/trace/操作审计
  migrations/                        # 仅独立Portunex库，独立runner

portunex-web/                        # R2起创建，独立React工程
  src/app/                           # root/router/providers
  src/modules/<业务模块>/            # pages/components/api/hooks/tests
  src/shared/                        # 受限UI、HTTP、格式工具
  public/                            # 审核过的静态资源，不放备份包

execution-plane/isthmus-runtime/
  contracts/grpc/                    # 唯一原始Messages proto合同
  src/protocol/websocket/             # 第一阶段纯帧编解码及测试
  src/transport/{http,websocket,grpc}/ # 后续listener/流式适配
  src/runtime/{turn,cli,pool,session}/
  src/tools/mcp/
  src/credentials/
  src/app/                           # 后续装配，不能承载所有业务
  test/fixtures/                     # 合成fixture，无账号/真实正文

recovery/
  docs/                              # ADR、工具使用与恢复说明
  baselines/                         # 不含秘密的来源/哈希元数据
  contracts/portunex/<业务模块>/      # 可机器校验的已知/未知合同清单
  contracts/isthmus/                  # 协议合同索引，不复制proto
  tooling/recoverykit/
    evidence/                        # manifest验证、白名单保全
    workspace/                       # 当前WIP可恢复快照
    contracts/                       # 合同结构、证据/状态校验
    wire/                            # ADR-003新增：人工静态观察与原文字节锚定
    cli/                             # 命令解析和组合，不放算法
  tests/{evidence,workspace,contracts,wire,cli}/
  Makefile                           # 一条命令离线验证

openspec/changes/restore-online-stack-foundation/
  proposal.md / design.md / tasks.md / verification.md
  specs/recovery-foundation/spec.md
```

业务模块内按需采用 `domain/`、`application/`、`transport/`、`repository/`；小模块无需为一个文件创建四层空目录。测试就近或按同名模块分组，不放一个超大测试文件。

## 3. 依赖方向

- Portunex transport → 本模块 application → domain/ports；repository实现ports。跨业务域只能走显式应用接口。
- 业务域不直接写CCMAX或worker_runtime，不拥有Docker/Vault控制权。
- Redis key/pubsub/锁/后台任务必须带恢复域namespace，数据库独立role/连接池。
- runtime protocol纯编解码不依赖listener、CLI、OAuth或进程池；transport依赖turn接口，不能反向导入app。
- recovery工具不被生产代码依赖；不执行保全的JS/ELF/Shell。

## 4. 第一阶段实际范围

1. 建立本目录规则与独立OpenSpec，确认新PRD进入实施。
2. 实现离线、白名单、哈希校验的证据保全工具；拒绝越界路径、符号链接、覆盖和敏感文件。
3. 实现已有tracked diff与untracked内容的私有WIP快照；不自动恢复、不stash/reset、不提交/推送。
4. 按业务模块登记Portunex合同及未知项，建立可重复的验证命令；discovered不等于implemented或verified。
5. 固化isthmus原proto和已知WS帧合同，编写纯离线编解码测试，不实现网络listener或模型调用。
6. 为第一阶段增加本地验证与独立CI；真实备份、异机复制、完整schema归档和端到端业务仍保持未完成。

## 5. 保全与Git

可入Git：工具源码、合成fixture、无秘密的manifest、proto、合同清单、文档。

不得入Git：`.env`、私钥/Token、provider凭据、生产数据库/日志、旧发布包、未知内容的整体归档、工作区快照和原始ELF。参考材料存于用户指定的本机私有目录；仅本地副本不代表已完成异机灾备。

新命令必须使用显式输入/输出路径，排他创建，不自动连接服务器或上传；不能把`.gitignore`当成秘密扫描器。
