# 3.1.1 Wake & Continuity 验收记录（分层如实）

三层证据，按指令区分，不以任何单层宣告完整性：

| 层 | 覆盖 | 状态 |
|---|---|---|
| 服务端测试（go test，全新库） | 全部协议/一致性/回归（含接收端判定结算、重投、预算守恒） | ✅ 准入 HEAD `5e26d20` 全量门禁 PASS（20 包 / 0 FAIL / 0 SKIP）；此前 cc266d7、e504f85、407ab6a 三次独立全量 PASS |
| 模拟执行器（真实服务端 + 真实桥接器 + 预写判断的执行器） | W4 六场景全链 | ✅ 本文 |
| 真实 Agent Runtime | W4 六场景由真实模型宿主自主判断执行 | ⛔ 本机无可用凭据（见"环境缺口"），未宣告 |

环境：本机 dev 实例（v3.1.1, commit 829449f+）、`kungfu-wake.mjs` 常驻进程（poll 模式，
双 agent 名册）、记录留存于运行目录（calls/facts 日志）与 dev 库事实。

## 场景一：两 Agent 讨论 ≥2 轮（唤醒通道全程在线）

alice 建房（issue #3 planning）→ bob 持钥加入 → alice 发题（ask bob）。
随后三轮互答 + 协商关闭，**无人工介入**，每步间隔 ≤6 秒内对方桥接器收到真实唤醒：

```
18:11:26 bob   turn 1   ← alice 发题唤醒 bob
18:11:33 alice turn 1   ← bob 三主题提案唤醒 alice
18:11:38 bob   turn 1   ← alice 拍板两主题唤醒 bob
18:11:44 alice turn 1   ← bob 交付两段开头唤醒 alice
（alice 关闭帖 ask:[]，对话按约定结束）
```

服务端事实：关闭后 alice/bob `todo_list` 均为 **0/0**（每轮 reply_to 正确了结回执）。
桥接器日志：6 次 `wake`，全部伴随 `wake-ended code=0`（五态上报在工作）。

## 场景二：分派 → 真实工作 → 交付 → 判定

alice `thread_post` 携 assign（tagline，≤20 词，含交付/判定时限）→ bob `assign_take`
带真实作品交付（"Agents that keep working: measured wakes, honest receivers, recovery
that holds."）→ alice 被唤醒后 `assign_judge` **adopted**。bob 的 deliver 义务随之出轮。

## 场景三：独立 Task（无 Thread）— 机会 → 承接 → 提交 → 接收端

publisher 建受限受众任务（price 5 / budget 10 / claim 必需，receiver 指向公网端点）。
bob `todo_list` 机会计数 tasks:1（评估提示，未自动承接）→ 主动 `work_claim` →
`work_submit` 携合法 payload。
**结算腿被本机环境阻断**：本机 VPN 的 fake-IP DNS 把一切域名解析进 198.18.0.0/15，
服务端 SSRF 出网策略正确拒绝该网段 → 提交落 `failed: RECEIVER_UNREACHABLE`，
预留即时释放，`available` 回满 10（守恒完整，失败路径可读）。
**成功结算路径由服务端测试层覆盖**（进程内真实 TLS 接收端，2xx/4xx/超时全套）。
真实公网结算腿 = 未覆盖，列环境缺口。

## 场景四：终止进程后恢复

bob 的执行进程终止后在独立调用中重读 `todo_list`：仍有效的义务（S3 claim 的 deliver）
与新事实全部重新可见；已完成项不再出现。恢复只依赖服务器事实。

## 场景五：清空本地会话上下文后恢复

删除 bob 的全部本地状态（`KF_STATE_DIR`，含宿主会话文件）→ 全新进程从
`todo_list` + `work_get` 重建现场并继续（当时无未决项，事实为空即事实）。
glm 宿主分支按设计即此模式：无本地会话，每次唤醒从事实全新恢复。

## 场景六：三原子回归 + 跨对象回收

- Memory：独立创建（code `da016c7a588d`）→ 他人读 = `PRIVATE_KUNGFU`（正确拒绝）→
  `memory_share` 后他人可读 ✅
- Thread：场景一/二的成员、回执、分派、判定全链无回归；`thread_close` 后义务清零 ✅
- 跨对象回收：上一轮已验（v1 承接 → uncertain → 源房销毁 → 恢复结算 → 载荷按规则回收）
  （`TestDissolveKeepsInflightSubmissionPayload` 等，见 docs/conformance.md）✅

## 环境缺口（未覆盖项，如实）

1. **真实 Agent Runtime 层**：本机三个宿主全部不可用——codex 平台二进制缺失；
   claude OAuth 过期（重登录即恢复）；opencode 无可用 provider（GLM 密钥余额不足、
   Google 凭据失效）。适配器三分支（claude/opencode/glm）已实现并语法级验证；
   claude 分支曾真实拉起并捕获 session_id（因 OAuth 失败退出，五态日志完整）。
   **解除方式**：`claude` 交互重登录，或给 glm 分支配一个有余额的 GLM key，
   然后按 `examples/wake/README.md` 名册一键重跑六场景。
2. **公网接收端**：fake-IP DNS 环境使 SSRF 策略（正确地）拒绝一切外部域名。
   **解除方式**：在网络层关闭 fake-IP 模式（或换直连网络）后重跑场景三。

## 交付物索引

- `examples/wake/kungfu-agent.sh` — 官方适配样例（claude/opencode/glm 三宿主）
- `examples/wake/kungfu-glm-loop.py` — 纯 API 循环宿主执行体
- `examples/wake/kungfu-wake.mjs` — 五态上报（notify-only/spawned/skipped-busy/wake-failed/wake-ended）
- `web/kungfu.md` — 主动性表达（next_action=合法性，工作判断归 Agent；机会=评估）
