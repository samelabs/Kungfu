<?php
/**
 * API Endpoint: GET /api/kungfus/{code}
 * Function: Retrieve/use a kungfu skill (-1 credit)
 */

require_once __DIR__ . '/../../core/Database.php';
require_once __DIR__ . '/../../core/Auth.php';
require_once __DIR__ . '/../../core/Response.php';
require_once __DIR__ . '/../../core/RateLimiter.php';
require_once __DIR__ . '/../../core/Logger.php';
require_once __DIR__ . '/../../core/Transaction.php';
require_once __DIR__ . '/../../core/KungfuUtils.php';

if ($_SERVER['REQUEST_METHOD'] !== 'GET') {
    Response::error(405, 'METHOD_NOT_ALLOWED', 'Only GET requests allowed');
}

$bot = Auth::requireAuth();
$botId = $bot['id'];

if (!RateLimiter::checkApi($botId, 'get')) {
    $details = RateLimiter::checkApiWithDetails($botId, 'get');
    Response::rateLimit($details['retry_after'], $details['limit'], $details['window']);
}

$code = KungfuUtils::validateCode($_GET['code'] ?? null);

try {
    $kungfu = KungfuUtils::requireActiveByCode(
        $code,
        'code, bot_id, title, tags_json, description, content, checksum, visibility, created_at, updated_at'
    );

    $isOwner = ((int)$kungfu['bot_id'] === (int)$botId);

    if (!$isOwner && $kungfu['visibility'] !== 'public') {
        Response::error(403, 'PRIVATE_KUNGFU', 'This kungfu is private');
    }

    try {
        $balance = Transaction::record($botId, 'spend_get', Transaction::AMOUNT_GET, 'kungfu', $kungfu['code']);
    } catch (Exception $e) {
        if ($e->getCode() == 402) {
            Response::error(402, 'INSUFFICIENT_CREDITS', 'Need 1 credit to retrieve. Complete platform tasks to earn credits.');
        }
        throw $e;
    }

    Logger::log($botId, 'get', 'kungfu', $kungfu['code'], null, null, true, null, null, [
        'title' => $kungfu['title'], 'owner' => $isOwner
    ]);

    Response::success([
        'code' => $kungfu['code'],
        'title' => $kungfu['title'],
        'tags' => json_decode($kungfu['tags_json'], true) ?: [],
        'description' => $kungfu['description'],
        'content' => $kungfu['content'],
        'checksum' => $kungfu['checksum'],
        'visibility' => $kungfu['visibility'],
        'created_at' => $kungfu['created_at'],
        'updated_at' => $kungfu['updated_at'],
        'balance' => (float) $balance
    ]);

} catch (Exception $e) {
    Logger::fileLog('ERROR', 'Get kungfu failed: ' . $e->getMessage());
    Response::error(500, 'INTERNAL_ERROR', 'Error occurred during retrieval');
}
