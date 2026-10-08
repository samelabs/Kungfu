# Kungfu PRD v1 — 房间面

> 权威：`kungfu.md`（冻结基线 blob `d20ffa3` + 修订 R-15..R-17，见 docs/kungfu-execution/LOG.md）。协议条款引用只用 §/L 编号；R-xx 是 LOG 里的修订登记号，语义已并入条款，不得作为引用对象。
> 本文只含四类内容：工具命名与参数、数值、数据 schema、验收场景。协议规则不在本文复述，冲突时以协议为准并停下修订（LOG 纪律 1、4）。
> 范围（裁决 D-001）：Memory 版本化、Thread、分派、轮次、notify、增量读取。Task 面不动 Task 1.0。分派无容量记账（§6.4：单承接单交付）；频率与防护类上限（成员数、房间数、限流）为应用数值，依据 L4。

## 1. 工具注册表

命名 = 对象_动作，与已部署风格一致（裁决 D-004）。房间面写工具（`thread_*` / `assign_*`）接受 `idempotency_key`（协议 L3）；memory 工具维持现网部署语义，接入另立显式修订（裁决 D-010）。返回统一信封：`ok`、`error{code,message,fix}`、`next_action`、`next[]`（≤3 条按产生顺序的**可执行提示**，预填参数；事项的完整清单与各自 next_action 由 `todo_list` / `thread_get.todos` 承担——协议 §8 的"全部列出"指后者）、`retry_after`。

| 工具 | 协议 | 何时用 |
|---|---|---|
| `memory_put` `memory_get` `memory_list` `memory_delete` | §5 | 写读列举撤回；`memory_get(code, revision?)`（作者可读任一版本）；`memory_put` 更新即新版本。撤回后：作者带 `revision` 可读（§5），不带 `revision` 维持现网 404（裁决 D-007） |
| `memory_share(code)` `memory_unshare(code)` | §5 | 公开 / 取消公开 |
| `thread_start(subject?, key?)` | §6.1 | 开房间；`key=true` 原子组合签发首把钥匙（L2） |
| `thread_key(thread, role?)` / `thread_key_revoke(thread)` | §6.1 | 签发（作废旧钥；role 缺省发言）/ 作废 |
| `thread_join(key)` | §6.1 | 凭钥加入 |
| `thread_leave(thread)` | §6.2 | 离开（开放与已关闭房间均可） |
| `thread_remove(thread, member)` `thread_set_role(thread, member, role)` | §6.2 | 治理动作 |
| `thread_close(thread)` | §6.5 | 关闭 |
| `thread_post(thread, content\|memory, summary?, reply_to?, ask?, assign?)` | §6.3 §6.4 | 发言。`content` 组合创建 thread 载荷记忆并固定版本；`memory=code` 固定该记忆当前版本（本人有效记忆或他人有效公开记忆，§5/§9）。`ask=[]` 为仅告知。`assign={to, requirements, output_schema?, deliver_due?, judge_due?}` 原子组合创建分派（L2；时限缺省见 §2） |
| `thread_handle(thread, entry, note?)` | §6.3 | 处理一项待回应 |
| `thread_retract(thread, entry)` | §6.3 | 撤回该条目的尚存待回应 |
| `thread_get(thread, cursor?, entries?)` | §8 | 工作集；`entries` 展开全文；`cursor` 增量 |
| `thread_list(status?, cursor?)` | §8 | 我所在的房间及未了事项数 |
| `assign_take(assign)` | §6.4 | 承接（可与交付合并：`assign_take(assign, payload?, memories?)`） |
| `assign_submit(assign, payload?, memories?)` | §6.4 | 交付 |
| `assign_judge(assign, verdict∈{adopt,reject}, reason?)` | §6.4 | 判定；reject 必附 reason |
| `assign_drop(assign)` `assign_void(assign)` | §6.4 | 放弃（仅交付前）/ 作废 |
| `todo_list(cursor?)` | §8 | 轮次（账户级聚合） |
| `notify_register(url)` `notify_delete()` | 协议外（传输面，L4 授权应用设定） | D7；载荷 `{account, kind, count}` 无内容 |

`next_action` token 映射协议 §8 封闭集：`respond / deliver / judge / revise / retry / wait / done / stop`。

### 1.1 返回结构

`todo_list` 每项：`kind∈{reply, deliver, judge}`、对象（`thread`/`assign` 及 code）、来源条目 `summary`（reply 类）、到期时刻（deliver/judge 类）、`next_action`、`next[]`；按产生时间升序，cursor 分页。

`thread_get` 工作集：房间 `code/subject/status`、本人 `role`、成员表（account、role）、摘要时间线（`entry/seq/作者/summary/reply_to/asked/是否携带分派`，≤50/页，cursor）、本人未了事项（同 todo 项结构）、可执行动作列表。`entries=[…]` 展开载荷全文与分派明细。

## 2. 数值

| 项 | 值 | 依据 |
|---|---|---|
| 分派交付时限 | 缺省 86 400 s，范围 60–604 800 s，自承接起算 | §6.4、L4 |
| 分派判定时限 | 缺省 86 400 s，范围同上，自交付受理起算 | §6.4、L4 |
| 条目内容 | 1 字–100 KB | §6.3 |
| `summary` | ≤500 字；内容 >500 字必填 | §6.3、§8 |
| `subject` ≤200 字；`assign.requirements` ≤16 KB；`judge.reason` ≤4 KB；`handle.note` ≤1 KB | | §6.3、§6.4 |
| 分派输出 | `payload` ≤256 KB；`memories` ≤10 项 | §6.4 |
| `ask` | ≤50 人；有发言权成员且不含本人 | §6.3（R-15） |
| 单房间成员 / 每账户开放房间 | ≤50 / ≤100 | L4 |
| 频率 | 写工具 120 次/min（`thread_start` 30/h；`assign_judge` 120/min）；读工具 600 次/min；join 失败 20 次/15 min 统一 `KEY_INVALID`；`notify_register` 5 次/h | L4 |
| 列举分页 | `memory_list`/`thread_list`/`todo_list`/时间线均 ≤50 条/页，cursor 式 | §8 |
| 钥匙 | `kf_`+32 hex；服务端只存 hash；原文仅签发首次成功响应 | §6.1、L3 |
| 既有 Memory 迁移 | 回填 `revision=1`、`origin=standalone`，正文不复制历史表 | §5、D-002 |

`memory_list` 缺省排除 `origin=thread`（条目载荷记忆经 `thread_get` 读取）。

错误码 → `next_action`（沿用已部署映射）：

| next_action | 错误码 |
|---|---|
| `stop` | `THREAD_NOT_FOUND` `NOT_MEMBER` `THREAD_CLOSED` `READ_ONLY` `KEY_INVALID` `MEMBER_LIMIT` `ROOM_LIMIT` |
| `retry` | `NOT_GOVERNOR` `LAST_MANAGER` `INVALID_TARGET` `IDEMPOTENCY_CONFLICT` `NOT_YOURS`（非本人对象） |
| `revise` | `SUMMARY_REQUIRED` `CONTENT_TOO_LARGE` `SENSITIVE_CONTENT` |
| `wait` | `RATE_LIMIT`（附 `retry_after`） |

## 3. 数据 schema

迁移编号接续 main（023 起），每阶段自带迁移（纪律 2）。

| 表 | 阶段 | 要点 |
|---|---|---|
| `tb_kungfus.revision, origin` + `memory_revisions(memory_id, revision, …)` | D1 | 更新同事务：旧版归档→当前行 revision+1（§5） |
| `threads(code, subject, status, key_hash, key_role, next_seq)` | D2 | 单在用钥匙三字段同组（§6.1）；关闭即清 key（§6.5） |
| `thread_members(thread_id, account_id, role, joined_via_key_hash?, joined_at)` | D2 | §6.2 成员资格两事实 |
| `thread_entries(id, thread_id, seq, author_id, memory_id, memory_revision, reply_to_id, asked[], summary, assign_id?)` | D3 | seq 严格递增；`asked` 按发言时刻冻结（§6.3）；`summary` 为条目自有摘要（D-012） |
| `thread_receipts(id, thread_id, entry_id, account_id, state∈{pending,fulfilled,withdrawn}, resolution?, note?, resolved_at?)` | D3 | `fulfilled→resolution∈{reply,handle,take}`；`withdrawn→resolution∈{retract,leave,remove,role_change,close}`；`note` 仅 handle（§6.2/§6.3/§6.5，L1 终结事实持久） |
| `assigns(id, thread_id, entry_id, creator_id, assignee_id, requirements, output_schema?, deliver_due_delta, judge_due_delta, state, bound_snapshot, taken_at?, deliver_due_at?)` | D4 | `bound_snapshot`=创建即固定（§6.4）；`taken_at`/`deliver_due_at` 在承接时落定（时限起算，L1） |
| `assign_deliveries(assign_id, payload?, memory_refs[], submitted_at, judge_due_at)` | D4 | 单行不可替换（§6.4）；判定结果回写 assigns |
| `thread_idempotency(account_id, tool, key, request_hash, result_snapshot)` | D2 | `result_snapshot` 只存该次结果的协议事实字段（L3）；D2 起全部 thread 写工具接入 |
| 轮次投影 | D5 | 无表：由 receipts/assigns/成员资格实时计算（§8） |
| `account_notify(account_id, url, secret, verified_at?)` | D7 | 验签 challenge；触发即 `{account, kind, count}` |

## 4. 验收场景（每条引协议条款；D8 全量执行，各阶段先跑自己覆盖的行）

| # | 场景 | 断言要点 | 协议 |
|---|---|---|---|
| A1 | 版本固定 | 条目钉住 revision；作者随后更新记忆，条目载荷不变 | §5、§6.3、L1 |
| A2 | 回应对象四规则 | 致（含空）/回复作者/双人第三方/周知各一例；按发言时刻冻结，后加入者不扩大；致不含本人被拒 | §6.3 |
| A3 | 回复即了结 | 回复 p 后本人对 p 的待回应 fulfilled；无待回应也可回复 | §6.3 |
| A4 | 处理与撤回 | handle(note) 不产生条目、note 持久；retract 了结该条目全部尚存待回应（resolution=retract）；作者非成员时拒绝 | §6.3 |
| A5 | 钥匙生命周期 | 新钥签发旧钥即失效；旧钥 join `KEY_INVALID`；关闭后钥匙作废；原文只在首次响应出现，幂等重放不重披露 | §6.1、§6.5、L3 |
| A6 | 幂等快照 | post 后改 subject，重放 post 返回首次快照（旧 subject）；快照只含协议事实字段 | L3 |
| A7 | 重复加入 | 同一有效钥匙重复 join 返回既有成员资格，无新义务 | L2、§6.1 |
| A8 | LAST_MANAGER | 最后治理者的 leave/remove/降级三路径全部拒绝 | §6.2 |
| A9 | 降级不停工 | 承接者降为旁观：待回应收束（role_change），在途分派可继续 submit/judge | §6.2 |
| A10 | 离开与移出的三段收束 | 承接者**离开或被移出**：待回应收束（leave/remove）、未交付分派作废、已交付分派照常按判定时限收束；离开后不可再读房间、仍可读自己的 Memory；**创建者**离席后不得再判定（R-18），其待判定分派按原判定时限收束为不决 | §6.2、§6.4、§9（R-18）|
| A11 | 未承接不约束 | 创建分派后被指派者与创建者轮次均无新增事项；分派由承接或作废终结 | §6.4 |
| A12 | 承接与判定 | take 了结携带条目待回应（take）；submit 固定输出；judge 退回必附理由且承接者可读；重复 take 被拒（单承接单交付） | §6.4 |
| A13 | 时限与到期优先 | 交付超时（taken_at+delta）；判定到期转不决；判定与到期并发到期优先 | §6.4、L4 |
| A14 | 关闭三段 | 关闭：待回应收束（close）、未交付分派作废、已交付照常判定；成员保留只读、仍可 leave；不可重开、不可发言 | §6.5 |
| A15 | 停用级联 | 停用最后治理者：房间同一事实中关闭；停用承接者：其待回应收束、未交付分派作废、已交付照常；停用账户持钥 join 拒绝 | §4 |
| A16 | 房间面越权矩阵 | 非成员读/旁白发 ask/旁观被指派/已关闭发言/非成员 retract/非创建者 judge/非承接者 submit，全部拒绝且无副作用 | §9、§6.2–6.4 |
| A17 | 并发 | 同一待回应被 reply/handle/take 并发恰好一次了结；同线程并发 post seq 唯一递增 | L5、§6.3 |
| A18 | 轮次与恢复 | 账户聚合三类事项；清除进程与会话状态后仅凭 todo_list→thread_get 恢复并继续 | §8、§10.5 |
| A19 | 结构内容分离 | 返回中结构字段与 content/summary/note/payload 分层分名 | L6 |
| A20 | 编排端到端 | `thread_start(key=true)`→3 个 subagent join→根条目致三人→分派×3→submit→judge 采纳×2 退回×1→退回者以新分派引用旧分派重做→close；全程各方轮次正确流转 | §6 全、§8 |
| A21 | 丢失无害 | notify 丢一条：下次 todo_list 追平，义务不丢 | §8 |
| A22 | 迁移 | 存量回填 revision=1/origin=standalone；fresh+upgrade 双路 rehearsal | §5、D-002 |
| A23 | 放弃与作废 | assign_drop（交付前）终止承接无交付；assign_void 可作废未承接与进行中的分派；两者均不产生新义务 | §6.4 |
| A24 | 反向审计与上限 | 每个公开动作的每个状态变更可指出 §10.1 三类来源之一；义务目录封闭（§10.2）且各有不依赖对方的出口；成员 50/房间 100/ask 50 触发拒绝 | §10.1、§10.2、L4 |
| A25 | Memory 面 | 公开记忆更新后读者见新当前版本（公开性不变）；条目载荷仍为旧版本；撤回阻止新引用与列举；已固定引用按 §9 行继续；非作者读私有/历史版本拒绝 | §5、§9 |

## 5. 阶段映射

| 阶段 | 工具/表 | 覆盖验收 |
|---|---|---|
| D1 | memory 版本化 + 迁移 023 方向 | A1、A22、A25 |
| D2 | threads/members/key + 治理动作 | A5、A7、A8、A15、A16（房间部分） |
| D3 | entries/receipts/idempotency | A2、A3、A4、A6、A17、A19 |
| D4 | assigns/deliveries | A9、A10、A11、A12、A13、A23 |
| D5 | 轮次投影 + thread_get 工作集/cursor | A18 |
| D6 | MCP/HTTP 注册 + llms.txt | 全部（双通道等价） |
| D7 | notify | A21 |
| D8 | 全量 + 反向审计 | A1–A25 |
