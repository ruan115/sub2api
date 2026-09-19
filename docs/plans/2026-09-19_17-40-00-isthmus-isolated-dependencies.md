# 2026-09-19：解除真实数据库测试环境阻塞

基线 `32de688`，分支 `codex/claude-execution-plane-v1`。本轮仅补足阶段 3 的
隔离依赖与可重复运行入口；不实施阶段 2b，不把单独数据库测试当作双库授权闭环。

## 边界与预检

- 只在已授权的 `170.106.159.197` 新增测试资源；`43.153.75.220` 仅 SSH 跳板，
  跳板连接绑定 `en0`。不连接生产 `216.106.185.119`，不修改用户 SSH/Claude 权限配置。
- Docker daemon 必须为 `18f7810b-1ae6-4a3a-8825-a9d38136afd9`。保留原 4 个
  Portunex 业务容器，前后比较 ID、镜像、启动时间、重启次数和挂载元数据；不读 Env/Cmd/日志。
- 预检为 4 CPU、约 4.3 GiB available、34 GiB 可用磁盘，现有服务端口不复用。
- 测试镜像采用官方 MySQL 8.4 与 Redis 8，拉取后锁定 digest；无生产数据/凭据，
  不安装宿主软件，不修改既有镜像 tag，不开放公网端口，不调整宿主防火墙。
- 本地私有制品目录在仓库外；凭据仅写 0700 目录/0600 文件，不显示、不入 Git。

## 模块与执行步骤

1. `execution-plane/test/dependencies/fixture/`：生成随机测试秘密、只含 runtime
   migrations 与白名单 outbox 表的初始化 SQL、受限 DML 账号及测试环境变量。
   CCMAX 部分不是完整业务库，不启动 CCMAX 应用来执行业务迁移。
2. 本轮专用 Docker 网络、MySQL 数据卷和 Redis 数据卷，唯一名称及所有权 label。
   两服务分别最多 1 CPU/1 GiB 和 0.25 CPU/128 MiB，MemorySwap=Memory、restart=no；
   计划分别仅发布回环端口；实测 Docker 29 的 Internal 网络不启用端口发布，因此最终
   保留 Internal 网络，由 SSH 直达容器私网 IP，本地监听 `127.0.0.1:33379`、
   `127.0.0.1:63979`。远端宿主无新增数据库监听，不挂业务路径或 Docker socket。
3. 使用管理员仅初始化全新空库；测试使用两个各限自身 schema 的 DML 账号。
   Redis 使用独立认证，无 FLUSHDB。数据库容器不连接生产网络或模型上游。
4. 本地交叉编译 3 个 Linux/amd64 测试二进制，上传明确清单，在远端回环运行，
   避免 Redis 100ms TTL 用例受跨境 SSH RTT 干扰；最终在同一宿主通过专用
   Internal bridge 访问两个容器，不替换远端 Go/Bun。
5. 记录三个入口的 PASS/FAIL/SKIP、数据库版本/digest、资源约束与业务基线对照；
   保留测试依赖供 Claude 后续使用，提供限定目标的运行说明，不创建自动部署流程。

## 验收与交付

- `TestMySQLRepositoryIntegration`、`TestMySQLSourceIntegration`、
  `TestRedisBackendIntegration` 必须在独立真实依赖运行，不接受 SKIP 充作通过。
- fixture 单测、独立 review、相关 Go 检查、明确文件本地提交，不 push。
- `route/reconcile.go`、`image/lab/build.py`、`image/runtimekit/` 的既有 WIP 不变、不 stage。
- 本轮不证明 daemon 周期校验、双库 Coordinator 故障闭环或完整 VM/CLI 验收。
  特别是控制会话存活不能代替每槽位双库有效租约，阶段 2b 仍需单独实现。

## 执行记录

### 已完成

- SSH 经明确的 43.153.75.220 跳板和 `BindInterface=en0` 成功；未修改用户 SSH
  或 Claude 配置。默认 SSH alias 所指的另一跳板及直连失败，未在那里部署资源。
- 远端私有运行目录：`/var/tmp/isthmus-p5-OXuGowWB`。
- 本地私有制品：`/Users/ruanyang/My-project/api/z/isthmus-p5-dependencies.iIzAp9mu`。
  两处 fixture 目录 0700、文件 0600，凭据未入 Git。
- 唯一资源前缀 `isthmus-p5-20260919-1740`，两个容器、一个 Internal 网络、两个新卷，
  所有权 label 为 `isthmus.test.run=isthmus-p5-20260919-1740`；容器显式
  `traefik.enable=false`，避免现有反向代理自动发现。保留供后续验证，没有自动重启。
- MySQL **8.4.11**，runtime **26 表**，CCMAX outbox 辅助库 **3 表**；两个账号仅有
  各自 schema 的 SELECT/INSERT/UPDATE/DELETE，无 GRANT 权限。不是完整 CCMAX 库。
- Redis **8.10.1** 镜像，独立认证、无持久化、64 MiB maxmemory/noeviction。
- 内核回读：MySQL memory.max=1073741824、memory.swap.max=0、cpu.max=100000/100000、
  pids.max=256；Redis 对应 134217728、0、25000/100000、64；非特权、只读根、NNP。
  Redis 路由仅有 `172.19.0.0/16` 直连，无默认路由；并非完整工作负载网络攻击面验收。
- 原 4 业务容器的 ID、image、StartedAt、RestartCount、mount 元数据与历史基线完全一致。
  没读业务 Env/Cmd/日志；未连接 216、未启动模型调用、未改 Sub2/CCMAX 业务数据。

### 实际测试结果

| 测试 | 远端同宿主真实依赖 | 本地经 SSH 隧道 |
| --- | --- | --- |
| TestMySQLRepositoryIntegration | PASS，约 0.04–0.06s | FAIL：用例内 15s context 到期 |
| TestMySQLSourceIntegration | PASS，约 0.01–0.02s | FAIL：用例内 15s context 到期 |
| TestRedisBackendIntegration | PASS，约 0.12s | 未跑，100ms TTL 不适合跨境链路 |

三个远端入口首次通过后连续复跑 3 轮通过；本地失败后再次串行复跑全部通过。
本地失败未隐藏或修改用例期限来冒充通过。初始化后的首轮曾因 Internal 网络没有
实际发布回环端口而 connection refused；改为容器私网地址后通过，不是 SQL 缺陷。
检查 outbox、consumer、node、slot、execution lease 均为 0 行，合成测试清理完成。

本地全 execution-plane `go test -race -count=1 -timeout=180s ./...`、`go vet ./...`、
Linux/amd64 `go build ./...` 全通过（此全量运行未设置真实依赖变量，真实验证以上表单列）。
fixture 8 个测试函数与独立 review 通过。Reviewer 的 MySQL secret UID 可读性疑问，
已由固定官方镜像的实际初始化成功消除，未 chmod 放宽私密文件。

### 制品固定与接力

- MySQL amd64 manifest：`sha256:8c19b656bb381f163750b238852bd377ba5764e1ec30cdd3f02e55cf8e2f89b7`。
- Redis amd64 manifest：`sha256:de4d18872bf67ad0bd62224a712263975ca1d53b9b1d0d5d076082e7fa887890`。
- 镜像来自官方 Docker Hub，按 digest 拉取，无业务 tag 覆盖。参考
  [MySQL 官方镜像](https://hub.docker.com/_/mysql)、[Redis 官方镜像](https://hub.docker.com/_/redis)。
- 接力说明：[真实依赖运行入口](../../execution-plane/test/dependencies/README.md)。
- 本轮只解除数据库资源与实测基线阻塞。阶段 2b、真实 Coordinator 双库故障到 daemon
  托管连接回收的全链路仍未验收，不增加总体完成率、不改变 readiness。
