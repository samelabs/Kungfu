<?php

require_once dirname(__DIR__) . '/repositories/TaskRepository.php';
require_once dirname(__DIR__) . '/presenters/AgentTaskPresenter.php';
require_once dirname(__DIR__) . '/exceptions/AppException.php';

class TaskBoardService
{
    public static function listOpenTasks(): array
    {
        $rows = TaskRepository::listOpenTasks();
        $tasks = array_map([AgentTaskPresenter::class, 'listItem'], $rows);

        return [
            'tasks' => $tasks,
            'meta' => [
                'total' => TaskRepository::countOpenTasks(),
                'returned' => count($tasks)
            ]
        ];
    }

    public static function getOpenTask(string $code): array
    {
        $task = TaskRepository::findOpenTaskByCode($code);
        if (!$task) {
            throw new AppException(404, 'NOT_FOUND', 'Task not found');
        }

        return [
            'task' => AgentTaskPresenter::detail($task)
        ];
    }
}
