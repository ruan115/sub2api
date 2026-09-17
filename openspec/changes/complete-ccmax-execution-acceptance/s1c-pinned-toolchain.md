# S1c：固定工具链与原生 Linux 隔离验证

2026-09-17，先规划后开发。继 S1b 实构建后，总体基线26%，I=6/15。
只使用170.106.159.197；43.153.75.220只作SSH中转，不访问生产216.106.185.119。
不安装宿主软件、不替换Bun、不读取账号配置、不触发真实模型或变更现有服务。

## 模块与范围

```text
execution-plane/isthmus-runtime/image/
  artifacts/binaries.py       官方二进制锁、有限解包、架构与摘要校验
  artifacts/source.py         固定Git commit的逐文件fake应用源码制品
  locks/                     工具链和源码公开锁；不存放二进制、配置或密钥
  lab/toolchain.py            指定测试宿主的非特权工具兼容验证
  test/test_artifact_*.py     不联网的锁、换包、归档与源码负例
  test/test_lab_toolchain.py  容器安全参数、证据、失败清理合同
```

锁Bun1.4.2作为候选、1.3.9作为同架构对照；项目/CI的1.3.9不变。
CLI固定已留存线上版本2.1.258，从官方签名manifest重新验证，不执行安装器。
amd64与arm64分别锁摘要/尺寸/来源；只有amd64宿主，arm64不声明原生通过。
当前app仅23项源码/协议/package正向清单，标明fake-only，无真实CLI桥接。
不复制整个src目录、仓库、旧app/home卷，也不把gRPC proto当实现。

## 顺序与门槛

1. 官方元数据核对、实现有界二进制和源码制品模块；冻结source commit与逐文件摘要。
2. 官方Bun ZIP核外层摘要、仅解出指定单一ELF；Claude manifest验签并核官方指纹，
   原始ELF核大小/摘要。公共下载在任务专属目录，不接触用户GPG/keychain配置。
3. 复核宿主身份、资源和原有容器元数据。基于S1b精确image ID创建私有容器，
   先仅运行受240秒watchdog约束的sleep，再传入只含40个固定普通文件的tar到
   `/opt/isthmus-probe`专属512MiB可执行tmpfs（root所有、UID1000不可写）。
   Docker29实测cp连运行时tmpfs也会被只读rootfs检查拒绝，故不沿用cp方案。
   只允许容器内固定系统tar以UID0（仍cap-drop=ALL）解开白名单制品到专属tmpfs；
   不解包外部tar、不执行新工具。Bun/CLI和测试仍仅用UID1000，执行前核不可写性。
   根仍只读，home和/tmp仍noexec；不挂宿主目录或socket。
   这只是兼容性探针，**不是新的不可变发布镜像**；不docker commit、不复用floating tag。
4. 探针运行UID/GID1000、只读根、network=none、cap-drop=ALL、NNP、无端口、
   私有PID/IPC/cgroup、独立临时home/config、core=0、1 CPU/2GiB无swap、pids128。
   测试命令额外120秒watchdog；超时只停止核对所有权后的精确探针ID；日志有界；
   不在特权BuildKit执行Bun/CLI。运行时再次核cgroup限制、制品哈希和不可写权限。
5. 原生CLI只测--version；Bun候选测试fake合同、loopback正常关闭/异常关闭，
   1.3.9对照异常关闭。无账号、token、模型调用；失败不得更换未审查制品/联网补包。
6. 保存公开摘要与私有原始证据，清理仅本次探针；比较原有容器未改动。
   独立review、回归并阶段提交。下载成功不等于运行链路验收。

## 尚不关闭的门槛

I3须完整制品与依赖工具合同，不能仅因--version通过就计满；本阶段可先交付工具
与fake源码制品证据。后续不可变组合镜像用固定Debian digest+同9包配方重建，或
显式验证的OCI输入；独立BuildKit不能假设访问宿主Docker image store。
I4双实例持久home、I5空白专用宿主冷启动、R3真实CLI转发、K独立机器标识/密钥/
证书、出口隔离与控制面桥接仍单独验收。计分只按完整通过的3分原子项变化。

来源：[Bun1.4.2](https://github.com/oven-sh/bun/releases/tag/bun-v1.4.2)、
[Claude签名验证](https://code.claude.com/docs/en/setup#verify-the-manifest-signature)。
