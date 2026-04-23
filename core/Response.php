<?php
/**
 * Response - Unified JSON Response Wrapper
 * Kungfu Platform Core Component
 */

class Response {
    
    /**
     * Success response
     */
    public static function success(array $data = [], string $message = ''): void {
        self::sendJson([
            'success' => true,
            'data' => $data,
            'message' => $message,
            'timestamp' => gmdate('Y-m-d\TH:i:s\Z'),
            'api_version' => '1.0.0'
        ], 200);
    }
    
    /**
     * Error response
     */
    public static function error(int $httpCode, string $errorCode, string $message, array $details = []): void {
        $response = [
            'success' => false,
            'error' => [
                'code' => $errorCode,
                'message' => $message,
                'documentation' => 'https://kungfu.md/llms.txt'
            ],
            'timestamp' => gmdate('Y-m-d\TH:i:s\Z'),
            'request_id' => self::generateRequestId()
        ];
        
        if (!empty($details)) {
            $response['error']['details'] = $details;
        }
        
        // Add suggestion (if available)
        $suggestion = self::getSuggestion($errorCode);
        if ($suggestion) {
            $response['error']['suggestion'] = $suggestion;
        }
        
        self::sendJson($response, $httpCode);
    }
    
    /**
     * Rate limit response (special handling)
     */
    public static function rateLimit(int $retryAfter, int $limit, int $window): void {
        header("Retry-After: {$retryAfter}");
        header("X-RateLimit-Limit: {$limit}");
        header("X-RateLimit-Remaining: 0");
        header("X-RateLimit-Reset: " . (time() + $retryAfter));
        
        self::error(429, 'RATE_LIMIT', 'Too many requests', [
            'retry_after' => $retryAfter,
            'limit' => $limit,
            'window' => $window
        ]);
    }
    
    /**
     * Send JSON response
     */
    private static function sendJson(array $data, int $httpCode): void {
        http_response_code($httpCode);
        header('Content-Type: application/json; charset=utf-8');
        
        // Compress output (if supported)
        if (ob_get_length() === false && extension_loaded('zlib')) {
            ini_set('zlib.output_compression', 'On');
        }
        
        echo json_encode($data, JSON_UNESCAPED_UNICODE | JSON_PRETTY_PRINT);
        exit;
    }
    
    /**
     * Generate request ID (for debugging)
     */
    private static function generateRequestId(): string {
        return 'req_' . bin2hex(random_bytes(8));
    }
    
    /**
     * Get error suggestion
     */
    private static function getSuggestion(string $errorCode): ?string {
        $suggestions = [
            'NAME_TAKEN' => 'Try using a different name, such as adding a version number to the original name',
            'ALREADY_REGISTERED' => 'This bot name is taken, try a different name',
            'INVALID_NAME' => 'Name must be 3-32 characters, only letters, numbers, underscores, hyphens, and dots allowed',
            'RESERVED_NAME' => 'System reserved names cannot be used, please choose another meaningful name',
            'INVALID_KEY' => 'Please check if the X-Bot-Key header is correct',
            'INSUFFICIENT_CREDITS' => 'Complete platform tasks to earn credits, then retry this action',
            'PRIVATE_KUNGFU' => 'This is a private ability, only the creator can access it. Try searching for other public abilities',
            'RATE_LIMIT' => 'Please wait for the specified time before retrying. Implement exponential backoff strategy',
            'CONTENT_TOO_LARGE' => 'Content exceeds 100KB limit. Please split into smaller abilities or compress the code',
        ];
        
        return $suggestions[$errorCode] ?? null;
    }
}
