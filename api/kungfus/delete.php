<?php
/**
 * API Endpoint: DELETE /api/kungfus/{code}
 * Function: Soft delete kungfu
 *
 * System rule:
 * status is a system control field. "deleted" removes the kungfu from normal
 * retrieval/listing while preserving the row for audit and future system handling.
 * Do not replace this with a hard delete unless the lifecycle model changes.
 */

require_once __DIR__ . '/../../core/Database.php';
require_once __DIR__ . '/../../core/Auth.php';
require_once __DIR__ . '/../../core/Response.php';
require_once __DIR__ . '/../../core/Logger.php';
require_once __DIR__ . '/../../core/KungfuUtils.php';

if ($_SERVER['REQUEST_METHOD'] !== 'DELETE') {
    Response::error(405, 'METHOD_NOT_ALLOWED', 'Only DELETE requests allowed');
}

$bot = Auth::requireAuth();
$botId = $bot['id'];

$code = KungfuUtils::validateCode($_GET['code'] ?? null);

try {
    $db = Database::getInstance();

    $kungfu = KungfuUtils::requireOwnedActiveByCode(
        $code,
        (int)$botId,
        'id, code, bot_id, title',
        'Only the creator can delete this Kungfu'
    );

    // Soft-delete is intentional: status is the system lifecycle/ban control.
    $db->exec(
        "UPDATE tb_kungfus SET status = 'deleted' WHERE id = :id",
        [':id' => $kungfu['id']]
    );

    Logger::log(
        $botId,
        'delete',
        'kungfu',
        $kungfu['code'],
        null,
        null,
        true,
        null,
        null,
        ['title' => $kungfu['title']]
    );

    Response::success([
        'code' => $code,
        'title' => $kungfu['title'],
        'message' => 'Deleted (bots that already acquired can still use normally)'
    ], 'Deletion successful');

} catch (Exception $e) {
    Logger::fileLog('ERROR', 'Delete failed: ' . $e->getMessage());
    Response::error(500, 'INTERNAL_ERROR', 'Error occurred during deletion');
}
