# 第一阶段切片

- [x] F0.1 用户确认整套规划并要求模块独立目录。
- [x] F0.2 实现前记录ADR-001目录/依赖/实际范围。
- [x] F1.1 实现证据manifest校验、白名单保全与攻击性路径测试。
- [x] F1.2 实现WIP私有快照与完整性验证，不自动恢复或提交。
- [x] F1.3 对当前WIP生成一次快照并记录本地保全边界。
- [x] F2.1 按模块登记Portunex已知合同/数据库/页面基线和未知项。
- [x] F2.2 实现合同validator与失败用例、重复/缺来源/状态一致性检查。
- [x] F3.1 固化isthmus Messages proto及来源hash。
- [x] F3.2 实现独立WS frame codec与纯离线测试。
- [x] F4.1 组合CLI、Makefile与独立CI。
- [x] F4.2 运行全部新测试、已有execution-plane回归与diff检查。
- [x] F4.3 更新PRD/实施清单/verification，只勾选实际完成项。

本change完成不代表总R0/R1完成：完整schema、全部方法/响应、77实例一致性、真实数据一致性备份、异机恢复和整套业务仍有未完成项。
