# 认证静态二进制线索（非运行时合同）

2026-09-13 对固定 ELF 做普通文件、大小和 SHA-256 校验后，通过只读 SSH 扫描。
未执行原程序、读进程内存、取业务行或提交原 ELF。只保留 8 个审阅过的
ASCII 范围（共 2,611 字节）及 20 个固定 marker 的计数/前 12 个偏移。
见 `identity-literals.json`、`provenance.json` 和固定范围 collector。

| 字节范围（右端不包含） | 观察 | 不能推出 |
| --- | --- | --- |
| 50992–51000；88487–88574；242671–242760 | `argon2id`、argon2 0.5.3 的构建路径字面量 | login 实际算法/版本/参数 |
| 124002–124007 | `sess_` 字面量 | token 长度、随机源、摘要/明文存储和接受格式 |
| 297200–297545 | 嵌入的 SQL 注释提及 snowflake ids | ID 生成实现、epoch、worker 位、所有表都用该算法 |
| 507602–509583 | 一组 auth_sessions SQL 字面量 | 哪些入口/顺序会调用、事务/锁、参数变换、有效期政策 |
| 1058640–1058705；1058771–1058802 | 管理员权限、session 认证相关错误文案 | HTTP 状态/错误 DTO/具体路由及 API Key 权限边界 |

SQL 片段显示：按 token 检索的查询带 `expires_at > NOW()` 与 `deleted_at IS NULL`；
有 last-used、到期更新、按用户清理过期会话、按 created_at 保留 10 条的软删除语句；
INSERT 显式传入 id、token、expires_at 和时间参数。
这不证明 token 原文直接进入 SQL，也不证明每次登录一定执行 10 条限制，
不能独立推出续期时长、登出撤销范围或并发规则。

Rust rodata 中相邻 ASCII 字符串可能无分隔符；片段亦可能截断注释。
marker 没匹配不等于功能不存在；数字偏移和哈希不是调用栈证据。
这里不把静态观察提升到 wire catalog 的业务验证状态。

离线测试验证范围、文本哈希、collector/artifact 来源链接和安全标志；
未保存原 ELF，因此离线测试无法重新证明原 ELF 到片段的采集过程。
如需复核源锚点，只能在授权环境重新运行同一固定 collector 并比较输出。
