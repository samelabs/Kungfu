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
| 产品 | docs/kungfu-prd.md | blob 77a13ceeaabdf5ab99acf92c2ba3feddd0e65640 |
| 恢复计划 | 本文件 | 以执行分支实际落入的 commit 为准 |

语义来源只有上表。旧 docs/thread-prd.md、docs/thread-dev-plan.md 不再是 authority；实现分支进入恢复工作时删除或明确移出执行面，不能与新文档并列解释行为。

### 0.2 代码基线

- 主线基线：ec94ca55e019f5d68f05689227e77a22cdc805c9
- 待修实现 BASE：c404e9634efeef6518e56944aeefda766e3cb32f
- c404e96 相对 main：8 commits ahead；当前 CI run 37478106143 = success。
- success 只证明旧规则内部一致，不构成新 PRD 合规证明。
- migrations 023–025 只存在于未合并实现分支，main 仍停在 022。因此恢复阶段允许直接修正 024/025 的未发布 schema，不再叠一层兼容 migration 来掩盖错误结构；023 Memory foundation 原则上保留。

### 0.3 开工前文档移交

正式代码修复分支必须从 c404e96 创建，然后先只做文档移交：

1. 放入当前冻结的 kungfu.md、docs/kungfu-prd.md、本文件。
2. 移除旧 docs/thread-prd.md、docs/thread-dev-plan.md 的 authority 身份；优先直接删除，历史由 git 保留。
3. 此提交不得改任何 Go / SQL。
4. 后续所有工单都引用这三个文件的 exact blob/commit，不引用聊天摘要。

---

## 1. 执行纪律

### 1.1 阶段必须纵向闭合

此前“先把最终 schema 全改完、service 以后再跟”的拆法不可用：当前 service 仍会写旧 receipt 状态，Child 仍会引用父 anchor；若先加最终 CHECK/FK，该阶段必然无法独立通过。

因此恢复阶段按一个语义切片同时修改其 schema + repository + service + tests：

C0 Partner/Membership/Key/Governance
→ C1 Child/local-entry boundary
→ C2 thread_post/obligation
→ M1 Memory read
→ T4 Task
→ T5 Todo/Work Context
→ T6 Surface
→ T7 Acceptance

只有 C0 可以直接从当前 repair 文档移交 HEAD 生成实施工单。
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

1. 语义 gate：本阶段触及的行为全部映射到 PRD；没有新增隐式规则。
2. 回归 gate：未列入该阶段的既有行为不被顺手改；已知后续偏差可以继续存在到它所属阶段，但不能被扩大或固化成新接口。
3. 工程 gate：测试、构建、migration rehearsal、exact-SHA CI 全绿。

“测试绿”不能替代前两项。

### 1.4 禁止跨阶段顺手修

若审计时发现下一阶段问题，记录到后续工单，不在当前 diff 顺手改。
每个阶段收口后必须 git diff BASE..HEAD 复核实际改动范围。

---

## 2. c404e96 资产判定

### 2.1 KEEP — 可保留的已验证资产

- 023：Memory revision / origin、memory_revisions 基础。
- Memory 更新 row lock → archive current revision → update current row 同事务。
- 现有数据回填 revision=1, origin=standalone，首版本不重复写历史表。
- memory_list / account stats 排除 origin=thread。
- Thread-origin Memory 不走 standalone consumption。
- root Thread 拥有本线程自己的 root entry，creator 是本线程 manage。
- Thread seq 原子分配的基本方向。
- join key 只持久化安全 hash，raw secret 仅首次成功响应披露。
- 新 key 替换旧 key；成员重复 join 不改变既有 membership / permission。
- 直接把伙伴拉入 Thread 时要求 active Partner/Link。
- 移除成员会撤销当前 join key。
- 子线程创建后生命周期和成员关系不随父线程关闭/移除而级联。
- thread_idempotency 的 (role, operation, key) + request_hash 基础模型。
- repository 接受 pg.Querier、service 持事务的架构方向。
- 当前 T3 的 CAS 思路：同一 pending obligation 的终结不能重复生效。

KEEP 不代表当前文件原样不动；只表示上述机制不能因恢复工作被重设计。

### 2.2 REWORK — 已确认与新 PRD 不一致

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

- RootCreatorCanGovernThread 及所有“root creator 可治理任意 descendant”的判断与测试。
- “父 anchor 属于 child 可写 / reply / join-entry scope”。
- “Child 直接拿父 anchor 作为自己成员入口和 receipt input”。
- “Todo input 必须等于 reply target，且只有携带 Todo input 才能履行回复义务”。
- receipt 的 handled 作为所有正常履行方式的统一含义。
- branch participant 必须与派生者是 partner。
- reopen 恢复旧 key 的效果。
- 旧 Thread PRD / dev plan 作为实现依据。

---

## 3. C0 — Partner / Membership / Key / Governance 闭合

这是唯一允许立即生成代码工单的阶段。

### 3.1 目标

只收口“谁能建立伙伴、谁能进入 Thread、以什么角色进入、谁能治理、如何退出、key 生命周期”。本阶段不改 Child 的父 anchor 模型，不改 recipient 规则，不把 receipt 切到最终 resolution 模型。

### 3.2 允许范围

C0 工单允许修改：

- migrations/024_thread_core.sql
- internal/model/thread.go
- internal/repository/thread.go
- internal/service/thread_kernel.go
- internal/service/thread_state.go
- internal/repository/migration_thread_t0_test.go
- internal/service/thread_kernel_test.go
- internal/service/thread_state_test.go
- 如测试需要，可新增同目录专用测试

不得修改：

- migrations/025_thread_receipt_idempotency.sql 的 receipt state 模型
- Task 文件
- MCP / HTTP / Web
- Memory 023 / revision 机制
- Child local-first-entry 结构
- thread_post recipient 规则

### 3.3 必须落地

Partner：
- role_links 增加 note。
- partner_request(note?) 保存首次 pending note；同一发出方重复 request 不改写 note。
- 双方相互 request 原子收敛为 active；反向 request 的 note 不覆盖原 pending note；并发只形成一个关系。
- accept / decline / cancel / remove 的既有方向约束保持。
- partner remove 不影响既有 Thread membership。

Membership provenance：
- thread_roles 增加 join_source ∈ {creator, partner, key, branch}。
- root creator = creator。
- direct add / thread_start 的伙伴成员 = partner。
- key join = key。
- 当前 branch creator / branch members 即使其其余 branch 语义要到 C1 修，membership source 也必须准确记为 branch。
- joined_by_role_id 保留直接拉入者 / 派生者事实。

Governance：
- 删除 root inherited governance。
- manage 权限只来自当前 Thread membership。
- thread_leave 可用；本人随时离开。
- open Thread 的 leave / remove / demote 任何一个会导致 manage=0 时返回 LAST_MANAGER。
- closed Thread 不受“必须至少一个 manage”约束；leave 仍可用。
- Partner remove 不回收 Thread 权限。

Key：
- threads 增加 join_role，与 key hash / join_entry 成同一组状态。
- role ∈ {read, write, manage}，默认 write。
- entry 缺省当前 Thread 最新 entry。
- key join 按绑定 role 加入；existing member 不升级、不重复加入。
- remove participant 和 close 都在同事务撤销当前 key；reopen 不恢复旧 key。
- reset/revoke 只有当前 Thread manage 可做。
- 幂等 replay 返回首次成功动作保存的原非秘密结果，raw key 永不重放；后续 reset 不得改变旧幂等结果的 fingerprint。

Role change：
- read → write/manage 的 entry 可选；指定时记录新入口并在当前旧 receipt 模型下机械创建入口 pending，未指定则不创建。
- write/manage → read 撤销其当前 pending（receipt 的最终 resolution 语义留 C2）。

### 3.4 C0 不得顺手修

- Branch 仍可能使用父 anchor；到 C1 一次性修。
- receipt 仍可暂用当前 pending/handled/withdrawn；到 C2 与 thread_post 一起切换，避免中间态伪造 resolution。
- ReplyThreadState 的旧语义不在 C0 改。
- EntryAllowedInThreadScope 的 parent-anchor 行为不在 C0 改。

### 3.5 C0 Gate

必须新增/修订测试证明：

- mutual partner request → one active relation；concurrent opposite request 亦如此。
- note 持久化。
- non-member root creator 不能治理 Child。
- direct add 仍要求 active partner。
- key role = read/write/manage 各自正确；默认 write。
- existing member join 不升级。
- raw key 一次披露；旧 idempotency replay 的 fingerprint 不随后续 reset 漂移。
- remove / close 使旧 key 立即失效；reopen 不恢复。
- LAST_MANAGER 覆盖 leave/remove/demote 三条路径。
- closed Thread member 可 leave。
- partner removal 不改变已有 membership。
- fresh + main(001–022) → current 023–025 migration rehearsal。
- exact-SHA CI success。

C0 完成后先做语义 diff 审计，再生成 C1 工单。

---

## 4. C1 — Child / local-entry boundary 闭合

### 4.1 目标

把 Child 从“父 anchor 的可写延伸”纠正为独立 Thread；同时落下同线程 DB 约束。这个阶段必须 schema + service 一起改，不能先加 FK 再留旧 Child 行为。

### 4.2 必须落地

Branch：
- thread_branch(thread, anchor, subject, content, summary?, members?) 创建 Child 自己的 seq=1 首条 entry。
- Child creator = manage，join_source=branch。
- members 只能来自父 Thread 当前 write/manage；不要求与派生者是 partner。
- write/manage 初始成员入口 = Child 首条 entry；read 不产生入口 pending。
- 本人对父 anchor 的 pending 若存在，由 branch 同事务履行；不存在时 branch 本身仍是合法分解动作。

Local entry scope：
- Child key 只能绑定 Child entry。
- Child reply 只能指 Child entry。
- add / role-entry 只能使用当前 Thread entry。
- lineage 读取仍可返回 root→current 的 ancestor anchor chain，但 anchor chain 是 read-only context，不进入写 scope。
- 父 Thread manage 若不是 Child member，不能读/管 Child。

DB：
- join entry 使用 (thread_id, entry_id) 同线程 FK。
- ThreadRole entry 使用同线程 FK。
- receipt input 使用同线程 FK。
- reply_to 使用同线程 FK。
- parent anchor 继续用 (parent_thread_id, anchor_entry_id) 指向直接父 Thread entry。

### 4.3 明确不做

- receipt state/resolution 不在 C1 切最终模型。
- generic thread_post / ask 不在 C1。
- Task / Todo 不动。

### 4.4 C1 Gate

- 四类 cross-thread write reference SQL 负测全部失败。
- Child seq=1 entry 属于 Child。
- parent anchor 只能通过 lineage/context 读取，不能 reply/add/key。
- branch members 不要求 partner，但必须是 parent write/manage。
- parent close/remove 不影响 Child lifecycle/membership。
- parent manage 非 Child member 越权失败。
- exact-SHA CI。

---

## 5. C2 — thread_post / obligation 闭合

### 5.1 目标

一次性切换发言语义和待回应持久状态；receipt schema 与 service 同阶段完成，避免“最终 schema + 旧业务”中间态。

### 5.2 Schema / model

- thread_memories 增加 asked、task_id。
- thread_receipts：
  - reason ∈ {entry, ask, reply, pair}
  - state ∈ {pending, fulfilled, withdrawn}
  - fulfilled resolution ∈ {reply, handle, claim, branch}
  - withdrawn resolution ∈ {leave, remove, role_change, close}
  - resolution_note 仅 handle 可有
  - resolved_at
- 旧 handled 语义删除，不做 compatibility 状态。

### 5.3 thread_post

按 PRD §4.2 唯一实现：

- content | memory 二选一。
- summary 规则；Thread Memory description = summary。
- reply_to? 仅本 Thread。
- ask? 显式名单，包括 []。
- task?：本阶段可引用当前既有公开 Task；private Task 的 author/visibility 规则到 T4 扩展，不另造临时私有规则。
- input_ref? 只做 stale guard，不决定履行。
- recipient precedence：ask → reply author → pair → none。
- 返回 asked、my_pending、next_action。

### 5.4 obligation

- source reason 与 resolution 分离。
- reply 本身履行本人对 reply target 的 pending obligation；不依赖 input_ref。
- thread_handle(note?) = resolution handle；保存 note，不创建 entry。
- branch = resolution branch。
- leave/remove/role_change/close 转 withdrawn + 对应 resolution。
- claim enum 先存在；真正由 work_claim 写入到 T4。
- 同一 obligation 的终结 CAS 至多一次。
- input_ref stale 必须零副作用。

### 5.5 旧 ReplyThreadState

产品语义必须只有 thread_post 一套。

可保留内部兼容 wrapper 以缩小代码扰动，但 wrapper 只能调用同一 transition；不得继续有第二套 recipient/receipt 规则。旧“input 必须等于 reply target”必须删除。

### 5.6 C2 Gate

覆盖 PRD §12.2 相关并发和 §12.3 场景 1–7 的 Thread 部分：

- ask / reply / pair / ask:[]。
- direct reply 无 Todo input 也正确履行。
- input_ref stale 零写入。
- handle note。
- close / role change / leave 的 withdrawn resolution。
- reply vs handle / branch 并发不重复履行。
- summary 时间线与按需全文。
- exact-SHA CI。

---

## 6. M1 — Memory 读取闭合

这是 T4 前的独立小阶段。

- memory_get(code, revision?)。
- author 可读任一存在 revision。
- 非 author 直接读取只允许 public current revision。
- Thread / Task context 对 pinned private revision 的读取走其上下文授权，不放宽直接 memory_get。
- 不改变 standalone create/update/share/delete 的既有经济 policy。
- 不顺手做 Task harness pin；那属于 T4。

Gate：current/historical、soft-delete pinned、public unshare、非 owner historical 越权。

---

## 7. T4 — Task 按范式修正

T4 只有在 C0–C2 + M1 全部 exact-SHA gate 完成后才能生成具体工单；不得现在锁未来代码 BASE。

### 必须实现

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
   - output_hash = canonical payload + fixed Memory bindings；
   - request_key 幂等以完整 output_hash 判同/异。
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

## 8. T5 — Todo / Work Context / 可恢复性

Todo 只由持久状态计算：

- reply
- partner_request
- deliver
- assess

无事项时按 PRD wait / poll_after。

thread_get 返回：

- 摘要时间线分页；
- entries 按需全文；
- membership / role；
- my pending；
- child links；
- lineage anchor chain（read-only）。

partner_list 返回伙伴与共同开放 Thread。

履约记录必须能从 receipt resolution + Task terminal facts 计算；不得自行发明 reputation score、排名或新的公开评价工具。若产品要额外暴露面，先改 PRD。

Gate：清除进程/会话局部状态，只凭 todo → work context 恢复并继续。

---

## 9. T6 — MCP / HTTP / Product Surface

- 工具名、参数、next_action 只来自 PRD §8。
- 所有写工具同一 envelope / error / idempotency。
- MCP 与 HTTP 调同一 service transition，不允许双实现。
- llms.txt / public docs / Web 只表达已成立机制，不新增规则。
- 结构字段与 Agent 内容严格分开。

Gate：MCP / HTTP 等价；核心场景不依赖 UI。

---

## 10. T7 — 全量合规验收

最终同时通过：

1. PRD §12 全场景。
2. Task 1.0 全回归与账本不变式。
3. migration fresh + upgrade rehearsal。
4. 幂等 / concurrency / stale 随机压力。
5. 越权矩阵。
6. 进程状态清空后的恢复。
7. 两种异构运行时只凭 MCP 完成对话、群议、private Task。
8. 反向审计：逐个 public write action 指出 PRD 条款；无法指出来源的业务规则 = 不合格。
9. exact HEAD CI success + clean git status。

---

## 11. 每阶段工单固定模板

每个执行工单必须含：

- BASE：完整 SHA。
- Authority：kungfu.md / PRD exact blob 或 commit。
- Stage：只能一个。
- Allowed files：白名单。
- KEEP：本阶段绝不能破坏的机制。
- Required changes：逐条映射 PRD。
- Forbidden changes：不能顺手做什么。
- Negative tests：越权 / stale / rollback / idempotency / concurrency 中相关项。
- Stop conditions：遇到未定义语义停在哪里。
- Report：new HEAD、changed files、测试、CI run、未完成项。
- Gate：只有 exact SHA CI success 后下一阶段才能开工。

工单中不得出现“按 PRD 完成其余逻辑”“顺便补齐”“合理处理边界情况”这类把设计权交给执行 Agent 的措辞。

---

## 12. 第一个可执行工单边界

本计划完成后，只生成 C0：Partner / Membership / Key / Governance。

不提前加 final receipt CHECK/FK，不提前修 Child，不提前写 generic thread_post。
C0 收口并审计后，再基于它的 new HEAD 生成 C1。

这是防止第三次中断的硬控制点，不是进度建议。
