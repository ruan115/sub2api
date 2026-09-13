# 分阶段验证记录

初始日期：2026-09-13；2026-09-14 更新 I1。本 change 未完成旧认证闭环，不可上线。

## I0.1：定义与静态二进制证据

- PostgreSQL：仅白名单三表定义 catalog；READ ONLY、3s statement/1s lock timeout、ROLLBACK。
  采集 30 列、14 constraints、17 indexes、citext 1.8。没有业务行、PHC/token 值或环境配置。
- ELF：固定 45,810,904 字节及 SHA-256；最终 8 段 ASCII 共 2,611 字节，20 种 marker。
  未执行 ELF 或读进程内存。最终固定 collector 于 `2026-09-13T14:14:44Z` 复核，输出 JSON 与保留 artifact 完全一致。
- 所有定义表达式和片段先筛查，再人工审阅；不是任意二进制绝无秘密的保证。
- Python 全回归 129 PASS（其中新增 catalog 10、binary 8）；合同仍是 discovered/business_verification=false。
- 独立 review 覆盖 SQL、artifact、密码边界；静态证据不用于推断未证实的运行时算法/权限。
- Review 补强已合入：收窄未保留/截断片段的文字推断；严格 JSON loader、布尔版本号/重复键/额外字段拒绝测试。增量复核通过，无阻塞发现。
- `web-reverse-master` 自检 7/7 PASS；没有安装 OpenSpec CLI，未运行 CLI strict validate。

## I2.1：本地有界密码能力（非旧登录实现）

- 新增 `identity/password/` 独立模块，只有 Argon2id v19 能力；显式 Limits，无默认线上参数。
  未改 synthetic identity/session、HTTP 路由、前端 adapter、依赖清单或 lockfile。
- 严格 PHC、Base64 与资源检查；同步 KDF、有界 TryAcquire、取消后不返回成功。
  不把 context 当作可中断 KDF，也不在工作结束前释放槽；固定错误不回显输入。
- 6 个已有 x/crypto 上游固定向量（含多轮/并行、非整除 memory）；错误密码、参数溢出、
  资源拒绝、满载、预取消/运行中取消、密码字节不归一化及日志输入不泄露的错误检查通过。
- `go test -race -count=10 ./internal/portunex/identity/password` 与 vet PASS；两项 fuzz 各 10 秒 PASS。
  独立 reviewer 另跑 race/vet 与各 5 秒 fuzz，并核对新增向量与上游固定常量。
- 全 `internal/portunex/...` 与 demo 入口 race/vet 再次 PASS。
- `make -C recovery check-demo` PASS：初次 Python 128、Bun 111、React 42、Go race/vet、
  前端 type/build、普通回环 HTTP/WS 与关闭；review 新增严格元数据测试后 Python 全套 129 再次 PASS。
  新增密码向量后 Go 全套 race/vet 再次 PASS。
- 该回归不包含已知 Bun 1.3.9 服务端主动 WS close 的独立失败门槛；此次未重跑或解除该门槛。
- 独立 review 无阻塞发现，非阻塞建议已修复并增量复核。

## 明确未过的门槛

I0.2 未完成：密码调用路径/实际参数、ID epoch/worker 布局、token 生成/存储变换、
有效期/续期/撤销/并发、完整 User DTO 与错误状态/权限合同仍需独立证据。
SQL 中 `LIMIT 10` 不能单独证明所有登录的有效会话上限。

I1 的原运行时阻塞已解除：2026-09-14 用户授权后，项目外独立构建 PostgreSQL 18.6/citext 1.8，
真实合成库迁移和 repository 测试已完成。见 `postgres-verification.md`。
未启动 Docker Desktop、未替换系统软件、未借用线上 DB；不把本地 C locale 测试当生产 locale 或完整旧数据迁移证明。

I2.2–I5 未完成：会话与旧 HTTP transport、权限/重启回归及隔离原实现对照尚未接入。
现有 Go/React 合成演示、主系统认证、Bun 1.3.9 和线上服务不变，不推送、不部署。
