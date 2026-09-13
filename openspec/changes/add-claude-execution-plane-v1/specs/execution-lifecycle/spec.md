## ADDED Requirements

### Requirement: 账号上线必须是两阶段过程
账号 SHALL 先进入不可调度 provisioning，凭证、代理、slot 和 worker 全部验证成功后才进入 ready + schedulable。

#### Scenario: 容器创建失败
- **WHEN** 账号凭证有效但 slot 无法创建或探活
- **THEN** 账号 MUST 保持不可调度并展示具体 runtime step
- **THEN** 系统 MUST 不把账号误报为上号成功

### Requirement: 生命周期必须通过事务 Outbox 驱动
账号业务变更和 runtime outbox event MUST 在同一 CCMAX MySQL 事务提交。orchestrator SHALL 至少一次消费并按 event/generation 幂等执行，并周期性 reconciliation。

CCMAX 的全部 runtime outbox writer MUST 在业务事务中先取得同一 commit-order fence，再分配
event sequence，并把该 fence 持有到 commit 或 rollback。authority、onboarding 和 lifecycle
事件 MUST 共享同一个有序 consumer checkpoint；consumer MUST 只 ack 当前最早未完成事件。
启动 preflight MUST 拒绝缺失、越界、跳号或 blocked checkpoint，以及非空历史上的隐式
sequence-0 bootstrap。永久错误 MUST durable block 且不得自动 skip；暂时错误 MUST 保持同一
sequence 可重放，达到 retry budget 时 supervisor MUST 退出且不得推进 checkpoint。

#### Scenario: orchestrator 在处理事件中途重启
- **WHEN** 同一事件被重新投递
- **THEN** 最终 slot 状态 MUST 与 desired generation 一致
- **THEN** 系统 MUST 不创建第二个有效 slot/epoch

#### Scenario: 后写事务先尝试提交
- **WHEN** 两个 producer transaction 并发写入 runtime outbox，较早取得 fence 的事务尚未结束
- **THEN** 后一个事务 MUST 在分配 sequence 前等待
- **THEN** consumer 可见并确认的 sequence 顺序 MUST 与事务 commit 顺序一致
- **THEN** 较早事务 rollback 产生的自增 gap MUST NOT 阻止后续已提交事件被消费

#### Scenario: ordered event 无法安全处理
- **WHEN** 当前最早事件格式非法、权限失效、完整性冲突或语义不可判定
- **THEN** consumer MUST 记录不含秘密的 failure class/code/sequence/claim version 并阻塞
- **THEN** 系统 MUST NOT 消费或确认任何更晚事件
- **THEN** 只有受鉴权且写入审计、并对 exact sequence 与 blocked claim version 做 CAS 的恢复动作 MAY 重新开放该事件

### Requirement: Onboarding 启动必须脱离全局 Outbox 重试
onboarding runtime event SHALL 幂等投影为不含秘密的 durable start trigger 与 desired slot。
独立 coordinator SHALL 以短租约和有界批次处理 trigger，并且只有 atomic healthy-slot starter
可以创建 workflow/proxy lease。暂时没有健康 assignment 时 SHALL 仅重排该 trigger，不得阻塞
全局 runtime outbox checkpoint。

#### Scenario: event 已投影但节点尚未形成健康 assignment
- **WHEN** coordinator claim 到有效 trigger，但 starter 无法验证 fresh healthy assignment 或 live lease
- **THEN** trigger MUST 保留原 event/intent/account/generation/reservation identity 并延迟重试
- **THEN** runtime outbox MAY 继续处理后续已安全投影的事件
- **THEN** intent 到期后 trigger MUST 进入 expired 且不得 claim/decrypt credential

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
