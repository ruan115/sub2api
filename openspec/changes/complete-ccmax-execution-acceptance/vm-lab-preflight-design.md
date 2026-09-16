# N3 前置：专用 Docker 实验宿主的只读预检

日期：2026-09-17；先记录设计再实现。属于 [固定百分比计划](../../../docs/plans/isthmus-container-delivery-v1.md) N3 的局部前置，不是一个可单独增加工程验收分的完成项。

## 问题与本轮停止线

现有 docker-e2e.sh 在验证宿主身份之前沿用 DOCKER_HOST/current context，调用 docker info/build，还可能使用默认 Colima及客户端proxy配置。旧预检也证明普通Colima重启会重新生成共享和端口转发。因此不能直接执行旧入口或启动已有default/旧probe VM。

本轮只提供可运行的**只读 Docker 端点检查**：绑定显式本地Unix socket、预期daemon ID/名称，要求空白容器/卷和默认网络，拒绝环境代理/隐式context。只输出固定字段与计数，不回显info/proxy/日志/任意异常。没有启动/停止/创建/删除接口，不调用docker、colima、SSH或shell。

Docker API本身无法证明外层Linux VM共享目录、端口转发、宿主防火墙或恶意daemon没有伪造响应。成功返回 `docker_endpoint_checked`，并固定 `isolation_verified=false`、`execution_permitted=false`；缺少实际VM检查不能因一份JSON声明就变PASS。后续需专门的受控实验环境启动/实际挂载与转发检查/规则生命周期才能关闭N3。

## 文件结构

- `recoverykit/lab/config.py`：显式参数与本地endpoint格式/类型/权限约束。
- `recoverykit/lab/engine.py`：标准库Unix HTTP，仅固定GET路径、deadline和响应大小上限；不读取Docker客户端配置或环境。
- `recoverykit/lab/preflight.py`：daemon身份/版本/能力和资源空白检查、脱敏报告。
- `recoverykit/cli/`：新增 `lab inspect` 参数与装配，保持现有统一安全输出。
- `recovery/tests/lab/`：策略负例、假的实际Unix HTTP server、超时/超限、无POST/外联；`tests/cli/` 验证错误不泄密。
- 模块README和verification：精确区分工具单测、Docker端点检查和真实隔离验收。

## 最小合同与验证

1. 必须显式提供绝对socket路径、预期Engine ID、预期daemon name；不接受TCP/SSH/HTTP endpoint，不采用默认context；拒绝危险代理/context/TLS环境，异常不输出环境值。
2. socket必须是当前用户拥有的实际Unix socket，处于当前用户独占且无symlink的直接父目录；所有祖先不得symlink或非可信可写目录。拒绝全局 `/var/run/docker.sock` 作为该工具入口，不自动改权限。API前后复核socket的inode/类型，变化失败；信任当前用户和OS，不能抵御同权限恶意进程竞态。
3. 请求只有 `/version`、协商后的 `/info`、`/containers/json?all=1`、`/networks`、`/volumes`。要求Linux、rootful、非Swarm、准确Engine ID/name、支持版本、runc及AppArmor/seccomp；空容器/卷，网络仅bridge/host/none。不读取image层、Env、账号home、凭据或日志。
4. 单请求和全检查deadline、每响应上限、严格JSON对象类型/重复键拒绝；3xx/非JSON/畸形/缺字段/未知运行能力失败。读取info可能在内存包含daemon配置字段，绝不原样返回或写文件。
5. 测试使用临时用户私有目录及合成Unix server，断言只出现固定GET，不启动Docker/VM。正常成功仍不能执行工作负载；身份不符在读取资源前停止。异常、代理/默认context、非空宿主、错误网络、安全能力缺失、symlink、socket替换、超时和超大响应均须拒绝。
6. 旧 `make docker-e2e` 本轮不接入、不执行；新命令成功不能被用来假装旧脚本获得授权。N3/N4与镜像I2–I5仍需后续真实证据。
