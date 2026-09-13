# I1 开发前设计：独立 PostgreSQL 与认证持久化基础

2026-09-14：用户已授权项目外隔离下载/构建 PostgreSQL，并继续开发、review 和本地提交。
本切片不做新的线上采集，不读取生产业务行，也不安装/启动 Docker 或替换系统软件。

## 模块与职责

```text
recovery/runtime/postgres/                 # 固定官方源码/校验信息、隔离构建说明
backend/internal/portunex/platform/postgres/testcluster/
                                          # 仅 portunex_integration tag：自建临时集群、验证与关闭
backend/internal/portunex/migrations/       # 合成恢复库 schema；显式执行，不加载 Sub2API SQL
backend/internal/portunex/users/repository/postgres/
                                          # 用户读取，nullable/decimal/bigint 原语义
backend/internal/portunex/identity/repository/postgres/
                                          # 会话 SQL 存储原语；不决定 token/ID/有效期策略
recovery/docs/postgres-foundation-2026-09-14.md
                                          # 构建来源、真实数据库测试证据与剩余门槛
```

## 数据库与进程隔离

- 选择与证据一致的官方 PostgreSQL 18.6 源码；下载后核对官方 SHA-256，再执行构建。
  `--prefix` 固定为项目外新建私有目录，构建产物和数据库不入 Git，不使用 sudo 或修改 PATH 配置。
- 编译环境没有 ICU 开发包时使用 `--without-icu`，本地验证使用 C/UTF-8 locale。
  这是测试环境配置，不宣称等同生产 locale、ICU 排序或 Linux 构建。
- helper 仅接受显式本机二进制目录，不接受 DSN、既有 data directory、数据库 URL 或 Docker context。
  必須自己创建全新临时 data/socket 目录；不继承 PG*/DATABASE_URL/动态加载等运行环境。
- PostgreSQL 只开放私有 Unix socket（目录 0700），禁用 TCP listener 和 host 认证；不使用默认 socket。
  显式数据库名、测试角色、socket 路径和连接选项，不让环境变量重定向连接。
- 连接后校验当前数据库、角色、版本、data_directory、listen_addresses 和本次随机 marker。
  没有运行时或失败时测试明确失败，不静默 skip。关闭只针对本次启动的进程/目录；关闭失败必须可见。
- 不复用已有 Sub2API integration harness，不执行其 migrations 或启动 Redis。

## schema 与 repository 边界

- 固定独立 `portunex_identity` schema，citext 安装在 public 并显式限定类型。
  使用已审阅的三表定义；这是新恢复迁移，不是 SQLx 原始迁移还原，绝不对生产运行。
- 移植 users/auth_sessions/api_keys 的列/default/check/FK/index 语义；如迁移时限制与线上不同须说明。
  不添加猜测的 ID generator、token hashing、默认 TTL、NOT NULL 或 soft-delete cascade。
- schema 执行必须显式事务；首次创建冲突失败，不用 IF NOT EXISTS 静默接受未知表。
  数据库 helper 的测试先初始化自己的测试库，再应用新 schema；无自动生产入口。
- repository 只提供内部 SQL 原语，显式 schema-qualified 表名/参数化值；不注册服务/路由。
  用户 nullable 字段不得当作完整 User DTO，NUMERIC 保持 decimal，所有 ID 为 int64。
  会话传入的是明确的 storage value，不接受该参数就声称外部 Bearer token 未经变换入库。
  ID、到期、撤销时间由调用方明确给出，不猜测旧策略；不把相邻 SQL 推成调用链。
- 数据库错误不得把密码 PHC、token、邮箱、连接串或 SQL 细节暴露给调用方。

## 验证

真实本地 18.6 + citext：列/默认/约束/索引对照、NULL、软删除唯一性、FK 物理 cascade、
软删除不 cascade、精确 numeric、bigint、timestamptz、会话过期读取；repository 合成读写、
并发唯一冲突、事务回滚和重连。还需覆盖错误脱敏、SQL 参数与 namespace 隔离。
测试结束验证进程退出；普通测试不启动数据库。旧登录/me/logout 与原实现对照仍为独立未完成门槛。
