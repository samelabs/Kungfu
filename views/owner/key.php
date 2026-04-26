<section class="panel">
    <h2><?= htmlspecialchars(app_t('owner.key.heading', [], $APP_LOCALE)) ?></h2>
    <div id="keyBox" class="keybox overview-keybox is-empty"></div>
    <div class="actions">
        <button class="btn primary" type="button" id="copyKeyBtn"><?= htmlspecialchars(app_t('owner.key.copy', [], $APP_LOCALE)) ?></button>
    </div>
    <div class="actions">
        <button class="btn primary" type="button" id="resetKeyBtn"><?= htmlspecialchars(app_t('owner.key.reset', [], $APP_LOCALE)) ?></button>
    </div>
    <div id="resetNotice" class="notice"><?= htmlspecialchars(app_t('owner.key.notice', [], $APP_LOCALE)) ?></div>
</section>
