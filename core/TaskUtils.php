<?php
/**
 * Task utility helpers for platform tasks.
 */

require_once __DIR__ . '/PublicCode.php';

class TaskUtils {
    public const MIN_OPEN_BUDGET = 1000.0;

    public static function validateCode(?string $code): string {
        return PublicCode::require($code);
    }

    public static function openBudgetWhereClause(string $alias = 't'): string {
        $prefix = $alias !== '' ? $alias . '.' : '';
        return $prefix . "status = 'open' AND " . $prefix . "price > 0 AND " . $prefix . "budget >= " . self::MIN_OPEN_BUDGET . " AND " . $prefix . "budget >= " . $prefix . "price";
    }

    public static function formatTaskForAgent(array $row): array {
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

    public static function formatTaskListItem(array $row): array {
        $task = self::formatTaskForAgent($row);
        return $task;
    }
}
