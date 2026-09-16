# VM 隔离优先：身份、固定出口与 TLS 验收

日期：2026-09-16；基线 `1dac649`。用户要求先确认 VM 链路和防泄漏，再推进后续功能，因此暂停 B2b2b/C 等业务接线，先修复隔离前置条件。只在本地开发、合成测试和提交；不 SSH、不借用账号、不部署、不读取真实凭据。

用户随后明确：需要每 VM 独立机器标识/密钥/证书，不需要 TLS 指纹伪装。已核查的线上静态证据与不能宣称一致的部分见 [身份/TLS 对照](../../../recovery/docs/vm-identity-tls-baseline-2026-09-16.md)。线上脚本的独立 home 证书存放不等于独立密钥生成，不复制真实线上密钥；机器标识来源尚未证实。

## 已确认事实与未确认边界

- 已保全的线上样本使用 Docker/runc；当前 PRD 的 VM 是每账号 ExecutionSlot，不是独立内核的 KVM/Firecracker。真实 hypervisor provider 属于单独的架构选择，不静默改写成“已经实现”。
- provider 创建时有只读根、非 root、cap drop、内部 bridge 和独立 tmpfs；但旧实例复用漏比 account_hash，Inspect 的隔离核对没有排除额外挂网/部分高权限配置。这是需要关闭的代码缺口。
- 实际 worker RunProcess 使用 ProxyFromEnvironment，缺少显式强制代理，NO_PROXY 可改变路径；其生产入口未强制 upstream origin 使用 HTTPS。仅设置 HTTP_PROXY/HTTPS_PROXY 不能作为防直连证据。
- [Docker 官方 internal 网络说明](https://docs.docker.com/reference/cli/docker/network/create/#network-internal-mode---internal) 明确允许访问 bridge gateway 上的宿主服务。因此 internal=true 不是宿主端口白名单，不把它认作完整 VM 出口隔离。
- [Go ProxyFromEnvironment](https://pkg.go.dev/net/http#ProxyFromEnvironment) 使用环境代理/NO_PROXY；[TLS Config](https://pkg.go.dev/crypto/tls#Config) 中证书验证、SNI、会话和 KeyLogWriter 是独立配置。目标站 TLS 和到 HTTPS 代理的 TLS 是两次不同的握手。
- 本地 Colima 的 default 与 ccmax-cache-probe 当前均 Stopped。旧测试 VM 可能保存恢复制品，不自动启动旧实例/旧容器或沿用任意 Docker context；真实 Linux 网络验收尚未执行。
- 信任边界是受控宿主、Docker daemon 与宿主内核。容器配置核对不能防止恶意宿主伪造 inspect 或读取进程内存，也不是独立内核隔离；若要把宿主/共享内核排除出信任域，必须单独选择真实 hypervisor 与运维方案，不能以随机机器标识解决。

## 模块与本轮切片 VM0a

| 模块 | 改动职责 | 验证 |
| --- | --- | --- |
| `internal/provider/docker/sandbox*.go`、对应 tests | 复用实例账号归属、单一专用网络、网络元数据及 sandbox 高权限/挂载/namespace 检查；原 provider.go/engine.go 只接线与 typed inspect 字段 | 先复现旧实现错误接纳，再加拒绝/回归；Inspect/Start/RuntimeEndpoint 不接纳漂移实例；不自动销毁异常实例 |
| `internal/worker/fixedtransport/` | 显式无凭据内部 proxy、无环境/NO_PROXY/localhost 直连 fallback、验证型 TLS 默认；不提供任意伪装/关闭证书校验选项 | 固定 proxy、URL 边界、代理故障不直连、实际本地 CONNECT+TLS 握手/证书拒绝 |
| `internal/worker/process.go` 与配置测试 | 启动前要求独立 EGRESS_PROXY 配置；真实执行/上号只用 HTTPS，同一固定 transport；退出关闭连接池 | 缺配置即失败，不先监听；恶意环境无效；既有真实 worker 合成回归 |
| provider 的 worker bootstrap env | 传入已有 SlotSpec.Network 中的无凭据内部代理，不从宿主继承代理/身份/调试环境 | 合同测试、无账户明文/代理密码、无全量环境复制 |
| 本目录 verification/tasks 与模块 README | 逐项记 review、实际测试与开放门槛 | 只勾选 VM0a，不把库测试/旧 Docker PASS 当作当前整链通过 |

先提交本设计；随后实现和独立交叉 review、离线 race/vet、文本/evidence 扫描和阶段 Git 提交。所有真实数据库/Redis DSN 从测试环境显式移除。

## VM0b–VM0d：本轮修补不能代替的门槛

1. **网络强制策略（VM0b）**：专用 Linux 测试环境验证 worker→仅指定出口端口、拒绝其他宿主端口/metadata/其他 slot/额外 bridge/公网 IPv4/IPv6/UDP/DNS。选择宿主防火墙或独立 egress namespace 时先写规则生命周期与回滚合同；不直接修改本机/线上防火墙。需要处理 daemon 重启、规则丢失和运行中漂移的 fail-closed，不只启动时检查。
2. **实例身份和 TLS（VM0c）**：复用当前 account-hash/slot/epoch/generation 权威。每实例进程私钥独立生成，不从模板复制；证书绑定准确 runtime 身份、由受信控制面签发，轮换/旧代/跨槽拒绝，私钥不进入 Env/argv/image/log。机器标识、证书摘要和 TLS ClientHello 特征分开管理；同实例稳定、重建按生命周期换代，不逐请求随机制造身份。现有 worker RPC 仍无生产 mTLS 装配，不能用上游 HTTPS 代替它。
3. **TLS 兼容证据**：默认原生、验证证书的 TLS；目标 CLI/版本未确定前不生成任意 JA3/JA4 或声称模仿成功。需要时对自有回环接收端采集无凭据 ClientHello，分别记录 SNI/ALPN/版本/构建摘要，未知项待验证。指纹不是账号授权，也不是恶意宿主不可伪造的证明。
4. **真实组合（VM0d）**：当前代码的控制台→控制面→host-agent→隔离 worker→出口代理→TLS 假上游，证书错误、代理宕机、DNS/IPv6/跨槽绕过、过期/断连必须失败关闭；双层 lease、在途流撤销与真实 registry 仍按 B2/B3/B4 实现。HTTP/SOCKS5 远程代理不自带传输加密，公网代理认证的保护需 HTTPS 或已验证的加密隧道；现有支持不等于已验收无泄漏。

只有这些门槛有当前代码的实际证据后才继续放行业务链路。VM0a 完成不表示“绝对不泄漏”，不声明真实 VM 或整体验收已通过。
