# ADR-003：静态wire观察锚定

在实现前记录，沿用ADR-001/002。原始资料不执行、不进Git，观察文件也不自动变成已验证API合同。

```text
recovery/tooling/recoverykit/wire/
  schema.py          严格观察文档结构及限额
  anchors.py         原始文件哈希、UTF-8字节片段及范围校验
  validation.py      关联已有catalog记录、组合校验及安全摘要
  errors.py          固定错误类别
  __init__.py        有限公开入口
recovery/tests/wire/  合成源文件、篡改/边界/秘密/引用测试
recovery/docs/wire.md 文档结构与人工审核边界
recovery/tooling/recoverykit/cli/  仅增加命令接线
```

命令接受四个显式输入：观察JSON、已审阅artifact manifest、私有source-root、既有catalog-root。复用evidence的no-follow读取、文件大小/哈希校验、秘密筛查及重复JSON键拒绝。验证过程只读，不复制源文件，不运行JS、不访问网络、不改catalog状态。

观察JSON包含版本、文档ID、artifact manifest ID和有界observations。每条观察有稳定ID、已有catalog API记录ID、与catalog一致的path、受限aspect、人工statement，以及一个或多个anchor；anchor记录manifest内的相对文件路径、UTF-8字节区间`[start,end)`、片段SHA-256及片段必须包含的literal。不是字符偏移；不能截断多字节字符。重复/未知字段、无效ID、越界、过大片段、未知source或catalog引用都失败。

这是**来源锚定**而非语义证明：hash与位置只证明某段原文确实存在，不能证明人工statement为真或旧客户端已兼容。method/body/envelope等判断仍需人工静态复核和后续对照测试。CLI只输出计数、`source_anchored`及`business_verification=false`，不打印statement、源代码、路径或失败原文。没有生产输入时只做合成测试，不给catalog打勾。

本轮SSH只试一次，仍在KEX阶段关闭；不扫描其他端口、不绕过限制。Bun修复的代码/版本/测试证据单独记录，不与wire工具的通过状态混为一谈。生产服务、数据库、现有5.5c、Vue/React页面均不变。

## 独立审查补充：catalog读取边界

审查用合成材料复现：旧catalog loader的`resolve/read_text`可跟随祖先软链接，并有目录检查后文件换链窗口；该依赖会削弱wire的no-follow边界。因此本切片包含对`tooling/recoverykit/contracts/`读取层的定向加固，不改变catalog业务结构或状态语义。文件系统能力按需独立为`contracts/filesystem.py`，安全测试单独放`tests/contracts/test_loader_security.py`。

catalog从显式root起的所有祖先、owner/module和manifest均用descriptor/no-follow读取，拒绝特殊文件；每个manifest最多1MiB、合计最多16MiB、最多256份，并限制目录枚举数量。读取同一regular fd并检查前后fstat稳定性；目录检查后换链不得读取目录外数据。仍只load一次后在同一内存上验证及建立索引。不能只增加第二次`is_symlink`检查来掩盖竞争窗口。
