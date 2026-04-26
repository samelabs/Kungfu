<section class="auth-shell panel">
    <h2><?= htmlspecialchars(app_t('owner.auth.login_heading', [], $APP_LOCALE)) ?></h2>
    <form id="loginForm" novalidate>
        <label><?= htmlspecialchars(app_t('owner.auth.kungfu_id', [], $APP_LOCALE)) ?></label>
        <input name="name" autocomplete="username" required minlength="6" maxlength="32">
        <label><?= htmlspecialchars(app_t('owner.auth.password', [], $APP_LOCALE)) ?></label>
        <input name="password" type="password" autocomplete="current-password" required minlength="6" maxlength="128">
        <div class="actions">
            <button class="btn primary" type="submit"><?= htmlspecialchars(app_t('owner.auth.login', [], $APP_LOCALE)) ?></button>
            <a class="btn" href="<?= htmlspecialchars(app_i18n_locale_url($APP_LOCALE, '/owner/register')) ?>"><?= htmlspecialchars(app_t('owner.auth.register', [], $APP_LOCALE)) ?></a>
        </div>
        <div id="loginNotice" class="notice"><?= htmlspecialchars(app_t('owner.auth.login_notice', [], $APP_LOCALE)) ?></div>
    </form>
</section>
