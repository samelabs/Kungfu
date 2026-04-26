<?php

require_once dirname(__DIR__) . '/core/Database.php';
require_once dirname(__DIR__) . '/core/Response.php';
require_once dirname(__DIR__) . '/core/Transaction.php';
require_once dirname(__DIR__) . '/core/Logger.php';
require_once dirname(__DIR__) . '/core/TaskUtils.php';
require_once dirname(__DIR__) . '/core/TaskCheckService.php';
require_once dirname(__DIR__) . '/core/TaskDeliveryService.php';
require_once dirname(__DIR__) . '/exceptions/AppException.php';
require_once dirname(__DIR__) . '/repositories/TaskRepository.php';
require_once dirname(__DIR__) . '/repositories/TaskLogRepository.php';

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
            throw new AppException($e->getHttpCode(), $e->getApiErrorCode(), $e->getMessage(), $e->getDetails(), $e);
        }

        $postResult = TaskDeliveryService::postJson(
            $postapi,
            TaskDeliveryService::buildPayload($taskCode, $input),
            [
                'network_code' => 'POSTAPI_NETWORK_ERROR',
                'network_message' => 'Task postapi request failed',
                'network_message_prefix' => 'Task postapi request failed: ',
                'rejected_code' => 'POSTAPI_REJECTED',
                'rejected_message' => 'Task postapi returned a non-success status',
            ]
        );

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
            throw new AppException(
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

    private static function settleDeliveredSubmission(string $taskCode, int $botId, float $price): float
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

            TaskRepository::decrementTaskBudgetForDelivery((int)$task['id'], $price, TaskUtils::MIN_OPEN_BUDGET);

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
            TaskLogRepository::insert([
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
