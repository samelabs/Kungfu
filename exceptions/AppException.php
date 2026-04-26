<?php

class AppException extends Exception
{
    private int $httpCode;
    private string $errorCode;
    private array $details;

    public function __construct(int $httpCode, string $errorCode, string $message, array $details = [], ?Throwable $previous = null)
    {
        parent::__construct($message, 0, $previous);
        $this->httpCode = $httpCode;
        $this->errorCode = $errorCode;
        $this->details = $details;
    }

    public function getHttpCode(): int
    {
        return $this->httpCode;
    }

    public function getErrorCode(): string
    {
        return $this->errorCode;
    }

    public function getDetails(): array
    {
        return $this->details;
    }
}
