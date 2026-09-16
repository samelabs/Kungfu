/* Admin roles management: list, create, edit, permission assignment. */
'use strict';

async function loadRoles() {
    const json = await adminGet('/api/admin/roles');
    if (!json.success) throw new Error(apiError(json, 'Failed to load roles'));
    state.roles = json.data.roles || [];
}

async function loadPermissions() {
    const json = await adminGet('/api/admin/permissions');
    if (!json.success) throw new Error(apiError(json, 'Failed to load permissions'));
    state.permissions = json.data.permissions || [];
}

function renderRoles() {
    const card = document.getElementById('adminRolesCard');
    if (!card) return;
    if (!state.roles.length) {
        card.innerHTML = '<p class="muted">No roles.</p>';
        return;
    }
    const canManage = hasPermission('admin.roles.manage');
    const rows = state.roles.map(r => {
        const perms = (r.permissions || []).map(p => `<span class="tag">${escapeHtml(p)}</span>`).join(' ');
        const actions = [];
        if (canManage && !r.is_system) {
            actions.push(`<button class="btn small" data-act="rename" data-id="${r.id}" data-name="${escapeHtml(r.name)}">Rename</button>`);
            actions.push(`<button class="btn small" data-act="editdesc" data-id="${r.id}">Description</button>`);
            actions.push(r.status === 'active'
                ? `<button class="btn small danger" data-act="disable" data-id="${r.id}">Disable</button>`
                : `<button class="btn small" data-act="enable" data-id="${r.id}">Enable</button>`);
        }
        if (canManage && !r.is_system) {
            actions.push(`<button class="btn small" data-act="perms" data-id="${r.id}">Permissions</button>`);
        }
        return `<tr>
            <td>${r.id}</td>
            <td>${escapeHtml(r.code)}${r.is_system ? ' <span class="tag">system</span>' : ''}</td>
            <td>${escapeHtml(r.name)}</td>
            <td><span class="status ${r.status === 'active' ? 'ok' : 'muted'}">${escapeHtml(r.status)}</span></td>
            <td>${perms}</td>
            <td class="actions">${actions.join(' ')}</td>
        </tr>`;
    }).join('');
    card.innerHTML = `<table class="admin-table">
        <thead><tr><th>ID</th><th>Code</th><th>Name</th><th>Status</th><th>Permissions</th><th></th></tr></thead>
        <tbody>${rows}</tbody>
    </table>`;
}

function renderPermissions() {
    const card = document.getElementById('adminPermissionsCard');
    if (!card) return;
    card.innerHTML = '<ul class="perm-list">' + state.permissions.map(p =>
        `<li><code>${escapeHtml(p.code)}</code> — ${escapeHtml(p.description || '')}</li>`).join('') + '</ul>';
}

function permissionPickerOverlay(role, onApply) {
    const overlay = document.createElement('div');
    overlay.className = 'admin-overlay';
    const current = new Set(role.permissions || []);
    const options = state.permissions
        .filter(p => p.code !== '*')
        .map(p => `<label class="check">
            <input type="checkbox" name="permPick" value="${escapeHtml(p.code)}" ${current.has(p.code) ? 'checked' : ''}>
            <code>${escapeHtml(p.code)}</code>
        </label>`).join('');
    overlay.innerHTML = `<div class="admin-modal">
        <h3>Permissions — ${escapeHtml(role.code)}</h3>
        <div class="role-list">${options}</div>
        <p class="muted">Complete replacement: only the checked permissions will remain.</p>
        <div class="modal-actions">
            <button class="btn" data-x="cancel" type="button">Cancel</button>
            <button class="btn primary" data-x="ok" type="button">Apply</button>
        </div>
    </div>`;
    document.body.appendChild(overlay);
    overlay.querySelector('[data-x="cancel"]').addEventListener('click', () => overlay.remove());
    overlay.querySelector('[data-x="ok"]').addEventListener('click', () => {
        const codes = Array.from(overlay.querySelectorAll('input[name="permPick"]:checked'))
            .map(i => i.value);
        overlay.remove();
        onApply(codes);
    });
}

async function bindAdminRolesEvents() {
    const card = document.getElementById('adminRolesCard');
    if (card) {
        card.addEventListener('click', async (e) => {
            const btn = e.target.closest('button[data-act]');
            if (!btn) return;
            const id = parseInt(btn.dataset.id, 10);
            const act = btn.dataset.act;
            try {
                if (act === 'disable' || act === 'enable') {
                    // status-only PATCH: name/description preserved
                    const json = await adminMutate(`/api/admin/roles/${id}`, 'PATCH', {
                        status: act === 'disable' ? 'disabled' : 'active'
                    });
                    if (!json.success) throw new Error(apiError(json));
                } else if (act === 'rename') {
                    const role = state.roles.find(x => x.id === id);
                    const next = window.prompt('New role name:', role ? role.name : '');
                    if (next === null) return;
                    // name-only PATCH: status/description preserved
                    const json = await adminMutate(`/api/admin/roles/${id}`, 'PATCH', {name: next});
                    if (!json.success) throw new Error(apiError(json));
                } else if (act === 'editdesc') {
                    const role = state.roles.find(x => x.id === id);
                    const next = window.prompt('Role description (empty clears it):',
                        role && role.description ? role.description : '');
                    if (next === null) return;
                    const json = await adminMutate(`/api/admin/roles/${id}`, 'PATCH', {description: next});
                    if (!json.success) throw new Error(apiError(json));
                } else if (act === 'perms') {
                    const role = state.roles.find(r => r.id === id);
                    if (!role) return;
                    permissionPickerOverlay(role, async (codes) => {
                        const json = await adminMutate(`/api/admin/roles/${id}/permissions`, 'PUT', {permission_codes: codes});
                        if (!json.success) window.alert(apiError(json));
                        await loadRoles(); renderRoles();
                    });
                }
                await loadRoles(); renderRoles();
            } catch (err) {
                window.alert(err.message);
            }
        });
    }

    const createForm = document.getElementById('adminRoleCreateForm');
    if (createForm) {
        createForm.addEventListener('submit', async (e) => {
            e.preventDefault();
            try {
                const json = await adminMutate('/api/admin/roles', 'POST', {
                    code: document.getElementById('adminNewRoleCode').value.trim(),
                    name: document.getElementById('adminNewRoleName').value.trim(),
                    description: document.getElementById('adminNewRoleDesc').value.trim()
                });
                if (!json.success) throw new Error(apiError(json));
                setNotice('adminRoleCreateNotice', 'Role created', 'ok');
                createForm.reset();
                await loadRoles(); renderRoles();
            } catch (err) {
                setNotice('adminRoleCreateNotice', err.message, 'error');
            }
        });
    }
}
