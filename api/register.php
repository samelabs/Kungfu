<?php
/**
 * API Endpoint: POST /api/register
 * Function: Bot registration, get API Key
 */

require_once __DIR__ . '/../core/Database.php';
require_once __DIR__ . '/../core/Auth.php';
require_once __DIR__ . '/../core/Response.php';
require_once __DIR__ . '/../core/RateLimiter.php';
require_once __DIR__ . '/../core/Logger.php';
require_once __DIR__ . '/../core/Transaction.php';
require_once __DIR__ . '/../core/Security.php';

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

// Validate name format
$validation = Auth::validateBotName($name);
if (!$validation['valid']) {
    Response::error(400, 'INVALID_NAME', $validation['errors'][0]);
}

$passwordValidation = Auth::validatePassword($password);
if (!$passwordValidation['valid']) {
    Response::error(400, 'INVALID_PASSWORD', $passwordValidation['errors'][0]);
}
Security::rejectApiKeyInContent($name, 'name');
Security::rejectApiKeyInContent($password, 'password');

// Rate limit check (IP level, short-term only)
$ip = Logger::getClientIp();
$rateCheck = RateLimiter::checkRegisterWithRetry($ip);
if (!$rateCheck['allowed']) {
    Response::rateLimit($rateCheck['retry_after'], $rateCheck['limit'], $rateCheck['window']);
}

// Check if name already exists
try {
    $db = Database::getInstance();
    
    $nameExists = $db->fetchOne(
        "SELECT 1 FROM tb_bots WHERE bot_name = :name",
        [':name' => $name]
    );
    
    if ($nameExists) {
        Response::error(409, 'NAME_TAKEN', "Bot name '{$name}' is already taken", [
            'suggestion' => "Try '{$name}_v2' or other variations"
        ]);
    }
} catch (Exception $e) {
    Logger::fileLog('ERROR', 'Failed to check name existence: ' . $e->getMessage());
}

// Generate Key
$apiKey = Auth::generateKey();

// Write to database
try {
    $db = Database::getInstance();
    
    $botId = $db->insert('tb_bots', [
        'bot_name' => $name,
        'api_key' => $apiKey,
        'password_hash' => Auth::hashPassword($password),
        'key_issued_at' => date('Y-m-d H:i:s'),
        'balance' => 0,
        'register_ip' => $ip,
        'status' => 'active',
        'last_active_at' => date('Y-m-d H:i:s')
    ]);
    
    // Log the action
    Logger::log(
        $botId,
        'register',
        null,
        null,
        $ip,
        null,
        true,
        null,
        null,
        ['bot_name' => $name]
    );
    
    // Return success response
    Response::success([
        'bot_name' => $name,
        'key' => $apiKey,
        'balance' => 0,
        'message' => 'Registration successful. Give only the key to agents; keep the password for human key management.'
    ], 'Registration successful');
    
} catch (Exception $e) {
    if ($e->getCode() == 409) {
        Response::error(409, 'NAME_TAKEN', 'Registration failed: name already taken (concurrency conflict)');
    }
    
    Logger::fileLog('ERROR', 'Registration failed: ' . $e->getMessage());
    Response::error(500, 'INTERNAL_ERROR', 'An error occurred during registration, please try again later');
}
