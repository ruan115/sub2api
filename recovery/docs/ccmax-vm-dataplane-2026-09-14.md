# CCMAX 与隔离槽位调用：DP1 方案与验收

日期：2026-09-14。用户要求参考 VM 隔离机制完善 VM 与控制台调用。沿用 CCMAX 范围；Sub2API 继续拥有产品登录、权限与计费。本次不部署、不读取真实数据库/凭据、不启用 execution_onboarding 或 migrated。

## 事实与本轮目标

已有线上分析记录表明取样实例为 Docker/runc，基础镜像依赖外部应用与独立 home；并非 KVM/Firecracker。当前 ExecutionProvider 已有每账号 Docker 网络、只读根、资源限制与固定出口设计，继续保留，不改为共享凭据/共享账号 home，也不让网页持有 Docker 权限。

开发前代码断点：CCMAX 有 execution.v1 gRPC 客户端和 route cache，但 host-agent 只有 endpoint 广告，没有 ExecutionDataPlaneService 接收实现；Runtime.Execute 聚合整个响应且立即半关闭；Worker RPC 尚未核对请求中的 slot/epoch。gateway 选池仍限定 legacy，Bun 运行端仍为 fake，不应仅开启开关假装闭环。

DP1 实现可注入、默认不启动的宿主数据面以及逐事件 worker 客户端，用真实本机 gRPC + 合成 worker 做集成测试。浏览器不得直连 VM：控制台经 CCMAX 服务端边界进入私网 mTLS 数据面，再通过单账号 runtime ticket 访问隔离 worker。此切片不接生产 gateway、不改变旧 API 与收费路径，不声称真实模型/CLI 已运行。

## 文件职责（先规划后实现）

```text
execution-plane/internal/dataplane/
  server.go / stream.go       # ExecutionDataPlaneService 与双向有界转发
  auth.go / validation.go     # mTLS CCMAX 服务身份、消息/头/绑定检查
  resolver.go                 # authoritative assignment + lease + runtime lookup
  *_test.go                  # 合成协议/拒绝/租约失效测试
execution-plane/internal/hostagent/
  runtime_stream.go           # 无整流缓存的 worker stream adapter
  runtime_stream_test.go      # 票据、取消、半关闭、逐事件测试
  dataplane_integration_test.go # 真实回环 gRPC 两跳与隔离身份验证
execution-plane/internal/worker/
  rpcserver.go / *_test.go    # request slot/epoch 与现有 ticket identity 一致
```

新功能按模块独立，不扩展根目录大 gateway.go，不修改旧前端预览或 Portunex 演示后端。不复制 proto；CCMAX 的既有 execution.v1 wire contract 为调用入口。

## 不变量与验收

- 外部数据面只允许经过 CA 验证、TLS 1.3 的 CCMAX 服务证书；不能以 node 证书或任意 Bearer 代替。
- 输入必须匹配 authority 的 account/slot/node/epoch/route generation，且 assignment ready、lease current。客户端不得指定任意 endpoint/providerRef/代理。
- 请求前及流过程中复核 authority/lease；失效或后端不可用即取消，不回退明文/legacy。宿主不持签票私钥，只使用注入的 TicketSource。
- 首帧必须 begin；后续只能 tool_result/cancel；半关闭不是取消；上下文取消和超时传到 worker。响应逐事件转发，背压不形成无界 slice/队列。
- Worker 再核对 account/slot/epoch；generation 权威在宿主 resolver，不伪称 ticket 已包含 generation。
- 只转发明确允许的请求头；禁止 Authorization/Cookie/代理凭据；错误消息固定，不回显底层内部详情。
- 测试：mTLS 拒绝、错误账号/节点/epoch/generation、租约失效、首 chunk 早于完成、tool_result、取消、半关闭、CountTokens、请求大小与错误脱敏，race/vet。

## 后续仍未完成

生产签票控制 RPC、host-agent 生产装配、真实 worker CLI/HTTP 完整流式、Bun slot IPC、Token 刷新、CCMAX gateway 分组/选池/usage 接入、管理台运行状态与生命周期写操作、Docker 实际隔离 E2E、部署与 canary 均不是此切片通过即可放行。采用逐段集成，禁止浏览器直接访问 worker，也不恢复第二套权限/计费权威。

## 验收结果

DP1 服务库及本机合成闭环完成；生产装配仍未完成。

- 新增独立 `internal/dataplane` 模块；外层 TLS 1.3 + 已验证 `ccmax` 服务证书，权威快照与租约检查；只查找已存在 runtime，不因 Execute 创建/启动 VM。`SnapshotSource` 是可信一致性投影接口，生产投影/连接表实现不在本轮虚构。
- 新 runtime adapter 保留 mode/session/headers/route generation，clone 后转 opaque account，逐事件响应、工具回传、半关闭及取消。不持签票私钥、不新增生产凭据来源。
- Worker 加强 account/slot/epoch 校验，并拒绝未携带 generation 的内部请求。旧收集式 helper 只保留供本地 Docker fixture，改为调用方显式传 generation；生产升级须配套更新客户端，不能独立替换 worker。
- 真实回环两跳测试：服务证书握手 + 一次性 Ed25519 worker 票据；首段早于完成、工具结果、半关闭、取消帧/context、租约撤销/存储失败、代变化、CountTokens、无客户端证书/错误服务证书/跨账号拒绝。合成 worker 发送 4 MiB 累计响应，每块 64 KiB，relay 不进行整包收集。这不是原 HTTP executor 已解除 2 MiB 限制的证明。
- 独立 review 发现并修复 1 项 P2：lease I/O 期间快照可能跨过有效期。已在 I/O 返回后再次检查，fake-clock 用例覆盖 Validate 与 Resolve 最后一次检查；reviewer 已复核关闭。

实际命令与结果：

```sh
cd execution-plane
go test -race -count=1 -timeout=120s ./...       # 通过
go vet ./...                                  # 通过
go test -race -count=3 -timeout=60s ./internal/hostagent -run TestDataPlaneWorkerLoopback
# 连续三轮通过
go test -tags docker_e2e -run '^$' ./internal/hostagent
# 仅编译通过，没有运行 Docker

cd ../ccmax-manager
go test -race -count=1 -timeout=120s -run 'TestExecution|TestChooseExecution|Test.*Migrated|Test.*RuntimeOutbox' ./...
# 现有执行客户端、迁移隔离、outbox 定向回归通过
```

新增复验入口 `make -C execution-plane dataplane-check`；无默认 listener、无部署命令。请求/响应头使用明确白名单，未支持的 `ListModels` 返回 Unimplemented。运行时正文、私钥、证书与截图不进入 Git。原版前端预览与 Sub2API/CCMAX 现有入口未改；生产功能标志和真实账号迁移状态未变。

下一阶段建议 DP2：确定宿主运行时注册表与权威状态投影，补受认证的生产签票通道和 host-agent 装配，再接 CCMAX 只读执行状态；之后才做 gateway 选池/usage 分流、真正 worker 增量 SSE/CLI 和 lifecycle 按钮。每步单独验证，不将 DP1 作为 §31 上线门槛通过。
