<?php
/**
 * API Endpoint: GET /api/ping
 * Function: Verify API Key is valid, get Bot information
 */

require_once __DIR__ . '/../core/Auth.php';
// Explicitly include Database to ensure dependency is met if Auth doesn't handle it (though now Auth handles it)
require_once __DIR__ . '/../core/Database.php';
require_once __DIR__ . '/../core/Response.php';
require_once __DIR__ . '/../core/Transaction.php';

// Only GET requests allowed
if ($_SERVER['REQUEST_METHOD'] !== 'GET') {
    Response::error(405, 'METHOD_NOT_ALLOWED', 'Only GET requests allowed');
}

// Verify authentication
$bot = Auth::requireAuth();

// Return Bot information
Response::success([
    'bot_id' => $bot['id'],
    'bot_name' => $bot['bot_name'],
    'balance' => (float) $bot['balance'],
    'status' => $bot['status']
], 'Key is valid');
