<?php
$siteFooterLocale = $siteFooterLocale ?? ($APP_LOCALE ?? 'en');
$siteFooterLanguageOptions = $siteFooterLanguageOptions ?? app_i18n_language_options($siteFooterLocale);
$siteFooterLangSwitchId = $siteFooterLangSwitchId ?? 'site-lang-switch';
?>
<footer class="site-footer">
    <div class="site-footer-meta">
        <div class="site-footer-title"><span class="site-logo site-footer-logo" aria-hidden="true">🥋</span><span>Kungfu.md</span></div>
        <div class="site-footer-copy">Copyright © 2026 Kungfu.md. All rights reserved.</div>
        <div class="site-footer-contact">Contact: <a href="mailto:ad@live.it">ad@live.it</a></div>
    </div>
    <div class="site-footer-lang">
        <label for="<?= htmlspecialchars($siteFooterLangSwitchId) ?>">Lang</label>
        <select id="<?= htmlspecialchars($siteFooterLangSwitchId) ?>" onchange="window.location.href=this.value">
            <?php foreach ($siteFooterLanguageOptions as $option): ?>
                <option
                    value="<?= htmlspecialchars(app_i18n_locale_url($option['code'])) ?>"
                    <?= $siteFooterLocale === $option['code'] ? 'selected' : '' ?>
                ><?= htmlspecialchars($option['label']) ?></option>
            <?php endforeach; ?>
        </select>
    </div>
</footer>
