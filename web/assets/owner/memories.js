// memories.js — the /owner/memories views (WO-30 §3): this agent's
// memories (memory_list) and one memory's content (memory_get), with a
// revision picker — every version is immutable, switching only changes
// what is displayed. All calls go through the unified owner tool
// bridge; only reads. Memory content is rendered as plain text.

const memoriesState = {
    list: { items: [], hasMore: false, offset: 0 },
    detail: null,
    revisions: [],
    loading: false
};

function memCall(tool, args) {
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

function memT(key) { return t(`memories.${key}`); }

function memFmtDate(iso) {
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

// ---- /owner/memories (list) ----

async function loadMemoriesPage() {
    const env = await memCall('memory_list', memoriesState.list.offset > 0 ? {offset: memoriesState.list.offset} : {});
    const rows = Array.isArray(env.kungfus) ? env.kungfus : [];
    memoriesState.list.items = memoriesState.list.items.concat(rows);
    memoriesState.list.offset += rows.length;
    memoriesState.list.hasMore = !!env.has_more;
}

function renderMemories() {
    const host = qs('#memoryList');
    if (!host) return;
    if (!memoriesState.list.items.length) {
        host.innerHTML = `<div class="section-state state-empty" role="status">
            <p class="section-state-text">${escapeHtml(memT('empty'))}</p>
        </div>`;
    } else {
        const rows = memoriesState.list.items.map((m) => `<tr>
            <td><a href="${ownerUrl('/owner/memories/' + encodeURIComponent(String(m.code || '')))}">${escapeHtml(String(m.title || ''))}</a></td>
            <td class="mono">${escapeHtml(String(m.revision ?? ''))}</td>
            <td>${escapeHtml(String(m.visibility || ''))}</td>
            <td>${escapeHtml(memFmtDate(m.updated_at))}</td>
        </tr>`).join('');
        host.innerHTML = `<div class="table-wrap"><table class="data-table">
            <thead><tr>
                <th>${escapeHtml(memT('col_title'))}</th>
                <th>${escapeHtml(memT('col_revision'))}</th>
                <th>${escapeHtml(memT('col_visibility'))}</th>
                <th>${escapeHtml(memT('col_updated'))}</th>
            </tr></thead>
            <tbody>${rows}</tbody>
        </table></div>`;
    }
    const pager = qs('#memoryPager');
    if (pager) pager.hidden = !memoriesState.list.hasMore;
    const more = qs('#memoryMore');
    if (more && !more.dataset.bound) {
        more.dataset.bound = '1';
        more.addEventListener('click', async () => {
            if (memoriesState.loading || !memoriesState.list.hasMore) return;
            memoriesState.loading = true;
            try {
                await loadMemoriesPage();
                renderMemories();
            } catch (error) {
                showToast(noticeText(error), 'error');
            } finally {
                memoriesState.loading = false;
            }
        });
    }
}

function runMemoriesView() {
    memoriesState.list.items = [];
    memoriesState.list.offset = 0;
    memoriesState.list.hasMore = false;
    return runSection('memories', loadMemoriesPage, '#memoryList', renderMemories, {
        isEmpty: () => memoriesState.list.items.length === 0,
        emptyKey: 'memories.empty'
    });
}

// ---- /owner/memories/{code} (detail with revision picker) ----

function memoryCodeFromPath() {
    const parts = window.location.pathname.split('/').filter(Boolean);
    return parts.length >= 3 && parts[0] === 'owner' && parts[1] === 'memories'
        ? decodeURIComponent(parts[2]) : '';
}

async function loadMemoryDetail(revision) {
    const code = memoryCodeFromPath();
    if (!code) throw apiError('VALIDATION_FAILED', 'missing memory code', 0, null);
    const args = {code};
    if (revision) args.revision = revision;
    const env = await memCall('memory_get', args);
    memoriesState.detail = env;
    if (!revision && !memoriesState.revisions.length && env.revision) {
        // revisions are 1..current, immutable and append-only
        memoriesState.revisions = [];
        for (let i = 1; i <= Number(env.revision); i++) memoriesState.revisions.push(i);
    }
}

function renderMemoryDetail() {
    const env = memoriesState.detail;
    if (!env) return;
    const heading = qs('#memoryHeading');
    if (heading) heading.textContent = String(env.title || env.code || '');
    const meta = qs('#memoryMeta');
    if (meta) {
        meta.textContent = `${env.code || ''} · ${env.visibility || ''} · ${memT('revision_label')} ${env.revision ?? ''} · ${memFmtDate(env.updated_at)}`;
    }
    const revs = qs('#memoryRevisions');
    if (revs) {
        revs.innerHTML = memoriesState.revisions.map((rev) => {
            const isCurrent = String(rev) === String(env.revision);
            const label = `${rev}${isCurrent ? ` (${escapeHtml(memT('current'))})` : ''}`;
            return `<button class="btn${isCurrent ? ' primary' : ''}" type="button" data-revision="${rev}">${escapeHtml(label)}</button>`;
        }).join('');
        Array.from(revs.querySelectorAll('button[data-revision]')).forEach((btn) => {
            btn.addEventListener('click', async () => {
                try {
                    await loadMemoryDetail(Number(btn.dataset.revision));
                    renderMemoryDetail();
                } catch (error) {
                    showToast(noticeText(error), 'error');
                }
            });
        });
    }
    const content = qs('#memoryContent');
    if (content) content.textContent = String(env.content || '');
}

function runMemoryDetailView() {
    memoriesState.detail = null;
    memoriesState.revisions = [];
    return runSection('memory_detail', () => loadMemoryDetail(null), '#memoryContent', renderMemoryDetail, {
        isEmpty: () => false,
        emptyKey: 'memories.empty'
    });
}
