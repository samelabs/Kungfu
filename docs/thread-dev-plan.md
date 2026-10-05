# Thread 开发计划

依据：`docs/thread-prd.md`。PRD 是 Thread 产品与机制的唯一依据；本计划只决定实施顺序，不得新增 PRD 未定义的对象、权限、状态或协作语义。

## 1. 执行原则

1. **从 main 的现有 Memory / Role 架构扩展**，不复用任何已废弃 Thread 实验的业务实现。
2. **先修 Memory 原子，再建 Thread**。Memory 修正未通过兼容验收前，不开始 Thread schema。
3. **只有一个内容原子：Memory**。不新增 Message、Delivery、Handoff、Subthread、Event 等协作业务对象。
4. **只有一个权限事实源：ThreadRole**。MCP、HTTP、Owner 入口不得各自写权限判断。
5. **只有一个 Thread service 权限入口**。repository 只做持久化，不承担产品授权规则。
6. **Thread 内发言必须原子**：创建 Memory 与纳入 Thread 在同一事务完成。
7. **Thread 操作审计复用现有 operation log**；不为审计再造 ThreadEvent。
8. **迁移只追加**。执行时使用 main 当时的下一个迁移号，不沿用废弃分支的编号假设。
9. 每个工单完成后先审实际 diff 与不变量，再进入下一工单。发现需要新增 PRD 语义时停止实现，先改 PRD。

依赖：

```
WO-T1 Memory 原子
   ↓
WO-T2 Thread 内核
   ↓
WO-T3 Thread 协作服务
   ↓
WO-T4 协议接入与完整验收
```

---

## WO-T1 Memory 原子契约修正

### 目标

把现有 Memory 从“文档型条目”修正为真正可承载会话的通用信息原子，同时不改变 owner、visibility、checksum、soft delete、Task live-reference 等既有语义。

### 改动边界

只允许涉及：

- Memory 输入校验；
- Memory tool schema / 描述；
- 为 Thread 后续原子发布准备的 tx-safe Memory create primitive；
- 现有 Memory 相关测试和必要文档表达。

不得：

- 创建 Thread 表；
- 修改 Task 生命周期或 contract；
- 引入 Message 类型；
- 引入 Memory revision；
- 改 public/private 规则。

### 机制

`memory_put`：

- content 必填，trim 后至少 1 字符；
- title 可省略，省略时保存空字符串；
- tags 可省略，省略时保存 `[]`；
- description 继续可选；
- 最大长度与敏感内容扫描保持；
- 更新仍覆盖当前 Memory 内容，code 不变。

把当前 `Push` 中“创建新 Memory”的核心提取成可在已有事务中调用的内部 primitive。它必须：

- 由调用方提供已验证的 owner Role；
- 生成唯一 code；
- 应用与 standalone Memory 创建完全相同的 storage consumption policy；
- 写入 private / active Memory；
- 返回 Memory identity；
- 不自行提交外层事务。

standalone `memory_put` 继续通过该 primitive 创建 Memory，行为除输入最小约束外不变。

未来 Thread post 也必须调用同一 primitive，禁止复制一份 INSERT / checksum / consumption 逻辑。

### 验收

必须证明：

1. `content="收到"` 可以创建 private Memory；
2. title/tags 缺省时输出稳定为 `""` / `[]`；
3. 旧式 title + tags + description + content 创建/更新仍工作；
4. non-owner update、share、unshare、delete 行为不变；
5. public/private 读取规则不变；
6. checksum 对 content 的语义不变；
7. Task 的 `harness_refs` 继续读取 Memory **当前内容**，不引入快照；
8. Thread 尚不存在时，仓库所有既有 Memory/Task 测试仍通过；
9. tx-safe create primitive 在外层事务回滚时不留下 Memory 或消费副作用。

### 退出条件

只有 WO-T1 全部验收通过，才允许创建 Thread migration。

---

## WO-T2 Thread 数据内核

### 目标

只落 PRD 的三个持久关系：

```
Thread
ThreadRole
ThreadMemory
```

建立数据库约束和 repository primitives，不接 MCP，不做 Owner UI。

### 数据关系

#### Thread

至少表达：

- stable public code；
- `created_by_role_id`；
- `parent_thread_id` nullable；
- `anchor_memory_id` nullable；
- `status = open | closed`；
- `next_seq`；
- created / updated time。

约束：

- Root：parent 与 anchor 同时为空；
- Child：parent 与 anchor 同时非空；
- parent / anchor 不提供 update path；
- Child 创建时 service 必须验证 anchor 已存在于直接 parent；
- code 唯一。

#### ThreadRole

至少表达：

- thread；
- role；
- permission：read / write / manage；
- last_read_seq；
- joined_at。

约束：

- `(thread, role)` 唯一；
- Root creator 必须存在为 manage；
- descendant 中 Root creator 必须存在为 manage。

成员移除可以删除当前 ThreadRole 行；历史管理动作由现有 operation log 记录，不为此新增成员事件表。

#### ThreadMemory

至少表达：

- thread；
- memory；
- seq；
- added_by_role；
- created_at。

约束：

- `(thread, memory)` 唯一；
- `(thread, seq)` 唯一；
- relation 建立后无 delete/reorder API；
- seq 只由锁定 Thread 后的 `next_seq` 分配。

### Repository 边界

repository 只提供：

- 按 code / id 锁定 Thread；
- CRUD 当前 ThreadRole；
- append ThreadMemory；
- timeline 分页；
- descendants / parent 查询所需的最小读能力；
- scoped timeline 查询时可读取 active Memory 或 deleted tombstone 所需字段。

repository 不判断 read/write/manage，不判断跨 scope 传播权。

### 并发不变量

同一个 Thread 并发写入两条 Memory 时：

- seq 不冲突；
- 顺序确定；
- `next_seq` 与 ThreadMemory 同事务提交。

同一个 Role 并发加入同一 Thread：

- 最终只有一条 ThreadRole。

同一 Memory 并发纳入同一 Thread：

- 最终只有一个 ThreadMemory；重复调用返回既有关系或稳定冲突，不产生第二个 seq。

### 验收

数据库 / repository 层证明 PRD §12 中结构性不变量，包括 Root/Child pair、唯一键、seq、无重排、tombstone 可回读。

WO-T2 不对外注册任何 Thread 工具。

---

## WO-T3 Thread 协作服务

### 目标

建立 Thread 的唯一业务规则层。所有对外入口以后只能调用这一层。

### 唯一权限判定

服务层统一实现三个判断：

```
CanReadThread(role, thread)
CanWriteThread(role, thread)
CanManageThread(role, thread)
```

以及 Thread-scoped Memory read：

```
CanReadMemoryInThread(role, thread, memory)
```

规则严格来自 PRD：

- current ThreadRole permission；
- Root creator tree-wide manage；
- Thread private，不做 public fallback；
- Thread scoped read 不改变 `memory_get` 的直接权限语义。

### 服务动作

只实现以下语义：

1. 创建 Root Thread；
2. 创建 Child Thread；
3. Thread 内原子发布新 Memory；
4. owner 把自己的既有 Memory 纳入 Thread；
5. 读取 Thread + timeline；
6. 列出当前 Role 可见 Thread；
7. 新增 Role；
8. 调整 Role permission；
9. 移除 Role；
10. 更新 last_read_seq；
11. close；
12. reopen。

不实现 mention、notification、reaction、handoff、review、task、next actor。

### Child Thread 创建规则

创建 Child 时必须在一个事务内完成：

1. 锁 parent；
2. 验 parent open；
3. 验 anchor 属于直接 parent；
4. 验操作者对 parent 至少 write；
5. 计算 child 初始 Role 集合；
6. 若包含 parent 外 Role，验操作者是 **parent manage**；
7. 创建 child；
8. 写入 Root creator manage；
9. 写入 child `created_by` 的初始 manage（若不同）；
10. 写入其他 Role；
11. 将 anchor 作为 child 的首个 ThreadMemory 或专门的 anchor 展示关系——实现只能选一种，并保持 timeline 语义一致。

开发前必须在实现设计中固定第 11 步，不能边写边决定。推荐：**anchor 同时作为 child timeline 的 seq=1 Memory**，这样 Child 打开后上下文完整，且无需第二种内容呈现规则。

### Thread 内原子发布

`Post(thread, role, content/meta)`：

1. 锁 Thread；
2. 验 open + write；
3. 调用 WO-T1 的 tx-safe Memory create primitive；
4. 分配 next seq；
5. 建 ThreadMemory；
6. 同一事务提交；
7. 事务后记 operation log。

任何失败都不得留下孤立 Memory、孤立 ThreadMemory 或消费记录。

### 既有 Memory 纳入

只允许：

- actor 是 Memory owner；
- actor 对目标 Thread 有 write；
- Thread open。

public 不赋予非 owner 纳入权。

### Membership 治理

- read 不能改任何成员；
- write 不能改成员；
- manage 可以增删 / 调权限；
- Root creator 在任何 descendant 不可移除、不可降权；
- Child `created_by` 没有永久豁免；
- closed 时禁止新增 Role和升权，但允许 manage 移除普通 Role / 降权；
- remove 后 scoped read 立即失效。

### 生命周期

- close / reopen 仅 manage；
- closed 可读，可更新 read cursor；
- closed 禁止发 Memory、纳入 Memory、创建 Child、扩大受众；
- parent close 不级联 descendants。

### 服务验收场景

逐条实现 PRD §13，并增加权限反例：

- 非成员探测真实 code 与不存在 code 得到同样不可见结果；
- read Role 无法写；
- write Role 无法加人；
- Child creator 不能利用自己的 child manage 身份绕过 parent manage 拉外部 Role；
- 从 parent 移除后不能通过 parent 读 private Memory；
- sibling membership 不互通；
- owner unshare 后，已由 owner 纳入 Thread 的 Memory 仍可 Thread-scoped 读取；
- 非 owner 即使 Memory 当前 public，也不能把它固化进自己的 Thread；
- Root creator 可以撤销 Child `created_by` 权限；
- closed 后仍可降权/移除成员。

---

## WO-T4 协议接入与完整验收

### 目标

在 WO-T1～T3 内核稳定后，把 Thread 接入现有单一 tool registry，使 MCP 与 `POST /api/v1/<tool>` 共用同一 service、schema 和错误语义。

### 工具面设计原则

工具按动作语义组织，数量不是目标。正式注册前先用服务动作映射审一次，确保没有把数据库 CRUD 机械暴露成工具。

至少需要覆盖：

- thread create；
- thread get / timeline；
- thread list；
- child create；
- post；
- include owned Memory；
- member add / permission / remove；
- mark read；
- close / reopen。

可以合并相近管理动作，但不得隐藏权限差异。

### 返回语义

Thread 读取至少返回：

- Thread identity / status；
- parent / anchor（如有）；
- 当前 caller 的 permission；
- Roles；
- timeline，按 seq；
- 每条 Memory 的 owner、当前内容或 tombstone；
- caller last_read_seq / head_seq；
- Child Thread 摘要仅返回 caller 可见的部分。

非成员访问统一按不可见处理，不泄露 Thread 是否存在。

### Rate limit

Thread post 使用独立 action/rate bucket，不复用 standalone Memory `push = 60/hour`。

具体阈值在实现工单开始前按正常对话吞吐确定，并作为配置常量与测试事实；不得散落硬编码。

### Owner / 管理表面

第一版先完成 agent-facing MCP/HTTP 闭环。Owner UI 不是 Thread 核心验收前置，除非现有 Owner tool bridge 可以零业务逻辑复用注册表；不得为了 UI 复制 service 规则。

### 完整验收

必须同时通过：

1. PRD §13 全场景；
2. WO-T3 权限反例；
3. MCP / HTTP 同一动作返回的业务结果一致；
4. schema 与服务约束一致；
5. fresh database 全迁移；
6. 全仓测试；
7. 并发 seq / membership / duplicate include；
8. Thread post 事务故障回滚；
9. Memory / Task 既有行为回归；
10. 文档工具名、权限规则与实现一致。

通过后才允许更新 README / llms / skill / CHANGELOG / VERSION 等正式产品表达；文案必须从已实现事实生成，不得先承诺未完成能力。

---

## 2. 明确不复用的旧思路

执行中不得重新引入：

- 独立 Message 模型；
- delivery / review；
- handoff；
- next_actor / next_action 作为 Thread 协作状态；
- controller；
- workflow node / branch；
- mention 作为核心权限或调度关系；
- Thread Event 状态机；
- Memory snapshot / revision；
- 通过 CI 报错倒推产品模型。

## 3. 执行总退出条件

Thread 第一版完成必须能用一句话解释所有核心行为：

> Role 产生 Memory；Thread 以 private Role scope 组织 Memory 时间线；Child Thread 递归形成局部会话；ThreadRole 决定作用域权限，并允许成员在该作用域内读取被 owner 纳入的 private Memory。

若实现完成后还需要额外业务概念才能解释基本协作，视为模型偏离，不进入合并。
