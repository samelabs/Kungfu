function bindLogsTypeButtons() {
    qsa('[data-log-type]').forEach((button) => {
        button.addEventListener('click', async () => {
            state.logs.type = button.dataset.logType;
            state.logs.page = 1;
            if (state.logs.type !== 'task') {
                state.logs.taskCode = '';
            }
            // loading is silent
            try {
                await loadLogs();
                renderLogs();
                // loaded silently
            } catch (error) {
                showToast(noticeText(String(error)), 'error')
            }
        });
    });
}

function bindLogsTaskFilter() {
    const taskFilter = qs('#logTaskFilter');
    if (!taskFilter) return;
    taskFilter.addEventListener('change', async (event) => {
        state.logs.taskCode = String(event.currentTarget.value || '');
        state.logs.page = 1;
        // filter application is silent
        try {
            await loadLogs();
            renderLogs();
            // filter result visible in table
        } catch (error) {
            showToast(noticeText(String(error)), 'error')
        }
    });
}

function bindLogsPagination() {
    const prevBtn = qs('#logsPrevBtn');
    const nextBtn = qs('#logsNextBtn');
    if (prevBtn) {
        prevBtn.addEventListener('click', async () => {
            if (state.logs.page <= 1) return;
            state.logs.page -= 1;
            // pagination load is silent
            try {
                await loadLogs();
                renderLogs();
                // page load is silent
            } catch (error) {
                showToast(noticeText(String(error)), 'error')
            }
        });
    }
    if (nextBtn) {
        nextBtn.addEventListener('click', async () => {
            if (state.logs.page >= state.logs.totalPages) return;
            state.logs.page += 1;
            // pagination — silent
            try {
                await loadLogs();
                renderLogs();
                // page load is silent
            } catch (error) {
                showToast(noticeText(String(error)), 'error')
            }
        });
    }
}

function bindLogsHandlers() {
    if (SECTION !== 'logs') return;
    bindLogsTypeButtons();
    bindLogsTaskFilter();
    bindLogsPagination();
}
