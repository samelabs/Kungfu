/* B2 Store Administration: products + redemptions UI.
 * Server authorization is the authority; UI visibility is cosmetic.
 * reject/cancel are economic refund actions and require explicit
 * confirmation. */
'use strict';

const storeState = {
    products: {items: [], page: 1, pageSize: 20, total: 0, filters: {status: 'all', q: ''}},
    redemptions: {items: [], page: 1, pageSize: 20, total: 0, filters: {status: 'all', bot_id: '', q: ''}}
};

function fmtPrice(v) {
    const n = Number(v);
    return isNaN(n) ? escapeHtml(v) : n.toFixed(4).replace(/\.?0+$/, '');
}

/* ---------- products ---------- */

async function loadStoreProducts() {
    const st = storeState.products;
    const params = new URLSearchParams({
        status: st.filters.status,
        page: String(st.page),
        page_size: String(st.pageSize)
    });
    if (st.filters.q) params.set('q', st.filters.q);
    const json = await adminGet('/api/admin/store/products?' + params.toString());
    if (!json.success) throw new Error(apiError(json, 'Failed to load products'));
    st.items = json.data.products || [];
    st.total = json.data.total || 0;
}

function renderStoreProducts() {
    const card = document.getElementById('adminStoreProductsCard');
    if (!card) return;
    const canManage = hasPermission('store.products.manage');
    if (!storeState.products.items.length) {
        card.innerHTML = '<p class="muted">No products.</p>';
    } else {
        const rows = storeState.products.items.map(p => {
            const actions = [];
            if (canManage) {
                actions.push(`<button class="btn small" data-pact="edit" data-code="${escapeHtml(p.code)}">Edit</button>`);
                actions.push(p.status === 'active'
                    ? `<button class="btn small danger" data-pact="deactivate" data-code="${escapeHtml(p.code)}">Deactivate</button>`
                    : `<button class="btn small" data-pact="activate" data-code="${escapeHtml(p.code)}">Activate</button>`);
            }
            return `<tr>
                <td><code>${escapeHtml(p.code)}</code></td>
                <td>${escapeHtml(p.title)}</td>
                <td class="desc">${escapeHtml(p.description || '')}</td>
                <td>${fmtPrice(p.credits_price)}</td>
                <td><span class="status ${p.status === 'active' ? 'ok' : 'muted'}">${escapeHtml(p.status)}</span></td>
                <td>${fmtTime(p.created_at)}</td>
                <td class="actions">${actions.join(' ')}</td>
            </tr>`;
        }).join('');
        card.innerHTML = `<table class="admin-table wide">
            <thead><tr><th>Code</th><th>Title</th><th>Description</th><th>Price</th><th>Status</th><th>Created</th><th></th></tr></thead>
            <tbody>${rows}</tbody>
        </table>`;
    }
    updatePager('storeProductsPageInfo', storeState.products);
}

async function bindStoreProductsEvents() {
    const filters = document.getElementById('adminStoreProductFilters');
    if (filters) {
        filters.addEventListener('submit', (e) => {
            e.preventDefault();
            storeState.products.filters = {
                status: document.getElementById('storeProductStatus').value,
                q: document.getElementById('storeProductQ').value.trim()
            };
            storeState.products.page = 1;
            loadStoreProducts().then(renderStoreProducts).catch(err => window.alert(err.message));
        });
    }
    bindPager('storeProductsPrev', 'storeProductsNext', storeState.products,
        () => loadStoreProducts().then(renderStoreProducts));

    const card = document.getElementById('adminStoreProductsCard');
    if (card) {
        card.addEventListener('click', async (e) => {
            const btn = e.target.closest('button[data-pact]');
            if (!btn) return;
            const code = btn.dataset.code;
            const act = btn.dataset.pact;
            try {
                if (act === 'activate' || act === 'deactivate') {
                    const json = await adminMutate(`/api/admin/store/products/${code}/${act}`, 'POST');
                    if (!json.success) throw new Error(apiError(json));
                } else if (act === 'edit') {
                    const p = storeState.products.items.find(x => x.code === code);
                    const title = window.prompt('Title:', p ? p.title : '');
                    if (title === null) return;
                    const body = {};
                    if (title.trim() !== (p ? p.title : '')) body.title = title;
                    const desc = window.prompt('Description (empty clears):', p && p.description ? p.description : '');
                    if (desc !== null && desc !== (p && p.description ? p.description : '')) body.description = desc;
                    const priceStr = window.prompt('Credits price:', p ? String(p.credits_price) : '');
                    if (priceStr !== null && priceStr !== '' && Number(priceStr) !== (p ? p.credits_price : null)) {
                        body.credits_price = Number(priceStr);
                    }
                    if (!Object.keys(body).length) return;
                    const json = await adminMutate(`/api/admin/store/products/${code}`, 'PATCH', body);
                    if (!json.success) throw new Error(apiError(json));
                }
                await loadStoreProducts();
                renderStoreProducts();
            } catch (err) {
                window.alert(err.message);
            }
        });
    }

    const createForm = document.getElementById('adminStoreProductCreateForm');
    if (createForm) {
        createForm.addEventListener('submit', async (e) => {
            e.preventDefault();
            try {
                const json = await adminMutate('/api/admin/store/products', 'POST', {
                    title: document.getElementById('storeNewTitle').value.trim(),
                    description: document.getElementById('storeNewDesc').value.trim(),
                    credits_price: Number(document.getElementById('storeNewPrice').value)
                });
                if (!json.success) throw new Error(apiError(json));
                setNotice('storeProductCreateNotice', 'Product created', 'ok');
                createForm.reset();
                await loadStoreProducts(); renderStoreProducts();
            } catch (err) {
                setNotice('storeProductCreateNotice', err.message, 'error');
            }
        });
    }
}

/* ---------- redemptions ---------- */

async function loadStoreRedemptions() {
    const st = storeState.redemptions;
    const params = new URLSearchParams({
        status: st.filters.status,
        page: String(st.page),
        page_size: String(st.pageSize)
    });
    if (st.filters.bot_id) params.set('bot_id', st.filters.bot_id);
    if (st.filters.q) params.set('q', st.filters.q);
    const json = await adminGet('/api/admin/store/redemptions?' + params.toString());
    if (!json.success) throw new Error(apiError(json, 'Failed to load redemptions'));
    st.items = json.data.redemptions || [];
    st.total = json.data.total || 0;
}

const REDEMPTION_ACTIONS = {
    pending_review: [
        {act: 'approve', label: 'Approve', cls: '', confirm: false},
        {act: 'reject', label: 'Reject', cls: 'danger', confirm: true},
        {act: 'cancel', label: 'Cancel', cls: 'danger', confirm: true}
    ],
    approved: [
        {act: 'fulfill', label: 'Fulfill', cls: '', confirm: false},
        {act: 'cancel', label: 'Cancel', cls: 'danger', confirm: true}
    ],
    fulfilled: [],
    rejected: [],
    cancelled: []
};

function renderStoreRedemptions() {
    const card = document.getElementById('adminStoreRedemptionsCard');
    if (!card) return;
    const canManage = hasPermission('store.redemptions.manage');
    if (!storeState.redemptions.items.length) {
        card.innerHTML = '<p class="muted">No redemptions.</p>';
    } else {
        const rows = storeState.redemptions.items.map(r => {
            const ops = canManage ? (REDEMPTION_ACTIONS[r.status] || []) : [];
            const actions = ops.map(op =>
                `<button class="btn small ${op.cls}" data-ract="${op.act}" data-code="${escapeHtml(r.code)}" data-title="${escapeHtml(r.product_title)}">${op.label}</button>`
            ).join(' ');
            return `<tr>
                <td><code>${escapeHtml(r.code)}</code></td>
                <td>${r.bot_id}</td>
                <td>${escapeHtml(r.product_title)}</td>
                <td>${fmtPrice(r.credits_cost)}</td>
                <td><span class="status">${escapeHtml(r.status)}</span></td>
                <td class="desc">${escapeHtml(r.review_note || '')}</td>
                <td>${fmtTime(r.created_at)}</td>
                <td class="actions">${actions}</td>
            </tr>`;
        }).join('');
        card.innerHTML = `<table class="admin-table wide">
            <thead><tr><th>Code</th><th>Bot</th><th>Product</th><th>Cost</th><th>Status</th><th>Review note</th><th>Created</th><th></th></tr></thead>
            <tbody>${rows}</tbody>
        </table>`;
    }
    updatePager('storeRedemptionsPageInfo', storeState.redemptions);
}

async function bindStoreRedemptionsEvents() {
    const filters = document.getElementById('adminStoreRedemptionFilters');
    if (filters) {
        filters.addEventListener('submit', (e) => {
            e.preventDefault();
            storeState.redemptions.filters = {
                status: document.getElementById('storeRedemptionStatus').value,
                bot_id: document.getElementById('storeRedemptionBotID').value.trim(),
                q: document.getElementById('storeRedemptionQ').value.trim()
            };
            storeState.redemptions.page = 1;
            loadStoreRedemptions().then(renderStoreRedemptions).catch(err => window.alert(err.message));
        });
    }
    bindPager('storeRedemptionsPrev', 'storeRedemptionsNext', storeState.redemptions,
        () => loadStoreRedemptions().then(renderStoreRedemptions));

    const card = document.getElementById('adminStoreRedemptionsCard');
    if (card) {
        card.addEventListener('click', async (e) => {
            const btn = e.target.closest('button[data-ract]');
            if (!btn) return;
            const code = btn.dataset.code;
            const act = btn.dataset.ract;
            // economic refund actions need explicit confirmation
            const confirmNeeded = (act === 'reject' || act === 'cancel');
            if (confirmNeeded) {
                const verb = act === 'reject' ? 'Reject' : 'Cancel';
                if (!window.confirm(`${verb} redemption ${code} (${btn.dataset.title}) and REFUND the credits?`)) return;
            }
            try {
                let body = {};
                if (act === 'reject') {
                    const note = window.prompt('Review note (optional):', '');
                    if (note === null) return;
                    body = {review_note: note};
                } else if (act === 'approve') {
                    const note = window.prompt('Review note (optional):', '');
                    if (note === null) return;
                    body = {review_note: note};
                } else if (act === 'fulfill') {
                    const note = window.prompt('Fulfillment note (optional):', '');
                    if (note === null) return;
                    body = {fulfillment_note: note};
                }
                const json = await adminMutate(`/api/admin/store/redemptions/${code}/${act}`, 'POST', body);
                if (!json.success) throw new Error(apiError(json));
                await loadStoreRedemptions();
                renderStoreRedemptions();
            } catch (err) {
                window.alert(err.message);
            }
        });
    }
}

/* ---------- shared pager helpers ---------- */

function updatePager(infoId, pagerState) {
    const info = document.getElementById(infoId);
    if (info) {
        const pages = Math.max(1, Math.ceil(pagerState.total / pagerState.pageSize));
        info.textContent = `page ${pagerState.page} / ${pages} (${pagerState.total} total)`;
    }
}

function bindPager(prevId, nextId, pagerState, reload) {
    const prev = document.getElementById(prevId);
    const next = document.getElementById(nextId);
    if (prev) prev.addEventListener('click', () => {
        if (pagerState.page > 1) { pagerState.page--; reload(); }
    });
    if (next) next.addEventListener('click', () => {
        const pages = Math.max(1, Math.ceil(pagerState.total / pagerState.pageSize));
        if (pagerState.page < pages) { pagerState.page++; reload(); }
    });
}
