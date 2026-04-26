<section class="panel">
    <h2><?= htmlspecialchars(app_t('owner.logs.heading', [], $APP_LOCALE)) ?></h2>
    <p><?= htmlspecialchars(app_t('owner.logs.summary', [], $APP_LOCALE)) ?></p>
    <div class="actions">
        <button class="btn primary" type="button" data-log-type="credits"><?= htmlspecialchars(app_t('owner.logs.credits', [], $APP_LOCALE)) ?></button>
        <button class="btn" type="button" data-log-type="agent"><?= htmlspecialchars(app_t('owner.logs.agent_logs', [], $APP_LOCALE)) ?></button>
        <button class="btn" type="button" data-log-type="task"><?= htmlspecialchars(app_t('owner.logs.task_logs', [], $APP_LOCALE)) ?></button>
    </div>
    <div id="logsFilters" class="actions logs-filters">
        <select id="logTaskFilter" hidden>
            <option value=""><?= htmlspecialchars(app_t('owner.logs.all_tasks', [], $APP_LOCALE)) ?></option>
        </select>
    </div>
    <div id="logsSummary" class="keybox logs-summary"><?= htmlspecialchars(app_t('owner.logs.loading_logs', [], $APP_LOCALE)) ?></div>
    <div id="logsTableWrap" class="detail-box logs-table-wrap">
        <div class="muted"><?= htmlspecialchars(app_t('owner.logs.loading', [], $APP_LOCALE)) ?></div>
    </div>
    <div class="actions logs-pager">
        <button class="btn" type="button" id="logsPrevBtn"><?= htmlspecialchars(app_t('owner.logs.previous', [], $APP_LOCALE)) ?></button>
        <div class="mono" id="logsPageInfo"><?= htmlspecialchars(app_t('owner.logs.page_info', ['page' => 1, 'totalPages' => 1, 'total' => 0], $APP_LOCALE)) ?></div>
        <button class="btn" type="button" id="logsNextBtn"><?= htmlspecialchars(app_t('owner.logs.next', [], $APP_LOCALE)) ?></button>
    </div>
    <div id="logsNotice" class="notice"><?= htmlspecialchars(app_t('owner.logs.notice', [], $APP_LOCALE)) ?></div>
</section>
