<section class="panel">
    <div class="section-head">
        <div class="section-head-copy">
            <h2><?= htmlspecialchars(app_t('owner.tasks.heading', [], $APP_LOCALE)) ?></h2>
            <p><?= htmlspecialchars(app_t('owner.tasks.summary', [], $APP_LOCALE)) ?></p>
        </div>
        <div class="section-head-actions">
            <a class="btn primary" href="<?= htmlspecialchars(app_i18n_locale_url($APP_LOCALE, '/owner/tasks/new')) ?>"><?= htmlspecialchars(app_t('owner.tasks.new_task', [], $APP_LOCALE)) ?></a>
        </div>
    </div>
</section>
<section class="task-layout">
    <div class="panel">
        <h2><?= htmlspecialchars(app_t('owner.tasks.my_tasks', [], $APP_LOCALE)) ?></h2>
        <div class="task-list" id="taskList"></div>
    </div>
    <div class="panel">
        <div id="taskDetail">
            <p class="muted"><?= htmlspecialchars(app_t('owner.tasks.select_hint', [], $APP_LOCALE)) ?></p>
        </div>
        <form id="budgetForm" class="detail-box" hidden>
            <h3><?= htmlspecialchars(app_t('owner.tasks.add_budget', [], $APP_LOCALE)) ?></h3>
            <label><?= htmlspecialchars(app_t('owner.tasks.amount', [], $APP_LOCALE)) ?></label>
            <input name="amount" type="number" step="0.0001" min="0" required>
            <div class="actions">
                <button class="btn primary" type="submit"><?= htmlspecialchars(app_t('owner.tasks.add_budget_submit', [], $APP_LOCALE)) ?></button>
            </div>
        </form>
        <div id="taskNotice" class="notice"><?= htmlspecialchars(app_t('owner.tasks.notice', [], $APP_LOCALE)) ?></div>
    </div>
</section>
