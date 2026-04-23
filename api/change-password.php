<?php
/**
 * API Endpoint: POST /api/change-password
 * Function: Human owner changes bot password. Agent key remains unchanged.
 */

require_once __DIR__ . '/../core/Database.php';
require_once __DIR__ . '/../core/Auth.php';
require_once __DIR__ . '/../core/OwnerSession.php';
require_once __DIR__ . '/../core/Response.php';
require_once __DIR__ . '/../core/Logger.php';
require_once __DIR__ . '/../core/Security.php';

if ($_SERVER['REQUEST_METHOD'] !== 'POST') {
    Response::error(405, 'METHOD_NOT_ALLOWED', 'Only POST requests allowed');
}

$input = json_decode(file_get_contents('php://input'), true);
if (json_last_error() !== JSON_ERROR_NONE || !is_array($input)) {
    Response::error(400, 'INVALID_JSON', 'Request body must be valid JSON');
}

foreach (['password', 'new_password'] as $field) {
    if (empty($input[$field])) {
        Response::error(400, 'MISSING_FIELD', "Missing required field: {$field}");
    }
}

$sessionBot = OwnerSession::require();
$name = (string)$sessionBot['bot_name'];
$password = (string)$input['password'];
$newPassword = (string)$input['new_password'];

$nameValidation = Auth::validateBotName($name);
if (!$nameValidation['valid']) {
    Response::error(400, 'INVALID_NAME', $nameValidation['errors'][0]);
}

$passwordValidation = Auth::validatePassword($password);
if (!$passwordValidation['valid']) {
    Response::error(400, 'INVALID_PASSWORD', $passwordValidation['errors'][0]);
}

Security::rejectApiKeyInContent([$name, $password, $newPassword], 'human credentials');

$validation = Auth::validatePassword($newPassword);
if (!$validation['valid']) {
    Response::error(400, 'INVALID_PASSWORD', $validation['errors'][0]);
}
if (hash_equals($password, $newPassword)) {
    Response::error(400, 'PASSWORD_UNCHANGED', 'New password must be different from current password');
}

try {
    $db = Database::getInstance();
    $bot = $db->fetchOne(
        "SELECT id, bot_name, password_hash
         FROM tb_bots
         WHERE bot_name = :name AND status = 'active'",
        [':name' => $name]
    );

    if (!$bot || empty($bot['password_hash']) || !Auth::verifyPassword($password, $bot['password_hash'])) {
        Response::error(401, 'INVALID_CREDENTIALS', 'Bot name or password is incorrect');
    }

    $db->exec(
        "UPDATE tb_bots SET password_hash = :password_hash WHERE id = :id",
        [
            ':password_hash' => Auth::hashPassword($newPassword),
            ':id' => $bot['id']
        ]
    );

    Logger::log((int)$bot['id'], 'change_password', null, null, null, null, true, null, null, [
        'bot_name' => $bot['bot_name']
    ]);

    Response::success([
        'bot_name' => $bot['bot_name'],
        'message' => 'Password changed. Current agent key remains valid until reset-key is called.'
    ], 'Password changed');

} catch (Exception $e) {
    Logger::fileLog('ERROR', 'Change password failed: ' . $e->getMessage());
    Response::error(500, 'INTERNAL_ERROR', 'Error occurred while changing password');
}
