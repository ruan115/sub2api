## ADDED Requirements

### Requirement: 账号上线必须是两阶段过程
账号 SHALL 先进入不可调度 provisioning，凭证、代理、slot 和 worker 全部验证成功后才进入 ready + schedulable。

#### Scenario: 容器创建失败
- **WHEN** 账号凭证有效但 slot 无法创建或探活
- **THEN** 账号 MUST 保持不可调度并展示具体 runtime step
- **THEN** 系统 MUST 不把账号误报为上号成功

### Requirement: 生命周期必须通过事务 Outbox 驱动
账号业务变更和 runtime outbox event MUST 在同一 CCMAX MySQL 事务提交。orchestrator SHALL 至少一次消费并按 event/generation 幂等执行，并周期性 reconciliation。

#### Scenario: orchestrator 在处理事件中途重启
- **WHEN** 同一事件被重新投递
- **THEN** 最终 slot 状态 MUST 与 desired generation 一致
- **THEN** 系统 MUST 不创建第二个有效 slot/epoch

### Requirement: 归档必须可用于任意未删除账号
归档 SHALL 停止新调度，默认 drain 最长 15 分钟，销毁 slot，并保留密文凭证与代理预约。管理员 MAY 强制立即终止或显式选择归档并释放代理。

#### Scenario: 归档活跃账号
- **WHEN** 管理员归档仍有执行中的账号
- **THEN** 系统 MUST 先进入 draining 且拒绝新请求
- **THEN** 到达 drain 完成或超时后才销毁 slot

### Requirement: 软删除必须进入可批量处理的回收站
软删除 SHALL 撤销租约、销毁 slot、保留密文和代理预约，并支持批量恢复和批量彻底清除。系统 MUST 不自动清除软删除账号。

#### Scenario: 批量恢复
- **WHEN** 管理员恢复一个或多个软删除账号
- **THEN** 系统 MUST 重新验证代理与凭证并重建 slot
- **THEN** 只有成功账号才恢复调度，失败项 MUST 返回逐项原因

#### Scenario: 批量彻底清除
- **WHEN** 管理员确认 purge
- **THEN** 系统 MUST 销毁密文凭证、DEK、runtime 数据和恢复入口
- **THEN** 只允许保留不含正文/凭证的统计审计墓碑

### Requirement: 修改凭证或代理必须两阶段切换
系统 SHALL drain 旧 slot，创建无正式执行权的候选 slot，完成验证后提升 epoch 并切换；验证失败 SHALL 恢复旧 slot。

#### Scenario: 新代理验证失败
- **WHEN** 候选 slot 无法通过新代理完成验证
- **THEN** 旧配置 MUST 保持有效且可恢复调度
- **THEN** 候选 slot MUST 被销毁

### Requirement: drain 必须统一且可强制
归档、删除、节点维护、镜像升级和配置切换 SHALL 共用 drain 状态机，默认最长 15 分钟；管理员 MAY 显式强制终止。

#### Scenario: drain 中存在 tool wait
- **WHEN** 挂起工具会话在 drain deadline 前未完成
- **THEN** 系统 MUST 在 deadline 到达时终止会话并记录规范化原因
