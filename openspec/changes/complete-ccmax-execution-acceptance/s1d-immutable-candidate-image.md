# S1d：不可变候选工具链镜像

2026-09-17。先完成 S1c review 并汇报，再开发本阶段。基线总体26%，I=6/15。
Review 发现 ZIP64 可在普通 EOCD 预算检查后覆盖目录参数；先拒绝该非必需格式，
补构造解析器之前拒绝的负例。该缺陷处于尚未验收的 I3，不增加或扣减已验收项。

## 边界与文件结构

```text
execution-plane/isthmus-runtime/image/
  runtimekit/                 候选镜像的正向清单、配方、离线上下文
  lab/build.py                复用有界 BuildKit，仅增加固定的候选构建路径
  lab/runtime.py              候选镜像隔离检查，非生产启动器
  test/test_runtime_*.py      上下文、换包、配方及越界负例
  test/test_lab_runtime.py    镜像运行参数、清理与证据合同
```

仅在已批准的170.106.159.197测试；43.153.75.220只作SSH中转。
不访问216.106.185.119，不改现有业务、账号、数据库、UI、宿主Bun或Docker配置。
不读取真实凭据，不触发模型请求，不推送镜像或Git。

## 固定输入与依赖取舍

- 重用 S1b 官方 Debian13 digest、同9个已校验 deb 包，保持离线安装及依赖闭包。
- 重用 S1c 固定 Git commit 的23个 fake-only应用文件；不复制整个仓库或测试目录。
- 重用已核验 Bun1.4.2候选、Bun1.3.9对照、Claude CLI2.1.258 amd64。
  两个Bun独立路径，不改项目/CI pin，不声称arm64运行通过。
- 旧 Dockerfile 的curl/证书/unzip安装链用于动态下载器，git用于交互调试，
  不因为旧安装脚本出现过就自动恢复。保留现有已审查基础包；缺依赖则明确失败，
  不联网补包、不带回setcap SYS_ADMIN或失败后无隔离降级。

## 实施与验证顺序

1. 修复ZIP64并复验、单独提交review修复。
2. 新建离线候选上下文：内嵌验证过的base上下文，额外只有26个应用/工具文件。
   清单以仓库锁为锚，不信任输入侧自签receipt。路径/链接/大小/摘要/权限有界检查；
   上下文放仓库外0700目录，不执行输入。默认命令仍为`/bin/false`。
3. 配方在固定基础包步骤之后仅COPY root所有的应用和工具至`/opt/isthmus`。
   不RUN Bun/CLI、不下载、不运行安装器、不开放端口、不生成共享机器标识/密钥。
   应用与工具对UID1000不可写，镜像配置明确fake-only与固定版本。
4. 在任务专属root-owned目录和有界BuildKit里实际构建；复用2CPU/2GiB无swap、
   pids512、无宿主bind/socket、单worker、900秒watchdog和cgroup观察。
   只让受信基础包维护脚本在特权builder执行，新工具只作为被COPY的字节。
5. 用实际image ID运行非特权探针：UID1000、只读根、network=none、cap-drop ALL、
   NNP、core=0、私有IPC/cgroup、无端口/bind/device，1CPU/2GiB无swap/pids128；
   home和/tmp为独立noexec临时空间。核26项哈希、不可写性、版本及fake loopback；
   测试源码不进入发布层。失败不降级，不忽略超时。
6. 清理本次精确标记的容器和缓存卷，比较原有业务元数据不变，保留候选镜像与
   私有原始证据。完成独立review、定向/全套回归，再阶段提交。

## 完成不等于上线

S1d交付的是amd64不可变候选，不是线上isthmus的完整运行实现。
I3仍须补完整运行依赖/双架构对应证据；I4持久home、I5空白宿主重建，
真实CLI转发、每实例机器标识/密钥/证书、出口策略和控制面桥接仍分项验收。
除非某个原子门槛完整通过，不提高26%的总体验收分数。
