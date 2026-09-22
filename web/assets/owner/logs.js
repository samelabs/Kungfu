// logsReload runs the logs read through the SHARED section lifecycle
// (runSection/sectionBox): loading → ready/empty/unavailable/error
// with a persistent Retry. No direct loadLogs()+renderLogs() paths
// and no toast-only errors remain for page-level reads.
//
// On pagination failure the page number is rolled back so the UI
// never shows "new page + old data" — the box shows the error state
// and Retry reloads the SAME (rolled-back) page.
function logsReload(previous) {
    return runSection('logs', loadLogs, '#logsTableWrap', () => renderLogs(), {
        isEmpty: () => !state.logs.items.length,
        emptyKey: 'js.state_logs_empty'
    }).catch(() => {
        if (previous && state.logs.page !== previous.page) {
            state.logs.page = previous.page;
            state.logs.taskCode = previous.taskCode;
            state.logs.type = previous.type;
        }
    });
}

function bindLogsTypeButtons() {
    qsa('[data-log-type]').forEach((button) => {
        button.addEventListener('click', () => {
            const previous = {type: state.logs.type, page: state.logs.page, taskCode: state.logs.taskCode};
            state.logs.type = button.dataset.logType;
            state.logs.page = 1;
            if (state.logs.type !== 'task') {
                state.logs.taskCode = '';
            }
            logsReload(previous);
        });
    });
}

function bindLogsTaskFilter() {
    const taskFilter = qs('#logTaskFilter');
    if (!taskFilter) return;
    taskFilter.addEventListener('change', (event) => {
        const previous = {type: state.logs.type, page: state.logs.page, taskCode: state.logs.taskCode};
        state.logs.taskCode = String(event.currentTarget.value || '');
        state.logs.page = 1;
        logsReload(previous);
    });
}

function bindLogsPagination() {
    const prevBtn = qs('#logsPrevBtn');
    const nextBtn = qs('#logsNextBtn');
    if (prevBtn) {
        prevBtn.addEventListener('click', () => {
            if (state.logs.page <= 1) return;
            const previous = {type: state.logs.type, page: state.logs.page, taskCode: state.logs.taskCode};
            state.logs.page -= 1;
            logsReload(previous);
        });
    }
    if (nextBtn) {
        nextBtn.addEventListener('click', () => {
            if (state.logs.page >= state.logs.totalPages) return;
            const previous = {type: state.logs.type, page: state.logs.page, taskCode: state.logs.taskCode};
            state.logs.page += 1;
            logsReload(previous);
        });
    }
}

function bindLogsHandlers() {
    if (SECTION !== 'logs') return;
    bindLogsTypeButtons();
    bindLogsTaskFilter();
    bindLogsPagination();
}