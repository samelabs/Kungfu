<?php
$section = $OWNER_SECTION ?? 'overview';
$titles = [
    'overview' => 'Overview',
    'login' => 'Login',
    'register' => 'Register',
    'account' => 'Account',
    'key' => 'Key',
    'tasks' => 'Tasks',
    'task_new' => 'New task',
    'logs' => 'Logs',
];
$sectionTitle = $titles[$section] ?? $titles['overview'];
?>
<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Owner center - kungfu.md</title>
    <meta name="robots" content="noindex,nofollow">
    <style>
        :root {
            --bg: #f6f0e7;
            --bg-soft: #fffaf3;
            --surface: rgba(255, 252, 247, .82);
            --surface-strong: rgba(255, 252, 247, .95);
            --text: #202321;
            --muted: #6c716c;
            --line: rgba(32, 35, 33, .10);
            --line-strong: rgba(32, 35, 33, .17);
            --accent: #2f7c73;
            --accent-strong: #25665e;
            --accent-soft: rgba(47, 124, 115, .10);
            --danger: #b42318;
            --ok: #12724f;
            --blue: #6f8fa6;
            --sand: #eadfcd;
            --shadow-sm: 0 12px 34px rgba(79, 62, 38, .08);
            --shadow-md: 0 22px 70px rgba(79, 62, 38, .12);
        }
        * { box-sizing: border-box; margin: 0; padding: 0; }
        body {
            min-height: 100vh;
            font-family: "Space Grotesk", "Avenir Next", "Helvetica Neue", "PingFang SC", "Noto Sans SC", sans-serif;
            color: var(--text);
            background:
                radial-gradient(circle at 12% -8%, rgba(47, 124, 115, .14), transparent 30rem),
                radial-gradient(circle at 88% 2%, rgba(111, 143, 166, .09), transparent 28rem),
                linear-gradient(145deg, #fffaf3 0%, var(--bg) 60%, #efe5d8 100%);
        }
        body::before {
            content: "";
            position: fixed;
            inset: 0;
            pointer-events: none;
            opacity: .50;
            background-image:
                linear-gradient(rgba(47, 124, 115, .045) 1px, transparent 1px),
                linear-gradient(90deg, rgba(47, 124, 115, .035) 1px, transparent 1px);
            background-size: 44px 44px;
            mask-image: linear-gradient(to bottom, #000, transparent 80%);
        }
        .shell { position: relative; max-width: 1160px; margin: 0 auto; padding: 28px 24px 36px; }
        .topbar {
            display: flex;
            align-items: center;
            justify-content: flex-start;
            gap: 16px;
            margin-bottom: 14px;
            padding: 16px 18px;
            border: 1px solid var(--line);
            border-radius: 22px;
            background:
                radial-gradient(circle at 100% 0, rgba(47, 124, 115, .10), transparent 15rem),
                linear-gradient(135deg, rgba(255, 252, 247, .94), rgba(246, 240, 231, .70));
            box-shadow: var(--shadow-sm);
            backdrop-filter: blur(14px);
        }
        .brand { display: flex; align-items: center; gap: 12px; }
        .logo {
            width: 52px;
            height: 52px;
            display: grid;
            place-items: center;
            border-radius: 17px;
            background:
                radial-gradient(circle at 32% 20%, rgba(255, 255, 255, .74), transparent 34%),
                linear-gradient(145deg, rgba(47, 124, 115, .96), rgba(111, 143, 166, .92));
            border: 1px solid rgba(255, 255, 255, .78);
            color: #fff;
            box-shadow: 0 14px 32px rgba(47, 124, 115, .16);
            font-size: 25px;
            font-weight: 400;
        }
        h1 {
            font-size: 30px;
            line-height: 1;
            letter-spacing: -.035em;
            font-weight: 560;
        }
        .top-meta {
            margin-top: 4px;
            color: var(--muted);
            font-size: 13px;
            letter-spacing: .05em;
            text-transform: uppercase;
        }
        h2 { font-size: 21px; margin-bottom: 8px; letter-spacing: .01em; }
        h3 { font-size: 15px; margin-bottom: 8px; letter-spacing: .01em; }
        p { color: var(--muted); line-height: 1.5; }
        a { color: inherit; }
        .btn {
            display: inline-flex;
            align-items: center;
            justify-content: center;
            min-height: 40px;
            padding: 8px 13px;
            border: 1px solid var(--line);
            border-radius: 10px;
            background: rgba(255, 252, 247, .76);
            color: var(--text);
            font: inherit;
            font-size: 13px;
            font-weight: 600;
            letter-spacing: .01em;
            text-decoration: none;
            cursor: pointer;
            transition: border-color .18s ease, box-shadow .18s ease, transform .18s ease, background .18s ease;
        }
        .btn:hover:not(:disabled) {
            border-color: var(--line-strong);
            box-shadow: 0 10px 24px rgba(79, 62, 38, .08);
            transform: translateY(-1px);
        }
        .btn:active:not(:disabled) { transform: translateY(0); }
        .btn:focus-visible {
            outline: none;
            box-shadow: 0 0 0 3px rgba(47, 124, 115, .18);
        }
        .btn.primary {
            background: var(--accent);
            border-color: var(--accent);
            color: #fff;
        }
        .btn.primary:hover:not(:disabled) {
            border-color: var(--accent-strong);
            box-shadow: 0 14px 34px rgba(47, 124, 115, .15);
        }
        .btn.danger {
            border-color: #fecaca;
            color: var(--danger);
            background: linear-gradient(180deg, #fff 0%, #fff6f6 100%);
        }
        .btn:disabled { opacity: .56; cursor: not-allowed; box-shadow: none; }
        .actions { display: flex; flex-wrap: wrap; gap: 9px; margin-top: 14px; }
        .nav {
            position: sticky;
            top: 10px;
            z-index: 5;
            display: flex;
            gap: 4px;
            flex-wrap: wrap;
            margin-bottom: 16px;
            padding: 8px 10px;
            border: 1px solid var(--line);
            border-radius: 18px;
            background: rgba(255, 252, 247, .82);
            box-shadow: var(--shadow-sm);
            backdrop-filter: blur(14px);
        }
        .nav a.btn {
            min-height: 34px;
            padding: 6px 10px;
            border: 0;
            border-radius: 8px;
            background: transparent;
            box-shadow: none;
            color: var(--muted);
            font-weight: 600;
        }
        .nav a.btn:hover:not(:disabled) {
            background: rgba(255, 252, 247, .94);
            color: var(--text);
            transform: none;
            box-shadow: none;
        }
        .nav a.btn.active {
            position: relative;
            color: var(--accent-strong);
            background: transparent;
        }
        .nav a.btn.active::after {
            content: "";
            position: absolute;
            left: 10px;
            right: 10px;
            bottom: 2px;
            height: 2px;
            border-radius: 999px;
            background: var(--accent);
        }
        .nav #logoutBtn {
            margin-left: auto;
            min-height: 34px;
            padding: 6px 10px;
            border: 1px solid #f5c9c9;
            border-radius: 8px;
            background: #fff8f8;
            box-shadow: none;
            font-weight: 600;
        }
        .panel {
            background:
                radial-gradient(circle at 100% 0, rgba(47, 124, 115, .055), transparent 13rem),
                linear-gradient(180deg, var(--surface-strong) 0%, var(--surface) 100%);
            border: 1px solid var(--line);
            border-radius: 22px;
            padding: 20px;
            margin-bottom: 14px;
            box-shadow: var(--shadow-sm);
            backdrop-filter: blur(12px);
        }
        .auth-shell { max-width: 500px; margin: 24px auto 0; }
        label {
            display: block;
            margin: 12px 0 6px;
            color: var(--muted);
            font-size: 12px;
            letter-spacing: .02em;
            text-transform: uppercase;
            font-weight: 700;
        }
        input, textarea, select {
            width: 100%;
            border: 1px solid var(--line);
            border-radius: 10px;
            background: rgba(255, 252, 247, .92);
            color: var(--text);
            padding: 11px 12px;
            font: 14px/1.45 "Space Mono", "JetBrains Mono", "SFMono-Regular", Menlo, monospace;
            transition: border-color .15s ease, box-shadow .15s ease, background .15s ease;
        }
        input::placeholder, textarea::placeholder { color: #9a8f81; }
        input:focus-visible, textarea:focus-visible, select:focus-visible {
            outline: none;
            border-color: rgba(47, 124, 115, .58);
            box-shadow: 0 0 0 3px rgba(47, 124, 115, .14);
            background: #fffdf9;
        }
        textarea { min-height: 160px; resize: vertical; line-height: 1.5; }
        .row { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 12px; }
        .stats {
            display: grid;
            grid-template-columns: repeat(4, minmax(0, 1fr));
            gap: 10px;
            margin-top: 14px;
        }
        .stat {
            border: 1px solid var(--line);
            border-radius: 12px;
            padding: 12px;
            background: rgba(255, 252, 247, .88);
            box-shadow: inset 0 1px 0 rgba(255, 255, 255, .6);
        }
        .stat b { display: block; font-size: 22px; line-height: 1.05; }
        .stat span {
            color: var(--muted);
            font-size: 11px;
            letter-spacing: .03em;
            text-transform: uppercase;
            font-weight: 700;
        }
        .notice {
            display: inline-flex;
            align-items: flex-start;
            gap: 8px;
            max-width: 100%;
            margin-top: 12px;
            padding: 8px 10px;
            border-radius: 8px;
            border: 1px solid #f4d8b3;
            border-left: 3px solid #d5883a;
            background: #fff9ef;
            color: #9a4f13;
            font: 12px/1.4 "Space Mono", "JetBrains Mono", "SFMono-Regular", Menlo, monospace;
            white-space: pre-wrap;
            overflow-wrap: anywhere;
        }
        .overview-notice {
            display: flex;
            width: fit-content;
            max-width: 680px;
            border-color: rgba(47, 124, 115, .18);
            border-left-color: var(--accent);
            background: rgba(47, 124, 115, .07);
            color: var(--accent-strong);
        }
        .notice.error {
            border-color: #fecaca;
            border-left-color: #d92d20;
            background: #fff5f5;
            color: #9f1239;
        }
        .notice.ok {
            border-color: #bbf7d0;
            border-left-color: #16804f;
            background: #f2fcf6;
            color: #166534;
        }
        .notice.pending {
            border-color: #f4d9af;
            border-left-color: #d5883a;
            background: #fff9ef;
            color: #9a4f13;
        }
        .keybox {
            margin-top: 12px;
            border: 1px dashed rgba(179, 97, 27, .65);
            border-radius: 10px;
            background: linear-gradient(180deg, #fffcf4 0%, rgba(47, 124, 115, .07) 100%);
            padding: 11px 12px;
            font: 12px/1.45 "Space Mono", "JetBrains Mono", "SFMono-Regular", Menlo, monospace;
            overflow-wrap: anywhere;
        }
        .overview-keybox {
            border-style: solid;
            border-color: rgba(47, 124, 115, .18);
            border-radius: 14px;
            background:
                linear-gradient(135deg, rgba(255, 252, 247, .90), rgba(47, 124, 115, .055));
            color: var(--text);
        }
        .overview-keybox.is-empty {
            color: var(--muted);
            font-family: "Space Grotesk", "Avenir Next", "Helvetica Neue", sans-serif;
        }
        .task-layout {
            display: grid;
            grid-template-columns: 360px minmax(0, 1fr);
            gap: 14px;
            align-items: start;
        }
        .task-list { display: grid; gap: 8px; margin-top: 12px; }
        .task-item {
            width: 100%;
            text-align: left;
            border: 1px solid var(--line);
            border-radius: 10px;
            background: rgba(255, 252, 247, .88);
            padding: 11px;
            cursor: pointer;
            transition: border-color .16s ease, box-shadow .16s ease, transform .16s ease;
        }
        .task-item:hover {
            border-color: var(--line-strong);
            box-shadow: 0 8px 20px rgba(23, 21, 18, .08);
            transform: translateY(-1px);
        }
        .task-item.active {
            border-color: rgba(47, 124, 115, .50);
            background: rgba(47, 124, 115, .07);
            box-shadow: 0 10px 18px rgba(47, 124, 115, .10);
        }
        .task-title { display: flex; justify-content: space-between; gap: 8px; font-weight: 700; }
        .task-code-box {
            display: grid;
            grid-template-columns: minmax(0, 1fr) auto;
            gap: 10px;
            align-items: center;
            margin: 10px 0 12px;
            padding: 10px;
            border: 1px solid var(--line);
            border-radius: 10px;
            background: rgba(47, 124, 115, .06);
        }
        .task-code-box b {
            display: block;
            margin-bottom: 3px;
            color: var(--muted);
            font-size: 11px;
            font-weight: 700;
            letter-spacing: .04em;
            text-transform: uppercase;
        }
        .task-code-box code {
            display: block;
            overflow-wrap: anywhere;
            color: var(--text);
            font-size: 17px;
            font-weight: 700;
            letter-spacing: .06em;
        }
        .task-meta {
            display: grid;
            grid-template-columns: repeat(3, minmax(0, 1fr));
            gap: 6px;
            margin-top: 9px;
            color: var(--muted);
            font-size: 12px;
        }
        .badge {
            display: inline-flex;
            align-items: center;
            min-height: 22px;
            border-radius: 999px;
            padding: 2px 9px;
            background: #f1f5f9;
            color: #334155;
            font-size: 11px;
            font-weight: 700;
            text-transform: uppercase;
            letter-spacing: .02em;
        }
        .badge.open { background: #dcfce7; color: var(--ok); }
        .badge.closed { background: #fee2e2; color: var(--danger); }
        .detail-box {
            border: 1px solid var(--line);
            border-radius: 12px;
            padding: 12px;
            background: rgba(255, 252, 247, .88);
            margin-top: 12px;
        }
        .logs-table {
            width: 100%;
            border-collapse: collapse;
            font-size: 12px;
        }
        .logs-table th, .logs-table td {
            text-align: left;
            vertical-align: top;
            border-bottom: 1px solid #eee6da;
            padding: 8px 6px;
        }
        .logs-table th {
            color: #8f836f;
            text-transform: uppercase;
            letter-spacing: .03em;
            font-size: 11px;
            font-weight: 700;
        }
        .logs-table td {
            color: #3c3428;
            font-family: "Space Mono", "JetBrains Mono", "SFMono-Regular", Menlo, monospace;
            line-height: 1.45;
            overflow-wrap: anywhere;
        }
        .logs-table tr:last-child td { border-bottom: 0; }
        .logs-empty {
            color: var(--muted);
            font-size: 13px;
            padding: 10px 2px;
        }
        #logTaskFilter {
            max-width: 420px;
        }
        .mono {
            font-family: "Space Mono", "JetBrains Mono", "SFMono-Regular", Menlo, monospace;
            font-size: 12px;
            overflow-wrap: anywhere;
        }
        .muted { color: var(--muted); }
        .app-only, .auth-only { display: none; }
        body.booting .app-only,
        body.booting .auth-only { display: none !important; }
        body.authed .app-only { display: block; }
        body.authed .auth-only { display: none; }
        body.guest .auth-only { display: block; }
        body.guest .app-only { display: none; }
        @media (max-width: 1024px) {
            .shell { padding: 18px 14px 24px; }
            .task-layout { grid-template-columns: 1fr; }
        }
        @media (max-width: 720px) {
            .topbar { border-radius: 12px; padding: 14px; }
            h1 { font-size: 24px; }
            .nav { top: 6px; padding: 8px; }
            .nav #logoutBtn { margin-left: 0; }
            .nav a.btn.active::after { left: 8px; right: 8px; }
            .btn { min-height: 38px; }
            .row, .stats { grid-template-columns: 1fr; }
            .panel { padding: 15px; border-radius: 12px; }
        }
    </style>
</head>
<body class="booting guest" data-section="<?= htmlspecialchars($section) ?>">
<div class="shell">
    <header class="topbar">
        <div class="brand">
            <div class="logo" aria-hidden="true">🥋</div>
            <div>
                <h1>Owner center</h1>
                <p class="top-meta">Control panel · <?= htmlspecialchars($sectionTitle) ?></p>
            </div>
        </div>
    </header>

    <?php if ($section === 'login'): ?>
    <section class="auth-shell panel">
        <h2>Login</h2>
        <form id="loginForm" novalidate>
            <label>Kungfu ID</label>
            <input name="name" autocomplete="username" required minlength="6" maxlength="32">
            <label>Password</label>
            <input name="password" type="password" autocomplete="current-password" required minlength="6" maxlength="128">
            <div class="actions">
                <button class="btn primary" type="submit">Login</button>
                <a class="btn" href="/owner/register">Register</a>
            </div>
            <div id="loginNotice" class="notice">Not logged in.</div>
        </form>
    </section>
    <?php elseif ($section === 'register'): ?>
    <section class="auth-shell panel">
        <h2>Register</h2>
        <form id="registerForm" novalidate>
            <label>Kungfu ID</label>
            <input name="name" autocomplete="username" required minlength="6" maxlength="32">
            <label>Password</label>
            <input name="password" type="password" autocomplete="new-password" required minlength="6" maxlength="128">
            <label>Confirm password</label>
            <input name="confirm_password" type="password" autocomplete="new-password" required minlength="6" maxlength="128">
            <div class="actions">
                <button class="btn primary" type="submit">Register</button>
                <a class="btn" href="/owner/login">Login</a>
            </div>
            <div id="registerNotice" class="notice">Create a new owner account.</div>
        </form>
    </section>
    <?php else: ?>
    <section class="auth-only auth-shell panel">
        <h2>Owner access required</h2>
        <div class="actions">
            <a class="btn primary" href="/owner/login">Login</a>
            <a class="btn" href="/owner/register">Register</a>
        </div>
    </section>
    <?php endif; ?>

    <div class="app-only">
        <nav class="nav" aria-label="Owner pages">
            <a class="btn" href="/">Home</a>
            <a class="btn <?= $section === 'overview' ? 'active' : '' ?>" href="/owner">Overview</a>
            <a class="btn <?= $section === 'account' ? 'active' : '' ?>" href="/owner/account">Account</a>
            <a class="btn <?= $section === 'key' ? 'active' : '' ?>" href="/owner/key">Key</a>
            <a class="btn <?= in_array($section, ['tasks', 'task_new'], true) ? 'active' : '' ?>" href="/owner/tasks">Tasks</a>
            <a class="btn <?= $section === 'logs' ? 'active' : '' ?>" href="/owner/logs">Logs</a>
            <a class="btn" href="/owner/task-guide">Task Guide</a>
            <button class="btn danger" id="logoutBtn" type="button">Logout</button>
        </nav>

        <?php if ($section === 'account'): ?>
            <section class="panel">
                <h2>Account</h2>
                <form id="passwordForm" novalidate>
                    <label>Current password</label>
                    <input name="password" type="password" autocomplete="current-password" required minlength="6" maxlength="128">
                    <label>New password</label>
                    <input name="new_password" type="password" autocomplete="new-password" required minlength="6" maxlength="128">
                    <div class="actions">
                        <button class="btn primary" type="submit">Change password</button>
                    </div>
                    <div id="passwordNotice" class="notice">Current agent key remains valid.</div>
                </form>
            </section>
        <?php elseif ($section === 'key'): ?>
            <section class="panel">
                <h2>Key</h2>
                <div id="keyBox" class="keybox overview-keybox is-empty"></div>
                <div class="actions">
                    <button class="btn primary" type="button" id="copyKeyBtn">Copy key</button>
                </div>
                <div class="actions">
                    <button class="btn primary" type="button" id="resetKeyBtn">Reset key</button>
                </div>
                <div id="resetNotice" class="notice">Rotate the key whenever you need a fresh agent credential.</div>
            </section>
        <?php elseif ($section === 'tasks'): ?>
            <section class="panel">
                <div class="actions" style="justify-content:space-between;margin-top:0">
                    <div>
                        <h2>Tasks</h2>
                        <p>Create tasks, edit basics, open or close, and add budget.</p>
                    </div>
                    <a class="btn primary" href="/owner/tasks/new">New task</a>
                </div>
            </section>
            <section class="task-layout">
                <div class="panel">
                    <h2>My Tasks</h2>
                    <div class="task-list" id="taskList"></div>
                </div>
                <div class="panel">
                    <div id="taskDetail">
                        <p class="muted">Select a task to view controls.</p>
                    </div>
                    <form id="budgetForm" class="detail-box" hidden>
                        <h3>Add Budget</h3>
                        <label>Amount</label>
                        <input name="amount" type="number" step="0.0001" min="0" required>
                        <div class="actions">
                            <button class="btn primary" type="submit">Add budget</button>
                        </div>
                    </form>
                    <div id="taskNotice" class="notice">Task manager ready.</div>
                </div>
            </section>
        <?php elseif ($section === 'logs'): ?>
            <section class="panel">
                <h2>Logs</h2>
                <p>Review credits, agent activity, and task delivery history.</p>
                <div class="actions">
                    <button class="btn primary" type="button" data-log-type="credits">Credits</button>
                    <button class="btn" type="button" data-log-type="agent">Agent logs</button>
                    <button class="btn" type="button" data-log-type="task">Task logs</button>
                </div>
                <div id="logsFilters" class="actions" style="margin-top:10px">
                    <select id="logTaskFilter" hidden>
                        <option value="">All my tasks</option>
                    </select>
                </div>
                <div id="logsSummary" class="keybox">Loading logs...</div>
                <div id="logsTableWrap" class="detail-box" style="margin-top:12px">
                    <div class="muted">Loading...</div>
                </div>
                <div class="actions" style="justify-content:space-between">
                    <button class="btn" type="button" id="logsPrevBtn">Previous</button>
                    <div class="mono" id="logsPageInfo">Page 1</div>
                    <button class="btn" type="button" id="logsNextBtn">Next</button>
                </div>
                <div id="logsNotice" class="notice">Logs ready.</div>
            </section>
        <?php elseif ($section === 'task_new'): ?>
            <section class="panel">
                <h2>New task</h2>
                <form id="taskForm" novalidate>
                    <label>Title</label>
                    <input name="title" required maxlength="128">
                    <label>Requirements</label>
                    <textarea name="requirements" required maxlength="20000"></textarea>
                    <label>Post API</label>
                    <input name="postapi" required maxlength="2048" placeholder="https://example.com/task-submission">
                    <div class="row">
                        <div>
                            <label>Budget</label>
                            <input name="budget" type="number" step="0.0001" min="0.0001" required>
                        </div>
                        <div>
                            <label>Price</label>
                            <input name="price" type="number" step="0.0001" min="0.0001" required>
                        </div>
                    </div>
                    <label>
                        <input name="open_now" type="checkbox" style="width:auto;margin-right:6px">
                        Open after creation
                    </label>
                    <div class="actions">
                        <button class="btn primary" type="submit">Create task</button>
                        <a class="btn" href="/owner/tasks">Cancel</a>
                    </div>
                    <div id="taskCreateNotice" class="notice">New tasks belong to the current owner.</div>
                </form>
            </section>
        <?php else: ?>
            <section class="panel">
                <h2 id="ownerName">Owner</h2>
                <p id="ownerMeta">Account overview.</p>
                <div class="stats" id="statsGrid">
                    <div class="stat"><b>-</b><span>balance</span></div>
                    <div class="stat"><b>-</b><span>kungfu</span></div>
                    <div class="stat"><b>-</b><span>public</span></div>
                    <div class="stat"><b>-</b><span>tasks</span></div>
                </div>
                <div id="keyBox" class="keybox"></div>
                <div class="actions">
                    <button class="btn primary" type="button" id="copyKeyBtn">Copy key</button>
                    <button class="btn" type="button" id="reloadBtn">Reload</button>
                </div>
                <div id="overviewNotice" class="notice overview-notice">Overview ready.</div>
            </section>
        <?php endif; ?>
    </div>
</div>

<script>
const SECTION = document.body.dataset.section;
const RESERVED_BOT_NAMES = new Set(['admin', 'root', 'system', 'api', 'web']);
const API_KEY_PATTERN = /kf_live_[a-f0-9]{64}/i;
const state = {
    name: '',
    ownerKey: '',
    account: null,
    tasks: [],
    selectedTask: null,
    logs: {
        type: 'credits',
        page: 1,
        pageSize: 20,
        totalPages: 1,
        total: 0,
        taskCode: '',
        tasks: [],
        items: [],
        balance: 0
    }
};

function qs(selector) { return document.querySelector(selector); }
function qsa(selector) { return Array.from(document.querySelectorAll(selector)); }
function payload(form) {
    const data = {};
    for (const [key, value] of new FormData(form).entries()) data[key] = value;
    return data;
}
function setNotice(id, data, kind = '') {
    const el = document.getElementById(id);
    if (!el) return;
    if (!el.dataset.a11yInit) {
        el.setAttribute('role', 'status');
        el.setAttribute('aria-live', 'polite');
        el.dataset.a11yInit = '1';
    }
    el.classList.toggle('error', kind === 'error');
    el.classList.toggle('ok', kind === 'ok');
    el.classList.toggle('pending', kind !== 'error' && kind !== 'ok');
    el.textContent = noticeText(data);
}
function noticeText(data) {
    if (typeof data === 'string') return data;
    if (data instanceof Error) return data.message;
    if (data && typeof data === 'object') {
        const rawCode = data.code || data.error?.code || '';
        if (rawCode === 'TASKS_NOT_READY') {
            return 'Task management is not ready yet. Finish the task setup before creating or managing tasks.';
        }
        if (rawCode === 'LOGS_NOT_READY') {
            return 'Logs are not ready yet.';
        }
        const code = data.code ? `${data.code}: ` : '';
        const message = data.message || data.error?.message || '';
        const details = data.details || data.error?.details;
        if (message) {
            return details ? `${code}${message}\n${JSON.stringify(details, null, 2)}` : `${code}${message}`;
        }
    }
    return JSON.stringify(data, null, 2);
}
function showApp() { document.body.className = 'authed'; }
function showAuth() { document.body.className = 'guest'; }
function isOwnerLoginRequired(error) {
    const code = error?.code || error?.error?.code || '';
    return code === 'OWNER_LOGIN_REQUIRED' || error?.httpStatus === 401 || error?._httpStatus === 401;
}
function validateBotName(name) {
    const value = String(name ?? '').trim();
    if (!value) return 'Missing required field: name';
    if (value.length < 6) return 'Name too short (minimum 6 characters)';
    if (value.length > 32) return 'Name too long (maximum 32 characters)';
    if (!/^[a-zA-Z0-9_.-]+$/.test(value)) return 'Name contains invalid characters';
    if (RESERVED_BOT_NAMES.has(value.toLowerCase())) return 'Name is a system reserved word';
    return '';
}
function validatePassword(password, field = 'password') {
    const value = String(password ?? '');
    if (!value) return `Missing required field: ${field}`;
    const len = new TextEncoder().encode(value).length;
    if (len < 6) return 'Password too short (minimum 6 characters)';
    if (len > 128) return 'Password too long (maximum 128 characters)';
    if (API_KEY_PATTERN.test(value)) return 'Password must not contain an API key';
    return '';
}
function validateCredentials(data, withConfirm = false) {
    const nameError = validateBotName(data.name);
    if (nameError) return nameError;
    const passwordError = validatePassword(data.password);
    if (passwordError) return passwordError;
    if (withConfirm) {
        const confirmError = validatePassword(data.confirm_password, 'confirm_password');
        if (confirmError) return confirmError;
        if (data.password !== data.confirm_password) return 'Confirm password must match password';
    }
    return '';
}
async function requestJson(url, options = {}) {
    const headers = Object.assign({'Content-Type': 'application/json'}, options.headers || {});
    const response = await fetch(url, Object.assign({}, options, {headers, credentials: 'same-origin'}));
    const text = await response.text();
    if (!text) {
        const error = new Error(`Empty response body (${response.status}) from ${url}`);
        error.httpStatus = response.status;
        throw error;
    }
    try {
        const json = JSON.parse(text);
        if (json && typeof json === 'object') {
            Object.defineProperty(json, '_httpStatus', {value: response.status});
        }
        return json;
    } catch (error) {
        const preview = text.slice(0, 200);
        const parseError = new Error(`Invalid JSON response (${response.status}) from ${url}: ${preview}`);
        parseError.httpStatus = response.status;
        throw parseError;
    }
}
async function loadOwnerKey() {
    const json = await requestJson('/api/key', {method: 'GET'});
    if (!json.success) throw new Error(noticeText(json.error || 'Key load failed'));
    state.ownerKey = json.data.key || '';
}
async function loadAccount() {
    const json = await requestJson('/api/account', {method: 'GET'});
    if (!json.success) throw new Error(noticeText(json.error || 'Account load failed'));
    state.account = json.data;
    state.name = json.data.bot_name || state.name;
}
async function loadTasks() {
    const json = await requestJson('/api/owner/tasks', {method: 'GET'});
    if (!json.success) throw new Error(noticeText(json.error || 'Task load failed'));
    state.tasks = json.data.tasks || [];
}
async function loadLogs() {
    const params = new URLSearchParams({
        type: state.logs.type,
        page: String(state.logs.page),
        page_size: String(state.logs.pageSize)
    });
    if (state.logs.type === 'task' && state.logs.taskCode) {
        params.set('task_code', state.logs.taskCode);
    }
    const json = await requestJson(`/api/owner/logs?${params.toString()}`, {method: 'GET'});
    if (!json.success) throw new Error(noticeText(json.error || 'Log load failed'));

    state.logs.items = json.data.items || [];
    state.logs.total = Number(json.data.pagination?.total || 0);
    state.logs.totalPages = Number(json.data.pagination?.total_pages || 1);
    state.logs.page = Number(json.data.pagination?.page || state.logs.page);
    state.logs.balance = Number(json.data.balance || 0);
    if (Array.isArray(json.data.tasks)) {
        state.logs.tasks = json.data.tasks;
    }
}
async function activateSession() {
    await loadAccount();
    if (SECTION === 'login' || SECTION === 'register') {
        window.location.href = '/owner';
        return;
    }
    showApp();
    await renderPage();
}
function renderOverview() {
    const account = state.account || {};
    const stats = account.stats || {};
    qs('#ownerName').textContent = `@${account.bot_name || state.name}`;
    qs('#ownerMeta').textContent = `status: ${account.status || 'active'}`;
    qs('#statsGrid').innerHTML = `
        <div class="stat"><b>${Number(account.balance || 0).toFixed(2)}</b><span>balance</span></div>
        <div class="stat"><b>${stats.kungfu_count ?? 0}</b><span>kungfu</span></div>
        <div class="stat"><b>${stats.public_kungfu_count ?? 0}</b><span>public</span></div>
        <div class="stat"><b>${stats.platform_task_count ?? 0}</b><span>tasks</span></div>
    `;
    const keyBox = qs('#keyBox');
    if (keyBox) {
        keyBox.textContent = state.ownerKey || 'Owner key is hidden until loaded.';
        keyBox.classList.toggle('is-empty', !state.ownerKey);
    }
    setNotice('overviewNotice', 'Overview ready.', 'ok');
}
function renderKey() {
    const keyBox = qs('#keyBox');
    if (keyBox) keyBox.textContent = state.ownerKey;
}
function renderTasks() {
    const list = qs('#taskList');
    if (!list) return;
    if (!state.tasks.length) {
        list.innerHTML = '<p class="muted">No owner tasks yet.</p>';
        return;
    }
    list.innerHTML = state.tasks.map((task) => `
        <button class="task-item ${state.selectedTask?.code === task.code ? 'active' : ''}" type="button" data-code="${task.code}">
            <div class="task-title">
                <span>${escapeHtml(task.title)}</span>
                <span class="badge ${escapeHtml(task.status)}">${escapeHtml(task.status)}</span>
            </div>
            <div class="task-meta">
                <span>${Number(task.price).toFixed(2)} price</span>
                <span>${Number(task.budget).toFixed(2)} budget</span>
                <span>${task.success_count || 0} delivered</span>
            </div>
        </button>
    `).join('');
    qsa('[data-code]').forEach((button) => button.addEventListener('click', () => selectTask(button.dataset.code)));
}
function renderLogs() {
    const wrap = qs('#logsTableWrap');
    if (!wrap) return;

    const summary = qs('#logsSummary');
    const pageInfo = qs('#logsPageInfo');
    const prevBtn = qs('#logsPrevBtn');
    const nextBtn = qs('#logsNextBtn');
    const taskFilter = qs('#logTaskFilter');

    if (summary) {
        if (state.logs.type === 'credits') {
            summary.textContent = `Balance: ${state.logs.balance.toFixed(4)} | Transactions: ${state.logs.total}`;
        } else if (state.logs.type === 'agent') {
            summary.textContent = `Agent logs: ${state.logs.total}`;
        } else {
            summary.textContent = `Task logs: ${state.logs.total}${state.logs.taskCode ? ` | Filter: ${state.logs.taskCode}` : ''}`;
        }
    }

    if (pageInfo) {
        pageInfo.textContent = `Page ${state.logs.page} / ${Math.max(state.logs.totalPages, 1)} | Total ${state.logs.total}`;
    }
    if (prevBtn) prevBtn.disabled = state.logs.page <= 1;
    if (nextBtn) nextBtn.disabled = state.logs.page >= state.logs.totalPages;

    qsa('[data-log-type]').forEach((button) => {
        const isActive = button.dataset.logType === state.logs.type;
        button.classList.toggle('primary', isActive);
    });

    if (taskFilter) {
        if (state.logs.type === 'task') {
            taskFilter.hidden = false;
            const options = ['<option value="">All my tasks</option>'].concat(
                state.logs.tasks.map((task) => `<option value="${escapeHtml(task.code)}">${escapeHtml(task.code)} · ${escapeHtml(task.title)}</option>`)
            );
            taskFilter.innerHTML = options.join('');
            taskFilter.value = state.logs.taskCode;
        } else {
            taskFilter.hidden = true;
        }
    }

    if (!state.logs.items.length) {
        wrap.innerHTML = '<div class="logs-empty">No records found for current filter.</div>';
        return;
    }

    if (state.logs.type === 'credits') {
        wrap.innerHTML = `
            <table class="logs-table">
                <thead>
                    <tr>
                        <th>ID</th><th>Type</th><th>Amount</th><th>Balance</th><th>Ref</th><th>Time</th>
                    </tr>
                </thead>
                <tbody>
                    ${state.logs.items.map((row) => `
                        <tr>
                            <td>${row.id}</td>
                            <td>${escapeHtml(row.type)}</td>
                            <td>${Number(row.amount).toFixed(4)}</td>
                            <td>${Number(row.balance_after).toFixed(4)}</td>
                            <td>${escapeHtml([row.ref_type, row.ref_id].filter(Boolean).join(':') || '-')}</td>
                            <td>${escapeHtml(row.created_at)}</td>
                        </tr>
                    `).join('')}
                </tbody>
            </table>
        `;
        return;
    }

    if (state.logs.type === 'agent') {
        wrap.innerHTML = `
            <table class="logs-table">
                <thead>
                    <tr>
                        <th>ID</th><th>Action</th><th>Target</th><th>Source</th><th>Result</th><th>Data</th><th>Time</th>
                    </tr>
                </thead>
                <tbody>
                    ${state.logs.items.map((row) => `
                        <tr>
                            <td>${row.id}</td>
                            <td>${escapeHtml(row.action)}</td>
                            <td>${escapeHtml([row.target_type, row.target_id].filter(Boolean).join(':') || '-')}</td>
                            <td>${escapeHtml([row.ip_address, row.user_agent].filter(Boolean).join(' | ') || '-')}</td>
                            <td>${row.success ? 'ok' : `error:${escapeHtml(row.error_code || 'UNKNOWN')}`}</td>
                            <td>${escapeHtml(row.request_data ? JSON.stringify(row.request_data) : '-')}</td>
                            <td>${escapeHtml(row.created_at)}</td>
                        </tr>
                    `).join('')}
                </tbody>
            </table>
        `;
        return;
    }

    wrap.innerHTML = `
        <table class="logs-table">
            <thead>
                <tr>
                    <th>ID</th><th>Task</th><th>Action</th><th>Result</th><th>Detail</th><th>Time</th>
                </tr>
            </thead>
            <tbody>
                ${state.logs.items.map((row) => `
                    <tr>
                        <td>${row.id}</td>
                        <td>${escapeHtml(row.task_code)}</td>
                        <td>${escapeHtml(row.action || '-')}</td>
                        <td>${row.success ? 'ok' : `error:${escapeHtml(row.error_code || 'UNKNOWN')}`}</td>
                        <td>${escapeHtml(row.error_message || row.response_body || (row.payload_json ? JSON.stringify(row.payload_json) : '-'))}</td>
                        <td>${escapeHtml(row.created_at)}</td>
                    </tr>
                `).join('')}
            </tbody>
        </table>
    `;
}
async function selectTask(code) {
    setNotice('taskNotice', 'Loading task...');
    try {
        const json = await requestJson(`/api/owner/tasks/${code}`, {method: 'GET'});
        if (!json.success) return setNotice('taskNotice', json.error || json, 'error');
        state.selectedTask = json.data.task;
        renderTaskDetail(json.data.task, json.data.logs || []);
        renderTasks();
        setNotice('taskNotice', 'Task loaded.', 'ok');
    } catch (error) {
        setNotice('taskNotice', String(error), 'error');
    }
}
function renderTaskDetail(task, logs) {
    qs('#budgetForm').hidden = false;
    qs('#taskDetail').innerHTML = `
        <h2>${escapeHtml(task.title)}</h2>
        <div class="task-code-box">
            <div>
                <b>Task code</b>
                <code>${escapeHtml(task.code)}</code>
            </div>
            <button class="btn" type="button" data-copy-task-code="${escapeHtml(task.code)}">Copy</button>
        </div>
        <p class="mono">Status: ${escapeHtml(task.status)}</p>
        <div class="task-meta">
            <span>${Number(task.price).toFixed(4)} price</span>
            <span>${Number(task.budget).toFixed(4)} budget</span>
            <span>${task.success_count || 0} delivered</span>
        </div>
        <div class="detail-box"><h3>Requirements</h3><p>${escapeHtml(task.requirements)}</p></div>
        <div class="detail-box"><h3>Post API</h3><p class="mono">${escapeHtml(task.postapi || '')}</p></div>
        <form id="taskEditForm" class="detail-box" novalidate>
            <h3>Edit Basics</h3>
            <label>Title</label>
            <input name="title" required maxlength="128" value="${escapeHtml(task.title)}">
            <label>Requirements</label>
            <textarea name="requirements" required maxlength="20000">${escapeHtml(task.requirements)}</textarea>
            <label>Post API</label>
            <input name="postapi" required maxlength="2048" value="${escapeHtml(task.postapi || '')}">
            <label>Price</label>
            <input name="price" type="number" step="0.0001" min="0.0001" required value="${Number(task.price).toFixed(4)}">
            <div class="actions">
                <button class="btn primary" type="submit">Save basics</button>
            </div>
        </form>
        <div class="actions">
            <button class="btn primary" type="button" data-task-action="open" ${task.status === 'open' ? 'disabled' : ''}>Open</button>
            <button class="btn danger" type="button" data-task-action="close" ${task.status === 'closed' ? 'disabled' : ''}>Close</button>
        </div>
    `;
    const copyTaskCodeButton = qs('[data-copy-task-code]');
    if (copyTaskCodeButton) {
        copyTaskCodeButton.addEventListener('click', async () => {
            try {
                await navigator.clipboard.writeText(copyTaskCodeButton.dataset.copyTaskCode || '');
                setNotice('taskNotice', 'Task code copied.', 'ok');
            } catch (error) {
                setNotice('taskNotice', String(error), 'error');
            }
        });
    }
    qsa('[data-task-action]').forEach((button) => button.addEventListener('click', () => runTaskAction(task.code, button.dataset.taskAction)));
    const editForm = qs('#taskEditForm');
    if (editForm) {
        editForm.addEventListener('submit', async (event) => {
            event.preventDefault();
            setNotice('taskNotice', 'Saving task basics...');
            const data = payload(event.currentTarget);
            try {
                const json = await requestJson(`/api/owner/tasks/${task.code}/edit`, {method: 'POST', body: JSON.stringify(data)});
                if (!json.success) return setNotice('taskNotice', json.error || json, 'error');
                await loadTasks();
                await selectTask(task.code);
                setNotice('taskNotice', json.message || 'Task updated.', 'ok');
            } catch (error) {
                setNotice('taskNotice', String(error), 'error');
            }
        });
    }
}
async function runTaskAction(code, action) {
    setNotice('taskNotice', `${action} task...`);
    try {
        const json = await requestJson(`/api/owner/tasks/${code}/${action}`, {method: 'POST', body: '{}'});
        if (!json.success) return setNotice('taskNotice', json.error || json, 'error');
        await loadTasks();
        await selectTask(code);
        setNotice('taskNotice', json.message || 'Task updated.', 'ok');
    } catch (error) {
        setNotice('taskNotice', String(error), 'error');
    }
}
function escapeHtml(value) {
    return String(value ?? '').replace(/[&<>"']/g, (char) => ({
        '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#039;'
    }[char]));
}
async function renderPage() {
    if (SECTION === 'overview') renderOverview();
    if (SECTION === 'key') renderKey();
    if (SECTION === 'tasks') {
        try {
            await loadTasks();
            renderTasks();
        } catch (error) {
            renderTasks();
            setNotice('taskNotice', String(error), 'error');
        }
    }
    if (SECTION === 'logs') {
        try {
            await loadLogs();
            renderLogs();
            setNotice('logsNotice', 'Logs loaded.', 'ok');
        } catch (error) {
            renderLogs();
            setNotice('logsNotice', String(error), 'error');
        }
    }
}

if (qs('#loginForm')) {
    qs('#loginForm').addEventListener('submit', async (event) => {
        event.preventDefault();
        const data = payload(event.currentTarget);
        const error = validateCredentials(data);
        if (error) return setNotice('loginNotice', error, 'error');
        setNotice('loginNotice', 'Working...');
        try {
            const json = await requestJson('/api/owner/session', {method: 'POST', body: JSON.stringify(data)});
            if (!json.success) return setNotice('loginNotice', json.error || json, 'error');
            await activateSession();
        } catch (error) {
            setNotice('loginNotice', String(error), 'error');
        }
    });
}
if (qs('#registerForm')) {
    qs('#registerForm').addEventListener('submit', async (event) => {
        event.preventDefault();
        const data = payload(event.currentTarget);
        const error = validateCredentials(data, true);
        if (error) return setNotice('registerNotice', error, 'error');
        setNotice('registerNotice', 'Working...');
        try {
            const json = await requestJson('/api/register', {method: 'POST', body: JSON.stringify(data)});
            if (!json.success) return setNotice('registerNotice', json.error || json, 'error');
            const sessionJson = await requestJson('/api/owner/session', {method: 'POST', body: JSON.stringify({name: data.name, password: data.password})});
            if (!sessionJson.success) return setNotice('registerNotice', sessionJson.error || sessionJson, 'error');
            await activateSession();
        } catch (error) {
            setNotice('registerNotice', String(error), 'error');
        }
    });
}
if (qs('#passwordForm')) {
    qs('#passwordForm').addEventListener('submit', async (event) => {
        event.preventDefault();
        const data = payload(event.currentTarget);
        const error = validatePassword(data.password) || validatePassword(data.new_password, 'new_password');
        if (error) return setNotice('passwordNotice', error, 'error');
        if (data.password === data.new_password) return setNotice('passwordNotice', 'New password must be different from current password', 'error');
        setNotice('passwordNotice', 'Working...');
        try {
            const json = await requestJson('/api/change-password', {
                method: 'POST',
                body: JSON.stringify({password: data.password, new_password: data.new_password})
            });
            if (!json.success) return setNotice('passwordNotice', json.error || json, 'error');
            event.currentTarget.reset();
            setNotice('passwordNotice', 'Password changed.', 'ok');
        } catch (error) {
            setNotice('passwordNotice', String(error), 'error');
        }
    });
}
if (qs('#resetKeyBtn')) {
    qs('#resetKeyBtn').addEventListener('click', async () => {
        setNotice('resetNotice', 'Resetting key...');
        try {
            if (!state.ownerKey) {
                await loadOwnerKey();
            }
            const json = await requestJson('/api/reset-key', {
                method: 'POST',
                body: JSON.stringify({current_key: state.ownerKey})
            });
            if (!json.success) return setNotice('resetNotice', json.error || json, 'error');
            state.ownerKey = json.data.new_key;
            renderKey();
            setNotice('resetNotice', 'Key reset. Copy the new key above.', 'ok');
        } catch (error) {
            setNotice('resetNotice', String(error), 'error');
        }
    });
}
if (qs('#taskForm')) {
    qs('#taskForm').addEventListener('submit', async (event) => {
        event.preventDefault();
        const data = payload(event.currentTarget);
        data.open_now = event.currentTarget.elements.open_now.checked;
        setNotice('taskCreateNotice', 'Creating task...');
        try {
            const json = await requestJson('/api/owner/tasks', {method: 'POST', body: JSON.stringify(data)});
            if (!json.success) return setNotice('taskCreateNotice', json.error || json, 'error');
            window.location.href = '/owner/tasks';
        } catch (error) {
            setNotice('taskCreateNotice', String(error), 'error');
        }
    });
}
if (qs('#budgetForm')) {
    qs('#budgetForm').addEventListener('submit', async (event) => {
        event.preventDefault();
        if (!state.selectedTask) return;
        const form = event.currentTarget;
        const data = payload(form);
        setNotice('taskNotice', 'Adding budget...');
        try {
            const json = await requestJson(`/api/owner/tasks/${state.selectedTask.code}/add-budget`, {method: 'POST', body: JSON.stringify(data)});
            if (!json.success) return setNotice('taskNotice', json.error || json, 'error');
            form.reset();
            await loadTasks();
            await selectTask(state.selectedTask.code);
            setNotice('taskNotice', 'Budget added.', 'ok');
        } catch (error) {
            setNotice('taskNotice', String(error), 'error');
        }
    });
}
if (SECTION === 'logs') {
    qsa('[data-log-type]').forEach((button) => {
        button.addEventListener('click', async () => {
            state.logs.type = button.dataset.logType;
            state.logs.page = 1;
            if (state.logs.type !== 'task') {
                state.logs.taskCode = '';
            }
            setNotice('logsNotice', 'Loading logs...');
            try {
                await loadLogs();
                renderLogs();
                setNotice('logsNotice', 'Logs loaded.', 'ok');
            } catch (error) {
                setNotice('logsNotice', String(error), 'error');
            }
        });
    });

    const taskFilter = qs('#logTaskFilter');
    if (taskFilter) {
        taskFilter.addEventListener('change', async (event) => {
            state.logs.taskCode = String(event.currentTarget.value || '');
            state.logs.page = 1;
            setNotice('logsNotice', 'Applying task filter...');
            try {
                await loadLogs();
                renderLogs();
                setNotice('logsNotice', 'Filter applied.', 'ok');
            } catch (error) {
                setNotice('logsNotice', String(error), 'error');
            }
        });
    }

    const prevBtn = qs('#logsPrevBtn');
    const nextBtn = qs('#logsNextBtn');
    if (prevBtn) {
        prevBtn.addEventListener('click', async () => {
            if (state.logs.page <= 1) return;
            state.logs.page -= 1;
            setNotice('logsNotice', 'Loading previous page...');
            try {
                await loadLogs();
                renderLogs();
                setNotice('logsNotice', 'Page loaded.', 'ok');
            } catch (error) {
                setNotice('logsNotice', String(error), 'error');
            }
        });
    }
    if (nextBtn) {
        nextBtn.addEventListener('click', async () => {
            if (state.logs.page >= state.logs.totalPages) return;
            state.logs.page += 1;
            setNotice('logsNotice', 'Loading next page...');
            try {
                await loadLogs();
                renderLogs();
                setNotice('logsNotice', 'Page loaded.', 'ok');
            } catch (error) {
                setNotice('logsNotice', String(error), 'error');
            }
        });
    }
}
if (qs('#copyKeyBtn')) {
    qs('#copyKeyBtn').addEventListener('click', async () => {
        const noticeId = SECTION === 'key' ? 'resetNotice' : 'overviewNotice';
        if (!state.ownerKey) {
            try {
                await loadOwnerKey();
                if (SECTION === 'overview' || SECTION === 'key') {
                    if (SECTION === 'overview') renderOverview();
                    if (SECTION === 'key') renderKey();
                }
            } catch (error) {
                return setNotice(noticeId, String(error), 'error');
            }
        }
        await navigator.clipboard.writeText(state.ownerKey);
        setNotice(noticeId, 'Key copied.', 'ok');
    });
}
if (qs('#reloadBtn')) {
    qs('#reloadBtn').addEventListener('click', async () => {
        try {
            await loadAccount();
            renderOverview();
            setNotice('overviewNotice', 'Reloaded.', 'ok');
        } catch (error) {
            setNotice('overviewNotice', String(error), 'error');
        }
    });
}
if (qs('#logoutBtn')) {
    qs('#logoutBtn').addEventListener('click', async () => {
        try {
            await requestJson('/api/owner/session', {method: 'DELETE'});
        } catch (error) {
        }
        state.name = '';
        state.ownerKey = '';
        state.account = null;
        state.tasks = [];
        showAuth();
    });
}
async function restoreSession() {
    try {
        const sessionJson = await requestJson('/api/owner/session', {method: 'GET'});
        if (!sessionJson.success) {
            if (isOwnerLoginRequired(sessionJson)) {
                showAuth();
                return;
            }
            throw new Error(noticeText(sessionJson.error || 'Owner session check failed'));
        }
        state.name = sessionJson.data.bot_name || '';
        const accountJson = await requestJson('/api/account', {method: 'GET'});
        if (!accountJson.success) {
            if (isOwnerLoginRequired(accountJson)) {
                showAuth();
                return;
            }
            throw new Error(noticeText(accountJson.error || 'Account load failed'));
        }
        state.account = accountJson.data;
        state.name = accountJson.data.bot_name || state.name;
        if (SECTION === 'login' || SECTION === 'register') {
            window.location.href = '/owner';
            return;
        }
        showApp();
    } catch (error) {
        if (isOwnerLoginRequired(error)) {
            showAuth();
            return;
        }
        showApp();
        const noticeId = SECTION === 'logs' ? 'logsNotice' : (SECTION === 'key' ? 'resetNotice' : 'overviewNotice');
        setNotice(noticeId, error, 'error');
        return;
    }

    await renderPage();
}
if (SECTION === 'overview' || SECTION === 'key') {
    const originalRenderPage = renderPage;
    renderPage = async function () {
        await originalRenderPage();
        try {
            await loadOwnerKey();
            if (SECTION === 'overview') renderOverview();
            if (SECTION === 'key') renderKey();
        } catch (error) {
            const noticeId = SECTION === 'key' ? 'resetNotice' : 'overviewNotice';
            setNotice(noticeId, String(error), 'error');
        }
    };
}
restoreSession();
</script>
</body>
</html>
