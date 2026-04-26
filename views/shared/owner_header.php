<?php
$ownerHeaderLocale = $ownerHeaderLocale ?? ($APP_LOCALE ?? 'en');
$ownerHeaderTitle = $ownerHeaderTitle ?? 'Owner Workspace';
$ownerHeaderActionHref = $ownerHeaderActionHref ?? '';
$ownerHeaderActionLabel = $ownerHeaderActionLabel ?? '';
?>
<header class="owner-header">
    <div class="owner-header-brand">
        <div class="site-logo owner-header-logo" aria-hidden="true">🥋</div>
        <h1><?= htmlspecialchars($ownerHeaderTitle) ?></h1>
        <a
            class="owner-home-link"
            href="<?= htmlspecialchars(app_i18n_locale_url($ownerHeaderLocale, '/')) ?>"
            aria-label="<?= htmlspecialchars(app_t('common.home', [], $ownerHeaderLocale)) ?>"
            title="<?= htmlspecialchars(app_t('common.home', [], $ownerHeaderLocale)) ?>"
        >
            <svg viewBox="0 0 24 24" aria-hidden="true" focusable="false">
                <path d="M4 10.5 12 4l8 6.5V20a1 1 0 0 1-1 1h-4.5a.5.5 0 0 1-.5-.5v-4a2 2 0 1 0-4 0v4a.5.5 0 0 1-.5.5H5a1 1 0 0 1-1-1v-9.5Z"/>
            </svg>
        </a>
    </div>
    <?php if ($ownerHeaderActionHref !== '' && $ownerHeaderActionLabel !== ''): ?>
        <div class="owner-header-actions">
            <a class="btn primary" href="<?= htmlspecialchars($ownerHeaderActionHref) ?>"><?= htmlspecialchars($ownerHeaderActionLabel) ?></a>
        </div>
    <?php endif; ?>
</header>
