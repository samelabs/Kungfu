// threads.js — the /owner/threads views (WO-30 §3): the rooms this
// agent is in (thread_list) and one room's work set (thread_get) —
// summary timeline, members, assignment digest. All calls go through
// the unified owner tool bridge; every tool used is a read. Timeline
// and assignment digests page by cursor; content written by
// participants (subjects, summaries, notes) is rendered as escaped
// plain text, never markup.

const threadsState = {
    list: { items: [], cursor: null },
    detail: null, // the loaded thread_get envelope
    timelineCursor: null,
    assignCursor: null,
    loading: false
};

function thrCall(tool, args) {
    return requestJson(`/api/owner/tool/${tool}`, {method: 'POST', body: JSON.stringify(args || {})})
        .then((json) => {
            if (!json || typeof json !== 'object' || typeof json.ok !== 'boolean') {
                throw apiErrorFrom(json, 'js.task_load_failed');
            }
            if (!json.ok) {
                const err = json.error || {};
                throw apiError(err.code || 'TOOL_ERROR', err.message || '', json._httpStatus || 0, err.details || null);
            }
            return json;
        });
}

function thrT(key) { return t(`threads.${key}`); }

function thrFmtDate(iso) {
    if (!iso) return '';
    const d = new Date(iso);
    if (Number.isNaN(d.getTime())) return String(iso);
    const locale = document.documentElement.lang || undefined;
    try {
        return d.toLocaleString(locale, {dateStyle: 'medium', timeStyle: 'short'});
    } catch (e) {
        return String(iso);
    }
}

// ---- /owner/threads (list) ----

async function loadThreadsPage() {
    const env = await thrCall('thread_list', threadsState.list.cursor ? {cursor: threadsState.list.cursor} : {});
    const rows = Array.isArray(env.threads) ? env.threads : [];
    threadsState.list.items = threadsState.list.items.concat(rows);
    threadsState.list.cursor = env.next_cursor || null;
}

function renderThreads() {
    const host = qs('#threadList');
    if (!host) return;
    if (!threadsState.list.items.length) {
        host.innerHTML = `<div class="section-state state-empty" role="status">
            <p class="section-state-text">${escapeHtml(thrT('empty'))}</p>
        </div>`;
    } else {
        const rows = threadsState.list.items.map((row) => {
            const room = row.thread || {};
            const code = String(room.code || '');
            const subject = room.subject ? String(room.subject) : code;
            return `<tr>
                <td><a href="${ownerUrl('/owner/threads/' + encodeURIComponent(code))}">${escapeHtml(subject)}</a><span class="muted mono"> ${escapeHtml(code)}</span></td>
                <td><span class="badge ${escapeHtml(String(room.status || ''))}">${escapeHtml(String(room.status || ''))}</span></td>
                <td>${escapeHtml(String(row.role || ''))}</td>
                <td class="mono">${escapeHtml(String(row.open_items ?? 0))}</td>
                <td class="mono">${escapeHtml(String(row.open_invites ?? 0))}</td>
            </tr>`;
        }).join('');
        host.innerHTML = `<div class="table-wrap"><table class="data-table">
            <thead><tr>
                <th>${escapeHtml(thrT('col_subject'))}</th>
                <th>${escapeHtml(thrT('col_status'))}</th>
                <th>${escapeHtml(thrT('col_role'))}</th>
                <th>${escapeHtml(thrT('col_open_items'))}</th>
                <th>${escapeHtml(thrT('col_open_invites'))}</th>
            </tr></thead>
            <tbody>${rows}</tbody>
        </table></div>`;
    }
    const pager = qs('#threadPager');
    if (pager) {
        pager.hidden = !threadsState.list.cursor;
        const more = qs('#threadMore');
        if (more && !more.dataset.bound) {
            more.dataset.bound = '1';
            more.addEventListener('click', async () => {
                if (threadsState.loading || !threadsState.list.cursor) return;
                threadsState.loading = true;
                try {
                    await loadThreadsPage();
                    renderThreads();
                } catch (error) {
                    showToast(noticeText(error), 'error');
                } finally {
                    threadsState.loading = false;
                }
            });
        }
    }
}

function runThreadsView() {
    threadsState.list.items = [];
    threadsState.list.cursor = null;
    return runSection('threads', loadThreadsPage, '#threadList', renderThreads, {
        isEmpty: () => threadsState.list.items.length === 0 && !threadsState.list.cursor,
        emptyKey: 'threads.empty'
    });
}

// ---- /owner/threads/{code} (work set) ----

function threadCodeFromPath() {
    const parts = window.location.pathname.split('/').filter(Boolean);
    // /owner/threads/{code} → ['owner', 'threads', code]
    return parts.length >= 3 && parts[0] === 'owner' && parts[1] === 'threads'
        ? decodeURIComponent(parts[2]) : '';
}

async function loadThreadDetail() {
    const code = threadCodeFromPath();
    if (!code) throw apiError('VALIDATION_FAILED', 'missing thread code', 0, null);
    const env = await thrCall('thread_get', {thread: code});
    threadsState.detail = env;
    threadsState.timelineCursor = env.next_cursor || null;
    threadsState.assignCursor = env.assignments_next_cursor || null;
}

function threadMemberName(id) {
    const members = (threadsState.detail && Array.isArray(threadsState.detail.members)) ? threadsState.detail.members : [];
    for (const m of members) {
        if (String(m.account_id) === String(id)) return `@${m.account}`;
    }
    return String(id);
}

function renderTimelineEntries() {
    const host = qs('#threadTimeline');
    if (!host || !threadsState.detail) return;
    const timeline = Array.isArray(threadsState.detail.timeline) ? threadsState.detail.timeline : [];
    if (!timeline.length) {
        host.innerHTML = `<div class="section-state state-empty" role="status">
            <p class="section-state-text">${escapeHtml(thrT('empty'))}</p>
        </div>`;
        return;
    }
    const rows = timeline.map((entry) => {
        const receipts = Array.isArray(entry.receipts) ? entry.receipts : [];
        const receiptBits = receipts.map((rc) => {
            // a handle note is present only for the parties of that
            // obligation (the server decides); when present it is
            // rendered as plain text
            const note = rc.note ? ` — ${escapeHtml(String(rc.note))}` : '';
            return `<span class="chip mono">@${escapeHtml(threadMemberName(rc.account))}: ${escapeHtml(String(rc.state || ''))}${note}</span>`;
        }).join(' ');
        const assign = entry.assign ? ` <span class="badge assign">${escapeHtml(thrT('col_assign'))} #${escapeHtml(String(entry.assign.id))} · ${escapeHtml(String(entry.assign.state || ''))}</span>` : '';
        const reply = entry.reply_to_seq ? ` <span class="muted mono">↩ #${escapeHtml(String(entry.reply_to_seq))}</span>` : '';
        return `<li class="timeline-item">
            <div class="timeline-head"><span class="mono">#${escapeHtml(String(entry.seq ?? entry.entry ?? ''))}</span>
                <b>@${escapeHtml(String(entry.author || ''))}</b>${reply}${assign}</div>
            <p class="timeline-summary">${escapeHtml(String(entry.summary || ''))}</p>
            ${receiptBits ? `<div class="timeline-receipts">${receiptBits}</div>` : ''}
        </li>`;
    }).join('');
    host.innerHTML = `<ol class="timeline-list">${rows}</ol>`;
    const pager = qs('#timelinePager');
    if (pager) pager.hidden = !threadsState.timelineCursor;
}

function renderThreadMembers() {
    const host = qs('#threadMembers');
    if (!host || !threadsState.detail) return;
    const members = Array.isArray(threadsState.detail.members) ? threadsState.detail.members : [];
    const rows = members.map((m) => `<tr>
        <td>@${escapeHtml(String(m.account || ''))}<span class="muted mono"> ${escapeHtml(String(m.account_id ?? ''))}</span></td>
        <td>${escapeHtml(String(m.role || ''))}</td>
        <td>${escapeHtml(thrFmtDate(m.joined_at))}</td>
    </tr>`).join('');
    host.innerHTML = `<div class="table-wrap"><table class="data-table">
        <thead><tr>
            <th>${escapeHtml(thrT('col_account'))}</th>
            <th>${escapeHtml(thrT('col_role'))}</th>
            <th>${escapeHtml(thrT('col_joined'))}</th>
        </tr></thead>
        <tbody>${rows}</tbody>
    </table></div>`;
}

function renderThreadAssignments() {
    const host = qs('#threadAssignments');
    if (!host || !threadsState.detail) return;
    const assigns = Array.isArray(threadsState.detail.assignments) ? threadsState.detail.assignments : [];
    if (!assigns.length) {
        host.innerHTML = `<div class="section-state state-empty" role="status">
            <p class="section-state-text">${escapeHtml(thrT('empty'))}</p>
        </div>`;
    } else {
        const rows = assigns.map((a) => `<tr>
            <td class="mono">#${escapeHtml(String(a.assign ?? ''))}</td>
            <td>@${escapeHtml(threadMemberName(a.to))}</td>
            <td><span class="badge ${escapeHtml(String(a.state || ''))}">${escapeHtml(String(a.state || ''))}</span></td>
            <td>${escapeHtml(thrFmtDate(a.deliver_due_at))}</td>
            <td>${escapeHtml(thrFmtDate(a.judge_due_at))}</td>
        </tr>`).join('');
        host.innerHTML = `<div class="table-wrap"><table class="data-table">
            <thead><tr>
                <th>${escapeHtml(thrT('col_assign'))}</th>
                <th>${escapeHtml(thrT('col_account'))}</th>
                <th>${escapeHtml(thrT('col_state'))}</th>
                <th>${escapeHtml(thrT('col_deliver_due'))}</th>
                <th>${escapeHtml(thrT('col_judge_due'))}</th>
            </tr></thead>
            <tbody>${rows}</tbody>
        </table></div>`;
    }
    const pager = qs('#assignPager');
    if (pager) pager.hidden = !threadsState.assignCursor;
}

function renderThreadDetail() {
    const env = threadsState.detail;
    if (!env) return;
    const room = env.thread || {};
    const heading = qs('#threadHeading');
    if (heading) heading.textContent = String(room.subject || room.code || '');
    const meta = qs('#threadMeta');
    if (meta) {
        meta.textContent = `${room.code || ''} · ${room.status || ''} · ${env.role || ''}`;
    }
    renderTimelineEntries();
    renderThreadMembers();
    renderThreadAssignments();
}

// runThreadDetailView loads the room once; the timeline and assignment
// pagers append further digest pages onto the same envelope.
function runThreadDetailView() {
    return runSection('thread_detail', loadThreadDetail, '#threadTimeline', renderThreadDetail, {
        isEmpty: () => false,
        emptyKey: 'threads.empty'
    }).then(() => {
        bindThreadPagers();
    });
}

function bindThreadPagers() {
    const timelineMore = qs('#timelineMore');
    if (timelineMore && !timelineMore.dataset.bound) {
        timelineMore.dataset.bound = '1';
        timelineMore.addEventListener('click', async () => {
            const code = threadCodeFromPath();
            if (!threadsState.timelineCursor || !code) return;
            try {
                const env = await thrCall('thread_get', {thread: code, cursor: threadsState.timelineCursor});
                const timeline = Array.isArray(env.timeline) ? env.timeline : [];
                threadsState.detail.timeline = (threadsState.detail.timeline || []).concat(timeline);
                threadsState.timelineCursor = env.next_cursor || null;
                renderTimelineEntries();
            } catch (error) {
                showToast(noticeText(error), 'error');
            }
        });
    }
    const assignMore = qs('#assignMore');
    if (assignMore && !assignMore.dataset.bound) {
        assignMore.dataset.bound = '1';
        assignMore.addEventListener('click', async () => {
            const code = threadCodeFromPath();
            if (!threadsState.assignCursor || !code) return;
            try {
                const env = await thrCall('thread_get', {thread: code, assignments_cursor: threadsState.assignCursor});
                const assigns = Array.isArray(env.assignments) ? env.assignments : [];
                threadsState.detail.assignments = (threadsState.detail.assignments || []).concat(assigns);
                threadsState.assignCursor = env.assignments_next_cursor || null;
                renderThreadAssignments();
            } catch (error) {
                showToast(noticeText(error), 'error');
            }
        });
    }
}
