# S2b：实例证书安装与既有 worker/host-agent 双向 TLS

2026-09-17，承接 S2a `c08d680`。不另写虚拟机、不新建平行执行服务；复用
`runtimeidentity`、`worker.RunProcess`、`hostagent.Controller.Start`。

## 本次交付顺序

1. 扩展身份目录的原子状态，安装公有叶证书。私钥始终本地生成并留在原文件；
   证书必须匹配本地公钥及 accountHash/slot/node/epoch/runtime generation。
   CA 由受信配置提供，不从提交的证书包自行信任。首次安装和同证幂等；不同证书
   替换属于后续轮换阶段，本轮拒绝。残留临时文件、损坏状态和权限异常继续失败关闭。
2. 既有 URI 标识不变；签发端加入由完整 URI 的 SHA-256 前128位派生的唯一
   `rt-<digest>.execution.invalid` DNS SAN。CSR 仍只申请 URI，DNS 由签发端确定。
   拨号仍为私网字面 IP，TLS 的 ServerName 仅作验名，不触发 DNS 查询。
   客户端保留 Go 默认链/时间/EKU/hostname 验证，额外精确 URI/用途约束；不设置
   InsecureSkipVerify。服务端 TLS1.3、RequireAndVerifyClientCert、唯一正确 node URI。
3. 将控制权威 desired_generation 显式传进 SlotSpec、Docker 环境和 worker，
   与 epoch 分开。Controller 在创建实例前校验 TLS 配置，原拨号改为 mTLS，
   等待 gRPC Ready 确认实际握手而非仅建立 lazy client（当前协议没有 Inspect RPC，
   不能为探活额外消费单 scope 业务票或扩大授权）。worker 缺身份/证书/信任根
   时不监听，包括 fake activation；不允许明文回退。票据、scope、防重放及既有
   lease/session fencing 保留，TLS 不能取代业务授权。
4. 在真实 TCP 上组合既有 RunProcess / Controller 验证：正确身份+票据通过；
   错槽/代/node/CA/用途、明文、缺票/错票/重放拒绝。使用临时合成 CA/身份，
   不调用模型/真实账号/生产 API。Go race/vet、独立 review；有条件再在170原生
   Linux 的受限无网络容器运行同一测试二进制，私钥与材料留在一次性 tmpfs。

## 文件职责

- `internal/runtimeidentity/`：本地身份/安全存储、证书安装、严格 TLS 配置。
- `internal/pki/runtime.go`：对已经授权的精确 Binding 签发，不实现新的未认证入口。
- `internal/provider/`、`internal/hostagent/`：代号贯通与原客户端接线。
- `internal/worker/`：原服务启动前加载身份、安装后的证书和受信 CA。
- `cmd/instance-identity/`：按需提供有界公有证书安装操作，无私钥导出。
- 各模块测试与既有 `verification.md`：安全反例、真实 TCP 和运行边界。

## 不在本次冒认完成的部分

当前 host-agent 二进制仍只有健康服务，生产 Controller 装配、权威认证签发入口、
ready 前 CSR/证书投递、私有卷生命周期、CLI 原生服务与此 Go worker 的完整桥接
尚缺。不能在 waitReady 后才签发证书造成启动死锁；本轮组合测试使用预安装材料。
严格 Docker 模板必须明确尚无证书投递方案，不能以旧明文模板绕过。

两角色同一受限容器的 TCP 验证只证明当前组件握手及票据检查，不证明跨容器
网络隔离、实例到控制面 TLS 全生命周期或已与线上等价。K3/K4 完整门槛保持打开，
除非其所有授权/实际部署/lease 条件另有充分实证；本轮不预加验收分。

只改本地代码及本任务170测试资源；43仅SSH跳板，不访问216生产，不更换宿主Bun，
不改现有业务容器/网络/防火墙/UI/数据，不用线上凭据，不push。
保留暂停中的 `image/lab/build.py` 和 `image/runtimekit/` WIP。

## 本轮结果：S2b1

证书安装和当前Go worker/Controller真实TLS接线已实现并独立review，在170无网络
受限容器三组测试各三遍通过；完整离线race/vet和恢复工具回归通过。
[输入摘要、正反例及清理证据](verification.md#s2b1证书安装与实际组件mtls)。
review另修意图代与清理目标代/旧image混淆，不让换代卡死旧实例清理。
K3/K4整体未关闭，总体32%不变；S2b2继续补本计划列明的认证发证、启动前投递及
实际host-agent/双实例装配。旧Docker E2E在副作用前明确失败，不能当作绿色验收。
