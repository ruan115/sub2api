# S1b：受限 Linux 基础镜像实构建

2026-09-17，先规划后执行。用户确认测试机闲置且可用。仅在
`170.106.159.197`（ubuntu）执行，`43.153.75.220` 只转发 SSH；不访问或修改
生产 `216.106.185.119`。不替换原 `isthmus-vm-base` tag，不安装宿主软件、
升级/restart Docker、修改业务、DB、UI、端口或宿主防火墙。

## 宿主准入与权限边界

实测 amd64 / Linux 5.15、4 CPU、7.44 GiB RAM、可用约4.3 GiB、磁盘余42 GiB；
Docker 29.8.0 / API1.56 / Buildx0.37.0 / cgroupv2 / overlayfs。
daemon ID `18f7810b-1ae6-4a3a-8825-a9d38136afd9`，名字 `VM-0-12-ubuntu`。
已有4个 Portunex 容器及 CCMAX/MySQL/Redis/nginx 等服务，全部保留。
本轮是**共存测试宿主上的可信基础包构建**，不是空白专用宿主，不能绕过现有
`lab inspect` 的空白条件或关闭 N3/I5。

原计划采用 Docker-container Buildx；review 发现 v0.37 隐式添加 network.host
daemon entitlement，改为直接创建固定官方 BuildKit 容器并通过其内置 buildctl 构建。
Rootful BuildKit 同样需要特权。因此只允许固定官方 BuildKit 镜像、审查的 Dockerfile 和官方 Debian
包；这是可信构建工具的权限例外，**不是工作负载安全沙箱**。不挂宿主目录或
Docker socket，仅用本次新建的专属 BuildKit 缓存卷；不传 secret、SSH、build-arg、
host network、额外 entitlement 或环境代理。不用于不可信代码、CLI、账号请求。
后续运行容器必须非特权，不沿用此例外。

## 文件布局

```text
execution-plane/isthmus-runtime/image/
  lab/                         显式测试宿主流程与资源/制品证据（不是自动部署器）
  locks/                       实际验证的公开基础制品锁（不含凭据或二进制）
  test/                        对应离线负例与合同测试
  imagekit/                    复用 S1a 的锁/配方/私有上下文
openspec/changes/complete-ccmax-execution-acceptance/
  s1b-linux-base-build.md       本执行计划
  verification.md              实测结果、限制和剩余门槛
```

按实际需要建文件，不预建空模块。脚本只能接显式参数，不复制整个仓库或任意 home。
输出写入新建0700任务目录；命名/镜像tag/容器/builder/卷每轮唯一，不复用业务名称。

## 执行顺序

1. 记录已有容器 ID、镜像 ID、StartedAt/RestartCount、挂载元数据和运行服务基线，
   不读取业务 Env/命令参数/日志。再次确认 daemon 身份及资源余量。
2. 通过官方 HTTPS registry 获取并核对 Debian13-slim 和 BuildKit 的 amd64 manifest
   digest，记录实际 digest；不把 tag 或 image config ID 当 digest。获取失败即停止，
   不用第三方镜像站、隐式代理、旧缓存或 floating tag 冒充固定制品。
3. 独立有界联网准备：拉取固定官方镜像；新建非特权下载容器，最多1 CPU/512 MiB、
   pids128、无业务挂载/端口/凭据。APT 必须校验官方签名索引，不启用 trusted=yes /
   allow-unauthenticated。下载四核心包及相对固定基底的依赖闭包；不执行旧部署脚本。
   实测APT需要CHOWN/SETUID/SETGID/FOWNER/DAC_OVERRIDE管理其私有缓存和_apt降权；
   仅下载容器添加这五项，不添加SYS_ADMIN，不沿用到最终运行实例。
   记录 InRelease/索引哈希、APT校验结果、包control元数据、大小/SHA256和官方来源。
   HTTP签名APT和HTTPS来源记录若不同须说明，不伪称整个准备过程离线。
4. 用 S1a stage/verify 实际包生成私有上下文，上传/复制后再次 verify；仅发送白名单
   Dockerfile、包、锁、receipt。构建上下文不含源码仓库、配置、代理或密钥。
5. 唯一直接 BuildKit builder，显式 Unix daemon endpoint、空0700 Docker配置目录；
   固定 builder digest、memory=2GiB/memory-swap=2GiB、cpu-quota=200000/
   cpu-period=100000、pids512、restart-policy=no、max-parallelism=1。启动后核对真实 HostConfig 和 cgroupv2
   memory.max / memory.swap.max / cpu.max，未生效即停止。单次 build、15分钟墙钟，
   工作集预算8GiB/宿主剩余至少20GiB（监控阈值，不冒称文件系统硬配额）。
   超时停止准确所有权ID的本次builder，不仅结束SSH客户端；限额不包括系统Docker
   daemon拉取/导入开销，下载串行。构建RUN子进程须确认处于受限cgroup层级。
   不更改现有默认builder。所有 Dockerfile RUN `--network=none`；FROM/registry
   拉取仍可能联网，与离线包安装分开记证据。失败不运行APT联网修复。先导出本次
   唯一OCI/Docker制品再加载；不直接更新已有tag，不push。
6. 冒烟：仅本次 image ID，新非特权容器，network=none、read-only、cap-drop=ALL、
   no-new-privileges、无端口/宿主挂载、tmpfs、pids64、0.5CPU/128MiB。实际核对
   架构/包版本/UID GID/home权限/空machine-id/无实例密钥，默认false按预期退出。
   验证损坏上下文、代理/秘密不注入和失败构建不发布成功制品。失败不回退旧镜像。
7. 只停止/移除逐一核对本次所有权的容器/builder及对应缓存卷；不 prune、不清业务卷、
   不删除既有镜像。保留新镜像和私有证据供恢复。前后比较既有服务身份/运行状态，
   汇报新增/移除资源。独立review、回归后阶段提交。

## 计分与停止线

开始时总体23%，镜像20%。只有 I2 的真实构建入口、失败与无隐式代理/秘密注入
测试完整通过且review关闭，才可增加3分；仅包下载或构建成功都不单独加分。
I3还缺 app/Bun/CLI及架构边界，I4还缺双实例home持久化，I5缺空白专用环境完整
冷启动。本轮不启动模型请求，不生成/借用真实凭据，不打开任何 execution 开关。

官方参考：[Buildx Docker-container driver](https://docs.docker.com/build/builders/drivers/docker-container/)、
[BuildKit](https://github.com/moby/buildkit)、
[Debian包签名链](https://www.debian.org/doc/manuals/securing-debian-manual/deb-pack-sign.en.html)。
