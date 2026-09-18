# P3a / VM0b 前置：出口 CONNECT 结构性拒绝矩阵与在途回收

2026-09-19。承接时间命名规划
[2026-09-18_13-17-37-isthmus-claude-handoff.md](../../../docs/plans/2026-09-18_13-17-37-isthmus-claude-handoff.md)
第 4 节 P3 第一、二条。不重做 P1 swap 策略与 P2 双槽位合同，不改 provider，
不启用 `execution_onboarding`，不打开业务数据面，暂停中的 `image/lab/build.py`
与 `image/runtimekit/` 继续绕开。

本切片只做**应用层** CONNECT 网关策略。Linux 内核 netns/防火墙门（VM0b 正身）
仍未做，需要专用宿主；本文件不因此加分。

## 1. 结构性拒绝：独立于每槽 allowlist

`internal/hostagent/egress_deny.go` 的 `classifyEgressTarget` 在 allowlist 之前
判定。它同时作用于两处，任一处通过都不算放行：

- **注册时**：`newTargetPolicy` 发现任一规则命中拒绝类，整个 binding 失败。
  槽位被留在"无出口"，而不是"部分生效的规则集"。
- **请求时**：`ServeHTTP` 在 allowlist 比较前复判，作为纵深防御。

没有可关闭该策略的开关。

| 类 | 拒绝原因串 | 覆盖 |
| --- | --- | --- |
| 云 metadata | `cloud-metadata-endpoint` | 169.254.169.254、169.254.170.2、100.100.100.200、fd00:ec2::254、`metadata.google.internal`/`metadata.goog`/`metadata`/`instance-data*` |
| 链路本地 | `link-local-address` | 169.254.0.0/16、fe80::/10、224.0.0.0/24 本地控制块 |
| 未指定地址 | `unspecified-address` | 0.0.0.0、:: |
| 组播/广播 | `multicast-or-broadcast-address` | 可路由组播、255.255.255.255 |
| IPv6 字面量 | `ipv6-literal-target` | 含 `::ffff:` 映射形；固定上游路径是 IPv4-only |
| 名称解析端口 | `name-resolution-port` | 53、853、5353 |
| 不在 allowlist | `target-outside-slot-allowlist` | 既有 allowlist 结果 |

**编码规避**：`2852039166`、`0251.0376.0251.0376`、`0xa9fea9fe` 都解析到
169.254.169.254，但不是点分四段，会作为"主机名"绕过上述分类，再由上游代理解析。
`validTargetHost` 因此要求非 IP 字面量主机的最右标签以 ASCII 字母开头
（RFC 1123 不允许纯数字 TLD）。已知副作用：单标签数字开头的服务别名
（如 `8080-app`）会被拒绝；当前仓库没有这类目标。

每个拒绝都返回 **403 + `X-Execution-Egress-Deny` 头**，不是静默挂起。任意超时
不算隔离成功；测试断言的是原因串，不是"连不上"。

## 2. 在途 tunnel 回收

此前 `Unregister` 与 epoch 换代只删绑定，已建立的 CONNECT relay 继续转发；只有
执行租约 fencer 的 5 秒轮询能杀，而那是**另一个权威**（执行 lease，不是代理
绑定）。现在 `EgressRegistry` 发布 `EgressRevocation`，`EgressGateway` 订阅并
关闭 `slotID` 相同且 `epoch <= 撤销 epoch` 的 tunnel；已注册的更新代不受影响。

- 失败的 `Register` 不发事件：撤销只在成功路径上发布，拒绝的注册不能拆掉它没能
  替换的绑定。
- `resolve → hijack → trackTunnel → isCurrent 复核 → fencer.Admit`：先入表再复核，
  与撤销并发时二者必有一个命中，tunnel 不会逃逸。
- `Serve` 返回（含 nil listener 早退）时注销订阅，registry 不长期钉住网关。

## 3. 本切片**没有**证明的事

- 不是内核网络门。容器内进程直接建 socket 绕过本网关，本层管不了；宿主与跨槽
  可达性仍须 VM0b 的 netns/防火墙，在专用 Linux 上做。
- 只看 CONNECT 字面目标。**DNS rebinding**——字母开头的合法域名被解析到
  metadata/私网地址——不在本层覆盖范围，属残留风险。
- **DoH 共用 443**，无法按端口分离，只能由 allowlist 拦；不声称已关闭。
- loopback/私网目标**刻意允许**（allowlist 仍然管），因为本地夹具与同宿主上游
  代理就在 127.0.0.1；这不是"宿主隔离已完成"。
- 本切片尚未接进 host-agent daemon：`AllowedTargets` 目前无非测试生产者。
- `/readyz` 保持 503、`production_ready=false`；缺权威 `execution:lease:v1:`
  writer 时生产签发继续拒绝。

## 4. 验证

`internal/hostagent`：deny 矩阵 24 例（含 4 条允许对照）、数字编码地址拒绝、
失败注册零事件、Serve 注销、注册期整绑定失败并零残留、网关逐类 403 原因断言 +
允许对照实跑 echo、`Unregister` 与 epoch 换代各自回收在途 tunnel（断言"被关闭"
而非"超时"）。

两轮独立 adversarial review：第一轮发现数字编码 IPv4 绕过（HIGH）、失败注册误发
撤销、监听器泄漏，均已修；第二轮复核三项并发现 nil-listener 早退漏注销，已修。

本机离线：全量 `go test -race`、`go vet`、linux/amd64 `go build ./cmd/...` 通过；
`internal/hostagent` race ×10 通过。`make -C recovery check`：236 Python / 150 Bun /
186 镜像 Python，与基线一致。未 SSH、未连 Docker、未请求模型、未部署。

分数不变，仍 32%；VM0b/N3 保持开放。
