# 旧前端静态资源保全

本工具只读取 `216.106.185.119:/opt/gateway/public` 的明确前端资源，不登录网页、不访问业务 API、不运行旧 JS、不部署或改服务器文件。

Python 3 标准库，复用仓库现有 no-follow 私有文件操作、内容筛查和有界子进程实现。不下载依赖。源码、清单、采集结果和可执行应用是四件不同的事。

## 两阶段使用

从仓库根运行；下面的绝对路径须替换成用户指定的实际路径。所有 destination 必须不存在，其父目录已经存在且在 Git 仓库外。

```sh
PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=recovery/tooling python3 recovery/collectors/frontend/capture.py inventory \
  --identity-file /absolute/ssh-key --interface en0 \
  --destination /absolute/private-parent/frontend-inventory
```

此阶段传回文件名、大小和 hash，不传回内容。人工审阅 `inventory.json` 后，才执行：

```sh
PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=recovery/tooling python3 recovery/collectors/frontend/capture.py capture \
  --identity-file /absolute/ssh-key --interface en0 \
  --reviewed-inventory /absolute/private-parent/frontend-inventory/inventory.json \
  --destination /absolute/private-parent/frontend-capture

PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=recovery/tooling python3 recovery/collectors/frontend/capture.py verify \
  --destination /absolute/private-parent/frontend-capture
```

SSH 只执行工具内固定的 Python 只读采集程序。显式密钥和接口、严格 host key、BatchMode、无 agent/端口转发，不读取用户 SSH config，不交互请求密码。来源端同样拒绝软链接和特殊文件；capture 的路径/大小/hash 集合须与已审阅 inventory 完全相同。

## 原件分类，不是发布批准

- `files/`：通过现有 UTF-8 和可识别秘密模式筛查的文本原件，仍不允许直接 import/运行/发布。
- `quarantine/`：命中可识别秘密形状、无效文本或图像签名不符的原件；只保全，不输出匹配原文，也不绕过通用 evidence 工具的拒绝策略。
- `reference/`：仅通过基础签名检查的图片/纹理参考原件；未解码、未做全面图片内容审核，不能声称已通过文本秘密审核。
- `inventory.json` 与 `receipt.json`：完整性、分类和执行/发布未批准标记。回执最后写入；失败可能留下不完整私有目录，不自动覆盖或清理。

目录 0700、文件 0600；verify 重算每项分类、大小、hash，核对完整文件集合和权限。清单模式的 verified 只验证元数据，不表示文件已经下载。采集模式的 verified 也只证明这份私有副本与记录一致，不是签名存证或异机备份。

固定范围是 `index.html`、平铺 `assets/*.js/css` 与代码中列出的准确图标/图片/地球纹理路径。不读取已有 `web.zip`、`gateway.tar`、`__MACOSX`、配置、日志或数据库。这个范围不是“服务器所有文件”；未发现的外部依赖、source map、TSX、构建配置仍可能缺失。

CLI 只输出数量、字节数、分类及固定错误，不输出原件。错误时不要直接打印子进程内容排查；先使用合成测试与元数据定位。

## 离线验证

```sh
PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=recovery/tooling python3 -m unittest discover -s recovery/tests -t recovery -v
```

测试使用临时合成文件；没有默认 SSH、生产下载、真实账号或外部模型调用。当前范围与后续 UI 模块结构见 [CCMAX 恢复计划](../../../docs/plans/ccmax-frontend-recovery-v1.md)。
