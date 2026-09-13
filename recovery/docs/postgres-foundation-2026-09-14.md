# 隔离 PostgreSQL 与认证存储基础

日期：2026-09-14；分支 `codex/claude-execution-plane-v1`。
**已完成本地 I1 数据库基础；旧 login → me → logout 仍未接入，不可替换线上服务。**

## 本轮交付

- 用户授权后，从官方固定源码校验、构建 PostgreSQL 18.6 + citext 1.8，安装在项目外私有目录。
  不使用 sudo/Homebrew/Docker，不改变系统 PATH、Bun 或线上服务。
- [测试集群模块](../../backend/internal/portunex/platform/postgres/testcluster/README.md)：每次新 data/socket/数据库，
  只开放私有 Unix socket；拒绝 DSN/PG 路由变量，验证版本、data directory、身份与随机 marker。
- [独立迁移](../../backend/internal/portunex/migrations/README.md)：users/auth_sessions/api_keys 共 30 列、14 约束、17 索引，
  在 `portunex_identity` schema 内事务执行；不是原 SQLx 历史，也无自动生产升级入口。
- [用户读取](../../backend/internal/portunex/users/repository/postgres/README.md) 与
  [会话存储](../../backend/internal/portunex/identity/repository/postgres/README.md)：精确数据读取、显式会话写入/过期筛选/touch/单行撤销。
  未提供 token/ID 生成、TTL、用户授权或旧 HTTP DTO。

## 验证与 review

真实 PostgreSQL tagged race/vet 全过；四个数据库模块重复 2 次 race 全过。
实际 catalog 与留档定义对照、NULL/default、ASCII citext、软删唯一/物理 FK cascade、
numeric/bigint/time、DDL 回滚、8 并发唯一冲突、锁阻塞取消、public 同值 ID 影子表隔离及连接重建读回均已验证。

原有演示回归也通过：Python 最终 **132**、Bun **111**、React **42**，Go race/vet、前端类型/构建和普通 HTTP/WS 关闭通过。
独立 review 找到的两项 P2 已修复：异常停机保留数据、禁止 socket 路径被解析成多目录；增量复核通过，无剩余阻塞发现。

测试结束检查本次 postmaster 和临时集群目录均为 0。合成临时数据按正常退出流程清理；
源包与隔离安装产物保留。异常路径是纯决策测试，未注入真实 SIGSTOP 或证明强杀后无子进程残留。

完整记录见 [验证文档](../../openspec/changes/restore-portunex-legacy-identity/postgres-verification.md)。

## 下一步与限制

下一步先补齐旧密码调用路径、ID 生成、token 存储/生命周期及完整 DTO/错误/权限证据，
再把已具备的密码和存储模块组成 legacy login → me → logout。原实现对照前不提升兼容状态。

本地 C/UTF8、无 ICU 的 macOS 构建不等于 Linux/生产 locale；ownership/grants、Unicode 大小写、
全部旧业务特殊值、进程重启恢复仍需后续验证。当前 decimal 不表示 NUMERIC NaN，已验证安全失败，不会悄悄转零。
旧 Bun 主动关闭失败门槛与执行面上线门槛没有解除；`execution_onboarding`/`migrated` 仍不能开启。

## 重跑与留档

```sh
PORTUNEX_TEST_PG_BIN=/Users/ruanyang/My-project/api/z/sub2api-recovery-private.kbZovy/postgres-18.6-runtime.GSZYGx/install/bin \
  make -C recovery postgres-integration
```

换机器按 [源码固定版本与构建说明](../runtime/postgres/README.md) 新建隔离安装，不搬运真实 PG 数据目录。
数据库底座提交 `815fe80`；repository 与本进度文档属于下一本地提交。两次提交前均有仓库外校验快照。
本轮无推送/部署；本机 Git 与快照不是异地备份。
