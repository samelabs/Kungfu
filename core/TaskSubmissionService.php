<?php

require_once __DIR__ . '/Database.php';
require_once __DIR__ . '/Response.php';
require_once __DIR__ . '/Transaction.php';
require_once __DIR__ . '/Logger.php';
require_once __DIR__ . '/TaskUtils.php';
require_once __DIR__ . '/TaskCheckService.php';

class TaskSubmissionService
{
    private const MAX_TASK_RESPONSE_LOG_BYTES = 4000;

    public static function submit(array $task, int $botId, array $input): array
    {
        $postapi = trim((string)($task['postapi'] ?? ''));
        $price = (float)($task['price'] ?? 0);
        $taskCode = (string)$task['code'];

        try {
            TaskCheckService::run($postapi, $price, static function () use ($taskCode, $price): void {
                self::ensureBudgetAvailable($taskCode, $price);
            });
        } catch (TaskCheckException $e) {
            self::logTaskEvent(
                $taskCode,
                $botId,
                'kfcheck',
                null,
                false,
                null,
                null,
                $e->getApiErrorCode(),
                $e->getLogMessage()
            );
            Response::error($e->getHttpCode(), $e->getApiErrorCode(), $e->getMessage(), $e->getDetails());
        }

        $postResult = self::postToCustomerApi($postapi, self::buildCustomerPayload($taskCode, $input));

        if (!$postResult['success']) {
            self::logTaskEvent(
                $taskCode,
                $botId,
                'post_failed',
                null,
                false,
                $postResult['response_code'],
                null,
                $postResult['error_code'],
                null
            );
            Response::error(
                424,
                $postResult['error_code'] ?? 'TASK_POST_FAILED',
                'Task delivery failed. Please retry later.',
                [
                    'post' => [
                        'delivered' => false,
                        'response_code' => $postResult['response_code']
                    ]
                ]
            );
        }

        $balance = self::settleDeliveredSubmission($taskCode, $botId, $price);

        self::logTaskEvent(
            $taskCode,
            $botId,
            'post_succeeded',
            null,
            true,
            $postResult['response_code'],
            $postResult['response_body']
        );

        Logger::log($botId, 'task_submit', 'task', $taskCode, null, null, true, null, null, [
            'reward' => $price,
            'response_code' => $postResult['response_code']
        ]);

        return [
            'task_code' => $taskCode,
            'post' => [
                'delivered' => true,
                'response_code' => $postResult['response_code']
            ],
            'billing' => [
                'reward' => $price,
                'balance' => (float)$balance
            ]
        ];
    }

    private static function postToCustomerApi(string $postapi, array $payload): array
    {
        $body = json_encode($payload, JSON_UNESCAPED_UNICODE);
        if ($body === false) {
            Response::error(500, 'INTERNAL_ERROR', 'Failed to encode submission payload');
        }

        if (function_exists('curl_init')) {
            return self::postWithCurl($postapi, $body);
        }

        return self::postWithStreamContext($postapi, $body);
    }

    private static function postWithCurl(string $postapi, string $body): array
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
                'error_code' => 'POSTAPI_NETWORK_ERROR',
                'error_message' => 'Task postapi request failed: ' . $curlError,
            ];
        }

        if ($responseCode < 200 || $responseCode >= 300) {
            return [
                'success' => false,
                'response_code' => $responseCode,
                'response_body' => $responseBody,
                'error_code' => 'POSTAPI_REJECTED',
                'error_message' => 'Task postapi returned a non-success status',
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

    private static function buildCustomerPayload(string $taskCode, array $payload): array
    {
        $payload['task_code'] = $taskCode;
        return $payload;
    }

    private static function postWithStreamContext(string $postapi, string $body): array
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

        $responseBody = is_string($responseBody) ? $responseBody : null;
        if ($responseBody === null && $responseCode === null) {
            return [
                'success' => false,
                'response_code' => null,
                'response_body' => null,
                'error_code' => 'POSTAPI_NETWORK_ERROR',
                'error_message' => 'Task postapi request failed',
            ];
        }

        if ($responseCode === null || $responseCode < 200 || $responseCode >= 300) {
            return [
                'success' => false,
                'response_code' => $responseCode,
                'response_body' => $responseBody,
                'error_code' => 'POSTAPI_REJECTED',
                'error_message' => 'Task postapi returned a non-success status',
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

    private static function ensureBudgetAvailable(string $taskCode, float $price): void
    {
        $db = Database::getInstance();
        $task = $db->fetchOne(
            "SELECT budget, price, status
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

    private static function settleDeliveredSubmission(string $taskCode, int $botId, float $price): float
    {
        $db = Database::getInstance();
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

            $db->exec(
                "UPDATE tb_tasks
                 SET budget = budget - :price_debit,
                     status = CASE WHEN budget - :price_close_check_status < :min_open_budget_status THEN 'closed' ELSE status END,
                     closed_at = CASE WHEN budget - :price_close_check_closed < :min_open_budget_closed THEN NOW() ELSE closed_at END
                 WHERE id = :id",
                [
                    ':price_debit' => $price,
                    ':price_close_check_status' => $price,
                    ':price_close_check_closed' => $price,
                    ':min_open_budget_status' => TaskUtils::MIN_OPEN_BUDGET,
                    ':min_open_budget_closed' => TaskUtils::MIN_OPEN_BUDGET,
                    ':id' => $task['id']
                ]
            );

            $balance = Transaction::record($botId, 'earn_task', $price, 'task', $taskCode);

            if ($startedTransaction) {
                $db->commit();
            }

            return $balance;
        } catch (Exception $e) {
            if ($startedTransaction && $db->isConnected() && $db->inTransaction()) {
                $db->rollback();
            }
            throw $e;
        }
    }

    private static function logTaskEvent(
        string $taskCode,
        int $botId,
        string $action,
        ?array $payload,
        bool $success,
        ?int $responseCode = null,
        ?string $responseBody = null,
        ?string $errorCode = null,
        ?string $errorMessage = null
    ): void {
        try {
            $db = Database::getInstance();
            $db->insert('tb_task_logs', [
                'task_code' => $taskCode,
                'bot_id' => $botId,
                'action' => $action,
                'payload_json' => $payload ? json_encode($payload, JSON_UNESCAPED_UNICODE) : null,
                'response_code' => $responseCode,
                'response_body' => $responseBody !== null ? self::truncateForLog($responseBody) : null,
                'success' => $success ? 1 : 0,
                'error_code' => $errorCode,
                'error_message' => $errorMessage
            ]);
        } catch (Exception $e) {
            Logger::fileLog('ERROR', 'Task log write failed: ' . $e->getMessage(), [
                'task_code' => $taskCode,
                'action' => $action
            ]);
        }
    }

    private static function truncateForLog(string $value): string
    {
        if (strlen($value) <= self::MAX_TASK_RESPONSE_LOG_BYTES) {
            return $value;
        }

        return substr($value, 0, self::MAX_TASK_RESPONSE_LOG_BYTES) . '... [truncated]';
    }

}
