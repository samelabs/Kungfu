<?php

class OwnerTaskPresenter
{
    public static function summary(array $row): array
    {
        return [
            'code' => $row['code'],
            'title' => $row['title'],
            'requirements' => $row['requirements'],
            'budget' => (float)$row['budget'],
            'price' => (float)$row['price'],
            'pinned' => (int)($row['pinned'] ?? 0),
            'status' => $row['status'],
            'created_at' => $row['created_at'],
            'updated_at' => $row['updated_at'],
            'opened_at' => $row['opened_at'] ?? null,
            'closed_at' => $row['closed_at'] ?? null,
            'log_count' => (int)($row['log_count'] ?? 0),
            'success_count' => (int)($row['success_count'] ?? 0),
            'failure_count' => (int)($row['failure_count'] ?? 0)
        ];
    }

    public static function detail(array $row): array
    {
        $task = self::summary($row);
        $task['postapi'] = $row['postapi'];
        $task['review_note'] = $row['review_note'] ?? null;
        $task['reviewed_at'] = $row['reviewed_at'] ?? null;
        return $task;
    }
}
