# S2b3：正式 START 命令接入证书 bootstrap 与 mTLS

2026-09-18，基线 `429b109`。先修正式调用路径，不再用独立实验流程代替宿主接线。

## 已查明的断点

`SlotCommandExecutor.start` 目前只调用 provider.Start/Inspect；新worker在证书到达前
不会监听，因此该路径不负责发证，也不建立已认证连接。`Controller.Start`具备
bootstrap/mTLS，但会调用Create，不能直接用于只能针对既有CID的START命令。
`cmd/host-agent`仍只是健康HTTP骨架，orchestrator生产装配也未启用RuntimeEnrollment。
本轮不隐藏后两项、不自动开启daemon/业务开关，也不把新接线称为整链上线完成。

## 实现与目录

1. `internal/hostagent/runtime_existing.go`：Controller.StartExisting，准确已有
   instance/spec，在任何Start前、bootstrap后、TLS就绪后核对CID/slot/epoch/gen/image。
   禁止Create/recreate/delete；缺失/漂移/取消拒绝，错误不含证书或原始底层输出。
   使用原bootstrap、只接受准确身份的真实mTLS；不申请激活/业务票据。
2. `internal/hostagent/command_executor.go`仅增加可选认证启动接口及调用，不堆入证书
   逻辑。严格路径失败不回落到provider.Start、不发布健康成功；核后置CID及deadline。
   原nil路径仅为已有组件兼容，不能作为新正式装配入口。
3. `internal/hostagent/lifecycle/`：唯一provider实例组合原Coordinator、Controller与
   命令执行器；必须提供认证EnrollmentClient、trust和node certificate，固定使用
   StartExisting。按composition/adapter/test分文件；不增加ticket私钥或凭据持有。
4. 测试真实loopback TLS worker + 受认证Control签发 +正式START命令闭环；provider
   使用可观测fake，断言不Create、无票据请求、错误CA/节点/实例、超时/撤销拒绝。
   另外补命令启动失败、post-inspect漂移、取消、重放/旧代及并发竞争回归。

## 安全与验收边界

本轮本地/合成组件开发，不访问216生产，不读线上或170现有业务数据，无真实凭据、
模型请求、迁移、部署、push，不改宿主Bun/网络/防火墙。暂停build.py/runtimekit WIP
继续保留。先review/race/vet、适用恢复回归，再分阶段git提交。

现有实际Docker管理通道实证不替代本轮命令组合测试；本轮fake provider/TCP loopback
也不替代未来真实Docker adoption/跨容器mTLS/内核ACL验收。生产provider要求专用
Internal bridge，Docker创建这种网络会管理其自有宿主规则，必须单列实验范围，不能
承诺宿主规则字节不变；MemorySwap目前也缺严格生产核验，后续收紧而非弱化门禁。

仍开放：host-agent二进制完整装配、orchestrator持久化签发配置、实际跨容器mTLS、
在途lease失效、CLI桥接及出口矩阵。固定32%不因单项组件接通而提前增加。
