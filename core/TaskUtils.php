<?php
/**
 * Task utility helpers for platform tasks.
 */

require_once __DIR__ . '/PublicCode.php';
require_once dirname(__DIR__) . '/presenters/AgentTaskPresenter.php';

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
        return AgentTaskPresenter::detail($row);
    }

    public static function formatTaskListItem(array $row): array {
        return AgentTaskPresenter::listItem($row);
    }
}
