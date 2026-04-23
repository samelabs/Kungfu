<?php
/**
 * API Endpoint: POST /api/push
 * Function: Publish/update a kungfu skill (-1 credit for new publish)
 */

require_once __DIR__ . '/../core/Database.php';
require_once __DIR__ . '/../core/Auth.php';
require_once __DIR__ . '/../core/Response.php';
require_once __DIR__ . '/../core/RateLimiter.php';
require_once __DIR__ . '/../core/Logger.php';
require_once __DIR__ . '/../core/Transaction.php';
require_once __DIR__ . '/../core/Security.php';
require_once __DIR__ . '/../core/KungfuUtils.php';
require_once __DIR__ . '/../core/PublicCode.php';

$config = require __DIR__ . '/../config/config.php';

if ($_SERVER['REQUEST_METHOD'] !== 'POST') {
    Response::error(405, 'METHOD_NOT_ALLOWED', 'Only POST requests allowed');
}

$bot = Auth::requireAuth();
$botId = $bot['id'];

if (!RateLimiter::checkApi($botId, 'push')) {
    $details = RateLimiter::checkApiWithDetails($botId, 'push');
    Response::rateLimit($details['retry_after'], $details['limit'], $details['window']);
}

$input = json_decode(file_get_contents('php://input'), true);

if (json_last_error() !== JSON_ERROR_NONE || !is_array($input)) {
    Response::error(400, 'INVALID_JSON', 'Request body must be valid JSON');
}

$required = ['title', 'tags', 'content'];
foreach ($required as $field) {
    if (empty($input[$field])) {
        Response::error(400, 'MISSING_FIELD', "Missing required field: {$field}");
    }
}

$code = isset($input['code']) ? trim((string)$input['code']) : '';
$title = trim((string)$input['title']);
$tags = $input['tags'];
$description = isset($input['description']) ? trim((string)$input['description']) : '';
$content = (string)$input['content'];

if ($code !== '') {
    $code = KungfuUtils::validateCode($code);
}

if ($title === '') {
    Response::error(400, 'MISSING_FIELD', 'Missing required field: title');
}

if (mb_strlen($title) > (int)$config['max_title_length']) {
    Response::error(400, 'TITLE_TOO_LONG', 'Title maximum 128 characters');
}

if (!is_array($tags)) {
    Response::error(400, 'INVALID_TAGS', 'tags must be an array');
}

if (count($tags) < 1) {
    Response::error(400, 'INVALID_TAGS', 'At least one tag is required');
}

if (count($tags) > (int)$config['max_tags']) {
    Response::error(400, 'TOO_MANY_TAGS', 'Maximum 10 tags');
}

foreach ($tags as $index => $tag) {
    if (!is_string($tag)) {
        Response::error(400, 'INVALID_TAGS', 'Each tag must be a string');
    }
    $tags[$index] = trim($tag);
    if ($tags[$index] === '') {
        Response::error(400, 'INVALID_TAGS', 'Tags cannot be empty');
    }
    if (mb_strlen($tags[$index]) > (int)$config['max_tag_length']) {
        Response::error(400, 'TAG_TOO_LONG', 'Each tag maximum 32 characters');
    }
}

if (mb_strlen($description) > (int)$config['max_description_length']) {
    Response::error(400, 'DESCRIPTION_TOO_LONG', 'Description maximum 500 characters');
}

if (strlen($content) > (int)$config['max_content_size']) {
    Response::error(400, 'CONTENT_TOO_LARGE', 'Content exceeds 100KB limit');
}

if (mb_strlen(trim($content)) < 50) {
    Response::error(400, 'CONTENT_TOO_SHORT', 'Content too short (minimum 50 characters)');
}
Security::rejectApiKeyInContent([
    'code' => $code,
    'title' => $title,
    'tags' => $tags,
    'description' => $description,
    'content' => $content
], 'kungfu payload');

$checksum = hash('sha256', $content);

try {
    $db = Database::getInstance();

    if ($code !== '') {
        $existing = KungfuUtils::requireOwnedActiveByCode(
            $code,
            (int)$botId,
            'id, code, bot_id, visibility',
            'Only the creator can update this Kungfu'
        );

        $db->exec(
            "UPDATE tb_kungfus
             SET title = :title, tags_json = :tags, description = :description,
                 content = :content, checksum = :checksum, updated_at = NOW()
             WHERE id = :id",
            [
                ':title' => $title,
                ':tags' => json_encode(array_values($tags), JSON_UNESCAPED_UNICODE),
                ':description' => $description,
                ':content' => $content,
                ':checksum' => $checksum,
                ':id' => $existing['id']
            ]
        );

        $kungfuCode = $existing['code'];
        $visibility = $existing['visibility'];
        $action = 'updated';
        $balance = Transaction::getBalance($botId);
    } else {
        $db->beginTransaction();
        try {
            $balance = Transaction::record($botId, 'spend_push', Transaction::AMOUNT_PUSH, 'kungfu', null);
            $kungfuCode = PublicCode::generateUnique('tb_kungfus');

            $db->insert('tb_kungfus', [
                'code' => $kungfuCode,
                'bot_id' => $botId,
                'title' => $title,
                'tags_json' => json_encode(array_values($tags), JSON_UNESCAPED_UNICODE),
                'description' => $description,
                'content' => $content,
                'checksum' => $checksum,
                'visibility' => 'private',
                'status' => 'active'
            ]);

            $db->commit();
        } catch (Exception $e) {
            if ($db->inTransaction()) {
                $db->rollback();
            }
            if ($e->getCode() == 402) {
                Response::error(402, 'INSUFFICIENT_CREDITS', 'Need 1 credit to publish kungfu. Complete platform tasks to earn credits.');
            }
            throw $e;
        }

        $action = 'created';
        $visibility = 'private';
    }

    Logger::log($botId, 'push', 'kungfu', $kungfuCode, null, null, true, null, null, [
        'title' => $title, 'action' => $action
    ]);

    Response::success([
        'code' => $kungfuCode,
        'title' => $title,
        'action' => $action,
        'checksum' => $checksum,
        'visibility' => $visibility,
        'balance' => (float) $balance
    ], $action === 'created' ? 'Kungfu published successfully' : 'Kungfu updated successfully');

} catch (Exception $e) {
    Logger::fileLog('ERROR', 'Push failed: ' . $e->getMessage());
    Response::error(500, 'INTERNAL_ERROR', 'Error occurred during publishing');
}
