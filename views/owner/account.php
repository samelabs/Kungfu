<section class="panel">
    <h2><?= htmlspecialchars(app_t('owner.account.heading', [], $APP_LOCALE)) ?></h2>
    <form id="passwordForm" novalidate>
        <label><?= htmlspecialchars(app_t('owner.account.current_password', [], $APP_LOCALE)) ?></label>
        <input name="password" type="password" autocomplete="current-password" required minlength="6" maxlength="128">
        <label><?= htmlspecialchars(app_t('owner.account.new_password', [], $APP_LOCALE)) ?></label>
        <input name="new_password" type="password" autocomplete="new-password" required minlength="6" maxlength="128">
        <div class="actions">
            <button class="btn primary" type="submit"><?= htmlspecialchars(app_t('owner.account.submit', [], $APP_LOCALE)) ?></button>
        </div>
        <div id="passwordNotice" class="notice"><?= htmlspecialchars(app_t('owner.account.notice', [], $APP_LOCALE)) ?></div>
    </form>
</section>
