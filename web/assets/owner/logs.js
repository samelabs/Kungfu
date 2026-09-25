// logsReload runs the logs read through the SHARED section lifecycle
// (runSection/sectionBox): loading → ready/empty/unavailable/error
// with a persistent Retry. No direct loadLogs()+renderLogs() paths
// and no toast-only errors remain for page-level reads.
//
// On pagination failure the page number is rolled back so the UI
// never shows "new page + old data" — the box shows the error state
// and Retry reloads the SAME (rolled-back) page.
function logsReload(previous) {
    // The outcome tells the caller whether the load succeeded; the
    // section error itself is already persistently rendered by
    // runSection/sectionBox (persistent error + Retry stays).
    return runSection('logs', loadLogs, '#logsTableWrap', () => renderLogs(), {
        isEmpty: () => !state.logs.items.length,
        emptyKey: 'js.state_logs_empty'
    }).then((outcome) => {
        if (previous && outcome && !outcome.ok) {
            // Roll the read state back so Retry reloads the previous
            // page/filter/type and the UI never shows new-page+old-data.
            state.logs.page = previous.page;
            state.logs.type = previous.type;
            // Do NOT renderLogs() here — the persistent error state
            // must stay visible until Retry succeeds.
        }
        return outcome;
    });
}

function bindLogsTypeButtons() {
    qsa('[data-log-type]').forEach((button) => {
        button.addEventListener('click', () => {
            const previous = {type: state.logs.type, page: state.logs.page};
            state.logs.type = button.dataset.logType;
            state.logs.page = 1;
            logsReload(previous);
        });
    });
}

function bindLogsPagination() {
    const prevBtn = qs('#logsPrevBtn');
    const nextBtn = qs('#logsNextBtn');
    if (prevBtn) {
        prevBtn.addEventListener('click', () => {
            if (state.logs.page <= 1) return;
            const previous = {type: state.logs.type, page: state.logs.page};
            state.logs.page -= 1;
            logsReload(previous);
        });
    }
    if (nextBtn) {
        nextBtn.addEventListener('click', () => {
            if (state.logs.page >= state.logs.totalPages) return;
            const previous = {type: state.logs.type, page: state.logs.page};
            state.logs.page += 1;
            logsReload(previous);
        });
    }
}

function bindLogsHandlers() {
    if (SECTION !== 'logs') return;
    bindLogsTypeButtons();
    bindLogsPagination();
}