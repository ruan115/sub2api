# 认证基础切片：进度、review 与交付

日期：2026-09-13；分支 `codex/claude-execution-plane-v1`。
用户已确认旧 Bearer 认证规划。本轮完成定义证据和独立密码能力，
**还没有完成旧 login → me → logout，不是可上线认证替代品。**

## 已完成

- 开发前记录 [ADR-005 模块结构](../../openspec/changes/restore-portunex-legacy-identity/design.md)，
  按 password、数据库采集、二进制采集、各自测试目录拆分，没有重写演示 app。
- [认证表定义](../baselines/portunex/postgres/README.md)：30 列、14 约束、17 索引、citext 1.8；
  只有 catalog 元数据，无真实密码/会话。旧快照保持原样。
- [二进制静态证据](../baselines/portunex/binary/README.md)：固定指纹、8 个有界片段、
  20 个 marker；保留内容与原服务器 collector 输出再次比对一致。
- [独立密码模块](../../backend/internal/portunex/identity/password/README.md)：严格 Argon2id v19 PHC、
  显式资源政策、满载拒绝、运行中取消仍持槽直到实际结束、错误不回显凭据。
  这只是本地验证能力，不以原 ELF 中出现库名就认定原 login 实现。
- 独立 review 完成，无阻塞发现；已修复证据表述和 JSON 元数据检查建议，
  补了上游多线程/多轮/非整除内存向量，增量复核通过。

## 验证结果

最终 Python **129 PASS**；Bun **111 PASS**；React **42 PASS**；
Go 全 Portunex race/vet、前端类型/构建、普通回环 HTTP/WS 与关闭通过。
密码模块另经 race 重复 10 次和两项各 10 秒 fuzz；独立 review 也有单独复跑。
详见 [本切片验证记录](../../openspec/changes/restore-portunex-legacy-identity/verification.md)。

上述没有解除 Bun 1.3.9 服务端主动 WS 关闭的已知失败门槛，也不替代真实 PostgreSQL、
旧实现对照、执行面 MySQL/Redis Lua/TTL 或 PRD 上线验收。

## 仍缺什么，下一步做什么

1. **I0.2 旧行为证据**：确认密码调用路径与参数、Snowflake 实现/epoch/worker、
   token 生成及存储变换、到期/续期/撤销/并发、完整 DTO/错误/权限。静态 SQL 只是线索。
2. **I1 独立真实 PostgreSQL**：本机无 PG、本地 Docker daemon 不可用。
   已提出项目外隔离下载/构建请求，尚未执行；不启动用户整个 Docker 栈，不使用线上 DB。
   有可用运行时后，只对临时合成库验证恢复迁移、citext、精确 decimal、唯一性和软删除。
3. **I2.2–I5**：会话 repository/service → legacy HTTP → 权限/并发/重启/脱敏测试 →
   隔离旧程序的合成对照。未知项不硬编码成所谓兼容默认值。

旧接口和生产开关仍未挂接，不能设 `migrated` 或开启 `execution_onboarding`。
Provider、API Key 完整生命周期、计费/支付、生产 UI、部署、MCP/gRPC/执行面等仍按原总计划另行推进。

## 留档与范围

证据与规划已独立提交为 `0a9b955`；密码模块和本进度文档形成下一本地提交。
每次提交前用已有 workspace 工具在仓库外创建校验过的私有快照。
Git 与这些快照仍只在本机，不等于异地备份；本轮未推送、部署或替换系统 Bun。

本轮遵循 `web-reverse-master` 的只读 SSH／离线静态证据流程，将“存在的字面量”
和“已验证的运行时行为”分开记录，未执行旧程序或尝试真实账号登录。
