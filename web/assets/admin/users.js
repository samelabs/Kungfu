/* Admin users management: list, create, edit display name,
 * enable/disable, password reset, role assignment, force logout. */
'use strict';

async function loadUsers() {
    const json = await adminGet('/api/admin/users');
    if (!json.success) throw new Error(apiError(json, 'Failed to load admins'));
    state.users = json.data.users || [];
}

function renderUsers() {
    const card = document.getElementById('adminUsersCard');
    if (!card) return;
    if (!state.users.length) {
        card.innerHTML = '<p class="muted">No admins.</p>';
        return;
    }
    const canManage = hasPermission('admin.users.manage');
    const canRoles = hasPermission('admin.roles.manage');
    const rows = state.users.map(u => {
        const roles = (u.roles || []).map(r => `<span class="tag">${escapeHtml(r)}</span>`).join(' ');
        const actions = [];
        if (canManage) {
            actions.push(`<button class="btn small" data-act="rename" data-id="${u.id}" data-name="${escapeHtml(u.display_name)}">Rename</button>`);
            actions.push(`<button class="btn small" data-act="resetpw" data-id="${u.id}">Reset password</button>`);
            actions.push(u.status === 'active'
                ? `<button class="btn small danger" data-act="disable" data-id="${u.id}">Disable</button>`
                : `<button class="btn small" data-act="enable" data-id="${u.id}">Enable</button>`);
        }
        if (canRoles) {
            actions.push(`<button class="btn small" data-act="roles" data-id="${u.id}">Roles</button>`);
        }
        return `<tr>
            <td>${u.id}</td>
            <td>${escapeHtml(u.username)}</td>
            <td data-name-cell="${u.id}">${escapeHtml(u.display_name)}</td>
            <td><span class="status ${u.status === 'active' ? 'ok' : 'muted'}">${escapeHtml(u.status)}</span></td>
            <td>${roles}</td>
            <td class="actions">${actions.join(' ')}</td>
        </tr>`;
    }).join('');
    card.innerHTML = `<table class="admin-table">
        <thead><tr><th>ID</th><th>Username</th><th>Display name</th><th>Status</th><th>Roles</th><th></th></tr></thead>
        <tbody>${rows}</tbody>
    </table>`;
}

function rolePickerOverlay(title, currentRoleIds, onApply) {
    const overlay = document.createElement('div');
    overlay.className = 'admin-overlay';
    const options = state.roles
        .filter(r => r.status === 'active' || currentRoleIds.includes(r.id))
        .map(r => `<label class="check">
            <input type="checkbox" name="rolePick" value="${r.id}" ${currentRoleIds.includes(r.id) ? 'checked' : ''}>
            ${escapeHtml(r.code)}${r.is_system ? ' (system)' : ''}
        </label>`).join('');
    overlay.innerHTML = `<div class="admin-modal">
        <h3>${escapeHtml(title)}</h3>
        <div class="role-list">${options || '<p class="muted">No roles available.</p>'}</div>
        <div class="modal-actions">
            <button class="btn" data-x="cancel" type="button">Cancel</button>
            <button class="btn primary" data-x="ok" type="button">Apply</button>
        </div>
    </div>`;
    document.body.appendChild(overlay);
    overlay.querySelector('[data-x="cancel"]').addEventListener('click', () => overlay.remove());
    overlay.querySelector('[data-x="ok"]').addEventListener('click', () => {
        const ids = Array.from(overlay.querySelectorAll('input[name="rolePick"]:checked'))
            .map(i => parseInt(i.value, 10));
        overlay.remove();
        onApply(ids);
    });
}

async function bindAdminUsersEvents() {
    const card = document.getElementById('adminUsersCard');
    if (!card) return;
    card.addEventListener('click', async (e) => {
        const btn = e.target.closest('button[data-act]');
        if (!btn) return;
        const id = parseInt(btn.dataset.id, 10);
        const act = btn.dataset.act;
        try {
            if (act === 'disable') {
                const json = await adminMutate(`/api/admin/users/${id}/disable`, 'POST');
                if (!json.success) throw new Error(apiError(json));
            } else if (act === 'enable') {
                const json = await adminMutate(`/api/admin/users/${id}/enable`, 'POST');
                if (!json.success) throw new Error(apiError(json));
            } else if (act === 'rename') {
                const next = window.prompt('New display name:', btn.dataset.name || '');
                if (next === null) return;
                const json = await adminMutate(`/api/admin/users/${id}`, 'PATCH', {display_name: next});
                if (!json.success) throw new Error(apiError(json));
            } else if (act === 'resetpw') {
                const next = window.prompt('New password for this admin (min 8 chars):', '');
                if (next === null) return;
                const json = await adminMutate(`/api/admin/users/${id}/password`, 'PUT', {password: next});
                if (!json.success) throw new Error(apiError(json));
            } else if (act === 'roles') {
                const target = state.users.find(u => u.id === id);
                rolePickerOverlay('Roles for ' + (target ? target.username : '#' + id),
                    [], async (roleIds) => {
                        const json = await adminMutate(`/api/admin/users/${id}/roles`, 'PUT', {role_ids: roleIds});
                        if (!json.success) window.alert(apiError(json));
                        await loadUsers(); renderUsers();
                    });
            }
            await loadUsers();
            renderUsers();
        } catch (err) {
            window.alert(err.message);
        }
    });

    const createForm = document.getElementById('adminUserCreateForm');
    if (createForm) {
        createForm.addEventListener('submit', async (e) => {
            e.preventDefault();
            try {
                const json = await adminMutate('/api/admin/users', 'POST', {
                    username: document.getElementById('adminNewUsername').value.trim(),
                    display_name: document.getElementById('adminNewDisplayName').value.trim(),
                    password: document.getElementById('adminNewUserPassword').value
                });
                if (!json.success) throw new Error(apiError(json));
                setNotice('adminUserCreateNotice', 'Admin created', 'ok');
                createForm.reset();
                await loadUsers(); renderUsers();
            } catch (err) {
                setNotice('adminUserCreateNotice', err.message, 'error');
            }
        });
    }
}
