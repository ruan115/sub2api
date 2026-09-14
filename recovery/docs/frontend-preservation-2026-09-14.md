# 旧版前端已取回：CCMAX 外观恢复基线

日期：2026-09-14。U0 清单内原件保全已完成；U1 仅开始样式静态取证，**还没有重建/替换 CCMAX 界面，也没有视觉一致性验收**。

当前目标见 [CCMAX 恢复计划](../../docs/plans/ccmax-frontend-recovery-v1.md)。用户已确认希望沿用 `216.106.185.119` 的旧版前端，而不是继续采用新建的演示界面。Portunex 业务后端开发暂停。

## 实际取回了什么

来源固定为 SSH `root@216.106.185.119:/opt/gateway/public`，采集时间 `2026-09-14T06:28:25.579766+00:00`。

| 类型 | 文件数 |
| --- | ---: |
| JavaScript 发布文件 | 135 |
| CSS | 2 |
| HTML 页面入口 | 1 |
| SVG Logo | 1 |
| favicon、PNG/JPG 图片、AVIF 纹理 | 9 |
| 合计 | 148 |

总大小 **4,666,514 字节**。两次独立 SSH 读取分别形成清单和采集结果，准确路径/大小/SHA-256 完全一致；独立本地 verify 再次通过。此前保全的 11 个 JS 均与本次同名文件一致（11/11）。

历史记录的“150 个文件”不是本轮保全分母；本轮按前端白名单精确保全 148 项，不把根目录的两个压缩归档算作前端资源。未读取 `web.zip` 或含配置/日志/数据库的 `gateway.tar`，未读取 `__MACOSX`。

元数据：[文件清单与来源](../baselines/portunex/frontend/preservation-2026-09-14.json)。没有原 JS/CSS/图片进入 Git。

本地原件目录（仓库外，目录 0700 / 文件 0600）：

```text
/Users/ruanyang/My-project/api/z/sub2api-recovery-private.kbZovy/frontend-recovery-20260914.7EJ7Es/
  inventory/       # 第一次读取的纯元数据
  capture/
    inventory.json
    receipt.json
    files/         # 138 个通过文本策略的参考原件
    quarantine/    # 1 个命中可识别秘密形状的 JS
    reference/     # 9 个仅作基础签名检查的二进制图像
```

隔离文件为 `assets/_dashboard.providers-ABk0JmwU.js`。匹配原文未输出、未纳入 Git；尚未判断是真实凭据还是嵌入示例，不据此宣称发生泄漏。该文件仍可完整性保全，但不得因本次下载而自动解除隔离或直接 import。

## 已有的视觉依据

- 原页面 HTML 标题为 `DIDI`；Portunex 是已识别的内部项目/接口名称，不应把新建演示页当旧页面。
- 主 CSS `assets/root-C2_MaqGk.css`：162,108 字节；首页 CSS `assets/_index-Dwjc03si.css`：11,583 字节。
- 从主 CSS 静态解析得到 22 个选定的主题声明字节锚点，包含明暗背景/前景/卡片/边框/侧栏、圆角 `.625rem` 和 `Inter` 字体声明，见 [主题来源记录](../baselines/portunex/frontend/theme-observations.json)。
- 同时保留 RGB fallback 与 `@supports (color: oklab(0% 0 0%))` 内的 OKLCH 声明，不能只取最后一次字符串出现当作所有浏览器最终样式。
- HTML 存在 Google Fonts 的 preconnect 和 stylesheet 引用；本次没有抓取外部字体。字体声明存在不等于离线已有字体文件；后续预览还需明确本地字体与外联策略。
- 页面框架、Provider 管理、图表、日期筛选、弹层、主题切换、Logo 和地球动画纹理已经有参考产物；这不等于其全部状态、字段和功能已复刻。

本次用已安装的 PostCSS 解析器读取 CSS AST，无插件、不加载 stylesheet、不运行 JS、不创建浏览器、不发起字体或业务请求。下一阶段按照旧版视觉恢复 CCMAX 页面，不因通常的设计偏好换掉旧字体、颜色或布局。

## Review 与验证

- 文档范围独立 review：未发现阻塞项，明确保留 CCMAX 存量功能并暂停 Portunex 业务重建。
- 采集器主线程与独立 review：发现并修复 `_dashboard` 开头文件名被拒绝的问题；本地/远端校验都有回归测试。固定来源、禁代理/转发、no-follow、有界输出、准确清单、隔离分类和私有权限已检查。
- Python 全量 **157/157** 通过（原有 132 + 新采集器 21 + 仓库基线 4）。不把测试数量当作界面完成比例。
- `web-reverse-master` 离线 selftest 7/7 通过；属于工具自测，不是旧站行为或外观验收。
- 真实采集和独立 verify：148 项、4,666,514 字节，138 文本参考 / 1 隔离 / 9 二进制参考。执行与发布批准均为 false。

复核原件：

```sh
PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=recovery/tooling python3 recovery/collectors/frontend/capture.py verify \
  --destination /Users/ruanyang/My-project/api/z/sub2api-recovery-private.kbZovy/frontend-recovery-20260914.7EJ7Es/capture
```

## 仍然没有完成

原 TSX/组件工程、source map、依赖锁和构建配置没有恢复；生产 JS 不是可维护源工程。尚未安全运行旧前端、取得对照截图、重建 CCMAX React 页面、适配其 API、验证响应式/交互或更改 `go:embed` 入口。

下一步是 U1 视觉基线及隔离预览方案，再按模块实现总览、账号列表、账号详情。不是 Portunex 登录/计费开发，也不代表 execution-plane 已可上线。没有连接生产业务 API、导出数据库、部署、重启或替换系统 Bun。

原件目前只有本机私有副本，Git 也仅本地提交；均不构成异机灾备。推送仓库和加密异机保存原件应作为单独的备份动作处理。
