# 源码证据切片验收

日期：2026-09-13。范围：独立静态观察校验工具与Bun修复候选调查。不是旧业务兼容、数据恢复或上线验收。

## 交付范围

开发前先写ADR-003；实现按`wire/{schema,anchors,validation,errors}.py`拆分，测试单独放`tests/wire/`，既有CLI只负责接线。没有新增空业务目录，没有改变Go/React演示业务，也没有挂入生产路由。

`recoverykit wire verify`接收四个显式输入，验证严格schema、既有catalog API ID/path、来源文件整体哈希/大小、UTF-8字节片段哈希和literal。catalog只加载一次，对同一份内存完成结构验证和引用查找；来源文件也对已校验的同一份字节做锚定。

拒绝未知字段、重复JSON键/观察ID/anchor、bool下标、越界、多字节截断、过量输入、软链接及可识别秘密。源码只读、不执行，不访问网络，不改catalog。成功只返回计数、`source_anchored`及`business_verification=false`；输出不含路径、原文、statement或失败细节。来源锚定不能证明人工解读正确，更不能证明旧客户端兼容。

## 实际运行

| 检查 | 结果 |
| --- | --- |
| 新wire单测与CLI接线 | 18项wire、5项CLI通过；全部合成材料，含真实CLI成功/篡改/祖先软链接路径，不只mock接线 |
| catalog读取安全 | 新增7项回归通过，含各层软链接、检查后换链、FIFO、读取时变化、NUL和单文件/总量/数量/枚举预算 |
| 最终完整Python恢复工具 | 85项通过；在catalog加固与最后CLI测试之后重新完整运行 |
| `make -C recovery check-demo` | 最终exit 0：85项Python、111项Bun、Go 7包race/vet与独立命令编译、33项React、typecheck/build、实际正常回环HTTP/WS与停止均通过 |
| 旧execution-plane | `go test -count=1 ./internal/worker ./internal/hostagent ./internal/service ./internal/route`四包通过 |
| 合同清单 | 仍为14模块、116条记录、145项未知，`business_verification=false`；未新增生产观察或提升业务状态 |
| `make -C recovery runtime-shutdown-gate`，Bun 1.3.9 | **未通过**：probe exit 1、make exit 2；服务端主动关闭后无法验证原生stop完成。仍为独立未通过门槛 |
| 辅助检查 | 两份恢复CI及OpenSpec元数据YAML解析、tracked `git diff --check`及246个未跟踪文件空白检查通过；CI触发路径覆盖本切片。未推送或运行GitHub Actions；未安装OpenSpec CLI |
| 描述符压力检查 | 独立测试进程将自身软限制设为64后连续加载真实14模块catalog 100次通过；未更改系统或其他进程限制 |
| `web-reverse-master`离线自测 | 7项通过，未执行恢复包 |

本轮未重做UI浏览器验收，也未改UI；上一切片的浏览器结果保留为历史记录，不冒充本轮新结果。新测试及正常smoke通过不能覆盖独立停机失败。

## 仍未满足的条件

SSH只读复核一次仍返回`kex_exchange_identification: Connection closed by remote host`，发生于密钥认证前。本轮没有新增远程读取内容，更没有远程写入。缺失Portunex静态资源导致method/body/envelope/cookie等仍未知；36表名与聚合列/外键/迁移计数也不足以恢复完整DDL。不能据此开做声称兼容的旧接口或真实数据库迁移。

Bun已找到[上游修复PR #32488](https://github.com/oven-sh/bun/pull/32488)，并核实候选[1.4.2官方版本](https://github.com/oven-sh/bun/releases/tag/bun-v1.4.2)包含该修复。只读取发布/提交/制品元数据；尚未下载或执行候选。按`web-reverse-master`的外部依赖规则，已请求隔离下载确认；在确认前暂停该下载步骤，其他开发不受影响。详见runtime的`docs/bun-stop-fix-validation.md`。系统Bun、项目pin、CI仍为1.3.9，D4.1b保持未勾选。

未提交、推送、部署，未执行原始JS/ELF/脚本、模型调用、生产备份/数据导出、密码或Key获取；未改变服务器、防火墙、流量、旧Vue和execution_onboarding状态。

## WIP隔离与留档

独立审查实际复现了catalog loader祖先软链接、检查后换链及无界读取边界缺口。现已改为`contracts/filesystem.py`的逐级descriptor/no-follow读取，限制单文件1MiB、总量16MiB、256份manifest、每目录512项；同一regular fd前后fstat及父目录引用/稳定性检查均已接入。来源文件/观察文档的测试没有被拿来替代catalog测试。

命令行祖先软链接用例先在旧loader上实际失败（错误返回0），修复后返回2/`WireError`，无原文输出；原有13项catalog测试继续通过。主代理复查还修正了上下文管理器误包装业务错误及潜在setup清理问题，最终安全测试和完整回归均通过。没有把审查发现仅记入文档后留在代码里。

与上一切片`workspace-demo-final`比较：HEAD、分支、staged/unstaged/tracked patch的SHA-256全部相同；既有CCMAX和execution-plane内部50个未跟踪WIP文件字节相同。没有覆盖已有5.5c改动。

已创建仓库外私有`/Users/ruanyang/My-project/api/z/sub2api-recovery-private.kbZovy/workspace-evidence`并用独立`workspace verify`校验通过：255项（9份Git元数据/补丁、246份未跟踪文件），1,475,463字节。快照前后Git状态各部分与index字节SHA-256一致；再次比较上一切片的原有5.5c WIP仍一致。

验收台账更新后的交付副本另存同父目录的`workspace-evidence-final`，不覆盖上述快照；其`manifest.json`记录准确计数与大小，可使用`recoverykit workspace verify --directory`独立复验。已有保全代次不覆盖或删除。WIP快照不包含Git完整对象历史、被忽略产物、数据库或真实凭据，本机副本不能称为异机灾备。

## 下一步

先补齐授权静态资料与schema来源，使用新工具锚定人工观察，再冻结旧接口并实现兼容适配和独立持久化。Bun候选另在获准后进行隔离下载、官方哈希校验及两种真实关闭路径回归；只有实测通过才更新pin和关闭对应门槛。
