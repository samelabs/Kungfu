<?php

class TaskLogPresenter
{
    public static function ownerTaskDetailRow(array $row): array
    {
        return [
            'id' => (int)$row['id'],
            'bot_id' => $row['bot_id'] !== null ? (int)$row['bot_id'] : null,
            'action' => self::actionLabel((string)$row['action']),
            'response_code' => $row['response_code'] !== null ? (int)$row['response_code'] : null,
            'success' => (bool)$row['success'],
            'error_code' => $row['error_code'],
            'error_message' => $row['error_message'],
            'created_at' => $row['created_at']
        ];
    }

    public static function actionLabel(string $action): string
    {
        $labels = [
            'kfcheck' => 'Task check',
            'post_succeeded' => 'Delivery accepted',
            'post_failed' => 'Delivery failed',
        ];

        return $labels[$action] ?? $action;
    }
}
