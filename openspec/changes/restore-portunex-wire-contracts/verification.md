# 首批旧接口与认证结构证据验收

日期：2026-09-13。起点：`030d005`，分支 `codex/claude-execution-plane-v1`。
本切片是只读来源恢复及下一步规划，不是旧认证开发或整套兼容验收。

## 结果

| 检查 | 结果 | 范围 |
| --- | --- | --- |
| SSH | PASS | 同一密钥配合 `BindInterface=en0` 成功；不修改 SSH/隧道配置 |
| 指定静态文件 | PASS（12 份哈希一致） | 仓库外下载；仅显式命名资源，无静态根归档/环境/业务日志读取 |
| 可用文本保全 | PASS（11 份，163,689 字节） | 完整文件筛查、私有保全、独立 verify-preserved |
| Provider 页面包 | EXCLUDED | 两处凭据形状 URL 命中，原件不动、不曝光匹配值、不绕过规则；其页面分页未知 |
| 逐模块 wire 校验 | PASS（source only） | 27 条观察、60 锚点、7 API 记录；原下载目录和独立保全 files/ 均通过 |
| PostgreSQL 目录查询 | PASS（metadata only） | READ ONLY、pg_catalog search path、3s/1s timeout；三表 30 列；SQL 和规范化 JSON 分别有 SHA |
| 独立 review | PASS（已修正发现） | wire 描述中 role 未 trim 的 P3 修正；SQL 搜索路径、计数和列标志歧义均按审查收紧 |
| `make -C recovery check-demo` | PASS | Python 111、Bun 111、React 42 项；Go 恢复域 race/vet、React 类型/构建、正常 loopback HTTP/WS 及关闭 |
| 技能工具自测 | PASS（7/7） | `web-reverse-master/scripts/selftest.py`，仅工具自测，不算旧业务验收 |
| OpenSpec CLI | NOT RUN | 本机未安装命令；未自动安装依赖，不冒称严格 CLI 校验通过 |
| 旧认证/完整 DB/生产兼容 | NOT IMPLEMENTED / NOT VERIFIED | 未查询真实用户/密码/Key/session 值，未登录、发模型请求、支付或迁移 |

## 关键纠偏

旧前端消费 `token` / `user`，将 token 存入 `localStorage.portunex_token`，
共享 client 添加 Bearer。`users/me` 直接返回前端消费的 user；没有证据支持把
演示 `/__recovery__/v1/session` Cookie 模型当旧接口。Bearer 证据不排除浏览器 XSRF/Cookie。

三表为 users/auth_sessions/api_keys，列数 10/9/11；public 36 表、387 列、28 FK
与旧汇总一致，但全量约束/default/schema 尚未获得。`public.citext`、`numeric(30,18)`
是新的类型证据；bigint ID 的生成以及密码/token/key 的存储算法仍未知。

新增 9 项 PostgreSQL artifact 回归和 5 项 wire metadata 回归。后者不访问私有 JS，
只校验文档/schema/引用/边界，绝不代替需要原始字节的 `wire verify`。
本切片未修改既有 discovery catalog；所有实际校验仍是 `business_verification=false`。

## 私有材料与可复核方式

- 初始下载：`/Users/ruanyang/My-project/api/z/sub2api-recovery-private.kbZovy/portunex-static-20260913.vOikF6/`，含 12 份原文件，其中 1 份被排除。不是可公开材料。
- 已保全白名单：`/Users/ruanyang/My-project/api/z/sub2api-recovery-private.kbZovy/portunex-static-evidence-20260913/`，11 份原件，receipt/manifest 独立验证通过。
- WIP 留档目标：`/Users/ruanyang/My-project/api/z/sub2api-recovery-private.kbZovy/workspace-wire-contracts-20260913/`，提交前新建并验证，原快照不覆盖。
- 原始 JS、命中材料、隔离 Bun、真实数据、私钥不进 Git；Git 仅保存非秘密观察片段/哈希、目录元数据、采集 SQL、测试和规划。

使用仓库现有 `evidence verify-preserved` 与 `wire verify` 复跑，参数见
`recovery/contracts-wire/portunex/README.md`。快照只覆盖源码 WIP，不覆盖私有资产，
同机保全仍不是异机灾备。本轮只形成一个独立本地阶段提交，不 push 或部署。

## 后续门槛

下一方案为 `recovery/docs/portunex-identity-next-slice.md`：补足认证规则/schema →
独立测试库 → PHC/token 实现 → login/me/logout 对照，按模块分别组织。
依据本轮采用的 `web-reverse-master` 方案确认流程，先确认该新方案再进入代码还原。
未知项不以猜测、强制重新注册或降级安全模型填补。

系统/项目/CI Bun 仍为 1.3.9；原异常停机门槛仍未关闭。没有新 Linux/gRPC/Redis
集成、原二进制隔离运行、生产认证或部署证据；不能把本地 PASS 合并为可上线。
