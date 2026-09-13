# React 恢复子应用：第一切片结构

状态：本地 synthetic mock UI 骨架，不是生产 API 兼容实现。保留已观察的页面 URL；不修改现有 Vue 应用。

## 模块和依赖

```text
src/app/                  # 路由、依赖注入、会话门禁、外壳、入口样式
src/modules/identity/     # 登录/登出/会话模型与页面
src/modules/users/        # 用户列表及本模块模型
src/modules/apikeys/      # API Key 列表；只显示合成 key hint
src/modules/providers/    # Provider 列表及本模块模型
src/shared/               # 小型列表状态组件、页面导航、ports通用分页/错误
src/mock/                 # 明确隔离的合成数据及本地模拟 ports
src/adapters/             # 显式启用的本地Go演示HTTP适配；不是旧合同
tests/                    # 注入 ports 后的权限、会话和界面交互测试
```

页面只依赖注入的 ApplicationPorts，不直接 fetch、读取服务器配置或引用恢复 bundle。应用内的 UI 类型不是旧 HTTP DTO；后续已核实合同经独立 adapter 转换。默认没有网络 API 调用；`VITE_RECOVERY_BACKEND=go` 只启用 `/__recovery__/v1` 的同源 cookie 演示适配，由 Vite 固定代理到 `127.0.0.1:18090`，页面必须从回环地址访问。每个模块负责解析自己的响应行；共享解析仅包含分页和基本类型。HTTP 响应按 unknown 校验字段、枚举及时间，异常只显示固定错误，不回显原始响应。

## 本切片页面和视觉规则

- `/auth`、`/dashboard`、`/dashboard/users`、`/dashboard/api-keys`、`/dashboard/providers`。
- 采用克制的管理后台布局：浅底、细边框、固定导航、清晰表格和状态标签；保留 Portunex 名称，不引入营销品牌或外部字体/图片请求。
- 每页始终标明“本地演示 · 合成数据 · 未连接线上”；登录表单仅验证公布的演示凭据。
- 普通用户只能访问 dashboard；管理菜单不显示，直接访问列表 URL 显示无权限，且不调用列表 port。
- 默认 mock 会话仅存在页面内存；浏览器刷新后重新登录。显式 Go 模式使用本地 HttpOnly cookie，可在刷新后恢复未过期会话。两模式过期自动进入登录页，登出立即清理页面会话。
- 三个列表统一覆盖加载、空数据、请求失败、未授权和成功态；演示状态选择器允许可视验证。

## 交付门槛

React/ReactDOM 19.2.1；Vite 5.4.21、TypeScript 5.6.3、Vitest 2.1.9 和 jsdom 24.1.3 来自已安装版本证据。项目独立安装精确依赖并提交 lockfile，不跨项目导入 node_modules。

需通过 typecheck、unit 和 production build。开发/预览监听仅回环地址。没有生产鉴权、写入、支付、Provider 授权、远程数据或部署；这些不能由 mock 页面完成来推断。
