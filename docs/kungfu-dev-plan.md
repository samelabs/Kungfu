# Kungfu 实现恢复计划

> 状态：第三次开工前的唯一工程恢复计划。
> 目标不是重做 Thread，而是以已确认的权威语义纠正 c404e96 上 T0–T3 的偏离，保留正确资产，再继续 Task / Todo / Surface。
> 本计划不创造产品规则。若实现需要本文与 PRD 均未给出唯一答案，当前阶段停止在该点，先修 PRD；不得以“让代码自洽”为理由补规则。

---

## 0. 冻结基线

### 0.1 语义权威

| 层 | 文件 | 冻结版本 |
|---|---|---|
| 范式 | kungfu.md | blob f48c2f90908197b47247ecc0ccc6511399939a77 |
| 产品 | docs/kungfu-prd.md | blob f907a4f13965feed7422a428e518b6cc57853a86 |
| 恢复计划 | 本文件 | 以执行分支实际落入的 commit 为准 |

语义来源只有上表。旧 docs/thread-prd.md、docs/thread-dev-plan.md 不再是 authority；实现分支进入恢复工作时删除或明确移出执行面，不能与新文档并列解释行为。

### 0.2 代码基线

- 主线基线：ec94ca55e019f5d68f05689227e77a22cdc805c9
- 待修实现 BASE：c404e9634efeef6518e56944aeefda766e3cb32f
- c404e96 相对 main：8 commits ahead；当前 CI run 37478106143 = success。
- success 只证明旧规则内部一致，不构成新 PRD 合规证明。
- migrations 023–025 只存在于未合并实现分支，main 仍停在 022。因此恢复阶段直接修正 024/025 的未发布 schema，不再叠一层 026 兼容补丁来掩盖错误结构；023 Memory foundation 原则上保留。

### 0.3 开工前文档移交

正式代码修复分支必须从 c404e96 创建，然后先只做文档移交：

1. 放入当前冻结的 kungfu.md、docs/kungfu-prd.md、本文件。
2. 移除旧 docs/thread-prd.md、docs/thread-dev-plan.md 的 authority 身份；优先直接删除，历史由 git 保留。
3. 此提交不得改任何 Go / SQL。
4. 后续所有工单都引用这三个文件的 exact blob/commit，不引用聊天摘要。

---

## 1. 执行纪律

### 1.1 一个阶段只跨一个语义边界

禁止再把 T2–T5 交给一个 Agent 长跑。阶段按本文件 C0 → C1 → C2 → C3 → M1 → T4 → T5 → T6 → T7 串行推进。

只有 C0 可以直接以 c404e96 为代码 BASE 生成实施工单。
C1 及以后必须在前一阶段 exact HEAD + exact CI 成功后重新生成工单，并重新列允许修改文件。

### 1.2 实现没有产品设计权

遇到以下任一情况，当前阶段停止并报告具体未定义点：

- PRD 对同一行为存在两种可成立解释；
- 需要新增状态、义务、可见性规则、权限来源或终态；
- 需要新增一个用户可见动作才能把流程闭合；
- 一个失败分支不知道应是 retry / revise / wait / stop；
- 为了通过测试需要改变 PRD 没写的业务前置条件。

允许实现自行决定的只有局部技术细节：函数拆分、索引命名、内部 helper、等价 SQL 写法等；不得改变对外语义。

### 1.3 每阶段三重 gate

阶段完成必须同时满足：

1. 语义 gate：所有新增/修改行为可逐条映射到 PRD；没有新增隐式规则。
2. 回归 gate：未列入该阶段的既有行为不变；Task 1.0 在 T4 前不得被顺带改。
3. 工程 gate：测试、构建、migration rehearsal、exact-SHA CI 全绿。

“测试绿”不能替代前两项。

### 1.4 禁止跨阶段顺手修

若审计时发现下一阶段问题，记录到后续工单，不在当前 diff 顺手改。
每个阶段收口后必须 git diff BASE..HEAD 复核实际改动范围。

---

## 2. c404e96 资产判定

### 2.1 KEEP — 可保留的已验证资产

以下机制方向与新 PRD 一致，除为兼容新 schema 做机械适配外不重写：

- 023：Memory revision / origin、memory_revisions 基础。
- Memory 更新 row lock → archive current revision → update current row 同事务。
- 现有数据回填 revision=1, origin=standalone，首版本不重复写历史表。
- memory_list / account stats 排除 origin=thread。
- Thread-origin Memory 不走 standalone consumption。
- root Thread 拥有本线程自己的 root entry，creator 是本线程 manage。
- Thread seq 原子分配的基本方向。
- join key 只持久化安全 hash，raw secret 仅首次成功响应披露。
- 新 key 替换旧 key；成员重复 join 不改变其既有 membership / permission。
- 直接把伙伴拉入 Thread 时要求 active Partner/Link。
- 移除成员会撤销当前 join key。
- 子线程创建后生命周期和成员关系不随父线程关闭/移除而级联。
- thread_idempotency 的 (role, operation, key) + request_hash 基础模型。
- repository 接受 pg.Querier、service 持事务的架构方向。
- 当前 T3 的 CAS 思路：同一 pending obligation 的终结不能重复生效。

KEEP 不代表当前文件原样不动；只表示上述机制不能因恢复工作被重设计。

### 2.2 REWORK — 已确认与新 PRD 不一致

必须定向修正：

- root creator 跨整棵树继承 govern。
- Child 无本线程首条 entry，成员入口 / receipt / key 指向父 anchor。
- 本线程 reply/add/key scope 接受 direct parent anchor。
- ReplyThreadState 强制 reply、强制 input_entry_id == reply_to、只有携带 input 才履行义务。
- 回复只能请原作者，缺少 ask / pair / ask:[]。
- branch 成员复用 partner gate。
- 缺 thread_leave。
- close 不撤销 key，reopen 后旧 key 可复活。
- receipt source 只有 entry/reply，且 handled 混合 reply / handle / branch 等终结方式。
- thread_handle(note?) 无持久位置。
- Link 无 note，双方相互 request 不会直接 active。
- key 不保存 join_role，join 固定 write。
- membership 不保存稳定 join_source。
- remove / demote 可把开放 Thread 的最后一个 manage 移除。
- read → write/manage 强制 entry，违背“entry 可选”。
- key 幂等旧请求在后续 reset 后返回当前 fingerprint，而非首次动作原结果。
- Thread 的 entry / join entry / membership entry / receipt input 的 DB FK 未强制同线程归属。
- thread_memories 缺 asked / task_id。
- memory_get 无 author historical revision 读取面。
- work harness 当前读取 Memory latest，没有在 Claim 固定 revision。
- Todo 只覆盖 Thread，不覆盖 partner request / deliver / assess。
- Task 1.0 没有 contract version binding、private Task、author judge、Memory output、统一 output_hash。

### 2.3 DROP — 明确删除而不是兼容

以下规则本身是旧设计偏差，不保留 compatibility path：

- RootCreatorCanGovernThread 及所有“root creator 可治理任意 descendant”的判断与测试。
- “父 anchor 属于 child 可写 / reply / join-entry scope”。
- “Child 直接拿父 anchor 作为自己成员入口和 receipt input”。
- “Todo input 必须等于 reply target，且只有携带 Todo input 才能履行回复义务”。
- receipt 的 handled 作为所有正常履行方式的统一含义。
- branch participant 必须与派生者是 partner。
- reopen 恢复旧 key 的效果。
- 旧 Thread PRD / dev plan 作为实现依据。

---

## 3. C0 — 持久结构对齐

这是唯一允许立即生成代码工单的阶段。

### 目标

只修正 T0 schema / model / repository primitive，使数据库能够表达最新版 PRD；不改变 service 对外行为。

### 允许范围

首个 C0 工单只允许修改：

- migrations/024_thread_core.sql
- migrations/025_thread_receipt_idempotency.sql
- internal/model/thread.go
- internal/repository/thread.go
- internal/repository/migration_thread_t0_test.go
- 如必须增加 repository-only 测试，可新增同目录测试文件

不得改 thread_kernel.go / thread_state.go / API / MCP / Task。

### 必须落地

1. role_links.note。
2. threads.join_role，合法值与 Thread permission 一致。
3. thread_roles.join_source ∈ {creator, partner, key, branch}；保留必要 joined_by_role_id。
4. thread_memories.asked、task_id。
5. thread_receipts：
   - source reason ∈ {entry, ask, reply, pair}
   - state pending / fulfilled / withdrawn
   - resolution 封闭集合
   - resolution_note
   - resolved_at
6. 本线程 entry 归属由 DB 结构保证：
   - join entry
   - ThreadRole entry
   - receipt input
   - reply target
   均不能指向别的 Thread 的 entry。
7. 现有 idempotency 持久结构能够保存“首次动作原非秘密业务结果”，不能要求 replay 时从当前对象状态重新推导。
8. 023 不因 C0 被重写。

### C0 验收

- fresh DB：001 → revised 025 成功。
- upgrade rehearsal：main 001–022 → revised 023–025 成功。
- 既有 main 数据不合成 Partner / Thread / receipt。
- SQL 负测：四类 cross-thread entry reference 全部被 DB 拒绝。
- schema enum / CHECK 覆盖 PRD 封闭集合。
- 不调用任何 service 业务动作。
- exact-SHA CI。

C0 完成后先审 diff，确认没有 service 行为变化，再允许生成 C1 工单。

---

## 4. C1 — Partner / Membership / Key / Governance

### 目标

修正“谁能进入、谁能治理、凭什么同意、如何退出”，不碰发言 recipient 规则。

### 语义范围

- partner_request(note?) 持久化 note。
- 双方互相 request：同一 canonical relation 原子收敛为 active。
- direct add 仍要求 active partner；角色缺省 write。
- membership 写入稳定 join_source。
- 删除 root inherited governance；治理只看本 Thread 的 manage membership。
- key 持久化 join_role；role 缺省 write；entry 缺省本线程最新 entry。
- key join 按绑定 role 加入；read 不产生入口待回应，write/manage 后续由 C3 receipt 语义接上。
- key 幂等 replay 返回首次成功的原 fingerprint，永不重放 raw key。
- thread_leave。
- open Thread 的 leave/remove/demote 都执行 LAST_MANAGER guard。
- remove 同事务撤销当前 key。
- close 同事务撤销当前 key；reopen 不恢复旧 key。
- read → write/manage 的 entry 可选；指定时才产生入口待回应。
- Partner remove 不影响既有 Thread membership。

### 明确不做

- 不实现 generic thread_post。
- 不改 branch first-entry。
- 不做全局 Todo。
- 不碰 Task。

### Gate

必须覆盖 partner opposite-request race、last-manager 三条路径、key reset/replay、old-key invalidation、partner removal independence、非成员 root creator 无 descendant manage 权限。

---

## 5. C2 — Child Thread 本地化

### 目标

把 Child 从“父 anchor 的可写延伸”纠正为真正独立 Thread；父 anchor 仅作为 lineage 只读锚。

### 必须成立

- thread_branch(thread, anchor, subject, content, summary?, members?) 创建 Child 自己的 seq=1 首条 entry。
- Child creator = manage，entry 归 Child。
- members 只能来自父 Thread 当前 write/manage；不要求与派生者是 partner。
- write/manage 初始成员的入口是 Child 首条 entry。
- Child key 只能绑定 Child entry。
- Child reply 只能指 Child entry。
- lineage 读取可返回从 root 到当前 Child 的 anchor chain，但这些 ancestor entries 不进入 Child 写 scope。
- 父 Thread 后续 close/remove 不改变 Child lifecycle/membership。
- 父 Thread manage 若不是 Child member，不能读/管 Child。

### DROP gate

阶段结束必须搜索不到任何业务使用的 RootCreatorCanGovernThread，也不能再用“current thread OR direct anchor”作为写 scope。

---

## 6. C3 — thread_post 与待回应状态机

### 目标

一次性收口 Thread 的发言、回应对象、履行方式和并发；不在这一阶段实现 Task claim 集成之外的 Task 新机制。

### thread_post

按 PRD §4.2 唯一实现：

- content | memory 二选一。
- summary 规则。
- reply_to? 仅本 Thread。
- ask?：显式名单，包括 []。
- task? 只做引用持久化，Task 资格/结果逻辑留 T4。
- input_ref? 只做 stale guard，不决定履行。
- recipient precedence：ask → reply author → pair → none。
- 返回 asked、my_pending、next_action。

### obligation

- source reason 与 resolution 分离。
- reply / handle / branch / claim 各有独立 resolution。
- leave / remove / role_change / close 为 withdrawn resolution。
- thread_handle(note?) 保存 note，不创建新 entry。
- reply 本身履行本人对 reply target 的 pending obligation；不依赖 input_ref。
- 同一 obligation 的终结 CAS 至多一次。

### summary / content

- Thread content 仍以 origin=thread Memory 承载。
- summary 存该 Memory 的 description。
- 完整内容不塞入结构字段。

### C3 gate

覆盖 PRD §12.2 的并发矩阵和 §12.3 场景 1–7 中纯 Thread 部分。
旧“ReplyThreadState 即产品语义”的 API 不得继续决定新行为；可以保留内部兼容 wrapper，但 wrapper 必须完全落到 thread_post 的同一语义，不能存在第二套状态机。

---

## 7. M1 — Memory 读取闭合

这是 T4 前的独立小阶段。

- memory_get(code, revision?)。
- author 可读任一存在 revision。
- 非 author 直接读取只允许 public current revision。
- Thread / Task context 对 pinned private revision 的读取走其上下文授权，不放宽直接 memory_get。
- 不改变 standalone create/update/share/delete 的既有经济 policy。
- 不顺手做 Task harness pin；那属于 T4。

Gate：current/historical、soft-delete pinned、public unshare、非 owner historical 越权。

---

## 8. T4 — Task 按范式修正

T4 只有在 C0–C3 + M1 全部 exact-SHA gate 完成后才能生成具体工单；不得现在锁未来代码 BASE。

### 必须实现的既定产品语义

1. contract version：
   - author update 产生新 version；
   - active Claim / 未终结 Submission 存在时拒绝修改；
   - Claim / Submission 固定 version。
2. harness pin：
   - Claim 时解析并固定 Memory revision；
   - 有 Claim 的读取始终返回固定 revision。
3. private Task：
   - 不锁 credits，不产生 ledger；
   - author + 引入 Thread 的 current write/manage 可承接；
   - read 只读；
   - author judge。
4. Memory outputs：
   - submit 时固定 name/code/revision/checksum；
   - 统一 output_hash 覆盖 payload + Memory bindings，作为 request-key 幂等内容身份。
5. retention：
   - receiver：非终态为重投保留内容/绑定；终态清除，保留 output_hash + outcome facts；不形成平台 task_results。
   - author：settled 保留 accepted output 作为 task_results；rejected/failed 清输出内容/绑定，保留事实。
6. work_claim：
   - 成功后原子履行该 agent 所有仍 pending、且来源 entry 引用同一 Task 的 obligations，resolution=claim。
7. task_publish：
   - private → public 不可逆；
   - 无 active Claim / 未终结 Submission；
   - 才进入当前 Kungfu 产品的 credits/receiver 公开 Task policy。
8. 公开 Task 的 credits/receiver 行为除 PRD 明列修正外保持 Task 1.0。

### 禁止

- 不因为“public Task”在范式上新增任何必然经济语义。
- 不把 receiver result retention 改成平台存档。
- 不让 Task 修订影响已绑定旧 contract 的 Claim。
- 不把 thread content 随 publish 公开。

---

## 9. T5 — Todo / Work Context / 可恢复性

### Todo 必须只由持久状态计算

四类：

- reply
- partner_request
- deliver
- assess

无事项时按 PRD 的 wait / poll_after 规则。

### Thread work context

thread_get：

- 摘要时间线分页；
- 指定 entries 再展开完整内容；
- 当前 membership / role；
- my pending；
- child links；
- lineage anchor chain（Child only，read-only）。

partner_list 返回伙伴与共同开放 Thread。
履约记录必须能从 receipt resolution + Task terminal facts 计算；本阶段不得自行发明 reputation score、排名或新的公开评价工具。若产品需要额外暴露面，先改 PRD。

Gate：掉线后清除进程/会话本地状态，只凭 todo → work context 完成恢复。

---

## 10. T6 — MCP / HTTP / Product Surface

T6 才注册/暴露冻结接口。

- 工具名、参数、next_action 只来自 PRD §8。
- 所有写工具同一 envelope / error / idempotency 语义。
- MCP 与 HTTP 必须调用同一 service state transition，不允许双实现。
- 更新 llms.txt / public docs / Web 时只做已经成立机制的表达，不新增规则。
- A8：结构字段和用户/Agent 内容严格分开。

Gate：MCP / HTTP 双端等价测试；不依赖 UI 才能完成核心场景。

---

## 11. T7 — 全量合规验收

最终必须同时通过：

1. Kungfu PRD §12 全部场景。
2. Task 1.0 全部既有回归与账本不变式。
3. migration fresh + upgrade rehearsal。
4. 幂等 / concurrency / stale-input 随机压力。
5. 越权矩阵。
6. 进程状态清空后的恢复测试。
7. 两种异构运行时只凭 MCP 完成：
   - 两人对话并正常结束；
   - 群议 ask；
   - private Task 结构化传递。
8. 反向审计：逐个 public write action 指出 PRD 条款；任何无法指出来源的业务规则 = 不合格。
9. exact HEAD CI success + clean git status。

---

## 12. 每个阶段工单的固定模板

后续给执行 Agent 的工单必须包含以下全部字段，缺一不可：

- BASE：完整 SHA。
- Authority：kungfu.md / PRD exact blob 或 commit。
- Stage：只能一个。
- Allowed files：白名单。
- KEEP：本阶段绝不能破坏的机制。
- Required changes：逐条映射 PRD。
- Forbidden changes：明确不能顺手做什么。
- Negative tests：至少列越权 / stale / rollback / idempotency / concurrency 中相关项。
- Stop conditions：遇到未定义语义必须停在哪里。
- Report：new HEAD、changed files、测试、CI run、未完成项。
- Gate：只有 exact SHA CI success 后下一阶段才能开工。

工单中不得出现“按 PRD 完成其余逻辑”“顺便补齐”“合理处理边界情况”这类把设计权交给执行 Agent 的措辞。

---

## 13. 第一个可执行工单边界

本计划完成后，只生成 C0 工单。

C0 不修 service，不实现新 Thread 行为，只把未发布的 024/025 持久结构改到能准确表达 PRD。
C0 收口并审计后，再基于它的 new HEAD 生成 C1。

这是防止第三次中断的硬控制点，不是进度建议。
