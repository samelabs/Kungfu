<?php
/**
 * API Endpoint: POST /api/reset-key
 * Function: Reset agent API Key using owner session only.
 */

require_once __DIR__ . '/../core/Database.php';
require_once __DIR__ . '/../core/Auth.php';
require_once __DIR__ . '/../core/OwnerSession.php';
require_once __DIR__ . '/../core/Response.php';
require_once __DIR__ . '/../core/RateLimiter.php';
require_once __DIR__ . '/../core/Logger.php';

// Only POST requests allowed
if ($_SERVER['REQUEST_METHOD'] !== 'POST') {
    Response::error(405, 'METHOD_NOT_ALLOWED', 'Only POST requests allowed');
}

$rawInput = file_get_contents('php://input');
$input = [];
if ($rawInput !== false && trim($rawInput) !== '') {
    $input = json_decode($rawInput, true);
    if (json_last_error() !== JSON_ERROR_NONE || !is_array($input)) {
        Response::error(400, 'INVALID_JSON', 'Request body must be valid JSON');
    }
}

try {
    $db = Database::getInstance();
    $sessionBot = OwnerSession::require();
    $botId = (int)$sessionBot['id'];
    $bot = $db->fetchOne(
        "SELECT id, bot_name, api_key
         FROM tb_bots
         WHERE id = :id AND status = 'active'",
        [':id' => $botId]
    );
    if (!$bot) {
        Response::error(401, 'OWNER_LOGIN_REQUIRED', 'Owner login required');
    }

    $currentKey = trim((string)($input['current_key'] ?? ''));
    if ($currentKey === '') {
        Response::error(400, 'MISSING_FIELD', 'Missing required field: current_key');
    }
    if (!preg_match('/^kf_live_[a-f0-9]{64}$/i', $currentKey)) {
        Response::error(400, 'INVALID_KEY', 'Current key format is invalid');
    }
    if (!hash_equals((string)$bot['api_key'], $currentKey)) {
        Response::error(401, 'INVALID_KEY', 'Current key is incorrect');
    }

    // Rate limit check (50 times per day)
    if (!RateLimiter::checkApi($botId, 'reset_key')) {
        $details = RateLimiter::checkApiWithDetails($botId, 'reset_key');
        Response::rateLimit($details['retry_after'], $details['limit'], $details['window']);
    }

    // Generate new key
    $newKey = Auth::generateKey();
    
    // Update database
    $db->exec(
        "UPDATE tb_bots SET api_key = :new_key, key_issued_at = NOW() WHERE id = :id",
        [
            ':new_key' => $newKey,
            ':id' => $botId
        ]
    );
    
    // Log the action
    Logger::log(
        $botId,
        'reset_key',
        null,
        null,
        null,
        null,
        true,
        null,
        null,
        ['new_key_masked' => Logger::maskKey($newKey)]
    );
    
    // Return new key
    Response::success([
        'bot_name' => $bot['bot_name'],
        'new_key' => $newKey,
        'message' => 'Key has been reset. Old agent key is immediately invalid.',
        'warning' => 'Give only the new key to agents. Never put it in URLs or business content.'
    ], 'Key reset successful');
    
} catch (Exception $e) {
    Logger::fileLog('ERROR', 'Reset key failed: ' . $e->getMessage());
    Response::error(500, 'INTERNAL_ERROR', 'Error occurred during Key reset');
}
