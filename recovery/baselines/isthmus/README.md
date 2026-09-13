# isthmus text evidence allowlist

`manifest.json`只列第一阶段保全的9个静态文本原件，source-root由调用者明确提供。当前来源为仓库外私有分析目录，不包含整个目录、ELF、.pkg、ZIP、账号home、环境文件或私钥。

最初候选包含完整 `isthmus.bundle.mjs`，筛查命中了带用户名/密码 URL 的规则（1处，字节偏移893477），因此本阶段显式排除该文件，不输出匹配内容、不弱化检测、不修改原件。该命中可能是嵌入模板，尚未完成专门审查，不能据此断言存在真实凭据；原始大小和SHA仍单独登记在 `../online-stack/deployment.json`。保全成功只覆盖最终白名单9项。

这些哈希来自2026-09-12静态提取；关键部署文件在2026-09-13只读复核一致。保全只证明所列文件与基线一致，不证明所有77个实例同版本，也不构成完整部署备份。

原件不能被测试或生产代码import/执行。可维护的新协议代码位于`execution-plane/isthmus-runtime/`。
