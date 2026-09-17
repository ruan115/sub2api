# S2：双实例隔离与身份，先完成可运行的身份切片

2026-09-17；承接真实CLI单实例R3（`1446fd6`），当前30%。沿用已有基础镜像、
固定CLI/Bun、受限测试容器和CA库，不新增构建平台、不改变Sub2业务或默认入口。

## 顺序与本轮S2a

1. **S2a本地身份**：两个实际容器各自在0700 home内随机生成自己的P256私钥和
   逻辑机器标识；只输出公钥摘要/CSR。相同绑定重新启动身份工具保持身份；错账号、
   slot、node、epoch或generation、损坏/权限错误/链接文件拒绝复用，不自动覆盖。
   复用`pki.Authority`添加只接收准确绑定CSR的serverAuth签发库，不把CA私钥送入实例。
2. **S2b可信安装与mTLS**：受认证签发、证书原子安装、准确双方身份、运行服务实际
   TLS连接，跨实例/错误CA/用途/执行代拒绝。证书库测试不替代这一阶段。
3. **S2c出口**：在明确的专用资源中实现仅授权出口、跨实例/宿主/metadata/DNS/IPv6
   拒绝及规则撤销失败关闭。先写规则资源/回滚合同；不改共存业务的宿主防火墙。

S2a模块分开：`internal/runtimeidentity/`为绑定、私有存储、CSR；`internal/pki/runtime*`
复用现有签发；`cmd/instance-identity/`是无listener的初始化/公开申请命令；
`isthmus-runtime/image/lab/identity.py`仅实际双实例验收，复用已有Lab/上传门禁。

## 身份合同

- 绑定沿用`provider.RuntimeAccountID`的账号hash（SHA256前16字节，32位小写hex），
  以及slot/node受限标识、正整数epoch/generation；
  不接受原始账号/密码/token。URI为`spiffe://sub2api.execution/runtime/<node>/<slot>/<hash>/<epoch>/<generation>`。
- 私钥由实例本地安全随机数生成；不接受导入密钥参数。单个0600私有状态文件包含
  绑定、逻辑机器标识与PKCS8密钥，受0700目录保护；原子发布、并发初始化互斥。
  读取拒绝软/硬链接、特殊文件、权限/所有者错误和超限，错误不回显内容。
- CSR必须签名有效、准确唯一URI、空Subject、无额外DNS/IP/email/未知扩展，P256。
  签发调用方必须先完成授权；此方法不是一个可公开调用的注册API。
- 相同绑定的进程重启保留；任何绑定变化明确失败，待生命周期控制面决定新目录与换代。
  本轮tmpfs容器销毁后身份销毁，不冒称持久卷重建/restore已完成；同一旧绑定的回滚
  检测仍需外部权威/fencing，不用文件中的自报generation替代。
- 这里是应用逻辑机器标识，不修改共享镜像`/etc/machine-id`，不冒称已复刻CLI内部
  device-id或原线上机器标识；原CLI来源仍未知。

## 本轮验收与停止线

先提交计划，再纯模块/race测试、独立review；在170测试机同时创建两个networknone、
UID1000、只读根、cap0/NNP、独立tmpfs home、无hostbind/端口的容器。43只SSH中转。
私钥只能存在各自临时home；上传只含审查源码/二进制，不上传宿主home/配置/真实凭据。
检查两实例公钥/标识不同，同绑定再运行相同、错绑定拒绝且原件不变、对方文件不可见。
沿用上轮真实CLI回环五项回归确认新身份初始化不破坏单实例执行。
仅保留公开摘要/CSR和固定测试结果，不导出private状态；清理本轮精确容器并比对业务基线。

本轮不连接216生产，不用真实账号/模型，不替换宿主Bun、不push/部署，不启用网关。
只有完整通过并review后可考虑K2（2分）；K1、K3–K5、I4/I5、N3–N5及线上完整一致仍开放。
证书签发库即使通过也不给K3/K4加分。后续不以更多工具/单测数量替代真实隔离验证。
