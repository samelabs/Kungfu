<section class="panel">
    <h2 id="ownerName"><?= htmlspecialchars(app_t('owner.overview.heading', [], $APP_LOCALE)) ?></h2>
    <p id="ownerMeta"><?= htmlspecialchars(app_t('owner.overview.meta', [], $APP_LOCALE)) ?></p>
    <div class="stats" id="statsGrid">
        <div class="stat"><b>-</b><span><?= htmlspecialchars(app_t('owner.overview.balance', [], $APP_LOCALE)) ?></span></div>
        <div class="stat"><b>-</b><span><?= htmlspecialchars(app_t('owner.overview.kungfu', [], $APP_LOCALE)) ?></span></div>
        <div class="stat"><b>-</b><span><?= htmlspecialchars(app_t('owner.overview.public', [], $APP_LOCALE)) ?></span></div>
        <div class="stat"><b>-</b><span><?= htmlspecialchars(app_t('owner.overview.tasks', [], $APP_LOCALE)) ?></span></div>
    </div>
    <div id="keyBox" class="keybox"></div>
    <div class="actions">
        <button class="btn primary" type="button" id="copyKeyBtn"><?= htmlspecialchars(app_t('owner.overview.copy_key', [], $APP_LOCALE)) ?></button>
        <button class="btn" type="button" id="reloadBtn"><?= htmlspecialchars(app_t('owner.overview.reload', [], $APP_LOCALE)) ?></button>
    </div>
    <div id="overviewNotice" class="notice overview-notice"><?= htmlspecialchars(app_t('owner.overview.notice', [], $APP_LOCALE)) ?></div>
</section>
