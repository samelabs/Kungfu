# D8 验收记录

## A1–A25 对账（每行 → 测试锚点）

| # | 场景 | 测试 | 状态 |
|---|---|---|---|
| A1 | 版本固定 | TestRevisionUpdateArchivesExactlyOneNewRow + TestMemoryPinning（条目钉 revision） | ✓ |
| A2 | 回应对象四规则 | TestResponseObjectRules（含致本人拒、冻结不扩大） | ✓ |
| A3 | 回复即了结 | TestResponseObjectRules rule-2 分支 | ✓ |
| A4 | 处理与撤回 | TestHandleAndRetract（note 持久、retract 只动 pending、非作者拒） | ✓ |
| A5 | 钥匙生命周期 | TestThreadKeyLifecycle + TestPostIdempotency（原文不重披露） | ✓ |
| A6 | 幂等快照 | TestPostIdempotency + TestThreadIdempotencyReceipts + TestAssignIdempotencyAndRaces | ✓ |
| A7 | 重复加入 | TestThreadJoinDuplicateReturnsExisting | ✓ |
| A8 | LAST_MANAGER | TestThreadLastManagerThreePaths | ✓ |
| A9 | 降级不停工 | TestAssignMembershipAndDeparture（demoted assignee 照常 submit） | ✓ |
| A10 | 离开/移出三段收束 | TestCollectionHooks（回执）+ TestAssignMembershipAndDeparture（分派双侧 + 创建者离席 R-18） | ✓ |
| A11 | 未承接不约束 | TestAssignLifecycle（无回执产生） | ✓ |
| A12 | 承接与判定 | TestAssignLifecycle（take 了结携带回执、重复 take 拒、退回理由可读） | ✓ |
| A13 | 时限与到期优先 | TestAssignDeadlines（迟交被拒→sweep timed_out；迟判被拒→undecided） | ✓ |
| A14 | 关闭三段 | TestThreadCloseSemantics + TestAssignClosedRoom（待回应收束/未交付作废/已交付照常判定） | ✓ |
| A15 | 停用级联 | TestThreadDeactivationCascade（末位治理者停用→同事务关房）+ 持钥 join 拒 | ✓ |
| A16 | 越权矩阵 | TestThreadGovernanceAuthorization（房间）+ TestAssignLifecycle（judge/void 非当事拒） | ✓ |
| A17 | 并发 | TestPostConcurrency（恰一次了结、seq 严格递增）+ TestAssignIdempotencyAndRaces | ✓ |
| A18 | 轮次与恢复 | TestTodoListAggregatesAndRecovers（聚合/清项/投影稳定/空 wait）+ CursorPages | ✓ |
| A19 | 结构内容分离 | TestThreadToolsBothChannels（结构字段在、内容只在载荷字段） | ✓ |
| A20 | 编排端到端 | TestA20OrchestrationEndToEnd（1 主 3 工、钥匙交接、ack、分派、判×3、重做、关闭，双通道混跑） | ✓ |
| A21 | 丢失无害 | TestNotifyRegisterAndDispatch（不派发时 todo 照常持有义务） | ✓ |
| A22 | 迁移 | dev.sh test fresh 链全跑（001–027）+ TestMigration023*（回填双路） | ✓ |
| A23 | 放弃与作废 | TestAssignLifecycle（drop/void open/void taken/非创建者拒） | ✓ |
| A24 | 反向审计与上限 | 本文件反向审计表 + TestThreadMemberAndRoomLimits（成员 50/房间 100/ask 50） | ✓ |
| A25 | Memory 面 | TestRevisionPublicEvolution + TestRevisionWithdrawal + TestMemoryPinning（他人公开引用取消后不可读、自己固定引用撤回后可读） | ✓ |

## 反向审计（每个公开写动作 → 协议条款）

| 动作 | 条款 | 备注 |
|---|---|---|
| memory_put（创建/更新） | §5 | 单事务锁→归档→bump |
| memory_share / memory_unshare | §5 | 公开性属 Memory 整体 |
| memory_delete | §5 | 终态；固定引用按 §9 行继续 |
| thread_start | §6.1, L2 | key=true 原子组合 |
| thread_key / thread_key_revoke | §6.1 | 新钥作废旧钥；原文首响一次（L3） |
| thread_join | §6.1, L2 | 重复加入返回既有（两处显式无新效果成功之一） |
| thread_leave | §6.2, §6.4(R-18) | 收束回执 + 双侧未交付分派作废 |
| thread_remove / thread_set_role | §6.2 | LAST_MANAGER 闸；降旁观收束回执 |
| thread_close | §6.5 | 三段处置 + 清钥 + 成员保留只读 |
| thread_post | §6.3, §6.4, L2 | 四规则冻结、回复即了结、assign 原子组合 |
| thread_handle | §6.3 | note 随义务事实持久 |
| thread_retract | §6.3 | 只动 pending（L1） |
| assign_take | §6.4, R-18 | 成员资格即可；了结携带回执；可合并交付 |
| assign_submit | §6.4, L4 | 迟交被到期击败（拒绝） |
| assign_judge | §6.4, §6.5, R-18 | 关房旁路；创建者须仍在房；迟判被击败 |
| assign_drop / assign_void | §6.4 | 放弃限交付前；作废限未交付 |
| todo_list | §8 | 只读投影，永不落义务 |
| notify_register / notify_delete | 协议外（传输面，L4 授权应用） | challenge 验证；无内容载荷 |
| （到期转移）timed_out / undecided | L4, §10.1 | 内联拒绝 + RecoverAssigns 物化（D-014） |
| （收束转移）leave/remove/role_change/close | §6.2, §6.5 | 钩子内同事务 |
| （停用转移）成员资格终止/关房/分派作废 | §4 | TerminateAccountThreadMemberships 同事务 |

无来源规则检查：工具注册表 49 项逐一映射如上；未发现无法指出条款的业务规则。

## View=Facts 遗留裁定（D-013 backlog 关闭）

重读 L3/R-09：**快照=协议事实字段；首次结果可额外携带一次性秘密与投影（View 覆盖层）**。D2 工具的现行为（首次=Facts∪FirstOnly∪View，重放=存储的 Facts）**合规**——重放稳定返回事实快照，首次多出的 next 投影与密钥披露均为协议明文允许。D3/D4 工具采用更严格的 View=Facts 同形，两种风格并存均满足 L3。**不改代码**（D-005 修订门槛：frozen 行为默认不动）。

## 双通道等价

MCP 与 HTTP dispatch 共用同一 registry→service 转移（TestThreadToolsBothChannels：A 通道开房发读、B 通道加入发言，事实互通）。无第二套实现。

## Gate

dev.sh test 全绿（最终提交）。迁移链 001–027 fresh 跑通。
