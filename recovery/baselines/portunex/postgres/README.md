# 认证目录与定义线索（不是数据库恢复包）

## 新增定义采集（2026-09-13，独立于首轮快照）

`identity-definition.json` 与 `definition-provenance.json` 新增白名单三表的
30 列（含 default/collation）、14 个约束、17 个索引及 `public.citext 1.8` 元数据。
采集 SQL 为 `recovery/collectors/postgres/identity-definition.sql`，保持显式只读事务和短超时。
表达式只反编译为文本、不执行，先筛查和人工审阅；未读取业务行。

- 三个 ID、会话 expires_at 都无默认表达式；仍需应用生成策略证据。
- users.email、auth_sessions.token、api_keys.key_text 的唯一索引带 `deleted_at IS NULL`。
- 两个 user_id 外键为物理 `ON DELETE CASCADE`，不能据此假定软删除会级联。
- email/password_phc/role 的列非空标志为 false；role CHECK 的 admin/user 不禁止 NULL。
- NUMERIC(30,18) 不得映射成浮点；citext 的数据库 locale 和真实比较行为尚未验证。

这是定义元数据，不是完整备份或可直接上线的迁移：函数体、序列现值、真实凭据、
完整 SQLx 迁移正文及原认证业务行为未采集。原始 psql stdout 只在内存筛查，
来源文件明确不保留原输出；规范化文件有独立哈希，不能冒充原输出哈希。
下文保留首轮采集的范围说明，新增定义不改写旧快照。

## 首轮目录快照

`identity-inventory.json` 来自 2026-09-13 的显式只读 PostgreSQL catalog 查询，
执行文件为 `recovery/collectors/postgres/identity-inventory.sql`。
`provenance.json` 分别绑定 SQL 与规范化 JSON 的 SHA-256；不把格式化后的哈希
冒充原始 psql stdout 哈希，也不把采集器的声明当作独立审计证明。

连接使用现有容器内的本地 socket，未读取环境变量或密码。首次尝试的 `postgres`
数据库角色不存在，随后 `portunex` 角色成功连接同名库。最终查询使用
`READ ONLY`、`search_path=pg_catalog`、3 秒 statement timeout、1 秒 lock timeout，
单次 SELECT 后 ROLLBACK；返回 `transaction_read_only=on`。

## 本次得到什么

- public 下 36 个表对象、387 个有效列、28 个外键目录对象，与旧汇总一致。
- 另有 135 个索引目录对象、48 个 routine 目录对象、0 个非 internal trigger、0 个 policy。
- 三表 30 列：`api_keys` 11、`auth_sessions` 9、`users` 10。
- `users.email` 为 `public.citext`；`points` 和 `daily_recharge_limit` 为 `numeric(30,18)`。
- 三表 ID 均为 bigint；本轮未发现其 identity/generated/default 标志，生成算法仍未知。
- `auth_sessions.token`、`api_keys.key_text` 只证明字段名和类型存在，不能推出存储的是原文、摘要或密文。

这些计数的单位是 catalog 对象，不一定是独立业务实体：分区子对象、克隆约束
和无效索引可能被计数；routines 包括 pg_proc 中的普通函数、过程、聚合或窗口对象，
也可能包括扩展对象；非 internal trigger 为零不代表无内部约束触发器。
`column_not_null` 仅为列级 `attnotnull`，不包含 domain/CHECK 的有效非空约束；
`has_default` 仅为 `atthasdef`，并没有读取其表达式。

## 没有得到什么

本次未读取任何业务行、密码 PHC 值、Session/API Key 值、请求正文、环境配置、
`pg_authid`、sequence 当前值、默认/索引/视图/policy 表达式或函数体。
没有重新查询 `_sqlx_migrations` 行；28 条迁移数量仍来自旧时点记录。

完整 DDL、约束/索引/外键列顺序、扩展版本、sequence 依赖、函数/触发器定义、
迁移正文和真实认证行为仍需单独补齐。不得据此生成并运行生产 migration。

## 离线验证

```sh
PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=recovery/tooling \
  python3 -m unittest discover -s recovery/tests -t recovery -v
```

`tests/postgres/` 校验结构、顺序、类型、来源哈希与不可提升的范围声明；测试不执行 SQL。
新一次线上采集如有漂移，应审阅并新增记录，不能为了通过测试篡改采集值。
