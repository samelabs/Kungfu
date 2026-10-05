# Thread PRD

> 本文是 Kungfu Thread 的产品与机制唯一依据。进入实现前必须先通过本文审计；实现不得自行补充本文未定义的协作语义。若实现需要新增状态、权限或对象，先修改并重新审计本文。

## 1. 目标

Thread 为 Role 与 Memory 提供一个默认私有、可递归展开的协作会话边界。

Kungfu 已有两个基础原子：

- **Role**：行为主体。当前实现身份映射为 Bot。
- **Memory**：由 Role 产生并拥有的信息原子；默认 private，可显式 public。

Thread 不改变 Role 与 Memory 的所有权关系。Thread 负责把多个 Role 与 Memory 纳入同一个受控会话，使 Memory 在明确的成员边界、时间顺序和父子会话关系中流动。

核心关系：

```
Role ──produces──> Memory

Thread
├─ Roles
├─ ordered Memories
└─ child Threads
```

Thread 本身也可以有父 Thread，因此“主线”和“子对话”使用同一个模型。

## 2. 核心定义

### 2.1 Thread

Thread 是一个 **private conversation scope**。

它定义：

1. 当前会话有哪些 Role；
2. 当前会话包含哪些 Memory；
3. Memory 在当前会话中的顺序；
4. 当前会话是否由父 Thread 中某个 Memory 派生；
5. 当前会话的生命周期与每个 Role 的阅读位置。

Thread 默认且固定为 private。第一版不提供 public Thread。

### 2.2 Root Thread 与 Child Thread

Root Thread：

- 没有 parent；
- 没有 anchor Memory；
- 表达一条协作主线。

Child Thread：

- 必须有且只有一个 parent Thread；
- 必须锚定 parent Thread 中的一条 Memory；
- 表达围绕该 Memory 独立展开的局部会话；
- 仍然是完整 Thread，拥有自己的 Role、Memory、时间线和生命周期。

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

同一条 Memory 可以派生多个 Child Thread。

parent 与 anchor 在 Thread 创建后不可更改。由此保证 Thread 层级天然无环。

### 2.3 Memory 与 Thread 的关系

Memory 的 producer / owner 永远是产生它的 Role，Thread 不改变该关系。

Memory 可以被纳入多个 Thread；Thread 不复制 Memory 内容，只保存 Memory 与该 Thread 的会话关系和顺序。

```
Role A ─produces→ M1

M1 ∈ T0
M1 ∈ T1
```

这表示同一个信息原子进入了两个不同的私有会话范围，不产生两份 Memory。

同一 Memory 在同一 Thread 中只出现一次。需要形成新的时间线节点时，应产生新的 Memory。

第一版沿用 Kungfu 当前 Memory 的“活引用”语义：Thread 读取 Memory 时读取其当前有效内容。Memory 更新不产生 Thread 内快照；Memory 被删除后，Thread 保留该时间线关系，但内容不可再读，呈现为已删除占位。Thread 不引入 Memory revision。

## 3. 权限模型

权限只落在 **Thread ↔ Role** 关系上。Thread 不修改 Memory 的 owner 或 visibility。

每个 Thread Role 只有三档权限：

- **read**：进入 Thread，读取 Thread 中的 Memory 与可见的 Child Thread 元数据；
- **write**：包含 read；可以把自己拥有的 Memory 纳入 Thread，可以在 Thread 中产生新的 Memory，可以围绕 Thread 中的 Memory 创建 Child Thread；
- **manage**：包含 write；可以增删 Role、调整权限、关闭/重新打开 Thread，并在规则允许的情况下扩大 Child Thread 的受众。

Thread creator 创建时自动成为 manage，不能被移除或降权。第一版不支持 creator transfer。

### 3.1 Thread 对 private Memory 的权限穿透

Memory 默认 private，Thread 默认 private。两者通过成员关系形成受控共享域。

读取规则：

```
can_read(Role R, Memory M) =
    R owns M
    OR M is public
    OR exists Thread T:
         R has active ThreadRole in T
         AND M is contained in T
```

第三条只是一条额外合法访问路径：

- 不把 Memory 改成 public；
- 不转移 Memory ownership；
- 不授予 Thread 外访问权；
- Role 离开 / 被移出 Thread 后，该 Thread 路径立即失效。

直接 `memory_get` 的既有权限语义保持不变。Thread 内的 scoped read 由 Thread 的读取接口完成，不通过无上下文的 `memory_get` 绕开边界。

### 3.2 Child Thread 是新的 private scope

Child Thread 的 Role 集合独立于 parent，可以更小，也可以包含 parent 中不存在的 Role。

但扩大受众属于披露行为，必须受控：

- **只使用 parent 已有 Role 创建 Child Thread**：parent 的 write 或 manage 可以执行；
- **Child Thread 纳入 parent 之外的新 Role**：必须由 parent 的 manage 执行；
- Child Thread 的 anchor Memory 因创建动作进入 Child Thread 的会话上下文，因此 Child Thread Role 可通过 Child Thread 读取该 anchor；
- Child Thread 不能因此读取 parent 的其他 Memory。

这条规则防止普通 writer 通过创建 Child Thread 把 parent 中的 private Memory 泄露给外部 Role。

### 3.3 Memory 的再次纳入

一个 Role 可以把以下 Memory 纳入自己有 write 权限的 Thread：

1. 自己拥有的 Memory；
2. public Memory。

对“自己不拥有、仅通过某个 Thread 获得读取权”的 private Memory，第一版不提供任意跨 Thread 转发能力。

唯一例外是 **Child Thread anchor**：在父 Thread 中可见的 Memory 可以按 §3.2 作为子会话锚点进入 Child Thread；若 Child Thread 扩大受众，必须由 parent manage 执行。

这样区分：

- **读取权**：可以消费当前 scope 的 private Memory；
- **传播权**：不能因为读取权自动获得；
- **受控下钻**：通过 Child Thread anchor 在父级治理下局部披露。

## 4. Creator 与全景

Thread creator 对自己创建的 Thread 拥有完整视图和 manage 权限。

Root Thread creator 同时是整个 Thread tree 的治理者：

- 创建任何 Child Thread 时，Root creator 自动成为该 Child Thread 的 manage Role；
- Root creator 因而可以读取并管理所有 descendants；
- Child Thread 可以拥有额外 Role，但不能对 Root creator 隐藏。

“私有子对话”表示对子 Thread 之外的普通 Role 私有，不表示对该协作树的 Root creator 私有。

如果需要一个连 Root creator 都不可见的独立会话，应创建新的 Root Thread，而不是 Child Thread。

## 5. 时间线与状态

### 5.1 Timeline

Thread 中 Memory 以单调递增的 `seq` 排序。

```
T0
#1 M1
#2 M2
#3 M3
```

`seq` 只表达“该 Memory 何时进入这个 Thread”，不等于 Memory 的创建时间。

ThreadMemory 关系一旦建立，不重新排序。

### 5.2 阅读位置

每个 ThreadRole 维护自己的 `last_read_seq`。

```
thread head = 31
Role B last_read_seq = 24
=> B 的未读范围是 25..31
```

这是 Thread 第一版唯一需要的“即时会话状态”。通知、mention、在线状态不是核心模型，第一版不定义。

### 5.3 生命周期

Thread 只有：

- `open`
- `closed`

open：允许符合权限的成员写入、改成员、创建 Child Thread。

closed：保留全部读取能力，禁止新增 Memory、成员变更和 Child Thread 创建。

manage 可以 reopen。关闭 Child Thread 不影响 parent；关闭 parent 不自动关闭 descendants，但 parent 关闭后不能再创建新的 Child Thread。

第一版不提供 Thread 删除。

## 6. 数据不变量

实现必须保证：

1. 每个 Thread 有且只有一个 creator；
2. creator 始终是该 Thread 的 manage Role；
3. Root creator 显式存在于每个 descendant Thread，权限为 manage；
4. Child Thread 必须有 parent 与 anchor；Root Thread 两者都为空；
5. Child Thread 的 anchor Memory 必须存在于 parent Thread；
6. parent / anchor 创建后不可修改；
7. 同一 `(thread, role)` 只有一个有效成员关系；
8. 同一 `(thread, memory)` 只有一个时间线关系；
9. ThreadMemory `seq` 在 Thread 内唯一且单调递增；
10. 非 Thread Role 不得通过 Thread 读取其 private Memory；
11. ThreadRole 移除后，Thread 路径授权立即失效；
12. read 不能写；write 不能管理成员或扩大 Child Thread 受众；manage 才能治理受众；
13. scoped read 不产生 Memory ownership、public visibility 或跨 Thread 传播权；
14. closed Thread 不允许任何结构性写入；
15. Memory 的删除不删除 Thread / ThreadMemory，只使该 Memory 内容在该位置成为不可读占位。

## 7. 第一版场景

### 7.1 私有多人主线

A 创建 T0，纳入 B、C。

A/B/C 在各自权限允许时产生 Memory 并进入 T0。非成员 X 看不到 T0，也不能通过 T0 读取其中 private Memory。

### 7.2 局部子对话

T0 中 M2 需要 A、B 单独展开。

B（write）以 M2 创建 T1，只纳入 parent 已有 Role A、B。T1 独立推进，C 不读取 T1 内容。

### 7.3 局部引入外部 Role

T0 中 M2 需要外部 Role X。

普通 writer 不能直接把 X 纳入子会话。parent manage 创建 T1 或批准其受众，T1 Roles 为 A、B、X。X 可读 M2 与 T1 Memory，但不能读取 T0 的其他 Memory。

### 7.4 并行协作

同一个 M2 可以分别派生 T1、T2。两者拥有独立 Role 集合、时间线和阅读状态，互不泄露。

### 7.5 结果回主线

Child Thread 中某 Role 产生结果 Memory R。

若 R owner 同时对 parent 有 write，则该 owner 可直接把 R 纳入 parent。

若 R owner 不在 parent 或没有 write，则不能由其他普通成员代替其跨 scope 转发；需要先由 parent manage 调整 parent Role / 权限，由 R owner 自己纳入。第一版不引入“代替作者传播 private Memory”的机制。

### 7.6 Agent / 人接续

新的 Role 被加入某 Thread 后：

- 可以读取该 Thread 已有完整时间线；
- 可以读取该 Thread 中 private Memory；
- 从自己的 `last_read_seq` 继续；
- 不因此获得 parent / sibling / unrelated Thread 的访问权。

## 8. 非目标

第一版 Thread 不承担：

- Task、deadline、assignee、approval、delivery 等业务义务；
- 工作流步骤与 next actor；
- mention / attention routing；
- notification delivery；
- reaction；
- public Thread；
- Thread 搜索与推荐；
- Memory revision / snapshot；
- creator transfer；
- 任意 private Memory 的跨 Thread 转发；
- 多 parent Thread / DAG。

这些能力如未来需要，应建立在本模型之上，不能反向改变 Thread 的会话与权限边界。

## 9. 实现边界

第一版实现只能围绕以下持久关系建立：

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

字段名可按仓库规范调整，但不得改变本 PRD 的关系和不变量。

第一阶段不改 Memory 表结构。

## 10. 协议语义要求

实现接口名称在开发工单中确定；PRD 只规定语义：

- 创建 Root Thread；
- 创建 Child Thread；
- 读取 Thread 与其 timeline；
- 列出当前 Role 可见 Thread；
- 纳入 / 移除 / 调整 Thread Role；
- 把允许传播的 Memory 纳入 Thread；
- 更新 read cursor；
- close / reopen Thread。

所有 Thread 读取必须在服务端完成成员鉴权和 Memory scoped authorization。

非成员读取 private Thread 时按“不可见对象”处理，不泄露 Thread 是否存在。

## 11. 开发前退出条件

只有同时满足以下条件才能开始实现：

1. 本 PRD 的对象关系无冲突；
2. 权限穿透与传播边界无歧义；
3. 父子 Thread 的治理与 Root creator 全景成立；
4. §7 六个场景全部只使用本 PRD 的原语即可完成；
5. 权限反例无法绕过 §6 不变量；
6. 与现有 Memory ownership / public-private 语义兼容；
7. 不需要为了实现再发明第四个协作业务对象。
