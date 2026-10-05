# Thread 开发计划

依据：docs/thread-prd.md。PRD 是 Thread 产品与机制的唯一依据；本计划只决定实施顺序，不得新增 PRD 未定义的对象、状态、权限或协作语义。

## 1. 执行原则

1. 从 main 的现有 Role / Memory 架构扩展，不复用任何已废弃 Thread 实验的业务实现。
2. 先修 Memory 原子，再建 Thread。
3. 业务原子只有 Role / Memory / Thread。
4. thread_roles 与 thread_memories 只是关系表，不升格为业务模型。
5. Thread 授权只在一个 service 层计算：local ThreadRole + Root creator tree governance。
6. repository 只做持久化，不判断产品权限。
7. Thread 内 post 必须原子完成 Memory create + timeline append。
8. Thread 操作审计复用现有 operation log，不新增 ThreadEvent。
9. MCP 与 HTTP 继续共用现有单一 tool registry。
10. 发现需要第四个协作对象、membership 状态机或 workflow 状态时，立即停止并回到 PRD。

执行顺序：

~~~
WO-T1 Memory 原子
   ↓
WO-T2 Thread 数据内核
   ↓
WO-T3 Thread service
   ↓
WO-T4 协议接入与完整验收
~~~

---

## WO-T1 Memory 原子契约修正

### 目标

把 Memory 从偏文档型条目修正为真正的信息原子，同时保持 owner、visibility、checksum、soft delete、Task live-reference 语义不变。

### 改动

memory_put：

- content 必填，trim 后至少 1 字符；
- title 可省略，缺省保存空字符串；
- tags 可省略，缺省保存 []；
- description 可选；
- 最大长度、敏感内容扫描、checksum 与 update 语义保持。

从现有 Memory create 路径提取 tx-safe 内部 primitive：

- caller 提供已验证 owner Role；
- 生成唯一 code；
- 使用同一 storage consumption policy；
- 写入 private / active Memory；
- 返回 Memory identity；
- 不自行提交外层事务。

standalone memory_put 与后续 thread_post 都必须复用这一个 create primitive。

### 验收

1. 短 content 可以创建 Memory；
2. title / tags 缺省稳定为 "" / []；
3. 旧式 Memory 创建、更新、share、unshare、delete 行为不变；
4. checksum 语义不变；
5. Task harness_refs 继续读取当前 Memory 内容；
6. 外层事务回滚时，不留下 Memory 或消费副作用；
7. 既有 Memory / Task 测试全绿。

只有 WO-T1 通过后才进入 Thread migration。

---

## WO-T2 Thread 数据内核

### 目标

只落一个新业务对象 Thread，以及两张关系表：

~~~
threads
thread_roles
thread_memories
~~~

不接 MCP，不做 Owner UI。

### threads

至少：

~~~
id / code
created_by_role_id
parent_thread_id?
anchor_memory_id?
status: open | closed
next_seq
created_at / updated_at
~~~

约束：

- Root：parent / anchor 同时为空；
- Child：parent / anchor 同时非空；
- parent / anchor 不可更新；
- code 唯一；
- Child 创建前 service 验证 anchor 是直接 parent 中 active Memory。

### thread_roles

只表达：

~~~
thread_id
role_id
permission: read | write | manage
~~~

约束：

- (thread, role) 唯一；
- 不持久化 invited / active / left / removed；
- 不持久化 read cursor。

Root creator 的 tree-wide governance 是 Thread service 的派生规则，不需要向每个 descendant 复制 ThreadRole。

### thread_memories

只表达：

~~~
thread_id
memory_id
seq
added_by_role_id
created_at
~~~

约束：

- (thread, memory) 唯一；
- (thread, seq) 唯一；
- seq 单调递增；
- 建立后不删除、不重排。

### Anchor 表达

Child anchor 不重复写入 child thread_memories。

anchor_memory_id 是 Child 的上下文边，thread_get 单独返回 anchor；Child timeline 只记录 Child 内真正新增 / 纳入的 Memory。

### Repository

只提供最小 persistence primitives：

- find / lock Thread；
- list visible candidate data；
- set / remove ThreadRole；
- append ThreadMemory；
- timeline page by after_seq；
- parent / ancestor / descendant 查询；
- active Memory / tombstone 查询。

repository 不判断 read/write/manage，不判断 promotion，不判断 parent audience gate。

### 并发验收

- 同 Thread 并发 append 不重复 seq；
- 同 Role 并发 set 最终只有一条 relation；
- 同 Memory 并发 include 最终只有一个 timeline relation；
- Thread next_seq 与 append 同事务提交。

---

## WO-T3 Thread service

### 目标

建立 Thread 的唯一业务规则层，并严格对应 PRD 的八个动作。

### Authorization

只实现：

~~~
CanReadThread
CanWriteThread
CanManageThread
CanReadMemoryInThread
~~~

事实来源：

- local thread_roles permission；
- Root creator 对整棵 tree 的派生 manage；
- Thread private；
- Thread scoped Memory access；
- Memory 自身 owner / public 规则仅用于 Thread 外直接读取。

### 1. thread_create

一个动作同时创建 Root / Child。

Root：

- creator 自动 local manage；
- 可选初始 Roles。

Child：

1. lock parent；
2. parent 必须 open；
3. anchor 必须属于直接 parent 且 Memory active；
4. actor 至少 parent write；
5. 如初始 Roles 包含 parent 外 Role，actor 必须 parent manage；
6. 创建 Child；
7. Child created_by 自动 local manage；
8. 写入其余初始 Role relations。

anchor 不写入 child timeline。

### 2. thread_get

返回：

- identity / status；
- parent / anchor；
- caller effective permission；
- Roles + permission；
- head_seq；
- timeline(after_seq, limit)；
- caller 可见 Child summaries。

非成员访问真实 code 与不存在 code 对外表现一致。

### 3. thread_list

列出 caller 可访问的 Thread。

Root creator 必须能够看到其 tree descendants，即使没有显式 child ThreadRole。

### 4. thread_post

事务：

1. lock Thread；
2. 验 open + write；
3. 调 WO-T1 tx-safe Memory create；
4. 分配 seq；
5. append ThreadMemory；
6. commit。

失败不得留下孤立 Memory、消费记录或 ThreadMemory。

### 5. thread_include

允许：

A. owner inclusion
- actor owns Memory；
- actor 对 target 有 write；
- target open。

B. result promotion
- Memory 已存在于 source descendant Thread；
- target 是 source 的 ancestor；
- actor manage source；
- actor manage target；
- target open。

不允许：

- 非 owner 把 public Memory固化进 unrelated Thread；
- descendant → unrelated Root Thread promotion；
- lateral sibling promotion。

### 6. thread_role_set

manage 可以新增 Role 或改 permission。

Child 新增 parent 外 Role：

- actor 必须 child manage；
- actor 同时必须直接 parent manage。

closed：

- 不允许新增 Role；
- 不允许升权；
- 允许对既有 Role 降权。

### 7. thread_role_remove

manage 可移除普通 Role。

移除后，该 Thread scoped private Memory access 立即失效。

Root creator 的 tree governance 不通过 child ThreadRole 表达，因此不能被 child role_remove 消除。

### 8. thread_set_status

manage：

~~~
open ↔ closed
~~~

closed：

- 可读；
- 禁止 post / include；
- 禁止创建 Child；
- 禁止新增 Role / 升权；
- 允许 role_remove / 降权。

parent close 不级联 descendants。

### 权限反例验收

必须覆盖：

- read 不能 post；
- write 不能改 Role / status；
- child local manage 不能单独拉 parent 外 Role；
- sibling Thread 权限互不穿透；
- parent Role 被移除后立即失去 parent scoped read；
- Root creator 仍可治理 descendant；
- tombstone 不能作为新 anchor；
- 非 owner public Memory 不能被固化进 unrelated Thread；
- result promotion 只能 descendant → ancestor；
- closed 后仍可撤权。

---

## WO-T4 协议接入与完整验收

### 目标

只把 PRD 八个语义动作接入现有 tool registry：

~~~
thread_create
thread_get
thread_list
thread_post
thread_include
thread_role_set
thread_role_remove
thread_set_status
~~~

不单独暴露：

- child_create；
- ThreadRole CRUD；
- ThreadMemory CRUD；
- read cursor；
- parent / anchor mutation。

### thread_get 返回

至少：

- Thread identity / status；
- parent / anchor；
- effective caller permission；
- Roles；
- head_seq；
- timeline page；
- visible Child summaries。

增量继续使用：

~~~
thread_get(after_seq=N)
~~~

平台不持久化每个 Role 的阅读进度。

### Rate limit

thread_post 使用独立 Thread write bucket，不复用 standalone memory_put 的 push bucket。

阈值在实现前按正常会话吞吐确定，集中配置，不散落硬编码。

### 完整验收

必须同时通过：

1. PRD §14 全场景；
2. WO-T3 权限反例；
3. MCP / HTTP 同一动作行为一致；
4. tool schema 与 service 约束一致；
5. fresh DB 全迁移；
6. 全仓测试；
7. seq / role_set / duplicate include 并发测试；
8. thread_post 故障回滚；
9. Memory / Task 既有行为回归；
10. 对外文档与最终工具面一致。

通过后才更新 README / llms / skill / CHANGELOG / VERSION 等产品表达。

## 2. 禁止回流

不得重新引入：

- Message；
- Subthread；
- ThreadEvent；
- membership lifecycle；
- invite state；
- last_read_seq；
- delivery / review；
- handoff；
- next_actor / next_action；
- controller；
- workflow node / branch；
- Memory snapshot / revision。

## 3. 完成判据

Thread 第一版必须能用一句话解释：

> Role 产生 Memory；Thread 以 private Role scope 组织 Memory 时间线；Memory 可以展开 Child Thread；Thread 权限控制谁能看、写和改变会话边界；Child 结果可以受控回到祖先主线。

如果实现后仍需要额外业务概念才能解释基本协作，视为模型偏离，不进入合并。
