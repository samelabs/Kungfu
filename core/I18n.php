<?php

function app_i18n_supported_locales(): array
{
    return ['en', 'zh', 'ja', 'ko', 'es'];
}

function app_i18n_normalize_locale(?string $locale): string
{
    $value = strtolower(trim((string)$locale));

    if ($value === '') {
        return '';
    }

    if (strpos($value, 'zh') === 0) {
        return 'zh';
    }

    if (strpos($value, 'en') === 0) {
        return 'en';
    }

    if (strpos($value, 'ja') === 0) {
        return 'ja';
    }

    if (strpos($value, 'ko') === 0) {
        return 'ko';
    }

    if (strpos($value, 'es') === 0) {
        return 'es';
    }

    return '';
}

function app_i18n_locale_root(): string
{
    return ROOT_PATH . '/locales';
}

function app_i18n_resolve_locale(): string
{
    $supported = app_i18n_supported_locales();

    $queryLocale = app_i18n_normalize_locale($_GET['lang'] ?? null);
    if ($queryLocale !== '' && in_array($queryLocale, $supported, true)) {
        setcookie('lang', $queryLocale, [
            'expires' => time() + 31536000,
            'path' => '/',
            'httponly' => false,
            'samesite' => 'Lax',
        ]);
        $_COOKIE['lang'] = $queryLocale;

        return $queryLocale;
    }

    $cookieLocale = app_i18n_normalize_locale($_COOKIE['lang'] ?? null);
    if ($cookieLocale !== '' && in_array($cookieLocale, $supported, true)) {
        return $cookieLocale;
    }

    $acceptLanguage = strtolower((string)($_SERVER['HTTP_ACCEPT_LANGUAGE'] ?? ''));
    if (strpos($acceptLanguage, 'zh') !== false) {
        return 'zh';
    }
    if (strpos($acceptLanguage, 'ko') !== false) {
        return 'ko';
    }
    if (strpos($acceptLanguage, 'es') !== false) {
        return 'es';
    }
    if (strpos($acceptLanguage, 'ja') !== false) {
        return 'ja';
    }

    return 'en';
}

function app_i18n_merge_catalog(array $base, array $incoming): array
{
    foreach ($incoming as $key => $value) {
        if (isset($base[$key]) && is_array($base[$key]) && is_array($value)) {
            $base[$key] = app_i18n_merge_catalog($base[$key], $value);
            continue;
        }

        $base[$key] = $value;
    }

    return $base;
}

function app_i18n_load_locale(string $locale): array
{
    static $cache = [];

    if (isset($cache[$locale])) {
        return $cache[$locale];
    }

    $normalizedLocale = app_i18n_normalize_locale($locale);
    if ($normalizedLocale === '') {
        $normalizedLocale = 'en';
    }

    $catalog = [];
    $localeDir = app_i18n_locale_root() . '/' . $normalizedLocale;
    $files = glob($localeDir . '/*.php');

    if ($files === false) {
        $files = [];
    }

    sort($files);

    foreach ($files as $file) {
        $chunk = require $file;
        if (!is_array($chunk)) {
            continue;
        }
        $catalog = app_i18n_merge_catalog($catalog, $chunk);
    }

    $cache[$normalizedLocale] = $catalog;

    return $catalog;
}

function app_i18n_lookup(array $catalog, string $key): ?string
{
    $value = $catalog;

    foreach (explode('.', $key) as $segment) {
        if (!is_array($value) || !array_key_exists($segment, $value)) {
            return null;
        }
        $value = $value[$segment];
    }

    return is_string($value) ? $value : null;
}

function app_i18n_interpolate(string $value, array $vars = []): string
{
    if ($vars === []) {
        return $value;
    }

    $replace = [];
    foreach ($vars as $name => $replacement) {
        $replace['{{' . $name . '}}'] = (string)$replacement;
    }

    return strtr($value, $replace);
}

function app_i18n_get(string $key, ?string $locale = null, array $vars = []): string
{
    $resolvedLocale = $locale ?: app_i18n_resolve_locale();
    $primaryCatalog = app_i18n_load_locale($resolvedLocale);
    $value = app_i18n_lookup($primaryCatalog, $key);

    if ($value === null && $resolvedLocale !== 'en') {
        $value = app_i18n_lookup(app_i18n_load_locale('en'), $key);
    }

    if ($value === null) {
        return $key;
    }

    return app_i18n_interpolate($value, $vars);
}

function app_t(string $key, array $vars = [], ?string $locale = null): string
{
    return app_i18n_get($key, $locale, $vars);
}

function app_i18n_scope(string $scope, ?string $locale = null): array
{
    $resolvedLocale = $locale ?: app_i18n_resolve_locale();
    $catalog = app_i18n_load_locale($resolvedLocale);
    $value = $catalog[$scope] ?? null;

    if ($value === null && $resolvedLocale !== 'en') {
        $fallbackCatalog = app_i18n_load_locale('en');
        $value = $fallbackCatalog[$scope] ?? null;
    }

    return is_array($value) ? $value : [];
}

function app_i18n_locale_url(string $locale, ?string $path = null): string
{
    $normalizedLocale = app_i18n_normalize_locale($locale);
    if ($normalizedLocale === '') {
        $normalizedLocale = 'en';
    }

    $rawUri = $path ?? (string)($_SERVER['REQUEST_URI'] ?? '/');
    $parts = parse_url($rawUri);
    $basePath = $parts['path'] ?? '/';
    $query = [];

    if (isset($parts['query'])) {
        parse_str($parts['query'], $query);
    }

    $query['lang'] = $normalizedLocale;
    $queryString = http_build_query($query);

    return $queryString === '' ? $basePath : $basePath . '?' . $queryString;
}

function app_i18n_language_options(?string $displayLocale = null): array
{
    $locale = $displayLocale ?: app_i18n_resolve_locale();
    $options = [];

    foreach (app_i18n_supported_locales() as $code) {
        $options[] = [
            'code' => $code,
            'label' => app_t('common.lang_' . $code, [], $locale),
        ];
    }

    return $options;
}
