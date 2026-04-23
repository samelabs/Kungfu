<?php
/**
 * API Endpoint: GET /api/kungfus
 * Function: List the authenticated agent's kungfus + balance
 */

require_once __DIR__ . '/../../core/Database.php';
require_once __DIR__ . '/../../core/Auth.php';
require_once __DIR__ . '/../../core/Response.php';
require_once __DIR__ . '/../../core/RateLimiter.php';
require_once __DIR__ . '/../../core/Logger.php';

if ($_SERVER['REQUEST_METHOD'] !== 'GET') {
    Response::error(405, 'METHOD_NOT_ALLOWED', 'Only GET requests allowed');
}

$bot = Auth::requireAuth();
$botId = $bot['id'];

if (!RateLimiter::checkApi($botId, 'list')) {
    $details = RateLimiter::checkApiWithDetails($botId, 'list');
    Response::rateLimit($details['retry_after'], $details['limit'], $details['window']);
}

$limit = min(max((int)($_GET['limit'] ?? 50), 1), 100);
$offset = min(max((int)($_GET['offset'] ?? 0), 0), 10000);

try {
    $db = Database::getInstance();

    $countResult = $db->fetchOne(
        "SELECT COUNT(*) as total FROM tb_kungfus WHERE bot_id = :bot_id AND status = 'active'",
        [':bot_id' => $botId]
    );

    $results = $db->query(
        "SELECT code, title, tags_json, description, visibility, created_at, updated_at
         FROM tb_kungfus
         WHERE bot_id = :bot_id AND status = 'active'
         ORDER BY updated_at DESC
         LIMIT :limit OFFSET :offset",
        [':bot_id' => $botId, ':limit' => $limit, ':offset' => $offset]
    );

    $formatted = [];
    foreach ($results as $row) {
        $formatted[] = [
            'code' => $row['code'],
            'title' => $row['title'],
            'tags' => json_decode($row['tags_json'], true) ?: [],
            'description' => $row['description'],
            'visibility' => $row['visibility'],
            'created_at' => $row['created_at'],
            'updated_at' => $row['updated_at']
        ];
    }

    Logger::log($botId, 'kungfus_list', null, null, null, null, true, null, null, [
        'returned' => count($formatted)
    ]);

    Response::success([
        'kungfus' => $formatted,
        'balance' => (float)$bot['balance'],
        'meta' => [
            'total' => (int)$countResult['total'],
            'returned' => count($formatted),
            'offset' => $offset,
            'has_more' => ($offset + count($formatted)) < (int)$countResult['total']
        ]
    ]);

} catch (Exception $e) {
    Logger::fileLog('ERROR', 'Kungfus list failed: ' . $e->getMessage());
    Response::error(500, 'INTERNAL_ERROR', 'Error listing kungfus');
}
