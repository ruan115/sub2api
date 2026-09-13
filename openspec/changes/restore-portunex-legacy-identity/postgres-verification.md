# I1 PostgreSQL 分阶段验证

2026-09-14；用户已授权项目外隔离下载/构建，不涉及系统安装或生产数据库。
目录和运行边界先记录于 `postgres-design.md`，再分别实施。

## 数据库底座

- 官方 PostgreSQL 18.6 tar.gz：29,598,283 字节，SHA-256
  `983ee554ec53dbeb9b70797bef9fcf4e67e117e7e48ca1463cc80b3ff8e8ff3f`。
  HTTPS 下载、官方摘要匹配、7,944 tar entries 范围/类型检查后才解压构建。
- 构建目录：仓库外私有 `postgres-18.6-runtime.GSZYGx`；explicit install prefix，Apple clang 14.0.3，arm64。
  只用 `--without-icu --without-readline` 和同源 citext。版本、构建 flags、6 个本地文件指纹已留档于 `recovery/runtime/postgres/`。
- helper 只创建自己的临时合成库：目录/socket 0700、TCP disabled、host auth reject。
  固定 Unix-only connector 与 db/user/data_directory/version/socket/随机 marker 校验。
  Go 进程拒绝 PG*/DATABASE_URL/DSN，子进程仅继承固定环境。既有 Docker、主系统数据和 Bun 未改。
- 负向命令验证：缺少 runtime、带外部 PGHOST 均非零失败，未 skip 或回退到其他数据库。
- 新迁移在 `portunex_identity` 中恢复 3 表，实际 catalog 对照通过：30 列、14 约束、17 索引、citext 1.8。
  仅 schema 引用从 public 移到恢复域；保留 nullable/default/CHECK/FK/index 细节。
- 真实库回归：NULL/default/no-ID-generator、角色 CHECK、ASCII citext、软删除唯一、物理 cascade/软删不 cascade、
  exact numeric/bigint/timestamptz、已有 schema 冲突、extension 冲突后整个 DDL 事务回滚均通过。
- `go test -tags portunex_integration -race` 与 tagged vet 已通过；正常 `Close` 幂等、进程已 Wait 回收及目录删除有真实测试。

## Review 与已修复问题

两个独立 agent 分别复核 helper 和迁移；主 agent 复核存储代码/测试。
helper review 找到两项 P2，已修复且增量复核通过：

1. PG 子进程可自行 setsid，父进程退出/进程组 kill 不能证明全簇已退出。
   现在 initdb 失败、异常退出、强制退出和连接关闭错误都保留数据并报错；仅确认正常关闭后删除。
2. unix_socket_directories 是列表，继承的特殊 TMPDIR 路径可能增加额外 socket。
   现在启动前限制路径字符/长度，连接后再核对服务端实际单一 socket 配置。

异常路径测试是纯清理决策测试，不是实际 SIGSTOP、卡死 initdb 或强杀后零残留证明。
不得将异常保留目录自动递归删除；需要定位本次 owned instance 后再处理。

## 仍未证明

Linux/生产构建、生产 locale/Unicode citext、原 ownership/grants、完整 SQLx 历史没有被本地 C/UTF8 测试代替。
未启动旧 Portunex ELF、登录真实账号或确定 ID/token/TTL/旧接口合同，I0.2/I2.2–I5 仍开着。
本地安装指纹不宣称跨机器可重复字节级构建；二进制和数据不进 Git。
