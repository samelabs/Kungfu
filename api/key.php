<?php
/**
 * API Endpoint: GET /api/key
 * Function: Human owner retrieves current agent key using owner session.
 */

require_once __DIR__ . '/../core/Database.php';
require_once __DIR__ . '/../core/OwnerSession.php';
require_once __DIR__ . '/../core/Response.php';
require_once __DIR__ . '/../core/Logger.php';

if ($_SERVER['REQUEST_METHOD'] === 'GET') {
    $sessionBot = OwnerSession::require();
    try {
        $db = Database::getInstance();
        $bot = $db->fetchOne(
            "SELECT id, bot_name, api_key, balance, status, key_issued_at
             FROM tb_bots
             WHERE id = :id AND status = 'active'",
            [':id' => (int)$sessionBot['id']]
        );

        if (!$bot) {
            Response::error(401, 'OWNER_LOGIN_REQUIRED', 'Owner login required');
        }

        Logger::log((int)$bot['id'], 'key_get', null, null, null, null, true, null, null, [
            'bot_name' => $bot['bot_name'],
            'source' => 'owner_session'
        ]);

        Response::success([
            'bot_name' => $bot['bot_name'],
            'key' => $bot['api_key'],
            'balance' => (float)$bot['balance'],
            'status' => $bot['status'],
            'key_issued_at' => $bot['key_issued_at']
        ], 'Key retrieved');
    } catch (Exception $e) {
        Logger::fileLog('ERROR', 'Key get failed: ' . $e->getMessage());
        Response::error(500, 'INTERNAL_ERROR', 'Error occurred while retrieving key');
    }
}

Response::error(405, 'METHOD_NOT_ALLOWED', 'Only GET requests allowed');
