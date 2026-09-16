# 沿用 Sub2/CCMAX 的 isthmus 执行侧交付

2026-09-17，用户明确沿用 Sub2 计费、价格、倍率和产品权限，CCMAX 作为既有业务模板。先冻结本计划再开发。百分比继续使用[原100分台账](../../../docs/plans/isthmus-container-delivery-v1.md)，当前23%；旧计划本来没有重做计费，因此不因澄清范围而提高分数。

## 一条业务链、一个计费权威

调用方 → Sub2既有认证/价格/倍率/结算 → CCMAX既有协议与调度 → control/host-agent → 隔离isthmus/CLI → 固定出口 → 上游。

执行侧只提供协议事件及准确usage（含缓存分类），不能计算用户价格、扣费、复制用户倍率或建立第二套产品登录。桥接仍必须处理分发、取消、usage归属、凭据刷新、租约撤销及重建/恢复；已有模板不能自动补齐这些接线。功能兼容与安全验收通过后才可称本地执行路径达到目标；静态源码和合成上游不证明真实账号/生产部署与线上完全一致。

## 阶段和验收映射

| 阶段 | 交付 | 对应原门槛 | 完成时必须汇报 |
| --- | --- | --- | --- |
| S1 镜像工程 | 基础镜像、制品锁定、受控构建、home、冷启动 | I2–I5，N3前置 | 来源/版本/摘要、真实构建架构、缺失运行依赖 |
| S2 隔离 | 专用Linux、固定出口、拒绝矩阵及失效回收 | N3–N5 | 跨槽/宿主/DNS/IPv6/metadata正反例 |
| S3 身份 | 每实例标识/私钥/CSR/证书、mTLS和轮换 | K1–K5 | 唯一性、恢复/换代/旧代与跨槽拒绝 |
| S4 运行服务 | 真CLI、session/pool/MCP、HTTP/WS/gRPC及停机 | R3–R5 | 实际固定CLI→合成上游、兼容差异、进程回收 |
| S5 桥接 | control/host-agent/CCMAX接线、准确usage、凭据生命周期 | H3–H5、C1–C5、L1–L5 | 原业务路径保留、无重复计费、无明文回退 |
| S6 整体验收 | 当前组件整链、真实本地依赖、容量/稳定性、恢复 | E1–E5 | 对应运行证据，不以多份局部PASS拼接 |

阶段内允许小切片提交，每次明确“子切片完成”还是“整个阶段完成”；没有完整关闭原门槛不得加分。阶段汇报包含总体/镜像百分比、已完成/未完成、review发现与处理、实际测试、Git提交和下一切片。不创建后台自动任务，不承诺在没有运行证据时已完成。

## S1a 本轮实现：基础镜像配方与离线构建上下文

线上 `isthmus-vm-base` 本来不包含完整app/Bun/CLI。先交付**base-only**工程，不把Go-only worker或fake服务改名冒充原系统。本切片不会运行Docker/VM、下载/执行软件、操作线上，也不会替换现有Bun。

```text
execution-plane/isthmus-runtime/image/
  Dockerfile.base                 # 审查的base-only模板，严格替换固定digest/platform
  stage.py                        # 明确本地输入的命令入口，摘要/错误脱敏
  imagekit/lock.py                # 完整锁合同；无版本/digest猜测
  imagekit/context.py             # 排他、私有、白名单上下文及复核
  test/                          # 合成.deb字节、权限/篡改/错误负例
  README.md                      # 当前能力、来源信任和真实构建停止线
```

- 锁输入必须明确：schema、目标Linux架构、`docker.io/library/debian@sha256:…`、每个.deb的文件名/包名/版本/架构/大小/SHA-256/允许的官方来源URL；拒绝多余字段、重复包/路径、畸形值、浮动tag、跨架构、缺核心包。最多128包，单包64MiB，总量256MiB；元数据256KiB。
- 本轮最小核心包为 `ca-certificates`、`passwd`、`procps`、`util-linux`。锁必须同时列齐基底外的依赖；工具不自动解析APT依赖或获取任何字节。真实固定发布锁仍需官方索引来源和实际构建证据，不能提交虚构的版本/hash冒称已锁定。
- 仅复制显式文件到新建仓库外0700目录，普通文件0600；不遍历复制仓库、账号home或共享卷。不接受symlink/FIFO/硬链接，前后核文件身份/大小/hash。复用recoverykit安全文件helper，保持现有文本证据政策不变。合成包只验证字节流/清单逻辑，不是可安装Debian包。
- 模板不使用动态installer、APT网络、`COPY .`、secret/SSH mount、SYS_ADMIN/setcap或自动volume。所有RUN固定 `--network=none`，检查包哈希后离线dpkg安装，依赖未闭合即失败；创建UID1000的私有空home。默认 `/bin/false`，不默认启动shell或fake listener；无业务服务启动声明。
- 生成的Dockerfile、包校验文件和构建上下文都可单独复核；receipt最后写、成功仍声明 `image_built=false`、`execution_permitted=false`。缺文件或篡改不得产生成功receipt。工具不提供自动build/run/cleanup接口。
- 官方Docker文档说明RUN网络范围及构建context；`--network=none`不能替代专用宿主准入，也不能保证FROM阶段不拉取。实际builder必须在后续N3门槛中固定并检查，当前lab预检结果不能直接授权构建。

### S1a 验证

正常锁与实际临时文件输出/复核；错误schema/版本/来源/架构/大小/hash、缺失核心包、重复/不安全路径、symlink/FIFO/硬链接、读中变更、目的地覆盖/嵌套Git、上下文篡改/额外文件及receipt失败。CLI不回显路径、URL、包元数据或底层异常。静态模板测试不是Docker构建测试。独立review、全Python回归、定向多轮和内容扫描后单独提交。

S1a本身**不关闭I2/I3/I5，不增加23%/20%**。下一切片收集并核官方固定base及包依赖、专用Linux实际构建，再补Bun/CLI/app制品；不让一个只能生成上下文的工具取代镜像交付。

参考：[Dockerfile](https://docs.docker.com/reference/dockerfile/)、[build context](https://docs.docker.com/build/concepts/context/)。
