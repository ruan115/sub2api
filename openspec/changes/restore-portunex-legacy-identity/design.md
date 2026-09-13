# ADR-005：证据约束的认证模块与隔离测试

## 开发前文件结构

按职责建立目录，不先铺设空模块；旧 synthetic demo 保持不变。

```text
recovery/collectors/postgres/identity-definition.sql  # 只读白名单表的定义元数据
recovery/collectors/binary/                          # 固定二进制的有界静态片段
recovery/baselines/portunex/postgres/                 # 新采集文件，不改写历史快照
recovery/baselines/portunex/binary/                   # 已审阅片段/哈希，不提交原程序
recovery/tests/postgres/                             # 采集范围和来源检查
recovery/tests/binary/                               # 离线、合成的提取测试
backend/internal/portunex/identity/password/         # PHC 解析、参数政策、限流、验证
backend/internal/portunex/identity/session/          # 后续：token 生命周期
backend/internal/portunex/identity/repository/postgres/ # 后续：认证 SQL
backend/internal/portunex/users/repository/postgres/ # 后续：精确 user 读取
backend/internal/portunex/platform/postgres/         # 后续：独立测试库边界
backend/internal/portunex/migrations/                # 后续：恢复库迁移，不冒充原 SQLx SQL
backend/internal/portunex/identity/transport/legacyhttp/ # 后续：login/logout 旧 DTO
backend/internal/portunex/users/transport/legacyhttp/ # 后续：me
backend/cmd/portunex-compat-local/                   # 后续：显式本地入口
```

## 证据和实现边界

1. catalog 只读取 users/auth_sessions/api_keys 的列默认值、约束、索引、扩展元数据；短超时、显式只读、固定 search_path。不读取业务行、序列现值、函数体、配置或凭据。表达式先做秘密筛查和人工审阅再入库。
2. 固定路径、普通文件、大小和 SHA-256 校验后只做静态二进制分析。字面量/依赖名只说明存在，不说明运行时分支；SQL 字面量也不能独立证明调用路径。原 ELF 不执行、不提交。
3. 先实现独立 PHC 验证能力；已有 x/crypto 足够，不加密码库依赖。允许的算法和本地资源政策必须显式声明。不能把本地上限当原服务器参数。算法未证实时不得接到旧 HTTP 路由或接受真实记录。
4. PHC 在 KDF 前严格解析：总长、算法/版本、重复/未知参数、整数溢出、Base64、salt/hash 长度、内存、工作量和并行度。未知/超限 fail closed，不降级、不截断、不自动 rehash。
5. 密码计算同步执行，有界准入，满载立即拒绝；context 不能中断 x/crypto KDF，所以直到工作实际结束才释放槽。取消后不能建会话。错误不带密码、PHC、token。
6. 本地数据库不存在时，不借用线上 DB，不启动整个用户 Docker 栈。sqlmock 仅证明 SQL 交互，不算 PostgreSQL 语义验证；真实库门槛显式保持未通过。
7. 独立恢复库的 namespace、bigint、NUMERIC(30,18)、citext、软删除和索引语义经合成数据验证后才能接 transport。原 ID/token/到期/DTO 未知时不写猜测的兼容默认值。

## 验证和阶段交付

公开密码向量、畸形/超限 PHC、取消/并发/race/fuzz；离线证据来源一致性测试。
数据库、HTTP 和原实现对照分别记门槛，不用一次单元测试绿灯代替全部认证兼容。
每个有独立价值的切片先 review、保全工作区、测试，再仅本地提交。
