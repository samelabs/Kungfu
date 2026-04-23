<?php
/**
 * Logger - Operation Log Recording
 * Kungfu Platform Core Component
 */

class Logger {
    
    /**
     * Record operation log
     */
    public static function log(
        ?int $botId,
        string $action,
        ?string $targetType = null,
        ?string $targetId = null,
        ?string $ip = null,
        ?string $userAgent = null,
        bool $success = true,
        ?string $errorCode = null,
        ?string $errorMsg = null,
        ?array $requestData = null
    ): void {
        try {
            require_once __DIR__ . '/Security.php';
            $db = Database::getInstance();
            
            // Mask sensitive information
            $maskedRequestData = self::maskSensitiveData($requestData);
            
            $db->insert('tb_logs', [
                'bot_id' => $botId,
                'action' => $action,
                'target_type' => $targetType,
                'target_id' => $targetId,
                'ip_address' => $ip ?? self::getClientIp(),
                'user_agent' => $userAgent ?? ($_SERVER['HTTP_USER_AGENT'] ?? null),
                'request_data' => $maskedRequestData ? json_encode($maskedRequestData) : null,
                'success' => $success ? 1 : 0,
                'error_code' => $errorCode,
                'error_msg' => $errorMsg
            ]);
        } catch (Exception $e) {
            // Log recording failure should not affect main flow, but should log to file
            error_log("[Logger Error] Failed to write log: " . $e->getMessage());
        }
    }
    
    /**
     * Log request (convenience method)
     */
    public static function logRequest(
        ?int $botId,
        string $action,
        bool $success = true,
        ?string $errorCode = null,
        ?array $requestData = null
    ): void {
        self::log(
            $botId,
            $action,
            null,
            null,
            null,
            null,
            $success,
            $errorCode,
            null,
            $requestData
        );
    }
    
    /**
     * Mask sensitive data
     */
    private static function maskSensitiveData(?array $data): ?array {
        if ($data === null) {
            return null;
        }
        
        $masked = [];
        foreach ($data as $key => $value) {
            // Mask Key
            if (is_string($value) && preg_match('/kf_live_[a-f0-9]{64}/i', $value)) {
                $masked[$key] = Security::redactSecrets($value);
            }
            // Truncate long content fields
            elseif (is_string($value) && strlen($value) > 1000) {
                $masked[$key] = substr($value, 0, 1000) . '... [truncated]';
            }
            // Recursively process nested arrays
            elseif (is_array($value)) {
                $masked[$key] = self::maskSensitiveData($value);
            }
            else {
                $masked[$key] = $value;
            }
        }
        
        return $masked;
    }
    
    /**
     * Mask Key
     */
    public static function maskKey(string $key): string {
        require_once __DIR__ . '/Security.php';
        return Security::maskKey($key);
    }

    /**
     * @deprecated use Security::maskKey
     */
    public static function legacyMaskKey(string $key): string {
        if (strlen($key) <= 12) {
            return str_repeat('*', strlen($key));
        }
        
        return substr($key, 0, 8) . '****' . substr($key, -4);
    }
    
    /**
     * Get client IP
     */
    public static function getClientIp(): string {
        $ipSources = [
            'HTTP_CF_CONNECTING_IP', // CloudFlare
            'HTTP_X_FORWARDED_FOR',
            'HTTP_X_FORWARDED',
            'HTTP_X_CLUSTER_CLIENT_IP',
            'HTTP_FORWARDED_FOR',
            'HTTP_FORWARDED',
            'REMOTE_ADDR'
        ];
        
        foreach ($ipSources as $source) {
            if (!empty($_SERVER[$source])) {
                $ip = $_SERVER[$source];
                // Handle multiple IPs (X-Forwarded-For may have multiple)
                if (strpos($ip, ',') !== false) {
                    $ips = explode(',', $ip);
                    $ip = trim($ips[0]);
                }
                
                // Validate IP format
                if (filter_var($ip, FILTER_VALIDATE_IP)) {
                    return $ip;
                }
            }
        }
        
        return '0.0.0.0';
    }
    
    /**
     * Write to file log (backup)
     */
    public static function fileLog(string $level, string $message, array $context = []): void {
        $logDir = __DIR__ . '/../logs';
        if (!is_dir($logDir)) {
            mkdir($logDir, 0777, true);
        }
        
        $logFile = $logDir . '/' . date('Y-m-d') . '.log';
        $timestamp = date('Y-m-d H:i:s');
        $contextStr = empty($context) ? '' : ' | ' . json_encode($context);
        
        $line = "[{$timestamp}] [{$level}] {$message}{$contextStr}" . PHP_EOL;
        
        file_put_contents($logFile, $line, FILE_APPEND | LOCK_EX);
    }
}
