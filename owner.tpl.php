<?php
$section = $OWNER_SECTION ?? 'overview';
$ownerBrandTitle = 'Owner Workspace';
$sectionViews = [
    'account' => __DIR__ . '/views/owner/account.php',
    'key' => __DIR__ . '/views/owner/key.php',
    'tasks' => __DIR__ . '/views/owner/tasks.php',
    'logs' => __DIR__ . '/views/owner/logs.php',
    'task_new' => __DIR__ . '/views/owner/task_new.php',
    'overview' => __DIR__ . '/views/owner/overview.php',
];
$ownerJsCatalog = app_i18n_scope('owner', $APP_LOCALE);
$ownerLanguageOptions = app_i18n_language_options($APP_LOCALE);
$siteCssHref = '/assets/site.css?v=' . rawurlencode((string) filemtime(__DIR__ . '/public/assets/site.css'));
$ownerCssHref = '/assets/owner.css?v=' . rawurlencode((string) filemtime(__DIR__ . '/public/assets/owner.css'));
?>
<!DOCTYPE html>
<html lang="<?= htmlspecialchars($APP_LOCALE) ?>">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title><?= htmlspecialchars($ownerBrandTitle) ?> - Kungfu.md</title>
    <meta name="robots" content="noindex,nofollow">
    <meta name="application-name" content="Kungfu.md">
    <meta name="theme-color" content="#2f7c73">
    <meta name="msapplication-TileColor" content="#2f7c73">
    <meta name="mobile-web-app-capable" content="yes">
    <meta name="apple-mobile-web-app-capable" content="yes">
    <meta name="apple-mobile-web-app-status-bar-style" content="default">
    <meta name="apple-mobile-web-app-title" content="Kungfu.md">
    <link rel="manifest" href="/manifest.webmanifest">
    <link rel="icon" type="image/png" sizes="32x32" href="/assets/icons/favicon-32.png">
    <link rel="icon" type="image/png" sizes="16x16" href="/assets/icons/favicon-16.png">
    <link rel="icon" type="image/svg+xml" href="/assets/icons/app-icon.svg">
    <link rel="apple-touch-icon" sizes="180x180" href="/assets/icons/apple-touch-icon.png">
    <link rel="alternate" type="text/plain" href="https://kungfu.md/llms.txt" title="Agent Guide">
    <link rel="alternate" type="application/json" href="https://kungfu.md/openai.json" title="openai.json">
    <link rel="stylesheet" href="<?= htmlspecialchars($siteCssHref) ?>">
    <link rel="stylesheet" href="<?= htmlspecialchars($ownerCssHref) ?>">
</head>
<body class="booting guest" data-section="<?= htmlspecialchars($section) ?>" data-locale="<?= htmlspecialchars($APP_LOCALE) ?>">
<div class="shell">
    <?php
    $ownerHeaderLocale = $APP_LOCALE;
    $ownerHeaderTitle = $ownerBrandTitle;
    $ownerHeaderActionHref = '';
    $ownerHeaderActionLabel = '';
    require __DIR__ . '/views/shared/owner_header.php';
    ?>

    <?php
    if ($section === 'login') {
        require __DIR__ . '/views/owner/auth_login.php';
    } elseif ($section === 'register') {
        require __DIR__ . '/views/owner/auth_register.php';
    } else {
        require __DIR__ . '/views/owner/auth_required.php';
    }
    ?>

    <div class="app-only">
        <?php require __DIR__ . '/views/owner/_nav.php'; ?>

        <?php require $sectionViews[$section] ?? $sectionViews['overview']; ?>
    </div>

    <?php
    $siteFooterLocale = $APP_LOCALE;
    $siteFooterLanguageOptions = $ownerLanguageOptions;
    $siteFooterLangSwitchId = 'owner-lang-switch';
    require __DIR__ . '/views/shared/site_footer.php';
    ?>
</div>

<script>
window.APP_LOCALE = <?= json_encode($APP_LOCALE, JSON_UNESCAPED_UNICODE | JSON_UNESCAPED_SLASHES) ?>;
window.OWNER_I18N = <?= json_encode($ownerJsCatalog, JSON_UNESCAPED_UNICODE | JSON_UNESCAPED_SLASHES) ?>;
</script>
<script src="/assets/owner/core.js"></script>
<script src="/assets/owner/api.js"></script>
<script src="/assets/owner/render-overview.js"></script>
<script src="/assets/owner/render-tasks.js"></script>
<script src="/assets/owner/render-logs.js"></script>
<script src="/assets/owner/auth.js"></script>
<script src="/assets/owner/tasks.js"></script>
<script src="/assets/owner/logs.js"></script>
<script src="/assets/owner/init.js"></script>
<script src="/assets/pwa-register.js"></script>
</body>
</html>
