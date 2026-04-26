function bindLogsTypeButtons() {
    qsa('[data-log-type]').forEach((button) => {
        button.addEventListener('click', async () => {
            state.logs.type = button.dataset.logType;
            state.logs.page = 1;
            if (state.logs.type !== 'task') {
                state.logs.taskCode = '';
            }
            setNotice('logsNotice', t('logs.loading_logs'));
            try {
                await loadLogs();
                renderLogs();
                setNotice('logsNotice', t('logs.loaded'), 'ok');
            } catch (error) {
                setNotice('logsNotice', String(error), 'error');
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
        setNotice('logsNotice', t('logs.applying_filter'));
        try {
            await loadLogs();
            renderLogs();
            setNotice('logsNotice', t('logs.filter_applied'), 'ok');
        } catch (error) {
            setNotice('logsNotice', String(error), 'error');
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
            setNotice('logsNotice', t('logs.loading_previous'));
            try {
                await loadLogs();
                renderLogs();
                setNotice('logsNotice', t('logs.page_loaded'), 'ok');
            } catch (error) {
                setNotice('logsNotice', String(error), 'error');
            }
        });
    }
    if (nextBtn) {
        nextBtn.addEventListener('click', async () => {
            if (state.logs.page >= state.logs.totalPages) return;
            state.logs.page += 1;
            setNotice('logsNotice', t('logs.loading_next'));
            try {
                await loadLogs();
                renderLogs();
                setNotice('logsNotice', t('logs.page_loaded'), 'ok');
            } catch (error) {
                setNotice('logsNotice', String(error), 'error');
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
