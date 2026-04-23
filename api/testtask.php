<?php
/**
 * API Endpoint: POST /api/testtask/{code}
 * Function: Task owner tests the configured postapi without agent reward.
 *
 * System rule:
 * This is not a free dry-run. A successful owner test consumes task budget by design,
 * using the same per-delivery price as agent submissions. Do not remove the budget
 * settlement unless the task economics model changes.
 */

require_once __DIR__ . '/../core/Database.php';
require_once __DIR__ . '/../core/Auth.php';
require_once __DIR__ . '/../core/Response.php';
require_once __DIR__ . '/../core/TaskUtils.php';
require_once __DIR__ . '/../core/TaskCheckService.php';
require_once __DIR__ . '/../core/Logger.php';

const TESTTASK_MAX_RESPONSE_BYTES = 16000;
const TESTTASK_DB_ERROR_MESSAGE_MAX = 256;
const TESTTASK_DB_RESPONSE_BODY_MAX = 65535;
const TESTTASK_DB_PAYLOAD_JSON_MAX = 60000;

if ($_SERVER['REQUEST_METHOD'] !== 'POST') {
    Response::error(405, 'METHOD_NOT_ALLOWED', 'Only POST requests allowed');
}

$bot = Auth::requireAuth();
$botId = (int)$bot['id'];

$code = TaskUtils::validateCode($_GET['code'] ?? null);

$input = json_decode(file_get_contents('php://input'), true);
if (json_last_error() !== JSON_ERROR_NONE || !is_array($input)) {
    Response::error(400, 'INVALID_JSON', 'Request body must be valid JSON object');
}

try {
    $db = Database::getInstance();
    $task = $db->fetchOne(
        "SELECT * FROM tb_tasks WHERE code = :code",
        [':code' => $code]
    );

    if (!$task) {
        Response::error(404, 'NOT_FOUND', 'Task not found');
    }

    if ((int)$task['bot_id'] !== $botId) {
        Response::error(403, 'NOT_OWNER', 'Only the task owner can test this task');
    }

    $postapi = trim((string)($task['postapi'] ?? ''));
    $price = (float)($task['price'] ?? 0);
    try {
        TaskCheckService::run($postapi, $price, static function () use ($db, $code, $price): void {
            ensureTesttaskBudget($db, (string)$code, $price);
        });
    } catch (TaskCheckException $e) {
        logTesttaskEvent(
            $db,
            (string)$code,
            $botId,
            'kfcheck',
            $input,
            false,
            null,
            null,
            $e->getApiErrorCode(),
            $e->getLogMessage()
        );
        Response::error($e->getHttpCode(), $e->getApiErrorCode(), $e->getMessage(), $e->getDetails());
    }

    $postPayload = buildTesttaskPayload((string)$code, $input);
    $postResult = postTesttaskPayload($postapi, $postPayload);
    if (!$postResult['success']) {
        logTesttaskEvent(
            $db,
            (string)$code,
            $botId,
            'post_failed',
            $postPayload,
            false,
            $postResult['response_code'],
            $postResult['response_body'],
            $postResult['error_code'],
            $postResult['error_message']
        );
        Response::error(
            424,
            $postResult['error_code'] ?? 'TESTTASK_POST_FAILED',
            $postResult['error_message'] ?? 'Task test delivery failed',
            [
                'post' => [
                    'delivered' => false,
                    'response_code' => $postResult['response_code'],
                    'response_body' => truncateTesttaskResponse((string)($postResult['response_body'] ?? ''))
                ]
            ]
        );
    }

    logTesttaskEvent(
        $db,
        (string)$code,
        $botId,
        'post_succeeded',
        $postPayload,
        true,
        $postResult['response_code'],
        $postResult['response_body']
    );

    // Owner self-tests are charged to the task budget by design. This keeps
    // postapi tests economically equivalent to real delivered submissions.
    $billing = settleTesttaskBudget($db, (string)$code, $price);

    Response::success([
        'task_code' => $code,
        'post' => [
            'delivered' => true,
            'response_code' => $postResult['response_code'],
            'response_body' => truncateTesttaskResponse((string)($postResult['response_body'] ?? ''))
        ],
        'billing' => [
            'cost' => $price,
            'budget' => $billing['budget'],
            'status' => $billing['status']
        ]
    ], 'Task test delivered');
} catch (Exception $e) {
    Response::error(500, 'INTERNAL_ERROR', 'Error occurred during task test');
}

function ensureTesttaskBudget(Database $db, string $taskCode, float $price): void
{
    $task = $db->fetchOne(
        "SELECT id, budget, status
         FROM tb_tasks
         WHERE code = :code",
        [':code' => $taskCode]
    );

    if (!$task || $task['status'] !== 'open') {
        TaskCheckService::raise('TASK_NOT_OPEN');
    }

    if ((float)$task['budget'] < $price || (float)$task['budget'] < TaskUtils::MIN_OPEN_BUDGET) {
        TaskCheckService::raise('TASK_BUDGET_EXHAUSTED');
    }
}

function settleTesttaskBudget(Database $db, string $taskCode, float $price): array
{
    $startedTransaction = !$db->inTransaction();

    try {
        if ($startedTransaction) {
            $db->beginTransaction();
        }

        $task = $db->fetchOne(
            "SELECT id, budget, status
             FROM tb_tasks
             WHERE code = :code
             FOR UPDATE",
            [':code' => $taskCode]
        );

        if (!$task || $task['status'] !== 'open') {
            if ($startedTransaction && $db->inTransaction()) {
                $db->rollback();
            }
            Response::error(409, 'TASK_NOT_OPEN', 'Task is not open for submissions');
        }

        if ((float)$task['budget'] < $price || (float)$task['budget'] < TaskUtils::MIN_OPEN_BUDGET) {
            if ($startedTransaction && $db->inTransaction()) {
                $db->rollback();
            }
            Response::error(409, 'TASK_BUDGET_EXHAUSTED', 'Task budget is not enough for this submission');
        }

        $nextBudget = (float)$task['budget'] - $price;
        $nextStatus = $nextBudget < TaskUtils::MIN_OPEN_BUDGET ? 'closed' : (string)$task['status'];
        $shouldClose = $nextStatus === 'closed' ? 1 : 0;
        $db->exec(
            "UPDATE tb_tasks
             SET budget = :budget,
                 status = :status,
                 closed_at = CASE WHEN :should_close = 1 THEN NOW() ELSE closed_at END
             WHERE id = :id",
            [
                ':budget' => $nextBudget,
                ':status' => $nextStatus,
                ':should_close' => $shouldClose,
                ':id' => $task['id']
            ]
        );

        if ($startedTransaction) {
            $db->commit();
        }

        return [
            'budget' => $nextBudget,
            'status' => $nextStatus
        ];
    } catch (Exception $e) {
        if ($startedTransaction && $db->inTransaction()) {
            $db->rollback();
        }
        throw $e;
    }
}

function logTesttaskEvent(
    Database $db,
    string $taskCode,
    ?int $botId,
    string $action,
    ?array $payload,
    bool $success,
    ?int $responseCode = null,
    ?string $responseBody = null,
    ?string $errorCode = null,
    ?string $errorMessage = null
): void {
    try {
        $payloadJson = null;
        if ($payload) {
            $encoded = json_encode($payload, JSON_UNESCAPED_UNICODE);
            if (is_string($encoded)) {
                if (strlen($encoded) <= TESTTASK_DB_PAYLOAD_JSON_MAX) {
                    $payloadJson = $encoded;
                } else {
                    $preview = truncateByBytes($encoded, TESTTASK_DB_PAYLOAD_JSON_MAX - 120);
                    $payloadJson = json_encode([
                        '_truncated' => true,
                        'bytes' => strlen($encoded),
                        'preview' => $preview,
                    ], JSON_UNESCAPED_UNICODE);
                    if (!is_string($payloadJson) || strlen($payloadJson) > TESTTASK_DB_PAYLOAD_JSON_MAX) {
                        $payloadJson = '{"_truncated":true}';
                    }
                }
            }
        }

        $db->insert('tb_task_logs', [
            'task_code' => $taskCode,
            'bot_id' => $botId,
            'action' => $action,
            'payload_json' => $payloadJson,
            'response_code' => $responseCode,
            'response_body' => $responseBody !== null ? truncateByBytes($responseBody, TESTTASK_DB_RESPONSE_BODY_MAX) : null,
            'success' => $success ? 1 : 0,
            'error_code' => $errorCode,
            'error_message' => $errorMessage !== null ? truncateByBytes($errorMessage, TESTTASK_DB_ERROR_MESSAGE_MAX) : null
        ]);
    } catch (Exception $e) {
        Logger::fileLog('ERROR', 'Testtask log write failed: ' . $e->getMessage(), [
            'task_code' => $taskCode,
            'action' => $action
        ]);
    }
}

function postTesttaskPayload(string $postapi, array $payload): array
{
    $body = json_encode($payload, JSON_UNESCAPED_UNICODE);
    if ($body === false) {
        Response::error(500, 'INTERNAL_ERROR', 'Failed to encode test payload');
    }

    if (function_exists('curl_init')) {
        return postTesttaskWithCurl($postapi, $body);
    }

    return postTesttaskWithStream($postapi, $body);
}

function buildTesttaskPayload(string $taskCode, array $payload): array
{
    $payload['task_code'] = $taskCode;
    return $payload;
}

function postTesttaskWithCurl(string $postapi, string $body): array
{
    $ch = curl_init($postapi);
    curl_setopt_array($ch, [
        CURLOPT_POST => true,
        CURLOPT_RETURNTRANSFER => true,
        CURLOPT_HTTPHEADER => [
            'Content-Type: application/json',
            'Content-Length: ' . strlen($body)
        ],
        CURLOPT_POSTFIELDS => $body,
        CURLOPT_TIMEOUT => 10,
        CURLOPT_CONNECTTIMEOUT => 5,
    ]);

    $responseBody = curl_exec($ch);
    $curlError = curl_error($ch);
    $responseCode = (int)curl_getinfo($ch, CURLINFO_RESPONSE_CODE);
    curl_close($ch);

    $responseBody = is_string($responseBody) ? $responseBody : null;
    if ($curlError !== '') {
        return [
            'success' => false,
            'response_code' => $responseCode > 0 ? $responseCode : null,
            'response_body' => $responseBody,
            'error_code' => 'TESTTASK_NETWORK_ERROR',
            'error_message' => 'Task test postapi request failed: ' . $curlError,
        ];
    }

    return formatTesttaskPostResult($responseCode, $responseBody);
}

function postTesttaskWithStream(string $postapi, string $body): array
{
    $context = stream_context_create([
        'http' => [
            'method' => 'POST',
            'header' => "Content-Type: application/json\r\nContent-Length: " . strlen($body) . "\r\n",
            'content' => $body,
            'timeout' => 10,
            'ignore_errors' => true,
        ],
    ]);

    $responseBody = @file_get_contents($postapi, false, $context);
    $responseCode = null;
    if (isset($http_response_header[0]) && preg_match('/\s(\d{3})\s/', $http_response_header[0], $matches)) {
        $responseCode = (int)$matches[1];
    }

    if ($responseBody === false && $responseCode === null) {
        return [
            'success' => false,
            'response_code' => null,
            'response_body' => null,
            'error_code' => 'TESTTASK_NETWORK_ERROR',
            'error_message' => 'Task test postapi request failed',
        ];
    }

    return formatTesttaskPostResult($responseCode, is_string($responseBody) ? $responseBody : null);
}

function formatTesttaskPostResult(?int $responseCode, ?string $responseBody): array
{
    if ($responseCode === null || $responseCode < 200 || $responseCode >= 300) {
        return [
            'success' => false,
            'response_code' => $responseCode,
            'response_body' => $responseBody,
            'error_code' => 'TESTTASK_POST_REJECTED',
            'error_message' => 'Task test postapi returned a non-success status',
        ];
    }

    return [
        'success' => true,
        'response_code' => $responseCode,
        'response_body' => $responseBody,
        'error_code' => null,
        'error_message' => null,
    ];
}

function truncateTesttaskResponse(string $value): string
{
    if (strlen($value) <= TESTTASK_MAX_RESPONSE_BYTES) {
        return $value;
    }

    return substr($value, 0, TESTTASK_MAX_RESPONSE_BYTES) . '... [truncated]';
}

function truncateByBytes(string $value, int $maxBytes): string
{
    if ($maxBytes <= 0) {
        return '';
    }
    if (strlen($value) <= $maxBytes) {
        return $value;
    }

    return substr($value, 0, $maxBytes);
}
