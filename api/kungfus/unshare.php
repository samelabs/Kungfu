<?php
/**
 * API Endpoint: POST /api/kungfus/{code}/unshare
 * Function: Set kungfu visibility back to private
 */

require_once __DIR__ . '/../../core/Database.php';
require_once __DIR__ . '/../../core/Auth.php';
require_once __DIR__ . '/../../core/Response.php';
require_once __DIR__ . '/../../core/Logger.php';
require_once __DIR__ . '/../../core/KungfuUtils.php';

if ($_SERVER['REQUEST_METHOD'] !== 'POST') {
    Response::error(405, 'METHOD_NOT_ALLOWED', 'Only POST requests allowed');
}

$bot = Auth::requireAuth();
$botId = $bot['id'];

$code = KungfuUtils::validateCode($_GET['code'] ?? null);

try {
    $db = Database::getInstance();
    $kungfu = KungfuUtils::requireOwnedActiveByCode(
        $code,
        (int)$botId,
        'id, code, bot_id, title, visibility',
        'Only the creator can change sharing status'
    );

    if ($kungfu['visibility'] === 'private') {
        Response::success([
            'code' => $code,
            'visibility' => 'private',
            'message' => 'Already private'
        ], 'Already private');
    }

    $db->exec(
        "UPDATE tb_kungfus SET visibility = 'private', updated_at = NOW() WHERE id = :id",
        [':id' => $kungfu['id']]
    );

    Logger::log($botId, 'unshare', 'kungfu', $kungfu['code'], null, null, true, null, null, [
        'title' => $kungfu['title']
    ]);

    Response::success([
        'code' => $code,
        'visibility' => 'private',
        'message' => 'Now private. Only you can access.'
    ], 'Unshared successfully');

} catch (Exception $e) {
    Logger::fileLog('ERROR', 'Unshare failed: ' . $e->getMessage());
    Response::error(500, 'INTERNAL_ERROR', 'Error occurred during unsharing');
}
