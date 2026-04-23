<?php
/**
 * API Endpoint: /api/owner/session
 * Function: Owner center session login, status, and logout.
 */

require_once __DIR__ . '/../../core/OwnerSession.php';
require_once __DIR__ . '/../../core/Response.php';
require_once __DIR__ . '/../../core/Logger.php';

try {
    if ($_SERVER['REQUEST_METHOD'] === 'GET') {
        $bot = OwnerSession::current();
        if (!$bot) {
            Response::error(401, 'OWNER_LOGIN_REQUIRED', 'Owner login required');
        }

        Response::success([
            'bot_id' => (int)$bot['id'],
            'bot_name' => $bot['bot_name'],
            'status' => $bot['status']
        ], 'Owner session active');
    }

    if ($_SERVER['REQUEST_METHOD'] === 'POST') {
        $input = json_decode(file_get_contents('php://input'), true);
        if (json_last_error() !== JSON_ERROR_NONE || !is_array($input)) {
            Response::error(400, 'INVALID_JSON', 'Request body must be valid JSON');
        }

        if (empty($input['name']) || empty($input['password'])) {
            Response::error(400, 'MISSING_FIELD', 'Missing required fields: name, password');
        }

        $bot = OwnerSession::login((string)$input['name'], (string)$input['password']);
        Response::success([
            'bot_id' => (int)$bot['id'],
            'bot_name' => $bot['bot_name'],
            'status' => $bot['status']
        ], 'Owner login successful');
    }

    if ($_SERVER['REQUEST_METHOD'] === 'DELETE') {
        OwnerSession::logout();
        Response::success([], 'Owner logout successful');
    }

    Response::error(405, 'METHOD_NOT_ALLOWED', 'Only GET, POST, and DELETE requests allowed');
} catch (Exception $e) {
    Logger::fileLog('ERROR', 'Owner session API failed: ' . $e->getMessage(), [
        'method' => $_SERVER['REQUEST_METHOD'] ?? '',
        'path' => $_SERVER['REQUEST_URI'] ?? ''
    ]);
    Response::error(500, 'INTERNAL_ERROR', 'Owner session operation failed');
}
