# S2b2：受认证签发与启动前证书投递

2026-09-18，基线 `78e3bb6`。用户要求继续开发，并按功能分文件夹管理。
本轮补原启动链的证书 bootstrap，不增加另一套执行服务、不重写 VM 内核。

## 目录和职责（先约定再实现）

```text
execution-plane/internal/runtimeenrollment/          签发授权、权威绑定、幂等策略
execution-plane/internal/runtimeenrollment/storage/  公开证书 receipt 的 SQL/内存实现
execution-plane/internal/runtimebootstrap/           实例内初始化、CA pin、安装与有界等待
execution-plane/internal/hostagent/bootstrap/        CSR→控制面→安装的启动协调
execution-plane/internal/provider/docker/bootstrap/ 固定 Docker exec 协议与公开数据边界
execution-plane/test/runtimebootstrap/              当前组件组合验证（按需）
```

既有 `control` RPC、`worker` 入口、`provider/docker` 保留少量必要适配；不为了目录
拆分制造循环依赖，也不把新增所有功能继续堆进 `server.go` 或 `process.go`。
`runtime/store` 仅适配其既有权威数据，迁移仍用现有统一 migrations 目录。
既有暂停的 `image/lab/build.py`、`image/runtimekit/` WIP 不改、不提交。

## 启动顺序与接口

1. 可信 host 配置钉住确切公开 CA PEM 的 SHA256。Docker 实例 env 只有该非秘密 pin，
   没有 CA 私钥、node 私钥或认证 token。空 pin 保持上一阶段的预安装模式，缺材料拒绝。
2. 原 worker 正常启动：安全建立实例专属目录，本地随机生成 key/CSR，最多等待45秒；
   安装之前不监听业务 RPC。固定 `bootstrap-request` 子命令只读出公开绑定与CSR。
3. 原 Controller：Create→Start→Bootstrap→waitReady→实际 mTLS Ready。Bootstrap 经
   受限 Docker 管理通道导出 CSR，不新开 worker 明文网络端口。
4. 已认证 node 通过原 NodeControl gRPC 服务的新签发方法请求。必须证明当前 Control
   stream 同一节点/证书，绑定其活 session；账号、assignment、epoch/generation/image
   由权威 DB 推导。DB 与独立 Redis lease 前后均有效。不要求 provider_ref/healthy/
   worker ready，否则首次启动死锁。此授权只允许准确实例证书，不赋予业务执行权限。
5. 严格验证CSR。以 assignment+完整binding+SPKI钉住第一张叶证书，持久化成功后才
   回复；同key的不同ECDSA CSR字节仍返回同一证书，不同key、过期receipt拒绝，不能
   把重试当作隐式轮换。最终回复前重核权威/session/lease/取消/时间。
6. 宿主校验返回证书与预期CSR、公钥、binding、CA一致，固定安装子命令在实例内校验
   pin并原子安装。公共bundle可用单个有界base64 argv，经由专用typed exec，绝不接受
   私钥/token/任意命令。公开证书可能出现在本任务exec元数据，不属于秘密；日志仍不
   回显bundle。所有实例新程序非root执行；Docker操作前后核准确CID/UID/sandbox/代/image。
7. worker仅在已装证书完整验证后继续原 RunProcess 监听；错pin、错身份、损坏/旧状态、
   不可信证书、超时与取消都不进入就绪。不清空、重建或覆盖异常身份。

## 授权、持久化与兼容边界

签发方法在原NodeControl认证连接上增加，不更改现有调用方业务HTTP/WS/gRPC协议。
默认关闭，仅显式注入所有权威依赖后启用；不自动迁移DB或启用生产开关。
SQL receipt只存公开证书、绑定、SPKI/CA摘要、有效期，不持有实例私钥。
叶证书TTL独立于约45秒lease；后续业务ticket/lease fencing仍独立，不能以证书代替
持续授权。轮换、在线撤销/在途连接、home恢复仍属后续门槛。

node私钥只在宿主，CA私钥只在控制面，实例私钥只在实例。可信宿主和同UID程序仍是
现有信任假设；本轮不声称可抵御恶意宿主，也不冒称还原了CLI内部device-ID。
host-agent二进制完整控制/数据/出口装配尚缺；Controller接线不自动关闭H5。

## 验证与提交

- 正例：无预装叶证的实例本地init→已认证控制面签发→同key重试相同leaf→安装→真实
  worker/Controller mTLS；全过程使用临时合成材料，无真实账号或模型。
- 反例：无证书/错节点/过期或已撤销node证书、重连旧session、错误slot/epoch/gen/image、
  无DB/Redis lease、前后权威变化、不同key重试、错误CA pin、安装失败/超时/取消。
- SQL合同、并发幂等、敏感数据不返回；实际Go race/vet、独立review及原生Linux验证。
  SQL mock不当成真实数据库并发证明，单容器回环不当成双容器出口或CLI整链证明。
- 仅170受限本任务容器，43仅跳板，不访问216生产、不改已有业务/数据库/UI/防火墙。
  按阶段git提交，不push。完整门槛未通过不提高固定验收百分比。
