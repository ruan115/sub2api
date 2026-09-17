# 沿用 Sub2/CCMAX 的 isthmus 执行侧交付

2026-09-17，用户明确沿用 Sub2 计费、价格、倍率和产品权限，CCMAX 作为既有业务模板。先冻结本计划再开发。百分比继续使用[原100分台账](../../../docs/plans/isthmus-container-delivery-v1.md)，冻结时23%，S1b验收I2后为26%；不因澄清范围而提高分数。

## 一条业务链、一个计费权威

调用方 → Sub2既有认证/价格/倍率/结算 → CCMAX既有协议与调度 → control/host-agent → 隔离isthmus/CLI → 固定出口 → 上游。

执行侧只提供协议事件及准确usage（含缓存分类），不能计算用户价格、扣费、复制用户倍率或建立第二套产品登录。现有CCMAX也有quota/balance扣减路径；S5必须区分Sub2终端用户账和CCMAX既有服务账户/成本账，不擅自删除现有逻辑，也不对同一用户请求重复结算。桥接仍必须处理分发、取消、usage归属、凭据刷新、租约撤销及重建/恢复；已有模板不能自动补齐这些接线。功能兼容与安全验收通过后才可称本地执行路径达到目标；静态源码和合成上游不证明真实账号/生产部署与线上完全一致。

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

阶段总进度包含此前验收的基础：S1=I为40%（I1/I2），S2=N为40%，S3=K为0%，S4=R为40%，S5=H+C+L为4/30=13.3%，S6=E为0%；合计26/100，不是把六个百分比做简单平均。S1a以下为历史切片记录；当前S1b见末尾。

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
- 生成的Dockerfile、包校验文件和构建上下文都可单独复核；receipt最后写、成功仍声明 `image_built=false`、`execution_permitted=false`。预检/复制失败不发布receipt；最后写入或复核失败可能保留receipt文件但不得返回成功，文件存在不能替代独立verify。工具不提供自动build/run/cleanup接口。
- 官方Docker文档说明RUN网络范围及构建context；`--network=none`不能替代专用宿主准入，也不能保证FROM阶段不拉取。实际builder必须在后续N3门槛中固定并检查，当前lab预检结果不能直接授权构建。

### S1a 验证

正常锁与实际临时文件输出/复核；错误schema/版本/来源/架构/大小/hash、缺失核心包、重复/不安全路径、symlink/FIFO/硬链接、读中变更、目的地覆盖/嵌套Git、上下文篡改/额外文件及receipt失败。CLI不回显路径、URL、包元数据或底层异常。静态模板测试不是Docker构建测试。独立review、全Python回归、定向多轮和内容扫描后单独提交。

S1a本身**不关闭I2/I3/I5，不增加23%/20%**。下一切片收集并核官方固定base及包依赖、专用Linux实际构建，再补Bun/CLI/app制品；不让一个只能生成上下文的工具取代镜像交付。

参考：[Dockerfile](https://docs.docker.com/reference/dockerfile/)、[build context](https://docs.docker.com/build/concepts/context/)。

## 本轮桥接只读审计：复用业务，不省略接线

- Sub2的模型价格解析和用户/分组倍率已有实现：[价格解析](../../../backend/internal/service/model_pricing_resolver.go)、[usage与倍率](../../../backend/internal/service/gateway_usage_billing.go)；已有API-key账号的base_url可以接Anthropic兼容网关，不新增另一套Sub2计价入口。
- CCMAX的`chooseExecutionDispatch`只有定义/测试，gateway候选SQL目前仍筛legacy；migrated是被排除，不是已经接到新执行面。S5需增加无明文的候选/预留及运行元数据投影，在旧token刷新/认证头构造前分流，保留失败关闭。不能只添加一条HTTP转发。
- execution_client已有Execute/CountTokens/Cancel，但未由gateway消费；worker只实现OAUTH_API，Models数据面仍Unimplemented。实际CLI、三种transport、模型列表与工具续接仍需R/C门槛。
- CCMAX的`recordUsage`目前同时更新quota/balance，见[现有账务实现](../../../ccmax-manager/main.go)。S5明确终端用户账/服务账户账归属，防止response片段与Completed重复入账；输入/输出、缓存读写、5m/1h分类与部分失败计量须经原路径处理。
- 旧`ensureGatewayAccountToken`读写CredentialsJSON，只保留服务legacy；migrated仍需Vault原子换版和独立刷新/撤销。HTTP断连还需传到gRPC/CLI并释放原并发与调度资源。

本次只读审计未改Sub2/CCMAX业务源码、价格、余额或网关配置；源码引用不是整链运行证据。

## S1b 子阶段结果与下一项

S1c后续已交付[固定制品与原生Linux工具验证](verification.md#s1c固定制品与原生linux工具验证)：
官方CLI2.1.258验签/版本启动、两版Bun110项runtime测试和候选正常/异常关闭各5轮、
23文件fake源码/14文件测试分离、40项payload哈希与无网络/不可写权限验证。
未产出不可变组合运行镜像，真实CLI所需工具闭包还未验全；I3继续开放，总体26%、镜像40%。
本段补充下方S1b记录，不把fake源码改称完整isthmus。

已按[Linux实构建计划](s1b-linux-base-build.md)在170测试机完成基础层；43.153.75.220仅SSH中转，未访问216生产。官方Debian基底与BuildKit固定digest，9个真实包共7,993,428字节；实际APT签名链、metadata/control/hash核对、离线dpkg构建、cgroup限额归属、非特权无网络冒烟和准确清理均有[运行证据](verification.md#s1b原生linux基础镜像构建)。新context/空BuildKit缓存复跑使用最终代码完整通过，未用手工补状态跳过门槛。

I2新增3分：总体26%、镜像40%。共享测试宿主上的特权可信builder不等于工作负载隔离；N3/I5继续开放。下一子阶段S1c/I3固定并验证Linux Bun、真实CLI、当前app及必需工具，明确amd64/arm64边界；不替换宿主Bun、不接真实账号，不用Go-only/fake冒充完整isthmus服务。独立home持久化(I4)、运行冷启动(I5)、出口拒绝矩阵(S2)和独立密钥证书(S3)仍待完成。
