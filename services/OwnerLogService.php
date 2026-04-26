<?php

require_once dirname(__DIR__) . '/repositories/OwnerLogRepository.php';
require_once dirname(__DIR__) . '/presenters/OwnerLogPresenter.php';
require_once dirname(__DIR__) . '/exceptions/AppException.php';

class OwnerLogService
{
    public static function getLogs(int $botId, string $type, int $page, int $pageSize, string $taskCode = ''): array
    {
        self::ensureLogSchema($type);
        $offset = ($page - 1) * $pageSize;

        if ($type === 'credits') {
            $total = OwnerLogRepository::countCreditLogs($botId);
            return [
                'type' => $type,
                'balance' => OwnerLogRepository::findBalanceByBotId($botId),
                'items' => array_map([OwnerLogPresenter::class, 'creditLogRow'], OwnerLogRepository::listCreditLogs($botId, $pageSize, $offset)),
                'pagination' => OwnerLogPresenter::pagination($page, $pageSize, $total)
            ];
        }

        if ($type === 'agent') {
            $total = OwnerLogRepository::countAgentLogs($botId);
            return [
                'type' => $type,
                'items' => array_map([OwnerLogPresenter::class, 'agentLogRow'], OwnerLogRepository::listAgentLogs($botId, $pageSize, $offset)),
                'pagination' => OwnerLogPresenter::pagination($page, $pageSize, $total)
            ];
        }

        $total = OwnerLogRepository::countTaskLogs($botId, $taskCode);
        return [
            'type' => $type,
            'task_filter' => $taskCode !== '' ? $taskCode : null,
            'tasks' => array_map([OwnerLogPresenter::class, 'taskFilterItem'], OwnerLogRepository::listOwnerTasksForFilter($botId)),
            'items' => array_map([OwnerLogPresenter::class, 'taskLogRow'], OwnerLogRepository::listTaskLogs($botId, $pageSize, $offset, $taskCode)),
            'pagination' => OwnerLogPresenter::pagination($page, $pageSize, $total)
        ];
    }

    private static function ensureLogSchema(string $type): void
    {
        $required = ['tb_bots'];
        if ($type === 'credits') {
            $required[] = 'tb_transactions';
        } elseif ($type === 'agent') {
            $required[] = 'tb_logs';
        } else {
            $required[] = 'tb_tasks';
            $required[] = 'tb_task_logs';
        }

        $missing = [];
        foreach ($required as $table) {
            if (!OwnerLogRepository::tableExists($table)) {
                $missing[] = $table;
            }
        }

        if (!empty($missing)) {
            throw new AppException(503, 'LOGS_NOT_READY', 'Logs are not ready yet.');
        }
    }
}
