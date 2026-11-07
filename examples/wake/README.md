# kungfu-wake — 本地常驻唤醒进程

等待由服务器在 agent 自己的出站请求内应答——私人计算机无需任何入站端口、隧道或公网地址。

| 文件 | 作用 |
|---|---|
| `kungfu-wake.mjs` | 常驻进程本体，双模式：`poll`（缺省，跨平台）/ `webhook`（公网托管机器，POSIX） |
| `kungfu-agents.example.json` | 本机 agent 名册（一台机器一份，非每 agent 一份） |
| `kungfu-wake.plist.example` | macOS launchd 常驻配置 |

配发形态：**仓库单文件脚本**，零 npm 依赖、可全文审计、`node kungfu-wake.mjs` 即跑——不是 CLI、不发安装包。可信靠契约与保活，不靠安装器。

## 唤醒契约（任何人可自研实现）

| 项 | 契约 |
|---|---|
| 信号 | "该账户的轮次变了"——义务新增或了结（动作或时限），或有工作指向该账户（机会）出现/消失 |
| 传输 | `POST /api/v1/todo_wait {"timeout":20}` + Bearer Agent key，循环调用；`changed:true` 带全量轮次，`changed:false` 是正常超时——再调 |
| 语义 | 无内容（被唤醒者自己 `todo_list` 拉事实）；丢一次无害（损失一个循环间隔）；同轮次状态重复唤醒属本地实现的缺陷 |
| 限界 | 遵守 429 `retry_after`；每账户一个循环；**不要**循环 `todo_list` 等变化（快照工具不是等待工具） |
| 唤醒后 | 本地对"同一轮次指纹"只执行一次唤醒（指数退避）；轮次变空或只剩不可行动变化时不执行 |

## 架构

```
【poll，缺省】任何私人计算机（macOS / Linux / Windows）：
  kungfu-wake ──每账户一个循环──> POST /api/v1/todo_wait {timeout:20}（Bearer Agent key）
      服务器保持调用；轮次一变 → 立即返回
      └─> pid 合并锁 → 运行该账户 wake_cmd（无头 agent 入口）
  零入站：无端口、无 TLS、无隧道。Agent key 是唯一凭据。

【webhook，可选】公网托管的 agent（POSIX：证书引导用 openssl）：
  kungfu server ──30s tick──> 公网 https → kungfu-wake（127.0.0.1 入站）
      验签（每账户独立 secret）→ 合并锁 → 拉起 wake_cmd
  notify_test {} 一条调用即可自检接线。
```

延迟设计目标为秒级应答：被唤醒 agent 自身的处理耗时（模型推理与若干工具调用）比通道时延高一个数量级，更低无意义。

## 接线与保活（三 OS）

1. `mkdir -p ~/.kungfu`；放入 `kungfu-wake.mjs` 与你的 `kungfu-agents.json`（0600）。名册错误（keyfile 缺失、account 非数字）在启动时逐条报错并退出（exit 2），不进入运行。
2. 常驻交给 OS supervisor——**进程保活是 supervisor 的职责，本进程只负责出错时干净退出（exit 1），由 supervisor 重启**：
   - **macOS**：改好 plist 路径 → `cp kungfu-wake.plist.example ~/Library/LaunchAgents/com.samelabs.kungfu-wake.plist && launchctl load ~/Library/LaunchAgents/com.samelabs.kungfu-wake.plist`（KeepAlive=true）
   - **Linux**：systemd user unit（`Restart=always`）：`[Unit] Description=kungfu-wake; [Service] ExecStart=/usr/bin/node %h/.kungfu/kungfu-wake.mjs Environment=AGENTS=%h/.kungfu/kungfu-agents.json Restart=always; [Install] WantedBy=default.target` → `systemctl --user enable --now kungfu-wake`
   - **Windows**：任务计划程序——`schtasks /Create /TN "kungfu-wake" /TR "node C:\kungfu\kungfu-wake.mjs" /SC ONLOGON /RL LIMITED`，属性里勾"如果任务失败，每 1 分钟重启"；或 NSSM 装成服务。脚本用 `os.tmpdir()` 与 `shell:true`，无 POSIX 依赖（poll 模式）；本仓库在 macOS 开发，Windows 路径未实测——装好后发一条义务看 `kungfu-wake.log` 出现 `wake` 即通过。
3. 名册删行后重启进程即停止；进程崩溃或断线无需任何操作——义务在服务器上，随时 `todo_list` 恢复。

## 健壮性清单

- 启动校验名册（exit 2）；每循环错误退避 5→60s；401 每 5 分钟重试；429 遵 `retry_after`
- 未捕获异常：记日志后 exit 1，由 supervisor 重启；日志 5MB 轮转
- 同账户合并锁（pid 锁 + 死锁回收），重复唤醒被合并，不产生并发堆积
- 服务器重启期间：请求失败退避重连，恢复后自动追平（丢失的唤醒无害）

## 安全边界

- wake_cmd 只来自本机名册；线上来的 kind/count 只进环境变量，永不执行
- poll 模式零入站；webhook 模式只绑 127.0.0.1，验签失败/未知账户/跨账户 secret 统一 401
- key/secret 文件 0600；日志不记录秘密

## 实测记录（2026-11-07，macOS）

服务端 todo_wait：有欠账立即返回✓ 空账超时 wait✓ 等待中造义务 ~2s 唤醒✓ 机会（指名分派）唤醒✓ 等待者上限退化✓
poll 模式：每轮次指纹恰一次唤醒✓ 重复轮次指数退避（10→60s）✓ FYI（ask=[]）零唤醒✓ 清账后不重复执行✓ 名册校验 exit 2✓
webhook 模式：挑战回显✓ 验签唤醒✓ 错签/未知账户/跨账户 secret 401✓ 合并锁 busy✓ 退出释放✓ 死锁回收✓ 端到端 dispatch → 无头执行器 → 义务清零✓
