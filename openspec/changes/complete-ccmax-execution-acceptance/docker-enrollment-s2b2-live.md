# S2b2-live：真实 Docker 证书管理通道

2026-09-18，基线 `b6bae7a`。本轮延续实例启动工作，不新增业务功能或虚拟机内核。

## 交付与目录

- `test/dockerbootstrap/`：仅实验使用的 Go 协调器，调用真实 Engine typed exec 和原
  受认证 NodeControl RPC；按控制面夹具、容器检查、传输、输入协议拆文件。
- `image/lab/livebootstrap/`：复用原 Lab daemon/基线/预算/精确清理，分别管理公开
  制品、实验 profile、子进程协议和双容器实验，不继续堆入 `mtls.py`。
- `internal/provider/docker/bootstrap_http_test.go`：真实 Unix HTTP/hijack wire 回归。

## 执行范围及明确例外

只用170测试宿主，43只作SSH跳板；不访问216生产，不读现有业务Env/Cmd/logs。
不改用户组、socket权限、宿主Bun、已有服务、DB、UI、网络或防火墙，不push/部署。
现有170用户不在Docker组。实验协调器属于**可信宿主管理程序**，由已有root Lab
启动，访问固定Unix Docker socket即有daemon管理权限；不能用非root UID或应用
allowlist冒称其具有安全沙箱。它不接收用户工作负载，仅处理两个准确CID。
子进程使用干净环境、core=0、单线程调度/软内存目标、输出上限和硬时限；不持久化
CA/node/ticket私钥，只在可信宿主进程RAM保存。真实worker全部UID1000执行。

本轮新增一个**仅实验**的只读文件挂载例外：各worker仅bind已核SHA、root所有、
单hardlink、0555的公开静态Go worker制品至 `/worker`。源必须为当前0700实验根下
`live-bin/worker`；不挂载目录、socket、home、配置或凭据。其余沿用固定S1b base、
readonly root/cap0/NNP/network-none/private namespace、1CPU/1GiB/no-swap/pids128、
私有tmpfs和有限PID1 watchdog。它不是最终镜像配方，也不放松生产provider禁bind规则。
两容器同时存在前MemAvailable须至少3GiB；既有业务容器基线必须保持一致。

## 顺序与验收边界

1. 本地固定当前worker/实验协调器的源码和ELF SHA；上传只含公开制品/代码的准确清单。
2. 协调器启动内存合成CA和原NodeControl TLS服务、node证书/活会话，只输出公开CA pin
   和ticket验证公钥。Lab据此创建独立A/B容器，公开绑定各自不同，绝不下发私钥。
3. 启动前后检查确切CID/owner/image/UID/挂载/资源/网络；协调器只允许这两个CID。
   真实 `HTTPEngine.BootstrapRequestExec` 导出CSR，再走真实受认证签发RPC及当前
   assignment/session/lease检查，经真实 `BootstrapInstallExec` 装回实例。
4. 验A/B SPKI不同、同key重试叶证不变、交叉证书/错CA安装拒绝、无有效lease拒绝；
   正确安装及相同证书重装成功，固定healthcheck只验证TCP监听前后变化。
5. 任何创建结果不确定不得猜ID清理或写PASS；finally只清理记录且owner匹配的CID。
   原业务基线相同、私有tmpfs销毁后才写成功证据，不删除已有镜像/网络/卷。

network-none不能让宿主TCP直达实例，且生产provider要求独占Internal bridge，故本轮
不使用/放宽完整provider adoption，也不声称跨容器Controller mTLS、CLI转发或出口
隔离已验。Memory授权/receipt不是实际SQL/Redis；真实SQL并发、宿主正式装配、
跨容器mTLS/lease失效、持久home、最终镜像和CLI整链仍开放。固定总分暂不提高。

本地race/vet、HTTP wire正反例及Python profile/协议/清理失败测试先通过；独立review
后才实跑Linux。按计划、实现、实证逐步git提交，暂停的build.py/runtimekit WIP不动。
