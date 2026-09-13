# 合同发现目录的维护规则

清单位置：`recovery/contracts/<owner>/<module>/manifest.json`。`owner` 目前仅 `portunex`、`isthmus`；每个模块目录仅存本模块manifest，文档放在 `recovery/docs/`，不把原始资产、秘密或日志放进合同目录。

模块按业务职责划分，例如 identity、users、apikeys、providers、pricing、billing、redemption。清单是恢复待办的可校验索引，不是自动生成服务的OpenAPI或完整数据库DDL。

最小格式示例（合成路径，不是线上接口）：

```json
{
  "schema_version": 1,
  "id": "portunex.example",
  "owner": "portunex",
  "status": "discovered",
  "evidence": [{
    "id": "reviewed_asset",
    "kind": "static_asset",
    "source": "reviewed-reference.js:10",
    "observed": "A literal path exists; its full behavior is not established."
  }],
  "discovered": {
    "api_paths": [{
      "id": "portunex.example.api.collection",
      "path": "/synthetic/example",
      "method": "unknown",
      "status": "discovered",
      "evidence": ["reviewed_asset"],
      "unknowns": ["Method, authentication, body and response remain unknown."]
    }]
  },
  "unknowns": ["Discovery is not an implementation or a behavior test."]
}
```

1. 每条记录有全目录唯一的稳定ID；证据引用必须指向本manifest声明的证据。`source`可描述私有材料位置，validator不打开该位置，也不证明来源文件仍存在。
2. API路径与method归属不能跨模块重复。method未知时用`unknown`，不要根据路径名称猜GET/POST。同一接口被多个页面使用时仍只有一个权威合同记录。
3. 记录支持 `api_paths`、`pages`、`database_entities`、`assets`、`protocols` 集合。数据库基线另见 `recovery/baselines/portunex/database_inventory.json`；只有表名/数量，不能作为migration输入。
4. 第一阶段所有合同均为`discovered`且有明确unknowns。后续`implemented`/`verified`必须带声明过的实现/验证证据ID；完整协议字段和golden fixture的强校验仍属R1后续范围。
5. `valid: true`只表示目录、类型、引用、状态声明和重复检查通过；`business_verification`始终为`false`。不能批量升级状态来冒充API或页面验收。
6. 空目录、重复JSON成员、重复ID、缺来源、未声明证据、无unknowns的discovered记录均须失败。
7. 文件读取不跟随root祖先、owner、module或manifest软链接，拒绝特殊文件及读取中变化。每份manifest≤1MiB、总量≤16MiB、最多256份，并限制目录枚举数量；超限须先合理拆分审阅批次，不关闭安全检查。

运行 `make -C recovery contracts` 或 `make -C recovery check`。不要在合成fixture中粘贴真实密码、Token、邮箱记录、支付回调正文或生产数据库行。

需要检查实际原件哈希和源码位置时，使用独立的[wire观察校验器](wire.md)。它关联现有record ID，不重复建立发现目录，也不自动修改这里的method、unknowns或status。
