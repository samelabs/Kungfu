#!/usr/bin/env node
// kungfu-wake — the resident local wake process for kungfu.md.
//
// One process per MACHINE, not per agent. Poll mode (default) is
// cross-platform (Node built-ins only): macOS, Linux, Windows.
//
//   poll (default — any private machine, zero public exposure):
//     For each roster account the process holds
//     one outbound todo_wait call (up to 20s) in a loop; the moment
//     the account's turn changes (new obligation, thread or Task, or
//     work addressed to it) the server answers and the process runs
//     that agent's wake_cmd. No inbound port, no TLS, no tunnel, no
//     webhook registration — the Agent key is the only secret.
//
//   webhook (COMPATIBILITY MODE, for agents already on public hosts;
//     not the default path — the poll loop above needs no endpoint
//     from any host; POSIX: the cert bootstrap uses openssl):
//     inbound https endpoint for the server's notify face
//     (internal/service/notify.go). Registration probe — POST
//     {"type":"verification","challenge":"<hex>"} answered 2xx with
//     the challenge echoed. Dispatch — POST {account, kind, count}
//     with X-Kungfu-Signature: sha256=hex(HMAC_SHA256(secret, rawBody)),
//     secret disclosed once by notify_register. Payload carries kind +
//     count, never content. Server timing: attempts every 30s, retry
//     while attempts < 5, then dropped — loss is harmless by design,
//     todo_list is the truth.
//
// THE WAKE CONTRACT (what any third-party implementation must
// provide — this file is one):
//   signal   : "the account's turn changed" — obligations added or
//              settled (by action or deadline), or work addressed to
//              the account appeared/disappeared (opportunities).
//              Identity, not counts: the server's wake_token
//              distinguishes "a different one of the same many" —
//              dedup on it, never on the counts alone
//   transport: POST /api/v1/todo_wait {"timeout":20} with Bearer
//              Agent key, looped; changed:true carries the full turn,
//              changed:false is the normal timeout answer — call again
//   semantics: content-free (the woken agent pulls todo_list facts),
//              loss-harmless (a missed wake costs one loop interval),
//              idempotent (waking twice for one turn state is the
//              local process's bug, not the server's)
//   limits   : honor retry_after — on 429 AND on the over-cap
//              changed:false degradation; one loop per account; never
//              loop todo_list waiting for change
//   failure  : started is not done. A wake command that fails to
//              start or exits non-zero re-arms its (still open) turn
//              for a bounded-backoff retry — a failed wake must never
//              become "this turn never wakes again"
//   states   : every wake is observable as exactly one of five log
//              events — notify-only (no executor configured),
//              spawned (submitted, running), skipped-busy (running,
//              merged), wake-failed (failed to start / non-zero
//              exit), wake-ended (process finished). ENDED is not
//              "work done": business completion is a server fact,
//              read back through todo_list, never assumed from an
//              exit code
//
// Robustness: nothing here may fail silently. Startup validates the
// roster (exit 2 on a broken one); every loop backs off on error;
// uncaught errors are logged and the process exits 1 so the OS
// supervisor (launchd KeepAlive / systemd Restart / Task Scheduler)
// restarts it. Logs rotate at 5MB. A stale pid lock (its process
// died) never stalls a loop.
//
// Security: wake commands come only from the local roster; kind/count
// reach the command as env vars only; nothing from the wire is
// executed; empty wake_cmd = log + desktop notification, zero side
// effects; one in-flight wake per account (pid lock with stale
// recovery) — wakes coalesce, they never queue or stampede.
//
// Usage: AGENTS=./kungfu-agents.json node kungfu-wake.mjs        (poll)
//        AGENTS=... MODE=webhook PORT=8443 node kungfu-wake.mjs
import https from 'node:https';
import crypto from 'node:crypto';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { execFileSync, spawn } from 'node:child_process';

const AGENTS_PATH = process.env.AGENTS || './kungfu-agents.json';
const PORT = Number(process.env.PORT || 8443);
const BIND = process.env.BIND || '127.0.0.1';
const LOG = process.env.LOGFILE || './kungfu-wake.log';
const CERT_DIR = process.env.CERT_DIR || '.';
const MODE = process.env.MODE || 'poll';
const LOG_MAX_BYTES = 5 * 1024 * 1024;

const cfg = JSON.parse(fs.readFileSync(AGENTS_PATH, 'utf8'));
const SERVER = (cfg.server || '').replace(/\/$/, '');
const MODE_FINAL = MODE === 'webhook' || cfg.mode === 'webhook' ? 'webhook' : 'poll';
const byAccount = new Map((cfg.agents || []).map((a) => [Number(a.account), a]));

// ---- logging with size rotation: the process must never fill the disk ----
const rotateLog = () => {
  try {
    if (fs.existsSync(LOG) && fs.statSync(LOG).size > LOG_MAX_BYTES) {
      fs.renameSync(LOG, LOG + '.old');
    }
  } catch {}
};
const log = (obj) => {
  const line = JSON.stringify({ ts: new Date().toISOString(), ...obj });
  console.log(line);
  rotateLog();
  try { fs.appendFileSync(LOG, line + '\n'); } catch {}
};

// ---- survive the unexpected: log and hand over to the OS supervisor ----
process.on('uncaughtException', (e) => { log({ event: 'crash', kind: 'uncaught', err: String(e).slice(0, 400) }); process.exit(1); });
process.on('unhandledRejection', (e) => { log({ event: 'crash', kind: 'unhandled', err: String(e).slice(0, 400) }); process.exit(1); });

// ---- desktop notification: macOS only; everywhere else the log line
// is the notification. Deliberately NO PowerShell fallback: a Windows
// MessageBox is MODAL — execFileSync would block the poll loop until
// a human clicks it. This process never waits on a human.
const notify = (msg) => {
  try {
    execFileSync('/usr/bin/osascript', ['-e', `display notification "${msg}" with title "kungfu wake"`]);
  } catch {}
};

// ---- roster validation: a broken line must not take the machine down ----
function validateRoster() {
  const errs = [];
  if (!SERVER) errs.push('roster: server is required');
  if (!cfg.agents?.length) errs.push('roster: no agents');
  for (const a of cfg.agents || []) {
    const who = a.name || `account ${a.account}`;
    if (!Number.isFinite(Number(a.account))) errs.push(`${who}: account must be the numeric account_id`);
    if (!a.name) errs.push(`${who}: name is required`);
    if (MODE_FINAL === 'webhook' && !a.secret) errs.push(`${who}: secret is required in webhook mode`);
    if (!a.keyfile) errs.push(`${who}: keyfile is required`);
    else if (!fs.existsSync(a.keyfile)) errs.push(`${who}: keyfile not found: ${a.keyfile}`);
  }
  return errs;
}

// ---- shared: coalescing wake (pid lock with stale recovery) ----
const lockPathOf = (name) => path.join(os.tmpdir(), `kungfu-wake-${name}.lock`);
const alive = (pid) => { try { process.kill(pid, 0); return true; } catch { return false; } };

function tryLock(name) {
  const lock = lockPathOf(name);
  try {
    fs.writeFileSync(lock, String(process.pid), { flag: 'wx' });
    return { lock, held: true };
  } catch {
    try {
      const pid = Number(fs.readFileSync(lock, 'utf8').trim());
      if (Number.isFinite(pid) && !alive(pid)) {
        fs.unlinkSync(lock);                       // stale recovery
        fs.writeFileSync(lock, String(process.pid), { flag: 'wx' });
        return { lock, held: true };
      }
    } catch {}
  }
  return { lock, held: false };
}

function runWake(agent, kind, count, onDone) {
  const { lock, held } = tryLock(agent.name);
  if (!held) {
    log({ event: 'skipped-busy', mode: MODE_FINAL, name: agent.name });
    onDone && onDone(false);                        // busy is not a failure
    return;                                          // coalesce, never queue
  }
  if (!agent.wake_cmd || !agent.wake_cmd.trim()) {
    try { fs.unlinkSync(lock); } catch {}
    notify(`${agent.name}: ${kind} ×${count}`);
    log({ event: 'notify-only', mode: MODE_FINAL, name: agent.name });
    onDone && onDone(false);                        // notified; nothing ran
    return;
  }
  const cmd = agent.wake_cmd.replaceAll('%KEYFILE%', agent.keyfile).replaceAll('%SERVER%', SERVER);
  const out = fs.openSync(path.join(CERT_DIR, `kungfu-wake-${agent.name}.out`), 'a');
  // shell:true picks /bin/sh on POSIX and cmd.exe on Windows — the
  // roster's wake_cmd stays plain shell text on every OS
  const child = spawn(cmd, { shell: true, detached: true, stdio: ['ignore', out, out],
    env: { ...process.env, KF_KIND: String(kind), KF_COUNT: String(count) } });
  try { fs.closeSync(out); } catch {}              // the child holds its own copy
  let settled = false;                              // 'error' and 'exit' both end the child exactly once
  const finish = (failed, detail) => {
    if (settled) return;
    settled = true;
    try { fs.unlinkSync(lock); } catch {}
    if (failed) log({ event: 'wake-failed', mode: MODE_FINAL, name: agent.name, ...detail });
    onDone && onDone(failed);
  };
  if (!child.pid) {
    // spawn refused outright (no shell, bad permission): no exit event
    // would ever fire — settle here or the lock stalls the loop forever
    finish(true, { reason: 'spawn-refused' });
    return;
  }
  fs.writeFileSync(lock, String(child.pid));       // the lock follows the child
  child.on('error', (e) => finish(true, { reason: 'spawn-error', err: String(e).slice(0, 200) }));
  child.on('exit', (code, signal) => {
    // the five observable wake states are distinct log events:
    // notify-only (no executor) / spawned (submitted, running) /
    // skipped-busy (still running, merged) / wake-failed (failed) /
    // wake-ended (process finished). ENDED is not "work done" —
    // business completion is a fact the server holds, read back
    // through todo_list, never assumed from an exit code.
    if (code === 0) log({ event: 'wake-ended', mode: MODE_FINAL, name: agent.name, code });
    finish(code !== 0, { code, signal });
  });
  child.unref();
  log({ event: 'spawned', mode: MODE_FINAL, name: agent.name, pid: child.pid });
}

// ---- poll mode: outbound long-poll per account ----
// Loop discipline: todo_wait answers changed=true IMMEDIATELY while
// the account still owes something (right for an agent's first call,
// a spin hazard for a resident process). It wakes once per TURN
// STATE (digest dedup), skips non-actionable changes (work that
// disappeared needs nobody woken), pauses while a woken child still
// runs, and backs off while a turn stays unhandled. The server's
// 12/min bucket is the backstop, not the brake.
const WAKE_SPACING_MS = 10_000;
const WAKE_SPACING_MAX_MS = 60_000;
const WAKE_FAIL_BASE_MS = 10_000;
const WAKE_FAIL_MAX_MS = 600_000;

// turnDigest fingerprints a wake answer: obligation handles (entry /
// assign / claim ids are monotonic — a NEW obligation always changes
// the digest) plus the server's wake_token — the opaque identity of
// the opportunity set. Counts alone cannot tell "task T2 arrived"
// from "task T1 ended": both leave opportunities at the same number;
// the token does not (external audit P1-03).
function turnDigest(body) {
  const todos = (body.todos || []).map((t) => [t.kind, t.thread || t.task, t.entry ?? t.claim ?? '', t.assign ?? '']);
  return JSON.stringify(todos) + '|' + (body.wake_token || JSON.stringify(body.opportunities || {}));
}

function actionable(body) {
  const opps = body.opportunities || {};
  return (body.todos || []).length + (opps.tasks || 0) + (opps.assignments || 0) > 0;
}

async function pollLoop(agent) {
  const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
  let key = '';
  let backoff = 5_000;
  let lastWake = '';
  let spacing = WAKE_SPACING_MS;
  let failStreak = 0;
  log({ event: 'watching', mode: 'poll', name: agent.name, server: SERVER });
  for (;;) {
    const lock = lockPathOf(agent.name);
    if (fs.existsSync(lock)) {                       // a wake is still running…
      // …unless the process that owned the child died between spawn
      // and its exit handler: a lock naming a dead pid is stale, and
      // an unrecovered stale lock would stall this loop forever
      try {
        const pid = Number(fs.readFileSync(lock, 'utf8').trim());
        if (Number.isFinite(pid) && pid > 0 && !alive(pid)) fs.unlinkSync(lock);
      } catch {}
      if (fs.existsSync(lock)) {
        await sleep(2_000);
        continue;
      }
    }
    if (!key) {
      try {
        key = fs.readFileSync(agent.keyfile, 'utf8').trim();
      } catch {
        log({ event: 'keyfile-unreadable', name: agent.name, path: agent.keyfile, retry_s: 300 });
        await sleep(300_000);
        continue;                                    // re-read next pass — no restart needed
      }
    }
    let res;
    try {
      res = await fetch(`${SERVER}/api/v1/todo_wait`, {
        method: 'POST',
        headers: { 'Authorization': `Bearer ${key}`, 'Content-Type': 'application/json' },
        body: JSON.stringify({ timeout: 20 }),
        signal: AbortSignal.timeout(35_000),
      });
    } catch (e) {
      log({ event: 'poll-error', name: agent.name, err: String(e).slice(0, 200), retry_ms: backoff });
      await sleep(backoff);
      backoff = Math.min(backoff * 2, 60_000);
      continue;
    }
    if (res.status === 401) {
      key = '';                                       // re-read next pass: the keyfile may have been fixed
      log({ event: 'unauthorized', name: agent.name, note: 'Agent key rejected — fix the keyfile, retrying in 5 min' });
      await sleep(300_000);
      continue;
    }
    if (res.status === 429) {                        // rate limited: honor retry_after
      const ra = (await res.json().catch(() => ({}))).retry_after ?? 10;
      log({ event: 'rate-limited', name: agent.name, retry_s: ra });
      await sleep(ra * 1000);
      continue;
    }
    let body = {};
    try { body = await res.json(); } catch {}
    // anything that is not a todo_wait answer — a 5xx, a proxy's HTML
    // error page — is an error: back off, never hot-loop it
    if (res.status < 200 || res.status >= 300 || typeof body.changed !== 'boolean') {
      log({ event: 'poll-error', name: agent.name, err: `unexpected answer: status ${res.status}, changed ${typeof body.changed}`, retry_ms: backoff });
      await sleep(backoff);
      backoff = Math.min(backoff * 2, 60_000);
      continue;
    }
    backoff = 5_000;                                 // a real answer resets the error backoff
    if (body.changed === true) {
      const digest = turnDigest(body);
      if (!actionable(body)) {
        // the change REMOVED work (assignment voided, receipt settled
        // by the other side…): nothing to wake for — remember the
        // state and keep waiting
        lastWake = digest;
        log({ event: 'non-actionable-change', name: agent.name });
        continue;
      }
      if (digest !== lastWake) {
        // a turn the woken agent has NOT already been shown: wake it
        lastWake = digest;
        spacing = WAKE_SPACING_MS;
        const todos = (body.todos || []).length;
        const opps = body.opportunities || {};
        log({ event: 'wake', mode: 'poll', name: agent.name, todos, opportunities: opps });
        // started is not done: a child that fails to start or exits
        // non-zero re-arms the SAME turn for a bounded-backoff retry
        // — "woken once" must never become "never woken again"
        // (external audit P1-04)
        runWake(agent, 'turn', todos + (opps.tasks || 0) + (opps.assignments || 0), (failed) => {
          if (!failed) { failStreak = 0; return; }
          failStreak++;
          const retry = Math.min(WAKE_FAIL_BASE_MS * 2 ** (failStreak - 1), WAKE_FAIL_MAX_MS);
          log({ event: 'wake-retry', mode: 'poll', name: agent.name, streak: failStreak, retry_ms: retry });
          setTimeout(() => { lastWake = ''; }, retry);
        });
      } else {
        // the SAME unhandled turn again: the woken command did not
        // drain it — back off, never re-spawn an identical state
        spacing = Math.min(spacing * 2, WAKE_SPACING_MAX_MS);
        log({ event: 'unchanged-turn', name: agent.name, spacing_s: spacing / 1000 });
      }
      await sleep(spacing);                         // give the woken agent room to act
      continue;
    }
    // changed:false — the server's own pacing answer: 1s after a
    // normal held timeout, 5s when the global waiter cap degraded the
    // call to an immediate empty answer. Honor it either way — an
    // over-cap storm of instant re-calls buys nothing but 429s
    // (external audit P2-05).
    const ra = typeof body.retry_after === 'number' && body.retry_after > 0 ? body.retry_after : 1;
    await sleep(ra * 1000);
  }
}

// ---- webhook mode: inbound https (public hosts, POSIX) ----
const sigOf = (secret, raw) => 'sha256=' + crypto.createHmac('sha256', secret).update(raw).digest('hex');

// constant-time signature check: the endpoint is public, the compare
// must not leak the expected digest through timing
const sigOk = (presented, expected) => {
  const a = Buffer.from(presented, 'utf8');
  const b = Buffer.from(expected, 'utf8');
  return a.length === b.length && crypto.timingSafeEqual(a, b);
};

function webhookServer() {
  execFileSync('/bin/sh', ['-c',
    `openssl req -x509 -newkey rsa:2048 -nodes -keyout ${CERT_DIR}/kungfu-wake.key ` +
    `-out ${CERT_DIR}/kungfu-wake.crt -days 14 -subj "/CN=kungfu-wake" 2>/dev/null || true`]);
  https.createServer(
    { key: fs.readFileSync(`${CERT_DIR}/kungfu-wake.key`), cert: fs.readFileSync(`${CERT_DIR}/kungfu-wake.crt`) },
    (req, res) => {
      // the endpoint is public: a body larger than the cap is not a
      // signal, it is an attack or a misconfiguration — drop the
      // connection instead of buffering it (dispatch payloads are a
      // few hundred bytes; 64 KB is generous)
      const chunks = [];
      let size = 0;
      let overflowed = false;
      req.on('data', (c) => {
        size += c.length;
        if (size > 65536) { overflowed = true; req.destroy(); return; }
        chunks.push(c);
      });
      req.on('end', () => {
        if (overflowed) return;
        const raw = Buffer.concat(chunks).toString('utf8');
        let body = {};
        try { body = JSON.parse(raw); } catch {}
        const send = (code, text) => { res.writeHead(code, { 'Content-Type': 'text/plain' }); res.end(text); };

        if (body.type === 'verification' && typeof body.challenge === 'string') {
          log({ event: 'verification' });
          return send(200, body.challenge);
        }
        const agent = byAccount.get(Number(body.account));
        const presented = String(req.headers['x-kungfu-signature'] || '');
        if (!agent || !sigOk(presented, sigOf(agent.secret, raw))) {
          log({ event: 'refused', account: body.account }); // uniform 401
          return send(401, 'bad signature');
        }
        log({ event: 'wake', mode: 'webhook', account: agent.account, name: agent.name, kind: body.kind, count: body.count });
        // compatibility mode's reliability boundary: a wake command
        // failure here does NOT re-arm a retry (the poll loop owns
        // failure recovery; a lost push is always harmless because
        // the account's next todo_wait catches the same turn)
        runWake(agent, body.kind, body.count, null);
        send(200, 'ok');
      });
    }
  ).listen(PORT, BIND, () => log({ event: 'listening', mode: 'webhook', port: PORT, agents: cfg.agents.length }));
}

// ---- entry: validate first — a broken roster exits before any loop ----
const rosterErrors = validateRoster();
if (rosterErrors.length) {
  for (const e of rosterErrors) console.error(e);
  process.exit(2);
}
if (MODE_FINAL === 'webhook') {
  webhookServer();
} else {
  log({ event: 'process-up', mode: 'poll', agents: cfg.agents.length, server: SERVER, pid: process.pid });
  for (const agent of cfg.agents) {
    pollLoop(agent);                                 // one loop per account, forever
  }
}
