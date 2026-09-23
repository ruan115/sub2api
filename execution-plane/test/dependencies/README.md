# Isthmus isolated dependency baseline / Claude handoff

2026-09-19，代码基线 `32de688`。本目录的 `fixture/` 只准备可再生的合成测试环境。
本轮已创建真实依赖并执行现有三个集成测试；**不是阶段 2b 或完整阶段 3 授权闭环验收**。

## 当前资源（只有这些是本轮新建）

| 项目 | 值 |
| --- | --- |
| 测试主机 | `170.106.159.197`，ubuntu；`43.153.75.220` 仅 SSH 跳板 |
| 远端私有目录 | `/var/tmp/isthmus-p5-OXuGowWB` |
| 本地私有目录 | `/Users/ruanyang/My-project/api/z/isthmus-p5-dependencies.iIzAp9mu` |
| MySQL 容器 | `isthmus-p5-20260919-1740-mysql` |
| Redis 容器 | `isthmus-p5-20260919-1740-redis` |
| Internal 网络 | `isthmus-p5-20260919-1740-net` |
| 独立卷 | `isthmus-p5-20260919-1740-mysql-data`、`isthmus-p5-20260919-1740-redis-data` |
| 所有权 label | `isthmus.test.run=isthmus-p5-20260919-1740` |
| MySQL 私网地址 | `172.19.0.2:3306` |
| Redis 私网地址 | `172.19.0.3:6379` |
| 本地 SSH 转发 | `127.0.0.1:33379` → MySQL；`127.0.0.1:63979` → Redis |

镜像来自官方 Docker Hub 并按 digest 拉取，无业务 tag 覆盖。重建时按此 digest 锁定，
否则不构成同一基线：

| 镜像 | amd64 manifest digest | 实际版本 |
| --- | --- | --- |
| MySQL | `sha256:8c19b656bb381f163750b238852bd377ba5764e1ec30cdd3f02e55cf8e2f89b7` | 8.4.11 |
| Redis | `sha256:de4d18872bf67ad0bd62224a712263975ca1d53b9b1d0d5d076082e7fa887890` | 8.10.1 |

Internal 网络无外网出口；Docker 29 在此网络不启用端口发布。不要为此改成业务网络、
host 网络或开放公网端口。两个容器 restart=no，重启宿主后不会自动启动。
当前私网 IP 是实测地址，不保证重建后仍相同；变更前检查容器 ID、标签和网络。

原 4 个 Portunex 容器及宿主现有 MySQL/Redis 不属于本轮资源，严禁操作。
两个数据库仅是 `isthmus_p5_runtime`（26 表）与 `isthmus_p5_ccmax`（3 个 outbox 辅助表）。
后者不满足完整 CCMAX/orchestrator 业务 schema preflight，不要当成完整业务库使用。

## 凭据和数据

本地与远端的 `fixtures/test.env` 含三个测试环境变量，0700 目录内文件权限 0600。
通过 `source` 私下使用，**不要 cat、打印、set -x、提交或贴到聊天**。
其余 root-password/admin.cnf/init.sql/redis.conf 也包含私密测试凭据，不能入 Git。
只有合成数据；运行账号仅有各自 schema 的 DML 权限，管理员只用于空库初始化。
不需要线上账号、生产凭据、SQL dump 或 Redis dump。

## 首选：远端同机复验

在当前电脑执行以下命令（无需安装远端 Go/Bun）：

```sh
ssh -F /Users/ruanyang/My-project/api/z/isthmus-p5-dependencies.iIzAp9mu/ssh_config \
  -S /Users/ruanyang/My-project/api/z/isthmus-p5-dependencies.iIzAp9mu/tunnel.sock \
  isthmus-p5-test \
  'bash /var/tmp/isthmus-p5-OXuGowWB/run-tests.sh'
```

远端运行顺序为 `TestMySQLRepositoryIntegration`、`TestMySQLSourceIntegration`、
`TestRedisBackendIntegration`。三个均在真实依赖通过，连续 3 轮复跑通过。
不要并发运行多个 outbox 用例：该测试从序号 0 消费，预期只有自己创建的事件。
脚本使用当前私网 IP，运行前可执行私有目录内 `environment.py inspect` 检查资源归属。
`check_business.py after` 仅检查原业务容器元数据，可用于确认未干扰业务。

私有目录保留本轮 operator 脚本和资源/检查 JSON；`environment.py create/initialize`
已经执行完，**不得重新运行**，不得在已有数据卷上重复应用非幂等 runtime 迁移。

二进制来自 `32de688` 的本地 Linux/amd64 交叉编译。修改相关代码后需要重新编译，
不能继续用这些旧二进制声称验证新代码。例（从 execution-plane 目录）：

```sh
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c \
  -o /绝对路径/新的私有制品目录/runtime-store.test ./internal/runtime/store
# outbox、lease 同理；核对上传目标仅是本轮专用目录，再更新测试二进制。
```

如果 Claude 自身仍阻止 SSH，请在其权限流程内批准这条精确目标命令或由用户运行；
不要改用其它工具来绕过其限制，也不要泛化允许所有 Bash/SSH。本轮已替其完成基线实测。

## 本地联调入口与限制

已建立的隧道属于本次 Codex 终端进程，不是永久后台服务；退出进程/断网后可能关闭。
检查：

```sh
ssh -F /Users/ruanyang/My-project/api/z/isthmus-p5-dependencies.iIzAp9mu/ssh_config \
  -S /Users/ruanyang/My-project/api/z/isthmus-p5-dependencies.iIzAp9mu/tunnel.sock \
  -O check isthmus-p5-test
```

确认当前容器地址且本地端口空闲后，在用户终端前台重新建立：

```sh
ssh -F /Users/ruanyang/My-project/api/z/isthmus-p5-dependencies.iIzAp9mu/ssh_config \
  -M -S /Users/ruanyang/My-project/api/z/isthmus-p5-dependencies.iIzAp9mu/tunnel.sock \
  -N -o ExitOnForwardFailure=yes \
  -L 127.0.0.1:33379:172.19.0.2:3306 \
  -L 127.0.0.1:63979:172.19.0.3:6379 isthmus-p5-test
```

另一个终端可私下 source 本地 `fixtures/test.env` 做联调。隧道能连接并完成 SQL，
但本轮本地运行两个 MySQL 用例均超过其内部 **15s context**，记录为 **FAIL**；
不要将其与约 0.05s/0.01s 的远端 PASS 混淆。Redis 用例 TTL 只有 **100ms**，
也应在远端运行。`go test -timeout` 不能改变用例内部的期限。

## 剩余工作和清理边界

下一步仍需将权威 Grant/Renew/Revoke、START 的真实 owner、认证双库校验、daemon
周期校验和连接回收接通。**控制会话仍在线不能证明某槽位的租约仍有效**；仅检查心跳
或 OfflineAfter 不足以替代 Redis+SQL 的每实例权威校验。

没有自动清理任务。容器/卷暂留供接力。停用时先比对 resources.json 内的精确 ID、
名字和所有权 label，再仅处理本轮两个容器、一个网络和两个卷。删除卷会丢失合成
测试数据；任何删卷动作应明确报告。严禁 prune、通配符删除、FLUSHDB 或操作原业务资源。

关闭本次隧道不会停止数据库：对上述专用 control socket 使用 `ssh -O exit`，
或结束用户终端中的前台 SSH。不要修改全局 SSH 或 Claude 权限配置。
