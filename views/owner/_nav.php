<nav class="nav" aria-label="Owner Workspace">
    <a class="btn <?= $section === 'overview' ? 'active' : '' ?>" href="<?= htmlspecialchars(app_i18n_locale_url($APP_LOCALE, '/owner')) ?>"><?= htmlspecialchars(app_t('owner.nav.overview', [], $APP_LOCALE)) ?></a>
    <a class="btn <?= $section === 'account' ? 'active' : '' ?>" href="<?= htmlspecialchars(app_i18n_locale_url($APP_LOCALE, '/owner/account')) ?>"><?= htmlspecialchars(app_t('owner.nav.account', [], $APP_LOCALE)) ?></a>
    <a class="btn <?= $section === 'key' ? 'active' : '' ?>" href="<?= htmlspecialchars(app_i18n_locale_url($APP_LOCALE, '/owner/key')) ?>"><?= htmlspecialchars(app_t('owner.nav.key', [], $APP_LOCALE)) ?></a>
    <a class="btn <?= in_array($section, ['tasks', 'task_new'], true) ? 'active' : '' ?>" href="<?= htmlspecialchars(app_i18n_locale_url($APP_LOCALE, '/owner/tasks')) ?>"><?= htmlspecialchars(app_t('owner.nav.tasks', [], $APP_LOCALE)) ?></a>
    <a class="btn <?= $section === 'logs' ? 'active' : '' ?>" href="<?= htmlspecialchars(app_i18n_locale_url($APP_LOCALE, '/owner/logs')) ?>"><?= htmlspecialchars(app_t('owner.nav.logs', [], $APP_LOCALE)) ?></a>
    <a class="btn" href="<?= htmlspecialchars(app_i18n_locale_url($APP_LOCALE, '/owner/task-guide')) ?>"><?= htmlspecialchars(app_t('owner.nav.task_guide', [], $APP_LOCALE)) ?></a>
    <button class="btn danger" id="logoutBtn" type="button"><?= htmlspecialchars(app_t('owner.nav.logout', [], $APP_LOCALE)) ?></button>
</nav>
