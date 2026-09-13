# ADR-004：先恢复旧调用证据，再实现兼容认证

日期：2026-09-13。

## 目录边界

```text
recovery/baselines/portunex/static-wire-artifacts.json  # 白名单、哈希与来源，不含原包
recovery/contracts-wire/portunex/
  identity/observations.json                          # 登录/登出调用与鉴权 helper
  users/observations.json                             # me 与管理用户列表
  apikeys/observations.json                           # 用户/管理 Key 列表
  providers/observations.json                         # Provider 列表
  README.md                                          # 本切片证据和未验证边界
recovery/docs/portunex-identity-next-slice.md            # 下一兼容认证切片的规划
recovery/collectors/postgres/identity-inventory.sql      # 显式只读目录查询，无业务行/默认表达式
recovery/baselines/portunex/postgres/identity-inventory.json # 审阅后的结构线索，不是 DDL
recovery/baselines/portunex/postgres/provenance.json     # 采集 SQL/规范化结果哈希
recovery/tests/postgres/                               # 离线结构、标志、排序、来源一致性检查
recovery/tests/wire/test_repository_observations.py     # 已入库观察的metadata/schema引用，不冒充原件校验
openspec/changes/restore-portunex-wire-contracts/        # 本切片计划与验收
```

原始静态文件仅放仓库外私有目录，再用已有 evidence 工具保全到另一个全新目录。
只按已经观察到的显式文件名读取，不打包整个静态根（其中存在未处置的敏感归档）。
不执行原 JS，不复用用户浏览器 Cookie，不提交生产业务请求。

## 方法与安全边界

1. 使用已成功的 SSH 密钥和 `BindInterface=en0`，先核对普通文件、大小、SHA-256。
2. 有界下载指定文件，比较远端和本地哈希，先做可识别秘密检查再静态审阅。
3. 人工追踪 call-site → 公共 client → UI 消费，分别记录方法、鉴权、body、返回字段和分页。
4. observations 使用原文件 UTF-8 字节范围和片段 SHA-256；复用 wire validator，不写第二套校验器。
5. 保持原 catalog 的 discovered 和业务 unknown；`source_anchored` 不认证服务端权限/状态/完整 schema。
6. 数据库检查限显式只读事务和短 timeout；不得查询业务行、环境配置、密码/Token、`pg_authid` 或请求正文。
7. schema 信息不足时，规划独立 catalog 采集切片，不猜测 DDL，也不根据演示 Cookie 模型推断旧认证。

首轮数据库线索只包含 public 对象计数，以及 users/auth_sessions/api_keys 的列类型、
nullable、identity/generated/has-default 标志。默认表达式、函数体、索引表达式、
policy 和注释不在本切片读取范围；字段名/类型不够还原数据库。

## 本轮不做

不改变现有 Go/React 演示、生产路由或认证存储；不安装依赖，不替换 Bun，不推送，不部署。
不把前端读取字段当作完整服务端响应，也不把客户端管理员菜单当作鉴权证据。
后续生产级兼容实现须先明确此处恢复到的真实合同及仍待验证项。
