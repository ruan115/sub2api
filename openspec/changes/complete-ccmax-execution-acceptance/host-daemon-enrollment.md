# S2b4：默认关闭的 host-agent 服务入口与持久化签发装配

2026-09-18，基线 `82863b6`。本轮把 S2b3 的严格 START 组合接入二进制，
不重写 VM、不扩展 Sub2 计费/UI，不用新实验脚本代替实际入口。

## 实施顺序与模块

1. `internal/hostagent/daemon/`：独立 config / identity / composition / run /
   health 文件及对应测试。显式 `EXECUTION_HOST_AGENT_RUNTIME_ENABLED=true` 才启用；
   关闭时不加载证书、不访问 Docker 或控制面。`cmd/host-agent` 显式注入 daemon
   selector，`service/bootstrap.go` 仅保留通用入口钩子，不反向依赖 host-agent。
   使用预签发的专属 node 证书、私钥和固定 CA；节点不持有 CA/票据签名私钥。
   配置固定本地 Unix Docker socket、私网/回环控制地址、TLS 验证名、无凭据 HTTPS
   upstream 与内部固定出口代理。校验在外部操作前完成，错误/日志不输出配置值。
2. daemon 复用 Docker provider、bootstrap.RPCClient、lifecycle.New、ControlClient。
   只启用生命周期命令，不接 credential activation、业务票据或数据面，不广告 docker
   生产调度/镜像能力；明确 lifecycle-only 标签与能力。健康接口只监听回环，
   `/readyz` 明确 503 / production_ready=false，不把控制连接或 TCP 存活当业务就绪。
   placement 对 lifecycle-only 标签或能力明确排除，含无能力要求及 sticky 请求。
   节点证书到期结束本次运行；无自动签发/自动替换身份。取消后封闭新命令并有界等待
   在途命令，先停控制工作再关闭依赖；不得静默留下继续修改实例的后台工作。
3. `internal/config/runtime_enrollment.go` 与 `internal/service/runtimeenrollment/`：
   控制面独立默认关闭的签发选项，启用时组合 SQL receipts / 已有 assignment store /
   独立 Redis lease validator，监听前只读 PING，退出/失败关闭自有连接。
   `EXECUTION_LEASE_REDIS_ADDR` 必须显式配置，不回退 route Redis；统一固定
   `execution:lease:v1:` key prefix，不使用 Memory authority/receipt 或自动授权。
   不创建表、不应用迁移；沿用已有 schema 只读核验。
4. 独立 review，错误身份/配置/缺租约拒绝、off 零 I/O、超时/清理/ready 状态测试；
   本地合成 TLS/Unix socket 组合验证，加离线 race/vet 和分阶段 git 提交。

## 已知缺口与不能宣称的结果

当前生产尚无 execution lease writer；Redis PING 只证明连接可用，不证明任何 slot
拥有租约。签发会在缺失真实租约时拒绝；本轮不自行写 lease/篡改账号/打开 onboarding。
已有 provider MemorySwap、真实跨容器 mTLS、出口 ACL、在途撤销与 CLI 桥接仍未全部
验收。因此 lifecycle-only 入口不是完整业务 host-agent，也不是上线开关；固定验收
台账仍为 32%，除非已有门槛的所有证据实际齐全。

本轮只在本地开发/合成测试，不 SSH、不部署、不读生产配置/凭据、不发送模型请求，
不影响 216 生产或 170 业务容器，不 push。既有 build.py/runtimekit WIP 原样保留。
