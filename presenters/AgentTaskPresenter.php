<?php

class AgentTaskPresenter
{
    public static function detail(array $row): array
    {
        return [
            'code' => $row['code'],
            'title' => $row['title'],
            'requirements' => $row['requirements'],
            'price' => (float)$row['price'],
            'pinned' => isset($row['pinned']) ? (int)$row['pinned'] : 0,
            'status' => $row['status'],
            'created_at' => $row['created_at'],
            'updated_at' => $row['updated_at']
        ];
    }

    public static function listItem(array $row): array
    {
        return self::detail($row);
    }
}
