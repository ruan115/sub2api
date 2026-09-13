# 分阶段验证记录

日期：2026-09-13。本 change 未完成旧认证闭环，不可上线。

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

## 明确未过的门槛

I0.2 未完成：密码调用路径/实际参数、ID epoch/worker 布局、token 生成/存储变换、
有效期/续期/撤销/并发、完整 User DTO 与错误状态/权限合同仍需独立证据。
SQL 中 `LIMIT 10` 不能单独证明所有登录的有效会话上限。

I1 未完成：本机未找到 PostgreSQL，两个已核验的本地 Docker socket daemon 不可用。
未启动 Docker Desktop、未安装新依赖、未借用线上 DB。隔离运行时下载构建另待用户答复。
sqlmock/静态检查都不算 PostgreSQL 语义或数据迁移验证。

I2.2–I5 未完成：会话与旧 HTTP transport、权限/重启回归及隔离原实现对照尚未接入。
现有 Go/React 合成演示、主系统认证、Bun 1.3.9 和线上服务不变，不推送、不部署。
