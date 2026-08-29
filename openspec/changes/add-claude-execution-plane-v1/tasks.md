## 0. 基线与规格

- [x] 0.1 确认 `docs/prd/claude-execution-plane-v1.md`
- [x] 0.2 从 `ccmax` 创建 `codex/claude-execution-plane-v1`
- [x] 0.3 固定源 commit、首台节点只读清单和 CCMAX Go 测试基线
- [x] 0.4 串行安装前端冻结依赖并完成 typecheck/Vitest 基线
- [x] 0.5 创建 proposal/design/specs/tasks/verification
- [ ] 0.6 在实现偏离时先更新 OpenSpec 再改代码

## 1. 工程骨架与协议

- [x] 1.1 创建 `execution-plane` Go module、Makefile 和固定工具链配置
- [x] 1.2 定义 control/dataplane/worker protobuf 与 N/N-1 兼容规则
- [x] 1.3 加入 proto 可复现生成与 generated-code drift 检查
- [ ] 1.4 实现配置加载、校验、结构化日志、Prometheus 和 OTLP 基础
- [x] 1.5 实现 fake Claude CLI 与 fake Anthropic 测试工具

## 2. Docker Provider 与本地闭环

- [x] 2.1 定义 ExecutionProvider 接口和 provider contract tests
- [x] 2.2 实现 Docker create/inspect/start/drain/stop/destroy/health
- [x] 2.3 强制非 root、read-only、cap-drop、seccomp/AppArmor、资源和 tmpfs 策略
- [x] 2.4 实现 worker local API 与 slot ticket/epoch 校验
- [x] 2.5 完成本地 host-agent → Docker worker → fake upstream E2E

## 3. Orchestrator、节点与防双活

- [x] 3.1 创建 `worker_runtime` MySQL migrations/repository
- [x] 3.2 实现一次性 node enrollment、mTLS 证书和轮换
- [x] 3.3 实现 NodeControlStream、heartbeat、capacity 和标签
- [x] 3.4 实现 least-loaded + spread placement
- [x] 3.5 实现 slot desired/actual reconcile 与幂等 job
- [x] 3.6 实现 execution epoch、15s renew、45s offline、90s failover
- [x] 3.7 实现 Redis 故障 fail-closed 和旧 epoch egress fencing 测试

## 4. CCMAX Outbox 与灰度

- [x] 4.1 增加账号/分组 execution 配置、mode health 和 runtime status migrations
- [x] 4.2 实现 transactional runtime_outbox 和 consumer checkpoint
- [x] 4.3 实现 CCMAX gRPC data-plane client、route cache 和取消传播
- [x] 4.4 实现 group/account feature flag，现有账号默认 legacy
- [x] 4.5 实现 migration status 和已迁账号禁止明文 legacy 回退

## 5. 凭证、固定出口与上号

- [x] 5.1 实现 AES-256-GCM + KMS provider 接口、腾讯 KMS 和 fake KMS
- [x] 5.2 实现 credential vault/version/one-time lease/rotation
- [x] 5.3 实现 host-agent HTTP CONNECT egress 与 HTTP/HTTPS/SOCKS5 转换
- [x] 5.4 实现 worker 网络不可绕过测试
- [ ] 5.5 迁移 Session Key/OAuth/Setup Token/API Key/Cookie 上号到 worker
- [ ] 5.6 实现 Token 刷新和原子 credential version 切换
- [ ] 5.7 实现 canary/批量单向明文凭证迁移与校验报告

## 6. oauth_api 数据面

- [ ] 6.1 实现 Messages stream/nonstream 与 gRPC backpressure
- [ ] 6.2 复用 CCMAX 现有 request transform/raw passthrough
- [ ] 6.3 实现 count_tokens worker direct call 与 local models 汇总
- [ ] 6.4 接入现有 Chat Completions 转换
- [ ] 6.5 实现真实 usage、错误 envelope、输出前一次重试和输出后禁止重试
- [ ] 6.6 实现 cli/api/total 三层并发与 Redis API Key 公平队列
- [ ] 6.7 实现 queue/reject、120s wait、15m execution 和 global 1000

## 7. cli_native 与 MCP

- [ ] 7.1 构建固定 Claude CLI 版本的 worker 镜像并禁用自动更新
- [ ] 7.2 实现 safe-mode/strict-MCP/no-builtins CLI runner
- [ ] 7.3 实现 stream-json → Anthropic SSE/nonstream adapter
- [ ] 7.4 实现每回合进程、`--resume`、15m tmpfs session
- [ ] 7.5 映射 max_tokens、thinking 和 effort
- [ ] 7.6 实现 capability matrix，无法无损时 auto 转 API
- [ ] 7.7 实现 MCP Bridge、多并行 tool_use、tool_result resume 和超时
- [ ] 7.8 实现模式独立健康、billing 400 和重试风暴抑制

## 8. 生命周期

- [ ] 8.1 实现两阶段 provisioning 与逐步骤状态
- [ ] 8.2 实现统一 15m drain 和 force terminate
- [ ] 8.3 允许任意未删除账号归档并保留代理预约
- [ ] 8.4 实现软删除回收站、批量恢复和批量 purge
- [ ] 8.5 实现凭证/代理候选 slot 两阶段切换和失败恢复
- [ ] 8.6 实现节点 drain、slot migration 和 session 失败语义

## 9. 正文审计、UI 与可观测性

- [ ] 9.1 实现强制敏感字段剔除、正文脱敏和 2 MiB 上限
- [ ] 9.2 实现 COS 压缩/信封加密、默认 3 天 lifecycle 和清理任务
- [ ] 9.3 实现正文独立 RBAC、查看/搜索/下载审计和默认禁用导出
- [ ] 9.4 实现中文节点、槽位、账号 runtime、镜像、队列和审计 UI
- [ ] 9.5 实现批量 drain/recreate/migrate/archive/delete/restore/purge
- [ ] 9.6 完成 Prometheus/OTLP 与 Webhook 告警

## 10. 发布、部署与灾备

- [ ] 10.1 构建 TCR immutable digest、SBOM 和漏洞扫描
- [ ] 10.2 实现 canary/rollout/pause/rollback 与 N/N-1 验证
- [ ] 10.3 编写幂等 Ansible Docker/WireGuard/host-agent/UFW roles
- [ ] 10.4 验证不覆盖首台节点现有 80/443 与 UFW 规则
- [ ] 10.5 配置 MySQL daily backup/binlog、KMS deletion protection 和恢复演练
- [ ] 10.6 在获得单独批准前保持远端 playbook check mode

## 11. 验证

- [x] 11.1 execution-plane `go test ./...`、`go test -race ./...`、`go vet ./...`
- [ ] 11.2 CCMAX `go test ./...` 和新增 integration/E2E
- [ ] 11.3 管理 UI lint/typecheck/Vitest
- [ ] 11.4 Docker/代理/KMS/Redis/MySQL/COS 故障注入
- [ ] 11.5 1000 长连接、200 runtime accounts、API Key 公平队列压测
- [ ] 11.6 24 小时 soak，无 OOM、死锁、无界队列或凭证泄漏
- [ ] 11.7 少量授权账号真实 canary，不使用真实账号做压力测试
- [ ] 11.8 更新 `verification.md` 关联每项证据
