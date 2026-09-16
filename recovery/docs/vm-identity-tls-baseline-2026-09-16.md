# VM 身份与证书：线上静态证据和本地差距

用户明确要求每个 VM 独立的机器标识、密钥和证书；不是 TLS ClientHello/浏览器指纹伪装。本页只分析已保全脚本，没有 SSH、读取证书/私钥/账号文件或执行原脚本。

## 可复核来源

私有静态来源目录为仓库外 `isthmus-static-analysis.HjfIFn/scripts-source`。本次读取前对5个所选静态文本运行 recoverykit 内容检查，通过后仅检查相关源码。原件不复制到 Git。

| 文件 | 本次 SHA-256 | 来源限制 |
| --- | --- | --- |
| bin/deploy-vm.sh | f303608b82cd87bba610c2a3d640218b299a0e2f0b37c2d4d010ffbcb6d7bac8 | 与已提交静态 manifest 相同 |
| bin/lib/backend/docker.sh | 4cd3d23a0d8c5c5358d9ec5f8312240bfa0debaf9d657926aa326f3e58a2df83 | 与已提交静态 manifest 相同 |
| bin/lib/common.sh | 3dea8593c5ce389ad1cc9b02098b57cb82dcf45877f3f24f4c811b84af78f845 | 本次本地静态指纹；不在最初9文件 manifest 内，不声称此刻线上仍一致 |

## 已确认与不能推断的内容

| 要素 | 固定源码锚点与事实 | 不能据此声称 |
| --- | --- | --- |
| 实例隔离 | docker.sh:491 的 be_instance_create 为每个实例挂载独立 home volume；基础 app/Bun/工具卷共享 | 独立内核 VM、完整宿主防火墙、所有共享卷只读或每实例 OS machine-id 已核验 |
| 入站端口 | docker.sh:496–504 的 grpcs 分支只发布专用 TLS 端口；其他明文 listener 不作为此模式的宿主映射 | 实际全部77个实例当前端口与源码配置一致、应用内 listener 无误或内层新 worker 已有 mTLS |
| 证书输入 | deploy-vm.sh:184–192 接收证书目录；1066–1086 允许从发布包 grpcs-certs 目录补齐 | 每实例自动新建密钥。相同源目录可能被用于多个实例；未读取公钥摘要进行实例间比较，不断言线上实际共享 |
| 独立存放 | common.sh:671–705 定义每实例 home 下 `.isthmus-grpcs`；772–797 创建0700目录，复制公有证书与0600 server.key | 独立路径等于独立密钥；静态源码等于实际权限/无symlink状态已验 |
| 客户端私钥边界 | 脚本不把 Portunex client 私钥放入执行实例 | 任何服务证书/密钥都可以互换，或仅服务端TLS即可替代客户端认证 |
| 重建语义 | common.sh:822–825 注明 transport/cert bundle 随独立 home 保留，可用于 recreate/restore/migrate | 当前本地每次新 tmpfs 与该持久化语义等价；证书重用一定安全或旧执行代自动失效 |
| OS/CLI 机器标识 | 所选部署脚本中未定位明确的每实例 machine-id/device-id 生成合同 | 原 CLI 或未取得的生成工具中不存在相关逻辑。发布注释中的 gen-grpcs-certs 工具本次没有源码证据 |

## 按用户要求采用的合同，而非未证实的“完全一致”

1. **身份来源唯一**：保留当前 account-hash、slot、node、epoch/generation 权威；不把明文账号写入 hostname/env/证书 Subject，不用随机 TLS 指纹替代权限。
2. **分清生命周期**：稳定的逻辑实例/CLI标识与每次进程私钥、每执行代证书分别管理。重启、升级、恢复、换账号、重建的保留/换代规则必须显式决定，旧代不能借恢复旧 home 复活。若 CLI 的线上机器标识来源仍未知，保留待验证，不造一个 UUID 冒称兼容。
3. **独立密钥**：每执行实例本地安全随机生成私钥，控制面只接收公钥/CSR并核准确归属。允许共用受信 CA 公钥，不共用 server 私钥；不从线上拷贝真实密钥，不把私钥打进镜像/Env/argv/日志或 Git。
4. **双向准确认证**：worker server 证书要绑定准确 runtime 身份，host-agent 核CA、SAN/身份、用途、时间与当前执行代；worker反向核调用方身份及一次性票。证书合法不代表账号业务已授权，ticket/lease/版本仍独立核对。
5. **先修链路再交付材料**：先关闭 provider 错误收养/网络漂移、环境代理直连和明文上游缺口，再做受认证注入/原子发布、短期证书轮换与撤销、私有材料恢复、真实 Linux 网络矩阵。不发放到未验隔离的实例。

当前已有按 worker 进程随机生成的 X25519 凭据接收密钥，也有控制面/节点 PKI 库；但 worker RunProcess 的 gRPC server 和 host-agent Runtime dial 仍未装配生产 mTLS，尚未具备每 slot/执行代服务端证书签发/安装的整条路径。已有库测试不能补成这些缺失的生产连接。

## 当前放行结论

仅确认上述静态生成/分发模式与差距，**不确认每台线上 VM 密钥唯一或新系统已与线上完整一致**。下一阶段以 [VM 隔离优先计划](../../openspec/changes/complete-ccmax-execution-acceptance/vm-isolation-design.md) 的 VM0a–VM0d 为准。维持业务接线暂停，直到当前代码的身份、TLS与实际隔离链路均获得对应证据。
