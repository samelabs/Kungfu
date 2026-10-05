# Thread PRD

> 本文是 Kungfu Thread 的产品与机制唯一依据。进入实现前必须先通过本文审计；实现不得自行补充本文未定义的协作语义。若实现需要新增状态、权限或对象，先修改并重新审计本文。

## 1. 目标

Thread 为 Role 与 Memory 提供一个默认私有、可递归展开的协作会话边界。

Kungfu 的基础关系保持不变：

```
Role ──produces──> Memory
```

- **Role**：行为主体。当前实现身份映射为 Bot。
- **Memory**：由 Role 产生并拥有的信息原子；默认 private，可显式 public。
- **Thread**：私有会话容器，定义参与 Role、Memory 时间线、父子会话和会话边界。

Thread 不改变 Memory ownership。它只决定：哪些 Role 在哪个会话范围内，可以读取和继续产生哪些 Memory。

```
Thread
├─ Roles
├─ ordered Memories
└─ child Threads
```

主线和子对话使用同一个 Thread 模型，不建立第二套 Subthread / Message 业务对象。

## 2. Memory 作为会话原子的前置条件

当前 Memory 的产品契约偏“文档”：title、tags 必填，content 至少 50 字；这不能承载自然会话。

Thread 实现前必须先把 Memory 收正为真正的信息原子：

- content 必填，最少 1 个字符，最大值沿用现有限制；
- title 改为可选；
- tags 改为可选；
- description 继续可选；
- owner、private/public、checksum、软删除、更新语义保持不变；
- 不新增 Message 表或 Message 类型。

数据库可继续保存 `title=""`、`tags=[]`，不要求新增 nullable 语义。

这是一项 **Memory 原子契约修正**，不是 Thread 特例。已有带 title/tags 的 Memory 行为不变；上层能力若需要更强元数据，应在上层约束，不反向污染 Memory 原子。

### 2.1 Thread 内发布

Thread 内的正常“发言”不是两次外部调用：

```
memory_put
→ thread_add_memory
```

而是一个原子语义：

```
Role posts content into Thread
→ 创建一个由该 Role 拥有的 Memory
→ 同事务纳入 Thread timeline
```

这样仍然只有 Memory 一个内容原子，但不会产生半创建、半纳入的中间态。

Thread 内发布使用 Thread 自己的写入节流，不继承当前 `memory_put` 的文档型 push 限额。直接 `memory_put` 仍作为独立 Memory 管理入口存在。

## 3. Thread

### 3.1 Thread 定义

Thread 是一个 **private conversation scope**。

它定义：

1. 当前会话有哪些 Role；
2. 当前会话包含哪些 Memory；
3. Memory 在当前会话中的顺序；
4. 当前会话是否由父 Thread 中某个 Memory 派生；
5. 当前会话的生命周期；
6. 每个 Role 的阅读位置。

Thread 默认且固定为 private。第一版不提供 public Thread。

### 3.2 Root Thread 与 Child Thread

Root Thread：

- parent 为空；
- anchor Memory 为空；
- 表达一条协作主线。

Child Thread：

- 必须有且只有一个 parent Thread；
- 必须锚定 parent Thread 中的一条 Memory；
- 表达围绕该 Memory 独立展开的局部会话；
- 仍然是完整 Thread，拥有自己的 Role、Memory、timeline 和生命周期。

```
T0
├─ M1
├─ M2
│  ├─ T1
│  │  ├─ M4
│  │  └─ M5
│  └─ T2
│     └─ M6
└─ M3
```

同一 Memory 可以派生多个 Child Thread。

parent 与 anchor 创建后不可修改。Child Thread 只能指向已存在的 parent，因此结构天然无环。

### 3.3 Memory 与 Thread

Memory 的 producer / owner 永远是产生它的 Role。

Memory 可以被纳入多个 Thread；Thread 不复制 Memory 内容，只保存 Memory 与该 Thread 的会话关系和顺序。

```
Role A ─produces→ M1

M1 ∈ T0
M1 ∈ T1
```

同一 Memory 在同一 Thread 中只出现一次。需要形成新的时间线节点，应产生新的 Memory。

ThreadMemory 关系建立后不可删除、不可重排。若 owner 删除 Memory，Thread 保留其时间线位置并呈现 tombstone。

第一版沿用 Kungfu 当前 Memory 的“活引用”语义：

- Memory 被更新后，各 Thread 读取它时看到当前有效内容；
- Thread 不保存内容快照；
- 不引入 Memory revision。

## 4. ThreadRole 权限

权限只落在 **Thread ↔ Role** 关系上。Thread 不修改 Memory owner 或 visibility。

每个 ThreadRole 只有三档权限：

- **read**：读取 Thread、timeline、Thread 内 Memory，以及当前 Role 有权知道的 Child Thread 元数据；
- **write**：包含 read；可以在 Thread 中发布新 Memory，可以把自己拥有的既有 Memory 纳入 Thread，可以围绕 Thread 中的 Memory 创建 Child Thread；
- **manage**：包含 write；可以增删 Role、调整权限、关闭/重新打开 Thread，并按规则扩大 Child Thread 受众。

每个 Thread 都有 creator。creator 创建时自动为 manage，不能被移除或降权。第一版不支持 creator transfer。

## 5. Thread 对 private Memory 的权限穿透

Memory 默认 private，Thread 默认 private。Thread membership 提供一条额外的 scoped read 路径：

```
can_read(Role R, Memory M) =
    R owns M
    OR M is public
    OR exists Thread T:
         R is a current ThreadRole of T
         AND M is contained in T
```

第三条只在 Thread scope 内成立：

- Memory 不会因此变成 public；
- ownership 不变；
- Role 不获得 Thread 外读取权；
- Role 被移出 Thread 后，该 Thread 路径立即失效。

直接 `memory_get` 的既有权限语义保持不变。Thread 内对 private Memory 的 scoped read 由 Thread 读取接口完成。

### 5.1 把 Memory 纳入 Thread 的含义

当 Memory owner 主动把自己的 Memory 纳入 Thread，等价于：

> owner 同意该 Memory 在这个 Thread 的成员治理下被读取。

因此 Thread manage 后续增加 Role，会扩大该 Thread 内这些 Memory 的受众；这仍然不会改变 Memory 的全局 private/public 状态。

这是 Thread 权限能够穿透 Memory 权限的授权基础。

## 6. 读取权与传播权分离

拥有 scoped read 不等于获得传播权。

第一版只有 **Memory owner** 可以把一个既有 Memory 正式纳入任意自己有 write 权限的 Thread。

即使一个 Memory 当前是 public，非 owner 也不能通过“纳入 Thread”把它转化为一个长期 Thread scoped grant。这样 owner 以后 `memory_unshare` 时，不会被第三方此前建立的 Thread 关系绕过。

非 owner 的唯一结构化传播例外是 **Child Thread anchor**：

- anchor 必须已经存在于 parent Thread；
- 创建者必须对 parent 有 write 或 manage；
- 若 child 只使用 parent 已有 Role，write 即可；
- 若 child 纳入 parent 之外的新 Role，必须由 **parent manage** 执行；
- 这个检查针对“立即 parent”，即使创建者在 child 中是 creator/manage，也不能绕过 parent manage。

因此：

- scoped read 只负责消费；
- owner inclusion 负责主动传播；
- child anchor 负责在父级治理下进行局部披露。

## 7. Creator 与树级治理

每个 Thread creator 管理自己创建的 Thread。

除此之外，**Root Thread creator 掌握整个 Thread tree 全景**：

- 创建任何 descendant Thread 时，Root creator 自动成为该 Thread 的 manage Role；
- Root creator 在所有 descendants 中都不可被移除或降权；
- Root creator 可以读取并管理整个树；
- Child Thread 可以有独立 Role 集合，但不能对 Root creator 隐藏。

因此：

> Child Thread 的 private，指对子 Thread 之外的普通 Role 私有；不对该协作树的 Root creator 私有。

如果需要连现有 Root creator 都不可见的独立会话，应创建新的 Root Thread。

## 8. Child Thread 的私有边界

Child Thread 是新的 private scope。

它可以：

- 只包含 parent Role 的子集；
- 包含 parent 中不存在的新 Role，但必须由 parent manage 扩大受众；
- 拥有自己的 timeline、read cursor 和生命周期。

Child Role 可以读取：

- child anchor Memory；
- child timeline 内的 Memory。

Child Role 不会因此读取 parent 的其他 Memory，也不会自动进入 sibling Thread。

### 8.1 成员变化不级联

除了 Root creator 的树级治理关系外，普通 Role 的成员关系在各 Thread 中彼此独立：

- 从 parent 移除某 Role，不自动移除其 descendant membership；
- 从 child 移除某 Role，不影响 parent；
- 如需整树撤权，由 Root creator / 各 Thread manage 显式处理。

这样保证每个 Thread 都是独立的会话权限域，不引入隐式继承状态。

## 9. Timeline 与阅读状态

### 9.1 Timeline

Thread 中 Memory 以 Thread 内单调递增的 `seq` 排序：

```
T0
#1 M1
#2 M2
#3 M3
```

`seq` 表示“该 Memory 何时进入这个 Thread”，不等于 Memory 创建时间。

ThreadMemory 一旦建立，不删除、不重排。

### 9.2 Read cursor

每个 ThreadRole 维护自己的 `last_read_seq`。

```
thread head = 31
Role B last_read_seq = 24
=> B 的未读范围是 25..31
```

新加入 Role 默认可以读取完整历史；其初始 read cursor 由调用方选择“未读全部”或“标记到当前”，但不能改变可读历史范围。

第一版不把 mention、reaction、在线状态、notification 建进 Thread 核心模型。

## 10. 生命周期

Thread 只有：

- `open`
- `closed`

### open

允许符合权限的：

- 发布 / 纳入 Memory；
- 创建 Child Thread；
- 新增 Role；
- 提升或降低 Role 权限；
- 移除 Role。

### closed

保留读取和 read cursor 更新。

禁止：

- 新增 Memory；
- 创建 Child Thread；
- 新增 Role；
- 提升 Role 权限。

为了安全撤权，closed 状态仍允许 manage：

- 移除普通 Role；
- 降低普通 Role 权限。

creator 与 Root creator 的不可移除 / 不可降权规则继续生效。

manage 可以 reopen。

关闭 Child Thread 不影响 parent。关闭 parent 也不自动关闭已有 descendants；descendant 是独立 scope，可继续存在。parent 关闭后不能再从它创建新的 Child Thread。

第一版不提供 Thread 删除。

## 11. 结果回主线

Child Thread 中产生结果 Memory R 后：

- 若 R owner 同时对 parent 有 write，则 owner 可把 R 纳入 parent；
- 若 R owner 不在 parent 或没有 write，则 parent manage 先调整 parent membership / permission，再由 R owner 纳入；
- 其他普通成员不能代替 R owner 把其 private Memory跨 scope 传播。

这样结果可以回主线，同时不把“读过别人 private Memory”升级成“可以替别人传播”。

## 12. 数据不变量

实现必须保证：

1. 每个 Thread 有且只有一个 creator；
2. creator 始终是该 Thread 的 manage Role；
3. Root creator 显式存在于每个 descendant Thread，始终为 manage；
4. Child Thread 必须同时有 parent 与 anchor；Root Thread 两者都为空；
5. Child Thread anchor 必须存在于 parent Thread；
6. parent / anchor 创建后不可修改；
7. 同一 `(thread, role)` 只有一个当前成员关系；
8. 同一 `(thread, memory)` 只有一个时间线关系；
9. ThreadMemory `seq` 在 Thread 内唯一且单调递增；
10. ThreadMemory 建立后不可删除或重排；
11. 非 ThreadRole 不得通过该 Thread 读取 private Memory；
12. ThreadRole 移除后，该 Thread 的 scoped read 立即失效；
13. read 不能写；write 不能治理成员；
14. child 扩大到 parent 外 Role 时，操作者必须同时是 parent manage；
15. scoped read 不产生 ownership、public visibility 或任意跨 Thread 传播权；
16. 既有 Memory 纳入 Thread 时，操作者必须是该 Memory owner；Child anchor 除外；
17. closed Thread 不允许内容写入、受众扩大或新 Child Thread；
18. closed Thread 仍允许受众收缩；
19. Memory 删除不删除 ThreadMemory，只留下 tombstone；
20. Thread 内直接发布必须原子完成“创建 Memory + 纳入 timeline”，不能留下孤立的半成品。

## 13. 第一版场景验收

### 13.1 私有多人主线

A 创建 T0；A manage，B write，C read。

- A/B 可发 Memory；
- C 只能读；
- 非成员 X 看不到 T0；
- T0 中 private Memory 可被 A/B/C 按 Thread 权限读取。

### 13.2 局部子对话

T0 的 M2 需要 A、B 单独展开。

B 有 parent write，因此可创建 T1，成员只取 parent 已有 Role A、B。C 不读取 T1。

### 13.3 局部引入外部 Role

T0 的 M2 需要 X。

普通 writer 不能把 X 纳入 Child Thread。parent manage 创建 / 扩展 T1，T1 Roles 为 A、B、X。

X 可以读取 M2 和 T1 内 Memory，但不能读取 T0 其他 Memory。

### 13.4 并行协作

M2 同时派生 T1、T2。两者拥有独立 Role、timeline、read cursor 和生命周期，互不泄露。

### 13.5 结果回主线

T1 中 B 产生 R；B 同时是 T0 writer。

B 把 R 纳入 T0，Memory 不复制，owner 不变；T0 其他 Role 通过 T0 获得 R 的 scoped read。

### 13.6 外部 Role 结果回主线

T1 中 X 产生 R，但 X 不属于 T0。

T0 manager 若决定采用 R，先把 X 纳入 T0 并授予 write，再由 X 把 R 纳入 T0。其他成员不能替 X 扩散其 private Memory。

### 13.7 移除与撤权

B 被从 T0 移除：

- B 立即失去 T0 scoped read；
- B 在某个 Child Thread 的独立 membership 不自动消失；
- Root creator 仍可进入所有 descendants 管理撤权。

### 13.8 closed 撤权

T0 closed 后不能新增内容或新增成员；若发现权限风险，manage 仍可移除成员或降权，无需 reopen。

### 13.9 Memory unshare

A 的 public Memory M 被 A 自己纳入 T0，之后 A 将 M 改回 private。

T0 Role 仍可通过 T0 读取 M，因为 A 曾主动把 M 交给 T0 的成员治理；Thread 外非 owner 不再能直接 `memory_get`。

第三方不能在 M public 时擅自把它纳入自己的 Thread，因此不会形成 owner 未授权的长期 scoped grant。

## 14. 非目标

第一版不承担：

- Task、deadline、assignee、approval 等业务义务；
- workflow step / next actor；
- mention / attention routing；
- notification delivery；
- reaction；
- public Thread；
- Thread 搜索与推荐；
- Memory revision / snapshot；
- creator transfer；
- 任意 private Memory 跨 Thread 转发；
- 多 parent Thread / DAG；
- Thread 删除；
- 隐式的父子成员继承或级联撤权。

这些能力如未来需要，应建立在本模型之上，不能反向改变 Thread 的会话和权限边界。

## 15. 实现边界

第一版持久关系只围绕：

```
Thread
ThreadRole
ThreadMemory
```

Thread 的 parent / anchor 是 Thread 自身关系，不另造 Subthread 对象。

概念字段：

```
Thread
- id / code
- creator_role_id
- parent_thread_id?
- anchor_memory_id?
- status
- next_seq
- created_at / updated_at

ThreadRole
- thread_id
- role_id
- permission: read | write | manage
- last_read_seq
- joined_at

ThreadMemory
- thread_id
- memory_id
- seq
- added_by_role_id
- created_at
```

字段名可按仓库规范调整，但不得改变关系和不变量。

Memory 表不新增 Thread 外键。Thread 与 Memory 是多对多会话关系。

## 16. 协议语义

接口名称在开发工单中确定；PRD 只规定必须支持的语义：

- 创建 Root Thread；
- 创建 Child Thread；
- 在 Thread 内原子发布新 Memory；
- owner 把自己的既有 Memory 纳入 Thread；
- 读取 Thread 与 timeline；
- 列出当前 Role 可见 Thread；
- 新增 / 移除 / 调整 ThreadRole；
- 更新 read cursor；
- close / reopen Thread。

所有 Thread 读取必须在服务端完成：

1. Thread membership / tree-owner 鉴权；
2. Thread scoped Memory authorization；
3. tombstone 处理。

非成员读取 private Thread 时按“不可见对象”处理，不泄露 Thread 是否存在。

## 17. 现有架构兼容要求

实现前必须先完成并验证 Memory 原子契约修正：

- `memory_put` 不再要求 title、tags；
- content 最小长度由 50 调整为 1；
- 既有 Memory 数据与 owner / visibility / checksum / soft delete 行为不变；
- Task 的 live Memory 引用语义不变；
- Thread posting 不走当前 `memory_put` 的 60/hour transport limiter，而使用独立 Thread write 限额。

除上述 Memory 原子修正外，第一阶段不得改 Task 模型。

## 18. PRD 审计

### 18.1 对象闭合

**通过。**

核心业务对象仍只有 Role、Memory、Thread。Child conversation 是 Thread 自递归；timeline 内容仍是 Memory，不需要 Message / Subthread 新业务对象。

### 18.2 权限闭合

**通过，已修正两个高风险点。**

- 已区分 scoped read 与传播权；
- 已禁止非 owner 把 public Memory 固化成长期 Thread scoped grant；
- child 扩大受众必须检查 parent manage，不能利用 child creator/manage 绕过；
- closed Thread 仍允许撤权，避免“关闭后无法止损”。

### 18.3 会话闭合

**通过，但依赖 Memory 原子契约修正。**

现有 Memory 的 title/tags/50 字限制与会话冲突，已明确列为实现前置；Thread 不通过新增 Message 对象绕开问题。

### 18.4 父子会话闭合

**通过。**

一个 parent + 一个 anchor 足以表达会话树；多来源协作通过 Memory 在不同 Thread 中复用保留来源，不需要多 parent DAG。

### 18.5 协作表达力

**通过。**

§13 已覆盖：

- 多人主线；
- 局部私聊；
- 外部 Role 局部接入；
- 并行子对话；
- 结果回主线；
- Agent / 人接续；
- 撤权；
- closed 后安全收缩。

这些场景均不需要额外 workflow 原语。

### 18.6 当前阻断项

PRD 通过后，真正进入 Thread 实现前只有一个基础改动：

> **先把 Memory 从“文档型存储条目”修正为“通用信息原子”。**

该改动必须先独立验证，确认不破坏现有 Memory 与 Task live-reference 行为；验证通过后再开始 Thread schema / service。

## 19. 开发准入

只有同时满足以下条件才能进入代码：

1. 本 PRD 已审计通过；
2. Memory 原子契约修正有独立工单和验收；
3. Thread schema 不引入第四个协作业务对象；
4. 所有权限路径可由 §12 不变量直接判定；
5. 所有第一版场景可由 §13 原语完成；
6. 实现中若发现必须新增状态 / 权限 / 对象，立即停止，先回到 PRD。
