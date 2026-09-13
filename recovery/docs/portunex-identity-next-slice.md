# 下一切片规划：旧 Bearer 认证兼容闭环

日期：2026-09-13。状态：证据驱动规划；尚未实现旧生产认证。
前置证据：[首批 wire 观察](../contracts-wire/portunex/README.md)、
[认证表结构线索](../baselines/portunex/postgres/README.md)。

## 1. 已确认的客户端事实

| 入口 | 静态客户端调用/消费 | 不能据此认定 |
| --- | --- | --- |
| `/portunex/auth/login` | POST，表单 email/password 对象，经 JSON client；读取返回体 token/user | 真实错误码、服务端密码规则、全部响应字段、token 生成和有效期 |
| `/portunex/users/me` | GET，直接将返回体当 user；从其 role 决定管理入口预加载 | 服务端鉴权完整规则、完整 User DTO、是否同时支持其他认证方式 |
| `/portunex/auth/logout` | POST，无显式 body；请求通过共享 Bearer client，成功后清本地 token | 服务端撤销范围、幂等/失败响应、cookie 行为 |
| 管理用户列表 | GET，offset/limit，按条件传 id/email/role；消费 users/total | 服务器默认排序、边界、权限和完整分页 DTO |
| 用户/管理 Key 列表 | 分别调用两个 GET collection；消费 api_keys/total | 管理员和用户的所有字段差异及完整 Key 生命周期 |

旧前端把 token 放在 `localStorage` 的 `portunex_token`，请求拦截器添加
`Authorization: Bearer ...`；客户端用 `sess_` / `ptx_` 分类字符串。
不能把演示 Cookie `/__recovery__/v1/session` 直接映射成旧认证，
也不能仅凭这些前缀就允许任意字符串通过服务端鉴权。

`localStorage` 是旧网页的客户端行为，不要求新后端明文存凭据。
后端可以在保持已确认 HTTP 合同的前提下使用摘要/加密保护；但旧 token 能否续用、
API Key 是否可用于管理登录等兼容问题必须先验证，不得默认双重接受。
共享 Axios 仍含浏览器 XSRF 逻辑；Bearer 证据不等于证明“完全无 Cookie/CSRF”。

## 2. 必须先补的未知项

1. 认证相关完整约束/索引/default、citext 扩展与大小写规则；ID 生成算法。
2. 原密码 PHC 算法、参数范围和校验规则：优先分析授权二进制/算法线索，之后只用合成 PHC 验证，不读取真实密码记录。
3. Session 与 Key 的服务端格式、摘要/加密方式、到期、软删除、last-used、撤销及并发规则。
4. login/me/logout 的完整响应/错误/鉴权合同，尤其普通用户、管理员与 API Key 的边界。
5. 旧业务 ID 与 Sub2API ID 的 namespace 映射；资金维持 NUMERIC/decimal 语义，不转浮点。

本轮 public catalog 计数不是完整 DDL；字段叫 token/key_text 不能证明存储算法。
需要真实业务请求或一次性数据副本才能确认的项，必须另行明确测试账号/合成依赖
和费用/操作边界，不能用生产账号冒烟或静默导出真实数据来填补。

## 3. 开发前的模块结构

只在需要时创建有实现的目录，保留现有演示；不把所有逻辑塞进 `app/`：

```text
backend/internal/portunex/
  platform/postgres/                    # 独立恢复库连接、事务边界
  identity/password/                    # 已确认 PHC 算法与资源上限
  identity/session/                     # token 生命周期、撤销、时钟
  identity/repository/postgres/          # 专用 SQL，与 transport 解耦
  identity/transport/legacyhttp/         # login/logout 的旧 DTO 和鉴权入口
  users/repository/postgres/             # 认证闭环需要的用户读取
  users/transport/legacyhttp/            # users/me，管理列表在后续切片
  migrations/                           # 新恢复库的迁移，不冒称原 SQLx 历史
backend/cmd/portunex-compat-local/        # 显式本地入口，默认不启动后台任务
recovery/tests/compat/identity/          # 合成旧合同/并发/权限对照 fixture
```

正式目录名称和 package 依赖在实现 change 中复核；本轮不创建空生产模块。
React 演示继续用 mock/Go 演示 adapter，不提前换成未经验证的旧鉴权实现。

## 4. 分步实施与验收

- I0：补证据并冻结上述三条旧接口；每项未知有来源/owner/验证方法。
- I1：独立测试库的 schema/migration 与 repository，用合成记录验证引用、唯一性、数值/时间语义。绝不指向现有生产库。
- I2：PHC 验证与有界 token 生命周期，禁止未知算法降级到演示 SHA-256；不记录密码或完整 token。
- I3：接入 login → me → logout，旧 DTO 留在 legacyhttp；继续保留恢复域隔离，不改变 Sub2API/CCMAX 认证。
- I4：重复/并发登录登出、过期/撤销、错误密码、普通用户越权、跨域同值 ID、重启后会话行为与日志脱敏回归。
- I5：用合成账户/fake 依赖对照旧实现；只有比较结果成立才把对应合同推进为已验证，最后形成 review 和独立本地提交。

注册、验证码/magic-link、OAuth/OIDC、Key 生命周期、Provider 管理、积分/支付、
MCP/gRPC/VM 接线不塞进此认证切片。它们继续保留在总计划的独立阶段。

## 5. 确认门槛与风险

本次采用 `web-reverse-master` 的“证据 → 方案 → 确认 → 还原”流程。
在进入这一新的旧认证实现切片前确认本方案；没有完整证据的行为保持未实现，
不以强制重新注册、Cookie-only、硬编码过期时间或猜测 ID 生成器宣称兼容。
即使本地通过，也不自动开启线上执行开关、升级 Bun、推送或部署。
