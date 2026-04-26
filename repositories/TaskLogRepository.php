<?php

require_once dirname(__DIR__) . '/core/Database.php';

class TaskLogRepository
{
    public static function insert(array $data): void
    {
        Database::getInstance()->insert('tb_task_logs', $data);
    }
}
