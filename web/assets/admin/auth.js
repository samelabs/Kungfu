/* Admin auth: restore browser state from GET /api/admin/session,
 * login/logout handlers. */
'use strict';

async function restorePrincipal() {
    const json = await adminGet('/api/admin/session');
    if (json.success && json.data) {
        state.principal = json.data;
        return true;
    }
    return false;
}

function requirePrincipalElseRedirect() {
    if (!state.principal && SECTION !== 'login') {
        window.location.assign('/admin/login');
        return false;
    }
    return true;
}

function bindAdminAuthEvents() {
    const loginForm = document.getElementById('adminLoginForm');
    if (loginForm) {
        loginForm.addEventListener('submit', async (e) => {
            e.preventDefault();
            setNotice('adminLoginNotice', '', '');
            const username = document.getElementById('adminLoginUsername').value.trim();
            const password = document.getElementById('adminLoginPassword').value;
            try {
                const json = await adminMutate('/api/admin/session', 'POST', {username, password});
                if (!json.success) throw new Error(apiError(json, 'Login failed'));
                state.principal = {
                    id: json.data.id,
                    username: json.data.username,
                    display_name: json.data.display_name,
                    roles: json.data.roles || [],
                    permissions: [],
                    csrf_token: json.data.csrf_token
                };
                // Re-fetch the full principal (with permissions) now that
                // the cookie is set.
                await restorePrincipal();
                window.location.assign('/admin');
            } catch (err) {
                setNotice('adminLoginNotice', err.message, 'error');
            }
        });
    }

    const logoutBtn = document.getElementById('adminLogoutBtn');
    if (logoutBtn) {
        logoutBtn.addEventListener('click', async () => {
            try {
                await adminMutate('/api/admin/session', 'DELETE');
            } catch (e) { /* cookie cleared server-side anyway */ }
            window.location.assign('/admin/login');
        });
    }
}
