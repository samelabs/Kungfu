<?php

class TaskCheckException extends Exception
{
    private $httpCode;
    private $apiErrorCode;
    private $details;
    private $logMessage;

    public function __construct(int $httpCode, string $apiErrorCode, string $message, string $logMessage, array $details = [])
    {
        parent::__construct($message);
        $this->httpCode = $httpCode;
        $this->apiErrorCode = $apiErrorCode;
        $this->logMessage = $logMessage;
        $this->details = $details;
    }

    public function getHttpCode(): int
    {
        return $this->httpCode;
    }

    public function getApiErrorCode(): string
    {
        return $this->apiErrorCode;
    }

    public function getDetails(): array
    {
        return $this->details;
    }

    public function getLogMessage(): string
    {
        return $this->logMessage;
    }
}

class TaskCheckService
{
    /**
     * Task readiness rule map:
     * rule_id => [http_code, error_code, agent_message, log_message]
     */
    private const RULES = [
        'POSTAPI_EMPTY' => [503, 'TASK_NOT_CONFIGURED', 'Task postapi is not configured', 'Task check: Post API is empty'],
        'POSTAPI_TOO_LONG' => [500, 'TASK_CONFIG_INVALID', 'Task postapi exceeds maximum length', 'Task check: Post API exceeds maximum length'],
        'POSTAPI_INVALID_URL' => [500, 'TASK_CONFIG_INVALID', 'Task postapi is not a valid URL', 'Task check: Post API is not a valid URL'],
        'POSTAPI_INVALID_SCHEME' => [500, 'TASK_CONFIG_INVALID', 'Task postapi must use http or https', 'Task check: Post API must use http or https'],
        'PRICE_INVALID' => [500, 'TASK_CONFIG_INVALID', 'Task price must be greater than zero', 'Task check: price must be greater than zero'],
        'TASK_NOT_OPEN' => [409, 'TASK_NOT_OPEN', 'Task is not open for submissions', 'Task check: task is not open for submissions'],
        'TASK_BUDGET_EXHAUSTED' => [409, 'TASK_BUDGET_EXHAUSTED', 'Task budget is not enough for this submission', 'Task check: task budget is not enough'],
    ];

    public static function run(string $postapi, float $price, callable $budgetChecker): void
    {
        self::validatePostapi($postapi);
        self::validatePrice($price);
        $budgetChecker();
    }

    public static function validatePostapi(string $postapi, int $maxLength = 2048): void
    {
        if ($postapi === '') {
            self::raise('POSTAPI_EMPTY');
        }

        if (strlen($postapi) > $maxLength) {
            self::raise('POSTAPI_TOO_LONG');
        }

        if (!filter_var($postapi, FILTER_VALIDATE_URL)) {
            self::raise('POSTAPI_INVALID_URL');
        }

        $scheme = strtolower((string)parse_url($postapi, PHP_URL_SCHEME));
        if (!in_array($scheme, ['http', 'https'], true)) {
            self::raise('POSTAPI_INVALID_SCHEME');
        }
    }

    public static function validatePrice(float $price): void
    {
        if ($price <= 0) {
            self::raise('PRICE_INVALID');
        }
    }

    public static function raise(string $ruleId, array $details = []): void
    {
        if (!isset(self::RULES[$ruleId])) {
            throw new TaskCheckException(500, 'INTERNAL_ERROR', 'Task check rule is not configured', 'Task check: unknown rule ' . $ruleId);
        }
        [$httpCode, $errorCode, $agentMessage, $logMessage] = self::RULES[$ruleId];
        throw new TaskCheckException($httpCode, $errorCode, $agentMessage, $logMessage, $details);
    }
}
