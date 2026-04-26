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
require_once __DIR__ . '/../repositories/BotRepository.php';
require_once __DIR__ . '/../presenters/AccountPresenter.php';
require_once __DIR__ . '/../exceptions/AppException.php';

// Only GET requests allowed
if ($_SERVER['REQUEST_METHOD'] !== 'GET') {
    Response::error(405, 'METHOD_NOT_ALLOWED', 'Only GET requests allowed');
}

try {
    $bot = Auth::requireAuth();
    $freshBot = BotRepository::findActiveBotSummaryById((int)$bot['id']);
    if (!$freshBot) {
        throw new AppException(401, 'INVALID_KEY', 'API Key is invalid or expired, please use X-Bot-Key header');
    }
    Response::success(AccountPresenter::agentPing($freshBot), 'Key is valid');
} catch (AppException $e) {
    Response::error($e->getHttpCode(), $e->getErrorCode(), $e->getMessage(), $e->getDetails());
}
