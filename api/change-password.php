<?php
/**
 * API Endpoint: POST /api/change-password
 * Function: Human owner changes bot password. Agent key remains unchanged.
 */

require_once __DIR__ . '/../core/OwnerSession.php';
require_once __DIR__ . '/../core/Response.php';
require_once __DIR__ . '/../core/Logger.php';
require_once __DIR__ . '/../services/ChangePasswordService.php';
require_once __DIR__ . '/../exceptions/AppException.php';

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

try {
    $sessionBot = OwnerSession::require();
    $name = (string)$sessionBot['bot_name'];
    $password = (string)$input['password'];
    $newPassword = (string)$input['new_password'];
    Response::success(ChangePasswordService::change($name, $password, $newPassword), 'Password changed');
} catch (AppException $e) {
    Response::error($e->getHttpCode(), $e->getErrorCode(), $e->getMessage(), $e->getDetails());
} catch (Exception $e) {
    Logger::fileLog('ERROR', 'Change password failed: ' . $e->getMessage());
    Response::error(500, 'INTERNAL_ERROR', 'Error occurred while changing password');
}
