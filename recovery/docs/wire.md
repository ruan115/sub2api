# 静态wire观察：来源可复核，语义不自动认证

本工具把人工静态观察关联到已有API发现记录与已审阅源文件。它不下载/执行JS、不登录、不发送API请求、不改catalog，也不创建旧DTO。只有同一段文字被哈希和位置证实，不能推断其一定处于真实请求的执行路径。

## 目录和输入

实现独立在`tooling/recoverykit/wire/`，测试在`tests/wire/`。CLI只装配。输入均显式指定：

1. 已有`recovery/contracts/`发现目录：引用其中唯一的`api_paths` ID，不引用页面/数据库条目。
2. 已人工审阅的artifact manifest：复用[evidence格式](../tooling/README.md)，每个源文件有size和SHA-256。
3. 私有source-root：只读、no-follow、UTF-8及秘密筛查；源文件不进入Git。
4. 人工观察JSON：建议与原材料一起私有存放，经审查后才决定是否可公开。工具不打印内容，不自动创建这份文件。

```sh
PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=recovery/tooling python3 -m recoverykit wire verify \
  --observations /absolute/private/observations.json \
  --manifest /absolute/private/artifacts.json \
  --source-root /absolute/private/reviewed-source \
  --catalog-root recovery/contracts
```

上面的路径都是需要显式替换的占位路径，不会自动访问服务器或发现本机文件。

## 观察文档v1

下例仅解释结构；路径、ID和占位hash是合成的，不是可直接运行的生产事实。

```json
{
  "schema_version": 1,
  "id": "synthetic.wire.review",
  "artifact_manifest_id": "synthetic.artifacts",
  "observations": [{
    "id": "synthetic.login.method",
    "record_id": "portunex.example.api.login",
    "path": "/synthetic/login",
    "aspect": "method",
    "statement": "This reviewed call-site fragment contains the POST literal.",
    "anchors": [{
      "artifact": "client.js",
      "start": 0,
      "end": 42,
      "sha256": "<SHA-256 of exactly these source bytes>",
      "literal": "POST"
    }]
  }]
}
```

所有层使用严格字段集合。`aspect`只能是`method/path/request_header/request_body/credentials/response_field/status/pagination/error`；这是观察分类，不是已推断出的HTTP合同。`statement`是人工描述，`literal`是需要在锚定片段中实际出现的UTF-8字面文字。动态表达式、转义字符串、模板拼接与默认参数不能由简单文本匹配自动判定。

`start`和`end`是原始UTF-8文件的字节偏移，区间为`[start,end)`，不是字符下标或格式化后行号。不能跨出文件、截断UTF-8字符、使用空片段或负数/bool下标。格式化过的文件是另一份artifact，必须有自己的哈希，不可复用原件偏移。

观察文档≤1MiB；最多256条观察，每条1–8个anchor，单片段≤8192字节。其他字段亦有上限。未知源文件、SHA不符、literal缺失、重复ID/anchor、未知record、path不匹配、重复JSON键、秘密命中或软链接都会拒绝。完整artifact仍受evidence的文件/总量限制，不能只截取一段后跳过全文件审核。

catalog亦必须通过no-follow读取，不能借其root祖先、owner/module或manifest软链接绕过边界。每份manifest≤1MiB、总量≤16MiB、最多256份；目录枚举有独立预算。若系统提供的路径经由软链接，请显式选用审核过的真实目录，而不是让工具自动跟随。

## 输出与下一步

成功输出只含`status: source_anchored`、数量与`business_verification: false`。这不是`verified`业务状态，不修改原目录，也不清除未知项。失败只显示固定错误类别，不显示原文、路径或statement。

人工仍需检查：调用链是否真实可达，method如何传递，body如何序列化，cookie/header来自哪里，响应字段是否只是UI消费的子集，以及分页、异常、权限与写入语义。之后才允许为明确的旧合同编写适配器和对照测试。

2026-09-13 后续使用 `BindInterface=en0` 成功采集并录入首批 Portunex 静态观察：11 份白名单文件、27 条观察、60 个锚点、7 个 API 记录均通过来源校验，详见 [采集记录](../contracts-wire/portunex/README.md)。一份 Provider 页面包因凭据形状 URL 筛查被排除。合成测试只证明校验器的安全边界，实际来源锚定也不等于服务端兼容验收；旧调用方兼容性仍不能打勾。
