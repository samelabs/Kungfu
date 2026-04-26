<section class="panel">
    <h2><?= htmlspecialchars(app_t('owner.task_new.heading', [], $APP_LOCALE)) ?></h2>
    <form id="taskForm" class="task-create-form" novalidate>
        <label><?= htmlspecialchars(app_t('owner.task_new.title', [], $APP_LOCALE)) ?></label>
        <input name="title" required maxlength="128">
        <label><?= htmlspecialchars(app_t('owner.task_new.requirements', [], $APP_LOCALE)) ?></label>
        <textarea name="requirements" required maxlength="20000"></textarea>
        <label><?= htmlspecialchars(app_t('owner.task_new.post_api', [], $APP_LOCALE)) ?></label>
        <input name="postapi" required maxlength="2048" placeholder="<?= htmlspecialchars(app_t('owner.task_new.post_api_placeholder', [], $APP_LOCALE)) ?>">
        <div class="row">
            <div>
                <label><?= htmlspecialchars(app_t('owner.task_new.budget', [], $APP_LOCALE)) ?></label>
                <input name="budget" type="number" step="0.0001" min="1000" required>
            </div>
            <div>
                <label><?= htmlspecialchars(app_t('owner.task_new.price', [], $APP_LOCALE)) ?></label>
                <input name="price" type="number" step="0.0001" min="0.0001" required>
            </div>
        </div>
        <label class="checkline">
            <input name="open_now" type="checkbox">
            <?= htmlspecialchars(app_t('owner.task_new.open_now', [], $APP_LOCALE)) ?>
        </label>
        <div class="actions form-actions">
            <button class="btn primary" type="submit"><?= htmlspecialchars(app_t('owner.task_new.create', [], $APP_LOCALE)) ?></button>
            <a class="btn" href="<?= htmlspecialchars(app_i18n_locale_url($APP_LOCALE, '/owner/tasks')) ?>"><?= htmlspecialchars(app_t('owner.task_new.cancel', [], $APP_LOCALE)) ?></a>
        </div>
        <div id="taskCreateNotice" class="notice"><?= htmlspecialchars(app_t('owner.task_new.notice', [], $APP_LOCALE)) ?></div>
    </form>
</section>
