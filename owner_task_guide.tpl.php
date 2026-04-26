<?php
$guideLocale = $APP_LOCALE ?? 'en';
$ownerBrandTitle = 'Owner Workspace';
$guideLanguageOptions = app_i18n_language_options($guideLocale);
$siteCssHref = '/assets/site.css?v=' . rawurlencode((string) filemtime(__DIR__ . '/public/assets/site.css'));
?>
<!DOCTYPE html>
<html lang="<?= htmlspecialchars($guideLocale) ?>">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title><?= htmlspecialchars(app_t('owner_guide.title', [], $guideLocale)) ?></title>
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
    <style>
        :root {
            --bg: #f6f0e7;
            --text: #202321;
            --muted: #6c716c;
            --line: rgba(32, 35, 33, .10);
            --accent: #2f7c73;
            --brand-ink: #3a342e;
            --heading-ink: #365750;
            --heading-soft: #46665f;
            --body-ink: #5c6762;
            --body-soft: #6d7772;
            --accent-ink: #2f5650;
            --surface: rgba(255, 252, 247, .82);
            --surface-strong: rgba(255, 252, 247, .95);
            --radius-lg: 22px;
            --shadow-sm: 0 14px 34px rgba(46, 79, 73, .07);
        }
        html { scrollbar-gutter: stable; }
        * { box-sizing: border-box; margin: 0; padding: 0; }
        body {
            min-height: 100vh;
            font-family: "Space Grotesk", "Avenir Next", "Helvetica Neue", "PingFang SC", "Noto Sans SC", sans-serif;
            color: var(--text);
            background:
                radial-gradient(circle at 12% -8%, rgba(47, 124, 115, .14), transparent 30rem),
                radial-gradient(circle at 88% 2%, rgba(111, 143, 166, .09), transparent 28rem),
                linear-gradient(145deg, #fffaf3 0%, var(--bg) 60%, #efe5d8 100%);
            line-height: 1.55;
        }
        body::before {
            content: "";
            position: fixed;
            inset: 0;
            pointer-events: none;
            opacity: .50;
            background-image:
                linear-gradient(rgba(47, 124, 115, .045) 1px, transparent 1px),
                linear-gradient(90deg, rgba(47, 124, 115, .035) 1px, transparent 1px);
            background-size: 44px 44px;
            mask-image: linear-gradient(to bottom, #000, transparent 80%);
        }
        .shell { position: relative; max-width: 1120px; margin: 0 auto; padding: 28px 24px 36px; }
        .panel {
            background:
                radial-gradient(circle at 100% 0, rgba(47, 124, 115, .055), transparent 13rem),
                linear-gradient(180deg, var(--surface-strong) 0%, var(--surface) 100%);
            border: 1px solid var(--line);
            border-radius: var(--radius-lg);
            padding: 22px;
            margin-bottom: 14px;
            box-shadow: var(--shadow-sm);
            backdrop-filter: blur(12px);
        }
        .panel h1 {
            font-size: clamp(28px, 4vw, 36px);
            line-height: 1.02;
            letter-spacing: -.032em;
            font-weight: 560;
            color: var(--heading-ink);
            margin-bottom: 10px;
        }
        .panel h2 {
            font-size: 23px;
            line-height: 1.14;
            letter-spacing: -.02em;
            font-weight: 560;
            color: var(--heading-ink);
            margin-bottom: 10px;
        }
        .panel h3 {
            font-size: 15px;
            line-height: 1.3;
            letter-spacing: .01em;
            font-weight: 650;
            text-transform: uppercase;
            color: var(--heading-soft);
            margin: 12px 0 8px;
        }
        p {
            color: var(--body-soft);
            margin-bottom: 10px;
            font-size: 14.5px;
            line-height: 1.6;
        }
        ul, ol { margin-left: 20px; color: var(--body-ink); }
        li { margin: 6px 0; line-height: 1.58; }
        code {
            background: rgba(47, 124, 115, .08);
            border: 1px solid var(--line);
            border-radius: 6px;
            padding: 2px 6px;
            font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
            font-size: 13px;
        }
        .btn {
            display: inline-flex;
            align-items: center;
            justify-content: center;
            min-height: 38px;
            padding: 8px 12px;
            border: 1px solid var(--line);
            border-radius: 999px;
            background: rgba(255, 252, 247, .76);
            color: var(--text);
            text-decoration: none;
            font-size: 14px;
            font-weight: 500;
            box-shadow: 0 10px 24px rgba(79, 62, 38, .08);
            backdrop-filter: blur(10px);
        }
        .btn.primary { background: var(--accent); border-color: var(--accent); color: #fff; box-shadow: 0 14px 34px rgba(47, 124, 115, .15); }
        .kicker {
            display: inline-block;
            margin-bottom: 8px;
            color: var(--accent);
            font-size: 11px;
            font-weight: 600;
            letter-spacing: .08em;
            text-transform: uppercase;
        }
        .doc-layout {
            display: grid;
            grid-template-columns: minmax(0, 1.45fr) minmax(280px, .9fr);
            gap: 16px;
            align-items: start;
        }
        .stack {
            display: grid;
            gap: 14px;
        }
        .hero {
            display: flex;
            flex-wrap: wrap;
            justify-content: space-between;
            gap: 12px;
            align-items: flex-start;
        }
        .hero p {
            max-width: 620px;
            margin-bottom: 0;
        }
        .facts {
            display: grid;
            grid-template-columns: repeat(3, minmax(0, 1fr));
            gap: 10px;
            margin-top: 14px;
        }
        .fact {
            border: 1px solid var(--line);
            border-radius: 16px;
            padding: 12px;
            background: rgba(255, 252, 247, .72);
            box-shadow: inset 0 1px 0 rgba(255, 255, 255, .64);
        }
        .fact b {
            display: block;
            margin-bottom: 4px;
            font-size: 12px;
            color: var(--accent);
            text-transform: uppercase;
            letter-spacing: .06em;
        }
        .fact span {
            color: var(--body-ink);
            font-size: 14px;
            line-height: 1.35;
        }
        .compact-list {
            margin-left: 18px;
        }
        .compact-list li {
            margin: 4px 0;
        }
        @media (max-width: 800px) {
            .doc-layout,
            .facts { grid-template-columns: 1fr; }
        }
    </style>
</head>
<body>
<div class="shell">
    <?php
    $ownerHeaderLocale = $guideLocale;
    $ownerHeaderTitle = $ownerBrandTitle;
    $ownerHeaderActionHref = app_i18n_locale_url($guideLocale, '/owner/tasks/new');
    $ownerHeaderActionLabel = app_t('owner_guide.create_task', [], $guideLocale);
    require __DIR__ . '/views/shared/owner_header.php';
    ?>

    <section class="panel">
        <span class="kicker"><?= htmlspecialchars(app_t('owner_guide.kicker', [], $guideLocale)) ?></span>
        <div class="hero">
            <div>
                <h1><?= htmlspecialchars(app_t('owner_guide.heading', [], $guideLocale)) ?></h1>
                <h2><?= htmlspecialchars(app_t('owner_guide.goal_title', [], $guideLocale)) ?></h2>
                <p><?= app_t('owner_guide.goal_intro', [], $guideLocale) ?></p>
            </div>
        </div>
        <ul class="compact-list">
            <li><?= app_t('owner_guide.goal_1', [], $guideLocale) ?></li>
            <li><?= app_t('owner_guide.goal_2', [], $guideLocale) ?></li>
            <li><?= app_t('owner_guide.goal_3', [], $guideLocale) ?></li>
        </ul>
        <p><code><?= htmlspecialchars(app_t('owner_guide.goal_flow', [], $guideLocale)) ?></code></p>
    </section>

    <section class="panel">
        <h2><?= htmlspecialchars(app_t('owner_guide.delivery_title', [], $guideLocale)) ?></h2>
        <p><?= app_t('owner_guide.delivery_intro', [], $guideLocale) ?></p>
        <ul class="compact-list">
            <li><?= app_t('owner_guide.delivery_1', [], $guideLocale) ?></li>
            <li><?= app_t('owner_guide.delivery_2', [], $guideLocale) ?></li>
            <li><?= app_t('owner_guide.delivery_3', [], $guideLocale) ?></li>
            <li><?= app_t('owner_guide.delivery_4', [], $guideLocale) ?></li>
        </ul>
        <div class="facts">
            <div class="fact">
                <b><?= htmlspecialchars(app_t('owner_guide.delivery_fact_json', [], $guideLocale)) ?></b>
                <span><?= app_t('owner_guide.delivery_fact_json_value', [], $guideLocale) ?></span>
            </div>
            <div class="fact">
                <b><?= htmlspecialchars(app_t('owner_guide.delivery_fact_success', [], $guideLocale)) ?></b>
                <span><?= app_t('owner_guide.delivery_fact_success_value', [], $guideLocale) ?></span>
            </div>
            <div class="fact">
                <b><?= htmlspecialchars(app_t('owner_guide.delivery_fact_failure', [], $guideLocale)) ?></b>
                <span><?= app_t('owner_guide.delivery_fact_failure_value', [], $guideLocale) ?></span>
            </div>
        </div>
        <p><?= app_t('owner_guide.delivery_note', [], $guideLocale) ?></p>
    </section>

    <div class="doc-layout">
        <div class="stack">
            <section class="panel">
                <h2><?= htmlspecialchars(app_t('owner_guide.api_title', [], $guideLocale)) ?></h2>
                <p><?= app_t('owner_guide.api_intro', [], $guideLocale) ?></p>
                <ul class="compact-list">
                    <li><?= app_t('owner_guide.api_1', [], $guideLocale) ?></li>
                    <li><?= app_t('owner_guide.api_2', [], $guideLocale) ?></li>
                    <li><?= app_t('owner_guide.api_3', [], $guideLocale) ?></li>
                    <li><?= app_t('owner_guide.api_4', [], $guideLocale) ?></li>
                    <li><?= app_t('owner_guide.api_5', [], $guideLocale) ?></li>
                    <li><?= app_t('owner_guide.api_6', [], $guideLocale) ?></li>
                    <li><?= app_t('owner_guide.api_7', [], $guideLocale) ?></li>
                    <li><?= app_t('owner_guide.api_8', [], $guideLocale) ?></li>
                </ul>
            </section>

            <section class="panel">
                <h2><?= htmlspecialchars(app_t('owner_guide.skill_title', [], $guideLocale)) ?></h2>
                <p><?= app_t('owner_guide.skill_intro', [], $guideLocale) ?></p>
                <ul class="compact-list">
                    <li><?= app_t('owner_guide.skill_1', [], $guideLocale) ?></li>
                    <li><?= app_t('owner_guide.skill_2', [], $guideLocale) ?></li>
                    <li><?= app_t('owner_guide.skill_3', [], $guideLocale) ?></li>
                    <li><?= app_t('owner_guide.skill_4', [], $guideLocale) ?></li>
                    <li><?= app_t('owner_guide.skill_5', [], $guideLocale) ?></li>
                    <li><?= app_t('owner_guide.skill_6', [], $guideLocale) ?></li>
                </ul>
                <p><?= app_t('owner_guide.skill_note', [], $guideLocale) ?></p>
            </section>

            <section class="panel">
                <h2><?= htmlspecialchars(app_t('owner_guide.requirements_title', [], $guideLocale)) ?></h2>
                <p><?= app_t('owner_guide.requirements_intro', [], $guideLocale) ?></p>
                <ul class="compact-list">
                    <li><?= app_t('owner_guide.requirements_1', [], $guideLocale) ?></li>
                    <li><?= app_t('owner_guide.requirements_2', [], $guideLocale) ?></li>
                    <li><?= app_t('owner_guide.requirements_3', [], $guideLocale) ?></li>
                    <li><?= app_t('owner_guide.requirements_4', [], $guideLocale) ?></li>
                    <li><?= app_t('owner_guide.requirements_5', [], $guideLocale) ?></li>
                    <li><?= app_t('owner_guide.requirements_6', [], $guideLocale) ?></li>
                    <li><?= app_t('owner_guide.requirements_7', [], $guideLocale) ?></li>
                    <li><?= app_t('owner_guide.requirements_8', [], $guideLocale) ?></li>
                    <li><?= app_t('owner_guide.requirements_9', [], $guideLocale) ?></li>
                </ul>
                <h3><?= htmlspecialchars(app_t('owner_guide.avoid_title', [], $guideLocale)) ?></h3>
                <ul class="compact-list">
                    <li><?= app_t('owner_guide.avoid_1', [], $guideLocale) ?></li>
                    <li><?= app_t('owner_guide.avoid_2', [], $guideLocale) ?></li>
                    <li><?= app_t('owner_guide.avoid_3', [], $guideLocale) ?></li>
                    <li><?= app_t('owner_guide.avoid_4', [], $guideLocale) ?></li>
                    <li><?= app_t('owner_guide.avoid_5', [], $guideLocale) ?></li>
                </ul>
            </section>
        </div>

        <div class="stack">
            <section class="panel">
                <h2><?= htmlspecialchars(app_t('owner_guide.create_title', [], $guideLocale)) ?></h2>
                <p><?= app_t('owner_guide.create_intro', [], $guideLocale) ?></p>
                <ul class="compact-list">
                    <li><?= app_t('owner_guide.create_1', [], $guideLocale) ?></li>
                    <li><?= app_t('owner_guide.create_2', [], $guideLocale) ?></li>
                    <li><?= app_t('owner_guide.create_3', [], $guideLocale) ?></li>
                    <li><?= app_t('owner_guide.create_4', [], $guideLocale) ?></li>
                    <li><?= app_t('owner_guide.create_5', [], $guideLocale) ?></li>
                    <li><?= app_t('owner_guide.create_6', [], $guideLocale) ?></li>
                </ul>
                <p><?= app_t('owner_guide.create_note_1', [], $guideLocale) ?></p>
                <p><?= app_t('owner_guide.create_note_2', [], $guideLocale) ?></p>
            </section>

            <section class="panel">
                <h2><?= htmlspecialchars(app_t('owner_guide.testing_title', [], $guideLocale)) ?></h2>
                <p><?= app_t('owner_guide.testing_intro', [], $guideLocale) ?></p>
                <ul class="compact-list">
                    <li><?= app_t('owner_guide.testing_1', [], $guideLocale) ?></li>
                    <li><?= app_t('owner_guide.testing_2', [], $guideLocale) ?></li>
                    <li><?= app_t('owner_guide.testing_3', [], $guideLocale) ?></li>
                    <li><?= app_t('owner_guide.testing_4', [], $guideLocale) ?></li>
                    <li><?= app_t('owner_guide.testing_5', [], $guideLocale) ?></li>
                    <li><?= app_t('owner_guide.testing_6', [], $guideLocale) ?></li>
                </ul>
                <p><?= app_t('owner_guide.testing_note_1', [], $guideLocale) ?></p>
                <p><?= app_t('owner_guide.testing_note_2', [], $guideLocale) ?></p>
            </section>

            <section class="panel">
                <h2><?= htmlspecialchars(app_t('owner_guide.checklist_title', [], $guideLocale)) ?></h2>
                <ul class="compact-list">
                    <li><?= app_t('owner_guide.checklist_1', [], $guideLocale) ?></li>
                    <li><?= app_t('owner_guide.checklist_2', [], $guideLocale) ?></li>
                    <li><?= app_t('owner_guide.checklist_3', [], $guideLocale) ?></li>
                    <li><?= app_t('owner_guide.checklist_4', [], $guideLocale) ?></li>
                    <li><?= app_t('owner_guide.checklist_5', [], $guideLocale) ?></li>
                    <li><?= app_t('owner_guide.checklist_6', [], $guideLocale) ?></li>
                    <li><?= app_t('owner_guide.checklist_7', [], $guideLocale) ?></li>
                    <li><?= app_t('owner_guide.checklist_8', [], $guideLocale) ?></li>
                </ul>
            </section>
        </div>
    </div>

    <?php
    $siteFooterLocale = $guideLocale;
    $siteFooterLanguageOptions = $guideLanguageOptions;
    $siteFooterLangSwitchId = 'guide-lang-switch';
    require __DIR__ . '/views/shared/site_footer.php';
    ?>
</div>
<script src="/assets/pwa-register.js"></script>
</body>
</html>
