<?php

require_once __DIR__ . '/AppException.php';

class RateLimitException extends AppException
{
    private int $retryAfter;
    private int $limit;
    private int $window;

    public function __construct(int $retryAfter, int $limit, int $window, string $message = 'Too many requests')
    {
        parent::__construct(429, 'RATE_LIMIT', $message, [
            'retry_after' => $retryAfter,
            'limit' => $limit,
            'window' => $window
        ]);
        $this->retryAfter = $retryAfter;
        $this->limit = $limit;
        $this->window = $window;
    }

    public function getRetryAfter(): int
    {
        return $this->retryAfter;
    }

    public function getLimit(): int
    {
        return $this->limit;
    }

    public function getWindow(): int
    {
        return $this->window;
    }
}
