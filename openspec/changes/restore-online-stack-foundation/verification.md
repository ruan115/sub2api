# 第一阶段验证记录

日期：2026-09-13。**第一阶段恢复底座切片验收通过**，按 ADR-001 先划分目录再实现；不代表完整R0/R1或业务复刻完成。

原有工作区已包含上一轮恢复文档/.gitignore和5.5c未提交工作。本阶段不自动提交/推送，不修改服务器，不迁移生产数据。gateway.tar移出、真实备份和异机存储仍等待各自授权。

## 范围与目录

- `recovery/tooling/recoverykit/{evidence,workspace,contracts,cli}/`：Python 3.9+ 标准库工具，各模块独立目录。
- `recovery/contracts/{portunex,isthmus}/`：按业务域登记发现清单，不充当完整API规范。
- `execution-plane/isthmus-runtime/`：固定 Bun 1.3.9，独立原proto与纯WS codec，无第三方依赖。
- `recovery/docs/architecture.md`：未来 Go Portunex 业务与独立 React 的功能模块布局；不创建空模块占位。
- `.github/workflows/recovery-foundation-ci.yml`：单独运行相同离线入口，无生产secret/发布/上传步骤。

## 已执行的独立验证

| 命令/检查 | 结果 | 边界 |
| --- | --- | --- |
| `make -C recovery check` | Python 55 tests通过；合同CLI通过；Bun 57 tests通过 | 收束全部编辑后主代理重跑，无生产依赖 |
| `bun --cwd execution-plane/isthmus-runtime test` | 57 pass，0 fail，206 assertions | 原proto完整性与合成WS bytes；不是listener/E2E |
| `cd execution-plane && go test -count=1 ./internal/worker ./internal/hostagent ./internal/service ./internal/route` | 4包无缓存重跑通过 | 未重跑全部Go/CCMAX/backend/DB集成 |
| `python3 /Users/ruanyang/.codex/skills/web-reverse-master/scripts/selftest.py` | 7个离线case全部通过 | 技能工具自检，不是恢复系统业务验收 |
| Ruby YAML parser读取新增workflow | 通过 | 语法检查，不是GitHub远端CI运行 |

本机未发现 OpenSpec CLI，本阶段未安装或声称已运行其validator；change按仓库spec-driven格式组织。

新增GitHub CI已配置Python3.9/3.12矩阵与Bun1.3.9；本机实际运行Python3.9.6和Bun1.3.9，尚未触发远端CI，不能声称Linux/3.12矩阵已经通过。`git diff --check`通过；另对77个新增/本阶段文件逐个执行no-index whitespace检查，清理两处末尾空行后全部通过。

合同报告为14个模块清单（Portunex13、isthmus1）、116条发现记录、145项显式未知，全部状态仍为discovered，`business_verification=false`。PG基线保存36个唯一表名、387列/28FK/28成功迁移计数和版本范围；没有生成数据库DDL或读取业务行。

## WIP快照验证

实际运行 `workspace snapshot` → `workspace verify`，目标为 `/Users/ruanyang/My-project/api/z/sub2api-recovery-private.kbZovy/workspace`。结果：**137个产物文件（9份Git状态/补丁材料 + 128个未跟踪文件），1,040,798字节**。

- 分支 `codex/claude-execution-plane-v1`，HEAD `cab5ef0ca4b87d89235164d7e1c09142d34e855b`。
- 保留原有5.5c与本阶段开发时点的改动，不声称这是“本轮开始前”的原始快照。先前91项是旧时点状态计数，不能当作当前新增文件数。
- 主代理独立比较快照前后：9份Git捕获结果逐一SHA一致，index文件SHA一致。工具未改HEAD、分支、暂存区或工作文件。
- 验证不需要原仓库，但真正恢复需要可取得记录HEAD的Git历史；忽略文件、数据库、外部运行物和秘密不在此快照内。
- 此快照先于本验收记录的收尾更新。交付时另建全新 `workspace-final` 子目录保存最终文档，不覆盖这个已验证快照；各自以目录内manifest为完整性依据。

## 本机证据保全

使用全新仓库外目录 `/Users/ruanyang/My-project/api/z/sub2api-recovery-private.kbZovy/evidence`。实际执行 `evidence verify` → `evidence preserve` → `evidence verify-preserved`，均通过：**9个文件，239,543字节**。目录0700、文件0600；工具对全部清单、字节hash、实际文件集合和权限进行了验证。

候选完整bundle触发credential-shaped URL筛查（1处），未输出匹配内容。该文件未进入保全清单，原因和原hash单独记录在 `recovery/baselines/`；原件不修改、不删除。不能将9项成功扩大成“所有运行资料已保全”。

这只是本机私有副本，不含PG记录、私钥、环境文件、ELF或镜像层，不是加密异机备份；没有验证灾难恢复。

## 审查闭环

独立审查提出的3项问题均已修复，并新增回归：

1. Git status/diff可能调用fsmonitor/clean/process filter：禁用hooks/fsmonitor，存在clean/process配置直接拒绝。
2. compact合法JSON经格式化可能膨胀超限：manifest/receipt/快照元数据均在建目录前检查最终编码大小，回归要求精确命中边界错误。
3. manifest ID可能携带凭据型字符串并被回显：验证ID，CLI输出前筛查完整安全摘要，参数错误也不回显用户值。

开发期validator还发现过重复接口归属；跨模块同一路径必须只有一个发现记录。文件改动过程中出现的暂态import错误不算通过证据，最终验收以收束后的完整测试为准。

## 当前未验证

完整Portunex API行为、Rust业务重建、React页面、完整schema、真实账号/支付/模型、生产网络、全栈E2E、数据恢复与异机灾备。

## 下一切片

继续补齐R1的登录/认证/用户/Key/Provider列表的完整method/body/envelope/RBAC合同，再按ADR-001创建Go恢复域、独立DB/Redis边界和React模块，打通假数据闭环；同时为isthmus实现fake TurnEngine与三传输入口。不能把当前codec当作服务启动或生产槽健康依据。
