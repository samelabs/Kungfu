<?php
/**
 * API Endpoint: POST /api/register
 * Function: Bot registration, get API Key
 */

require_once __DIR__ . '/../core/Auth.php';
require_once __DIR__ . '/../core/Response.php';
require_once __DIR__ . '/../core/RateLimiter.php';
require_once __DIR__ . '/../core/Logger.php';
require_once __DIR__ . '/../services/RegistrationService.php';
require_once __DIR__ . '/../exceptions/AppException.php';

// Only POST requests allowed
if ($_SERVER['REQUEST_METHOD'] !== 'POST') {
    Response::error(405, 'METHOD_NOT_ALLOWED', 'Only POST requests allowed');
}

// Get request body
$input = json_decode(file_get_contents('php://input'), true);

if (json_last_error() !== JSON_ERROR_NONE) {
    Response::error(400, 'INVALID_JSON', 'Request body must be valid JSON');
}

// Validate required fields
if (empty($input['name']) || empty($input['password'])) {
    Response::error(400, 'MISSING_FIELD', 'Missing required fields: name, password');
}

$name = trim($input['name']);
$password = (string)$input['password'];

// Rate limit check (IP level, short-term only)
$ip = Logger::getClientIp();
$rateCheck = RateLimiter::checkRegisterWithRetry($ip);
if (!$rateCheck['allowed']) {
    Response::rateLimit($rateCheck['retry_after'], $rateCheck['limit'], $rateCheck['window']);
}

try {
    Response::success(RegistrationService::register($name, $password, $ip), 'Registration successful');
} catch (AppException $e) {
    Response::error($e->getHttpCode(), $e->getErrorCode(), $e->getMessage(), $e->getDetails());
}
