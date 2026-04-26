<?php

require_once dirname(__DIR__) . '/core/Response.php';
require_once dirname(__DIR__) . '/core/TaskUtils.php';

class TaskValidator
{
    public static function validatePayload(
        string $title,
        string $requirements,
        string $postapi,
        float $budget,
        float $price,
        array $config
    ): void {
        self::validateBasics($title, $requirements, $postapi, $price, $config);
        if ($budget <= 0) {
            Response::error(400, 'INVALID_BUDGET', 'Budget must be greater than zero');
        }
        if ($budget < TaskUtils::MIN_OPEN_BUDGET) {
            Response::error(400, 'TASK_BUDGET_TOO_LOW', 'Budget must be at least 1000 credits');
        }
    }

    public static function validateBasics(
        string $title,
        string $requirements,
        string $postapi,
        float $price,
        array $config
    ): void {
        if ($title === '') {
            Response::error(400, 'MISSING_FIELD', 'Missing required field: title');
        }
        if (mb_strlen($title) > (int)$config['max_title_length']) {
            Response::error(400, 'TITLE_TOO_LONG', 'Title maximum 128 characters');
        }
        if ($requirements === '') {
            Response::error(400, 'MISSING_FIELD', 'Missing required field: requirements');
        }
        if (mb_strlen($requirements) > 20000) {
            Response::error(400, 'REQUIREMENTS_TOO_LONG', 'Requirements maximum 20000 characters');
        }
        self::validatePostapi($postapi);
        if ($price <= 0) {
            Response::error(400, 'INVALID_PRICE', 'Price must be greater than zero');
        }
    }

    public static function validatePostapi(string $postapi): void
    {
        if ($postapi === '') {
            Response::error(400, 'MISSING_FIELD', 'Missing required field: postapi');
        }
        if (strlen($postapi) > 2048) {
            Response::error(400, 'POSTAPI_TOO_LONG', 'Postapi maximum 2048 characters');
        }
        if (!filter_var($postapi, FILTER_VALIDATE_URL)) {
            Response::error(400, 'INVALID_POSTAPI', 'Postapi must be a valid URL');
        }
        $scheme = strtolower((string)parse_url($postapi, PHP_URL_SCHEME));
        if (!in_array($scheme, ['http', 'https'], true)) {
            Response::error(400, 'INVALID_POSTAPI', 'Postapi must use http or https');
        }
    }

    public static function assertOpenable(string $postapi, float $budget, float $price): void
    {
        self::validatePostapi($postapi);
        if ($price <= 0) {
            Response::error(400, 'INVALID_PRICE', 'Price must be greater than zero');
        }
        if ($budget < TaskUtils::MIN_OPEN_BUDGET || $budget < $price) {
            Response::error(400, 'TASK_BUDGET_TOO_LOW', 'Open tasks require enough budget');
        }
    }

    public static function parseMoney($value, string $field): float
    {
        if ($value === null || $value === '') {
            Response::error(400, 'MISSING_FIELD', "Missing required field: {$field}");
        }
        if (!is_numeric($value)) {
            Response::error(400, 'INVALID_MONEY', "{$field} must be numeric");
        }
        return round((float)$value, 4);
    }
}
