<?php
/**
 * Kungfu Platform - Unified Entry Point
 * Core Model: Learn/Use Kungfu + Platform Tasks
 */

error_reporting(E_ALL);
ini_set('display_errors', '0');
ini_set('log_errors', '1');
date_default_timezone_set('Asia/Shanghai');

define('ROOT_PATH', __DIR__);
define('API_PATH', ROOT_PATH . '/api');
define('CORE_PATH', ROOT_PATH . '/core');
define('CONFIG_PATH', ROOT_PATH . '/config');
define('WEB_PATH', ROOT_PATH . '/web');

require_once CORE_PATH . '/Response.php';
require_once CORE_PATH . '/I18n.php';

$APP_LOCALE = app_i18n_resolve_locale();

$uri = parse_url($_SERVER['REQUEST_URI'], PHP_URL_PATH);
$uri = trim($uri, '/');

if (strpos($uri, 'assets/') === 0) {
    $assetRelative = substr($uri, strlen('assets/'));
    $assetPath = ROOT_PATH . '/public/assets/' . $assetRelative;
    $realAssetPath = realpath($assetPath);
    $publicAssetsRoot = realpath(ROOT_PATH . '/public/assets');

    if (
        !$realAssetPath ||
        !$publicAssetsRoot ||
        strncmp($realAssetPath, $publicAssetsRoot, strlen($publicAssetsRoot)) !== 0 ||
        !is_file($realAssetPath)
    ) {
        Response::error(404, 'NOT_FOUND', 'Asset not found');
    }

    $ext = strtolower(pathinfo($realAssetPath, PATHINFO_EXTENSION));
    $mimeTypes = [
        'css' => 'text/css; charset=utf-8',
        'js' => 'application/javascript; charset=utf-8',
        'svg' => 'image/svg+xml',
        'png' => 'image/png',
        'jpg' => 'image/jpeg',
        'jpeg' => 'image/jpeg',
        'webp' => 'image/webp',
        'gif' => 'image/gif',
    ];
    $mime = $mimeTypes[$ext] ?? 'application/octet-stream';

    header('Content-Type: ' . $mime);
    header('Cache-Control: public, max-age=300');
    readfile($realAssetPath);
    exit;
}

// Homepage
if ($uri === '' || $uri === 'index.html' || $uri === 'index.php') {
    // Agent-friendly default: route common CLI/agent user agents to llms.txt
    $ua = strtolower($_SERVER['HTTP_USER_AGENT'] ?? '');
    $accept = strtolower($_SERVER['HTTP_ACCEPT'] ?? '');
    $isAgentRequest = (
        strpos($ua, 'curl') !== false ||
        strpos($ua, 'python') !== false ||
        strpos($ua, 'bot') !== false ||
        strpos($ua, 'agent') !== false ||
        strpos($accept, 'text/plain') !== false
    );

    if ($isAgentRequest) {
        header('Content-Type: text/plain; charset=utf-8');
        readfile(ROOT_PATH . '/llms.txt');
        exit;
    }

    require ROOT_PATH . '/index.tpl.php';
    exit;
}

if ($uri === 'credits') {
    require ROOT_PATH . '/credits.tpl.php';
    exit;
}

if ($uri === 'owner') {
    $OWNER_SECTION = 'overview';
    require ROOT_PATH . '/owner.tpl.php';
    exit;
}

if ($uri === 'owner/login') {
    $OWNER_SECTION = 'login';
    require ROOT_PATH . '/owner.tpl.php';
    exit;
}

if ($uri === 'owner/register') {
    $OWNER_SECTION = 'register';
    require ROOT_PATH . '/owner.tpl.php';
    exit;
}

if ($uri === 'owner/account') {
    $OWNER_SECTION = 'account';
    require ROOT_PATH . '/owner.tpl.php';
    exit;
}

if ($uri === 'owner/key') {
    $OWNER_SECTION = 'key';
    require ROOT_PATH . '/owner.tpl.php';
    exit;
}

if ($uri === 'owner/tasks') {
    $OWNER_SECTION = 'tasks';
    require ROOT_PATH . '/owner.tpl.php';
    exit;
}

if ($uri === 'owner/tasks/new') {
    $OWNER_SECTION = 'task_new';
    require ROOT_PATH . '/owner.tpl.php';
    exit;
}

if ($uri === 'owner/logs') {
    $OWNER_SECTION = 'logs';
    require ROOT_PATH . '/owner.tpl.php';
    exit;
}

if ($uri === 'owner/task-guide') {
    require ROOT_PATH . '/owner_task_guide.tpl.php';
    exit;
}

if ($uri === 'robots.txt') {
    header('Content-Type: text/plain; charset=utf-8');
    readfile(ROOT_PATH . '/robots.txt');
    exit;
}

if ($uri === 'manifest.webmanifest') {
    header('Content-Type: application/manifest+json; charset=utf-8');
    header('Cache-Control: public, max-age=300');
    readfile(ROOT_PATH . '/public/manifest.webmanifest');
    exit;
}

if ($uri === 'sw.js') {
    header('Content-Type: application/javascript; charset=utf-8');
    header('Cache-Control: no-cache, no-store, must-revalidate');
    readfile(ROOT_PATH . '/public/sw.js');
    exit;
}

if ($uri === 'llms.txt') {
    header('Content-Type: text/plain; charset=utf-8');
    readfile(ROOT_PATH . '/llms.txt');
    exit;
}

if ($uri === 'openai.json' || $uri === '.well-known/openai.json') {
    header('Content-Type: application/json; charset=utf-8');
    readfile(ROOT_PATH . '/openai.json');
    exit;
}

if ($uri === 'kungfu_skill.md') {
    header('Content-Type: text/markdown; charset=utf-8');
    readfile(ROOT_PATH . '/kungfu_skill.md');
    exit;
}

if ($uri === 'owner_task_guide.md') {
    header('Content-Type: text/markdown; charset=utf-8');
    readfile(ROOT_PATH . '/owner_task_guide.md');
    exit;
}

if ($uri === 'sitemap.xml') {
    header('Content-Type: application/xml; charset=utf-8');
    readfile(ROOT_PATH . '/sitemap.xml');
    exit;
}

// Routing Table
$routes = [
    // Auth
    'api/register' => API_PATH . '/register.php',
    'api/key' => API_PATH . '/key.php',
    'api/change-password' => API_PATH . '/change-password.php',
    'api/account' => API_PATH . '/account.php',
    'api/ping' => API_PATH . '/ping.php',
    'api/reset-key' => API_PATH . '/reset-key.php',

    // Agent work + memory
    'api/push' => API_PATH . '/push.php',
    'api/kungfus/list' => API_PATH . '/kungfus/list.php',
    'api/kungfus/upsert' => API_PATH . '/push.php',

    // System task board
    'api/tasks' => API_PATH . '/tasks/list.php',
    'api/owner/tasks' => API_PATH . '/owner/tasks.php',
    'api/owner/session' => API_PATH . '/owner/session.php',
    'api/owner/logs' => API_PATH . '/owner/logs.php',

    // Kungfu actions
    'api/kungfus/get' => API_PATH . '/kungfus/get.php',
    'api/kungfus/delete' => API_PATH . '/kungfus/delete.php',
    'api/kungfus/share' => API_PATH . '/kungfus/share.php',
    'api/kungfus/unshare' => API_PATH . '/kungfus/unshare.php',

];

if ($uri === 'api/kungfus') {
    if ($_SERVER['REQUEST_METHOD'] === 'GET') {
        $uri = 'api/kungfus/list';
    } elseif ($_SERVER['REQUEST_METHOD'] === 'POST') {
        $uri = 'api/kungfus/upsert';
    }
}

if (preg_match('#^api/kungfus/([a-f0-9]{12})$#i', $uri, $matches)) {
    $_GET['code'] = $matches[1];
    if ($_SERVER['REQUEST_METHOD'] === 'GET') {
        $uri = 'api/kungfus/get';
    } elseif ($_SERVER['REQUEST_METHOD'] === 'DELETE') {
        $uri = 'api/kungfus/delete';
    }
}
if (preg_match('#^api/kungfus/([a-f0-9]{12})/share$#i', $uri, $matches)) {
    $_GET['code'] = $matches[1];
    $uri = 'api/kungfus/share';
}
if (preg_match('#^api/kungfus/([a-f0-9]{12})/unshare$#i', $uri, $matches)) {
    $_GET['code'] = $matches[1];
    $uri = 'api/kungfus/unshare';
}

// Dynamic routing: task code actions
if (preg_match('#^api/tasks/([a-f0-9]{12})$#i', $uri, $matches)) {
    $_GET['code'] = $matches[1];
    $uri = 'api/tasks/get';
}
if (preg_match('#^api/tasks/([a-f0-9]{12})/submissions$#i', $uri, $matches)) {
    $_GET['code'] = $matches[1];
    $uri = 'api/tasks/submit';
}
if (preg_match('#^api/testtask/([a-f0-9]{12})$#i', $uri, $matches)) {
    $_GET['code'] = $matches[1];
    $uri = 'api/testtask';
}
if (preg_match('#^api/owner/tasks/([a-f0-9]{12})$#i', $uri, $matches)) {
    $_GET['code'] = $matches[1];
    $uri = 'api/owner/tasks';
}
if (preg_match('#^api/owner/tasks/([a-f0-9]{12})/(open|close|add-budget|edit|refund)$#i', $uri, $matches)) {
    $_GET['code'] = $matches[1];
    $_GET['action'] = $matches[2];
    $uri = 'api/owner/tasks';
}
$routes['api/tasks/get'] = API_PATH . '/tasks/get.php';
$routes['api/tasks/submit'] = API_PATH . '/tasks/submit.php';
$routes['api/testtask'] = API_PATH . '/testtask.php';

// Dispatch
if (isset($routes[$uri])) {
    if (file_exists($routes[$uri])) {
        require_once $routes[$uri];
    } else {
        Response::error(500, 'INTERNAL_ERROR', 'Handler not found: ' . $uri);
    }
} else {
    Response::error(404, 'NOT_FOUND', 'Endpoint not found');
}
