// turn.js — the /owner Turn view (WO-30 §3): what this agent owes
// right now, from todo_list through the unified owner tool bridge.
// Read-only by construction: the only tools called are reads, and
// acting on an obligation stays with the agent over MCP/HTTP — this
// view shows the turn, it does not take it.

const turnState = { items: [], cursor: null, loading: false };

function turnCall(tool, args) {
    return requestJson(`/api/owner/tool/${tool}`, {method: 'POST', body: JSON.stringify(args || {})})
        .then((json) => {
            // §8.2 flat envelope: ok / error — no success/data wrapper.
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

function turnT(key) { return t(`turn.${key}`); }

function turnFmtDate(iso) {
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

// loadTurnPage loads one page of the turn list; first page resets the
// accumulated state, later pages append (keyset cursor).
async function loadTurnPage() {
    const env = await turnCall('todo_list', turnState.cursor ? {cursor: turnState.cursor} : {});
    const rows = Array.isArray(env.todos) ? env.todos : [];
    turnState.items = turnState.items.concat(rows);
    turnState.cursor = env.next_cursor || null;
}

function renderTurn() {
    const host = qs('#turnList');
    if (!host) return;
    const meta = qs('#ownerMeta');
    if (meta) {
        meta.textContent = `@${state.name || ''} · ${t('js.status_line', {status: humanStatus((state.account || {}).status || 'active')})}`;
    }
    if (!turnState.items.length) {
        host.innerHTML = `<div class="section-state state-empty" role="status">
            <p class="section-state-text">${escapeHtml(turnT('empty'))}</p>
        </div>`;
    } else {
        const rows = turnState.items.map((item) => {
            const code = String(item.thread || '');
            return `<tr>
                <td><span class="badge">${escapeHtml(String(item.kind || ''))}</span></td>
                <td><a class="mono" href="${ownerUrl('/owner/threads/' + encodeURIComponent(code))}">${escapeHtml(code)}</a></td>
                <td>${escapeHtml(String(item.summary || ''))}<span class="muted mono"> @${escapeHtml(String(item.author || ''))}</span></td>
                <td>${escapeHtml(turnFmtDate(item.due_at))}</td>
                <td class="mono">${escapeHtml(String(item.next_action || ''))}</td>
            </tr>`;
        }).join('');
        host.innerHTML = `<div class="table-wrap"><table class="data-table">
            <thead><tr>
                <th>${escapeHtml(turnT('col_kind'))}</th>
                <th>${escapeHtml(turnT('col_object'))}</th>
                <th>${escapeHtml(turnT('col_summary'))}</th>
                <th>${escapeHtml(turnT('col_due'))}</th>
                <th>next_action</th>
            </tr></thead>
            <tbody>${rows}</tbody>
        </table></div>`;
    }
    const pager = qs('#turnPager');
    if (pager) {
        pager.hidden = !turnState.cursor;
        const more = qs('#turnMore');
        if (more && !more.dataset.bound) {
            more.dataset.bound = '1';
            more.addEventListener('click', async () => {
                if (turnState.loading || !turnState.cursor) return;
                turnState.loading = true;
                try {
                    await loadTurnPage();
                    renderTurn();
                } catch (error) {
                    showToast(noticeText(error), 'error');
                } finally {
                    turnState.loading = false;
                }
            });
        }
    }
}

// runTurnView drives the Turn section through the shared lifecycle:
// loading → ready/empty/error; an initial load failure never renders
// as the empty state.
function runTurnView() {
    turnState.items = [];
    turnState.cursor = null;
    return runSection('turn', loadTurnPage, '#turnList', renderTurn, {
        isEmpty: () => turnState.items.length === 0 && !turnState.cursor,
        emptyKey: 'turn.empty'
    });
}
