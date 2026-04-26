<section class="auth-only auth-shell auth-landing panel">
    <span class="auth-kicker"><?= htmlspecialchars(app_t('owner.auth.landing_kicker', [], $APP_LOCALE)) ?></span>
    <h2><?= htmlspecialchars(app_t('owner.auth.landing_title', [], $APP_LOCALE)) ?></h2>
    <p class="auth-copy"><?= htmlspecialchars(app_t('owner.auth.landing_copy', [], $APP_LOCALE)) ?></p>
    <div class="auth-feature-list" aria-hidden="true">
        <span class="auth-feature-pill"><?= htmlspecialchars(app_t('owner.auth.landing_tasks', [], $APP_LOCALE)) ?></span>
        <span class="auth-feature-pill"><?= htmlspecialchars(app_t('owner.auth.landing_budget', [], $APP_LOCALE)) ?></span>
        <span class="auth-feature-pill"><?= htmlspecialchars(app_t('owner.auth.landing_logs', [], $APP_LOCALE)) ?></span>
        <span class="auth-feature-pill"><?= htmlspecialchars(app_t('owner.auth.landing_key', [], $APP_LOCALE)) ?></span>
    </div>
    <div class="actions auth-actions">
        <a class="btn primary" href="<?= htmlspecialchars(app_i18n_locale_url($APP_LOCALE, '/owner/login')) ?>"><?= htmlspecialchars(app_t('owner.auth.login', [], $APP_LOCALE)) ?></a>
        <a class="btn" href="<?= htmlspecialchars(app_i18n_locale_url($APP_LOCALE, '/owner/register')) ?>"><?= htmlspecialchars(app_t('owner.auth.register', [], $APP_LOCALE)) ?></a>
    </div>
</section>
