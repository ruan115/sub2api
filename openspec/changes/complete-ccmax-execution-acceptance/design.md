# 设计与模块边界

## A：worker 实际 HTTP 增量执行

```text
worker/process.go                 # readiness/mode/凭据/固定请求构造与RPC适配
worker/upstream/                  # 独立有界HTTP响应读取和转发，不持凭据、不选路
worker/upstreamusage/             # 有界JSON/SSE usage观察，不写原始正文
worker/*upstream*_test.go          # 真正worker执行器的回环集成/错误/取消
```

`upstream` 不导入父包 worker，使用回调交付响应头/正文块；`upstreamusage` 不依赖 HTTP client 或 RPC。父执行器组合二者，把明确得到的 usage 放入现有 `ExecutionCompleted.usage_json`，不伪造缺失计数。请求凭据仍在既有受控路径注入，不传入旁路日志/观察器。

传输负责 HTTP 状态、原始字节、资源收尾和背压；观察器负责 SSE 完整性与 usage 的有界解析，不承担计费结算。接通真实网关仍属于后续 C 阶段。具体大小/编码/完成语义以实施计划 A 合同及新增测试为准。

## 后续装配原则

可信 assignment/lease 来自控制面，不以 Redis route 或调用方字段代替。host-agent 只获取一次性执行票据，不拥有签票私钥；控制台只能经过 CCMAX 服务端，不能直连 worker。新流程默认关闭，legacy 保持既有行为，迁入后故障不回退明文。

这不是逐字段复制线上所有不明行为的授权；未知 pipeline / MCP / credential 行为须继续按已保全证据和合成对照冻结合同。
