# Portunex 本地恢复演示

独立 React 子应用；不替换现有 Vue。当前页面是新的 UI 骨架，不是已验证的旧 HTTP 接口兼容实现。模块规则见 [docs/structure.md](docs/structure.md)。

```sh
cd portunex-web
npm ci --ignore-scripts
npm run dev
```

打开 `http://127.0.0.1:4178/auth`。默认完全使用页面内 synthetic mock，无 API 请求；演示会话只保存在内存。使用页面按钮填入管理员或普通用户账号。页面顶部可切换列表的 loading/empty/error/unauthorized 状态。

| 演示角色 | 邮箱 | 公开的合成密码 |
| --- | --- | --- |
| 管理员 | `admin@example.invalid` | `Demo-admin-2026!` |
| 普通用户 | `member@example.invalid` | `Demo-member-2026!` |

请勿输入真实凭据。普通用户仅显示工作台；三类管理列表有路由门禁与 mock port 双重限制。

## 显式连接本地 Go 演示

另行启动仓库的 `backend/cmd/portunex-demo`（监听 `127.0.0.1:18090`），然后：

```sh
VITE_RECOVERY_BACKEND=go npm run dev
```

页面将明确显示“本地 Go 演示”。Vite 只将 `/__recovery__` 代理到上述固定回环地址；通过同源 cookie 调用 `/__recovery__/v1` 演示合同。没有远程 base URL 配置，也不会访问 `/portunex/*` 生产接口。HTTP ports 不属于旧合同验证成果。

`npm run preview` 只预览默认 mock 构建，不提供 Go API 代理。源码/构建均不包含生产账号、Token、数据库或原服务器配置。

## 验证

```sh
npm run typecheck
npm test
npm run build
```

已知边界：没有创建/编辑/删除业务、支付、OAuth、Provider 上号、真实数据库或部署。会话过期、普通用户权限和列表状态有合成测试；不能由此推断线上兼容和全系统恢复完成。
