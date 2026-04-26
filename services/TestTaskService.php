<?php

require_once dirname(__DIR__) . '/core/TaskUtils.php';
require_once dirname(__DIR__) . '/core/TaskCheckService.php';
require_once dirname(__DIR__) . '/core/TaskDeliveryService.php';
require_once dirname(__DIR__) . '/core/Logger.php';
require_once dirname(__DIR__) . '/repositories/TaskRepository.php';
require_once dirname(__DIR__) . '/repositories/TaskLogRepository.php';
require_once dirname(__DIR__) . '/exceptions/AppException.php';

class TestTaskService
{
    private const MAX_RESPONSE_BYTES = 16000;
    private const DB_ERROR_MESSAGE_MAX = 256;
    private const DB_RESPONSE_BODY_MAX = 65535;
    private const DB_PAYLOAD_JSON_MAX = 60000;

    public static function deliver(int $botId, string $code, array $input): array
    {
        $task = TaskRepository::findTaskByCode($code);
        if (!$task) {
            throw new AppException(404, 'NOT_FOUND', 'Task not found');
        }

        if ((int)$task['bot_id'] !== $botId) {
            throw new AppException(403, 'NOT_OWNER', 'Only the task owner can test this task');
        }

        $postapi = trim((string)($task['postapi'] ?? ''));
        $price = (float)($task['price'] ?? 0);

        try {
            TaskCheckService::run($postapi, $price, static function () use ($code, $price): void {
                self::ensureBudgetAvailable($code, $price);
            });
        } catch (TaskCheckException $e) {
            self::logEvent(
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
            throw new AppException($e->getHttpCode(), $e->getApiErrorCode(), $e->getMessage(), $e->getDetails(), $e);
        }

        $postPayload = TaskDeliveryService::buildPayload((string)$code, $input);
        $postResult = TaskDeliveryService::postJson(
            $postapi,
            $postPayload,
            [
                'network_code' => 'TESTTASK_NETWORK_ERROR',
                'network_message' => 'Task test postapi request failed',
                'network_message_prefix' => 'Task test postapi request failed: ',
                'rejected_code' => 'TESTTASK_POST_REJECTED',
                'rejected_message' => 'Task test postapi returned a non-success status',
            ]
        );

        if (!$postResult['success']) {
            self::logEvent(
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
            throw new AppException(
                424,
                $postResult['error_code'] ?? 'TESTTASK_POST_FAILED',
                $postResult['error_message'] ?? 'Task test delivery failed',
                [
                    'post' => [
                        'delivered' => false,
                        'response_code' => $postResult['response_code'],
                        'response_body' => self::truncateResponse((string)($postResult['response_body'] ?? ''))
                    ]
                ]
            );
        }

        self::logEvent(
            (string)$code,
            $botId,
            'post_succeeded',
            $postPayload,
            true,
            $postResult['response_code'],
            $postResult['response_body']
        );

        $billing = self::settleBudget((string)$code, $price);

        return [
            'task_code' => $code,
            'post' => [
                'delivered' => true,
                'response_code' => $postResult['response_code'],
                'response_body' => self::truncateResponse((string)($postResult['response_body'] ?? ''))
            ],
            'billing' => [
                'cost' => $price,
                'budget' => $billing['budget'],
                'status' => $billing['status']
            ]
        ];
    }

    private static function ensureBudgetAvailable(string $taskCode, float $price): void
    {
        $task = TaskRepository::findTaskBudgetStatusByCode($taskCode);

        if (!$task || $task['status'] !== 'open') {
            TaskCheckService::raise('TASK_NOT_OPEN');
        }

        if ((float)$task['budget'] < $price || (float)$task['budget'] < TaskUtils::MIN_OPEN_BUDGET) {
            TaskCheckService::raise('TASK_BUDGET_EXHAUSTED');
        }
    }

    private static function settleBudget(string $taskCode, float $price): array
    {
        $db = Database::getInstance();
        $startedTransaction = !$db->inTransaction();

        try {
            if ($startedTransaction) {
                $db->beginTransaction();
            }

            $task = TaskRepository::findTaskBudgetStatusByCodeForUpdate($taskCode);
            if (!$task || $task['status'] !== 'open') {
                if ($startedTransaction && $db->inTransaction()) {
                    $db->rollback();
                }
                throw new AppException(409, 'TASK_NOT_OPEN', 'Task is not open for submissions');
            }

            if ((float)$task['budget'] < $price || (float)$task['budget'] < TaskUtils::MIN_OPEN_BUDGET) {
                if ($startedTransaction && $db->inTransaction()) {
                    $db->rollback();
                }
                throw new AppException(409, 'TASK_BUDGET_EXHAUSTED', 'Task budget is not enough for this submission');
            }

            $nextBudget = (float)$task['budget'] - $price;
            $nextStatus = $nextBudget < TaskUtils::MIN_OPEN_BUDGET ? 'closed' : (string)$task['status'];
            TaskRepository::updateTaskBudgetAndStatus((int)$task['id'], $nextBudget, $nextStatus, $nextStatus === 'closed');

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

    private static function logEvent(
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
                    if (strlen($encoded) <= self::DB_PAYLOAD_JSON_MAX) {
                        $payloadJson = $encoded;
                    } else {
                        $preview = self::truncateByBytes($encoded, self::DB_PAYLOAD_JSON_MAX - 120);
                        $payloadJson = json_encode([
                            '_truncated' => true,
                            'bytes' => strlen($encoded),
                            'preview' => $preview,
                        ], JSON_UNESCAPED_UNICODE);
                        if (!is_string($payloadJson) || strlen($payloadJson) > self::DB_PAYLOAD_JSON_MAX) {
                            $payloadJson = '{"_truncated":true}';
                        }
                    }
                }
            }

            TaskLogRepository::insert([
                'task_code' => $taskCode,
                'bot_id' => $botId,
                'action' => $action,
                'payload_json' => $payloadJson,
                'response_code' => $responseCode,
                'response_body' => $responseBody !== null ? self::truncateByBytes($responseBody, self::DB_RESPONSE_BODY_MAX) : null,
                'success' => $success ? 1 : 0,
                'error_code' => $errorCode,
                'error_message' => $errorMessage !== null ? self::truncateByBytes($errorMessage, self::DB_ERROR_MESSAGE_MAX) : null
            ]);
        } catch (Exception $e) {
            Logger::fileLog('ERROR', 'Testtask log write failed: ' . $e->getMessage(), [
                'task_code' => $taskCode,
                'action' => $action
            ]);
        }
    }

    private static function truncateResponse(string $value): string
    {
        if (strlen($value) <= self::MAX_RESPONSE_BYTES) {
            return $value;
        }

        return substr($value, 0, self::MAX_RESPONSE_BYTES) . '... [truncated]';
    }

    private static function truncateByBytes(string $value, int $maxBytes): string
    {
        if ($maxBytes <= 0) {
            return '';
        }
        if (strlen($value) <= $maxBytes) {
            return $value;
        }

        return substr($value, 0, $maxBytes);
    }
}
