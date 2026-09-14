# 原版前端本机隔离预览

日期：2026-09-14。用户明确要求“在本地部署启动一下给我看看”。本次仅授权本机视觉预览，不部署服务器、不连接真实 API，也不表示旧 TSX 或业务后端已经恢复。

## 运行前确定的方案

1. 读取此前保全的原件；准确校验仓库 pin、私有清单、回执和选用资源 hash。原件不改写、不复制进 Git，不在服务器端执行发布 JS。
2. 专用 Python 标准库预览服务：`127.0.0.1:18091` 显示永久预览说明和导航；`127.0.0.1:18092` 在受限 iframe 内承载原站 SPA。两者不同 origin，无线上代理、数据库或真实凭据。
3. 原 JS 硬编码 `http://216.106.185.119:8080`。仅在预览内存副本替换为本地数据端口，记录替换计数；保留原 HTML/ReactRouter 启动和 CSS，使原首页、管理框架与总览可展示。
4. HTML 与 React root 的 Google Fonts 引用一致改为本地空 CSS 和本地 preconnect；使用现有字体回退，明确外观可能有细小差异，不自动外联下载字体。其他外部 link 删除。
5. guard 在固定 entry JS 的内存副本前置，不添加 HTML head 节点，避免破坏原文档 hydration。它只在本地 origin 种合成 token，阻止外链、弹窗、表单提交、外部 fetch/XHR 和业务写操作。ESM 依赖早于 guard 执行，所以 CSP、iframe、浏览器网络拦截与服务端边界独立生效；服务端拒绝 POST/PUT/PATCH/DELETE/OPTIONS 等，不读取请求正文。
6. 精确 CSP、Host/Origin 校验、无目录浏览、无路径穿越、no-store、no-referrer、无 worker/对象/外部 frame。专用浏览器会话进一步拦截全部非两个本机 origin 的请求和 WebSocket，不沿用用户登录态。
7. `/portunex/users/me`、用户/平台 stats、Provider 列表/错误类型/窗口总览为明确合成 fixture；其他未实现 GET 返回预览专用错误。绝不把 mock 当作真实后台已兼容。
8. 默认 Provider 页仍使用占位。两次独立静态检查确认原隔离模块的两处敏感形状只位于 `placeholder` 字符串，独立复核 AST 除这两处 Literal.value 外没有变化。可显式运行 `prepare_provider.py`，按固定原件/字节片段/候选 hash，仅替换这两段完整示例文案，在新的私有目录生成派生副本。服务可选加载该副本，不读取或改写原 quarantine；原保全回执的执行批准标志不变，派生副本也不获生产批准。

## 文件结构

```text
recovery/preview/frontend/
  server.py                 # loopback 预览入口、准确资源映射和响应边界
  fixtures.py               # 明确合成的只读数据
  prepare_provider.py       # 固定字节/hash 的示例文案脱敏派生，不改原件
  guard.js                  # 预览页面侧限制，不是生产鉴权实现
  browser-guard.js           # 专用 Playwright 会话的请求/WS 拦截
  browser-check.js           # 页面、演示数据及请求拒绝检查
  playwright-cli.json       # 新浏览器会话/无 service worker
  README.md                 # 启动、停止、限制说明
recovery/tests/frontend/test_preview.py
recovery/tests/frontend/test_provider_preview.py
output/playwright/legacy-frontend/   # 忽略的截图/浏览器测试产物
```

这不是 `ccmax-manager/ui/` 的可维护 TSX 重建；不修改 CCMAX 当前 `web/`、`go:embed` 或 Sub2API 前端。运行副本中的路由、卡片数字和用户均为视觉演示，不新增 Portunex 登录/权限/计费实现。

## 验证要求

- 原首页和管理总览可加载，预览说明始终可见，明暗主题与页面导航可检查。
- 独立浏览器内非本机请求/WS 被拒绝；禁止上游地址、外部字体和业务写操作。用无网络的失败探针和 HTTP 边界测试验证，不用真实账号测试。
- 截图验证页面可见内容与未实现页的明确占位。报告页面错误/字体回退/模拟数据限制，不能只检查 HTTP 200。
- 服务仅监听两条 loopback 地址，启动失败不接管既有端口；停止仅终止本次创建的进程。

## 本轮验收

- 地址：`http://127.0.0.1:18091/`，受限旧 SPA 为 `http://127.0.0.1:18092/`。两者只绑定 `127.0.0.1`。运行原版首页、管理总览、Provider/账号列表；列表只有一条明确标注的合成 Claude Code 账号。
- 独立 headed Playwright 非持久会话 `ccmax-legacy-preview`。首页、账号页深色/浅色、总览导航和主题保留经截图与可见文本检查。最终浏览器 console 为 0 errors / 0 warnings。早期字体 CSP 与 hydration 报错通过一致字体替换及 entry 注入修复，没有隐藏错误。
- 浏览器 QA：总览与合成用户可见；外部 fetch、本地 POST、WebSocket、弹窗、beacon 拒绝；资源 origin 只有本地 `18092`，iframe sandbox 为 `allow-scripts allow-same-origin`。这是隔离预览检查，不是后端功能验收。
- `make -C recovery test`：191 项通过。覆盖资源 pin、派生文件校验、源文件不变、HTTP Host/Origin/路径边界、禁写、fixture 和字体/entry 变换；JS 语法检查通过。
- 独立代码 review 未发现默认端口预览阻塞问题；已补充浏览器脚本只适用默认端口的说明。
- 原版账号页截图：`output/playwright/legacy-frontend/.playwright-cli/page-2026-09-14T06-59-48-672Z.png`；浅色截图 `page-2026-09-14T07-00-30-637Z.png`；总览截图 `page-2026-09-14T07-01-02-264Z.png`。截图为忽略的本机产物，不进入 Git。

未实现：真实登录/计费、真实账号读写、Token 刷新、代理连通、上游执行、其他业务页 API。侧栏仍是原发布版导航，不表示所有页面可用，也不改变只维护 CCMAX 的后续开发范围。字体为本地回退；未做全路由或全尺寸兼容验收。停止请对本次预览命令 Ctrl-C；若从另一终端停止，先核对监听 PID 和完整命令，仅终止该预览进程。
