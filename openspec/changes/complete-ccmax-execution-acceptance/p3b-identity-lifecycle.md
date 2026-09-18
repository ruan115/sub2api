# P3b：实例身份生命周期合同与持久 home 边界

2026-09-19。承接时间命名规划
[2026-09-18_13-17-37-isthmus-claude-handoff.md](../../../docs/plans/2026-09-18_13-17-37-isthmus-claude-handoff.md)
第 4 节 P3 第三、四条，以及 [P3a](p3a-egress-deny-matrix.md)。不重做 P1/P2/P3a，
不改 provider，不加任何挂载，不启用业务开关，暂停中的 `image/lab/build.py` 与
`image/runtimekit/` 继续绕开。

`internal/identitylifecycle` 是**纯规则集**：不存储、不读取、不是第二份状态权威。
`Previous` 与 `Floor` 都由既有权威提供。当前没有任何生产代码 import 它。

## 1. 合同依据的三个既有事实

1. 身份材料（私钥、machine ID、已安装证书）都在 `instance-identity.json`，位于
   `/run/execution/identity`；provider 把 `/run` 挂成 **tmpfs**，`sandbox.go`
   只接受 `/tmp` 与 `/run` 两个 tmpfs、拒绝任何非 tmpfs 挂载。**容器一停，身份就
   没了**，重启起来的是一个不持有任何旧材料的新实例。
2. 对端校验是 **SPIFFE URI 精确匹配** + `NotBefore`/`NotAfter` + EKU + 链校验。
   `runtimeidentity/tls.go` 里没有 CRL、没有 OCSP、没有序列号或新鲜度字段；
   `Binding.URI()` 完全由五元组构造。**因此复用一个绑定元组，会让一张尚未过期的
   旧证书对一个更晚的实例验证通过。**
3. 证书回执唯一键是 `(slot_id, execution_epoch)`——SQL 迁移 014 与内存实现一致，
   **generation 不在键里**；且 `SameReceiptIdentity` 比对 `PublicKeySHA256`。
   所以只推进 generation 的再签发根本拿不到证书。

结论：凡是丢失实例私钥的事件，**epoch 与 generation 必须同时严格前进**；
generation 是每槽只增不复位的计数器，即使槽位换回旧账号也不回退。

## 2. 事件与规则

| 事件 | 含义 | 保留私钥 | 需再签发 | 绑定要求 |
| --- | --- | --- | --- | --- |
| `adopt` | 重新接管**仍在运行**的同一物理容器 | 是 | 否 | 绑定必须完全相同 |
| `restart` | stop/start、崩溃后重建 | 否 | 是 | epoch 与 generation 都严格前进 |
| `upgrade` | 换镜像 digest | 否 | 是 | 同 restart |
| `account-change` | 槽位改派给另一账号 | 否 | 是 | 账号必须变，且 epoch/generation 都前进 |
| `destroy` | 终结该绑定 | 否 | 否 | 不得指定后继绑定 |

另外：`SlotID` 与 `NodeID` 在所有非 destroy 事件中必须不变——换节点是 release +
re-place（新 assignment、新 epoch），不是本历史的一步；`lease.Claim` 也按
slot+node+epoch 键控。事件由执行动作的调用方提供，**永不从绑定反推**：反推会让
一次静默换号伪装成普通重启。

`ValidateHistory(floor, steps)` 逐步校验、要求相邻步骤首尾相接、destroy 之后不得
有任何步骤，并要求**第一步的 Previous 不低于 floor**。floor 是该槽历史上签发过的
最高 epoch/generation（含已销毁的），来自既有权威。没有 floor，一个槽被销毁后可以
从 generation 1 重生，而被销毁实例的证书仍在有效期内——正是本合同要挡的复用。

## 3. 本轮发现的既有缺陷（未修复，需用户决定）

**被停止的 runtime 容器目前无法重新 bootstrap。** 链路：

- `reconcile/reconcile.go:218` 把 `ActualStopped + DesiredReady` 路由为 `ActionStart`；
- `reconcile/control_executor.go:36-41` 用**原有** `ExecutionEpoch` 下发 START，
  且要求 `RuntimeGeneration == DesiredGeneration`，两者都不动；
- `hostagent/bootstrap/coordinator.go:52-63` 按 `spec.Epoch` / `spec.RuntimeGeneration`
  构造同一个 binding 去再签发；
- 但停止已抹掉 tmpfs 身份，实例呈递**新公钥**，而 `(slot, epoch)` 的回执钉住旧的
  `PublicKeySHA256`，`SameReceiptIdentity` 比对失败 → `ErrRejected`。

这是那条路径的缺陷，不是本合同的缺口：在同一绑定上恢复，**经由签发根本不可达**。
关闭它需要把 stopped 路由成 destroy → release → place 以分配新 epoch，属于控制面
行为变更，**本切片刻意不做**。`TestResumeAtTheSameBindingStaysRejected` 把这个冲突
固化下来，避免有人把绿色测试读成"重启现在能用"。

## 4. tmpfs-only 现状与最终持久 home

现状：只有 `/tmp` 与 `/run` 两个匿名 tmpfs，无 volume、无 host bind、无 home。
R4/R5 的 CLI 会话与工具状态最终需要持久 home，所以边界必须**在挂载之前**定死。

单向规则：**身份材料永远不得持久化**，无论 home 变成什么。一旦私钥、machine ID 或
已安装证书能活过容器，重启就不再强制换代，第 2 节的"元组不复用"从外部就无法保证。

`ValidatePersistentPaths` 因此是**白名单**而不是黑名单：Debian 基础镜像上
`/var/run` 是指向 `/run` 的符号链接，纯字符串的"不在 `/run/execution/identity` 之下"
会放行 `/var/run/execution/identity`。只允许 `PersistentHomeRoot = "/home"` 之下，
整类别名问题就没了；身份重叠检查仍保留并**先于**白名单执行，以便给出精确原因，
并在将来放宽根目录时依然有效。

通过该检查**不等于批准一个持久化设计**。仍未决、必须另行设计的：每实例归属与
uid 1000 所有权、按账号分离、换号/销毁时的擦除、加密与备份归属，以及挂载时按
**解析后真实路径**执行（镜像内 `/home` 下的符号链接本层覆盖不到）。本文件不挂载
任何东西，也不给 provider 加宿主目录。

## 5. 本切片**没有**做的事

- 没有生产代码调用本包；没有任何组件因此改变行为。
- 没有实现轮换：`InstallCertificate` 仍拒绝替换已安装证书，代次推进仍无人执行——
  仓库中 `Epoch`/`Generation` 依旧纯外部供给，**没有任何代码递增它们**。
- 没有做撤销传播：lease 撤销仍只标记 DB，不拆除身份或已建立的 worker mTLS 连接。
  （P3a 只回收了出口 tunnel，那是代理绑定，不是身份。）这归 P4。
- 没有连 Docker、没有 SSH、没有请求模型、没有部署。
- `/readyz` 保持 503、`production_ready=false`；缺权威 `execution:lease:v1:` writer
  时生产签发继续拒绝。

## 6. 验证

`internal/identitylifecycle`：事件矩阵含允许对照，每个拒绝断言**具体原因串**而非
仅 `errors.Is`；floor 阻断销毁后重生（含 epoch 高于 floor 但 generation 低于 floor
的半回退）；历史链接、destroy 终结性、换号来回不复位；持久路径白名单含
`/var/run` 别名、`/home` 本身、`/homework` 段边界、控制字符与超长路径。

两轮独立 adversarial review。第一轮 6 项：与回执唯一键矛盾（headline）、
destroy 后重生未锚定、测试空跑、`/var/run` 别名绕过、分层把 `provider/docker`
拉进纯规则集、`NodeID` 未约束——全部已修。第二轮确认前述修复，并指出身份重叠检查
被白名单遮蔽成死代码、缺控制字符/长度边界——已修（重排顺序 + 边界 + 可达性断言）。
`go list -deps` 确认生产包只依赖 `runtimeidentity`，`provider/docker` 仅出现在测试。

本机离线：全量 `go test -race`、`go vet`、linux/amd64 `go build ./cmd/...` 通过；
本包 race ×5 通过。`make -C recovery check`：236 Python / 150 Bun / 186 镜像 Python。

分数不变，仍 32%；VM0b/VM0c/N3 保持开放。合同与设计不是实证，不加分。
