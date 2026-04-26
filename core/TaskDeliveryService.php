<?php

require_once dirname(__DIR__) . '/exceptions/AppException.php';

class TaskDeliveryService
{
    public static function buildPayload(string $taskCode, array $payload): array
    {
        $payload['task_code'] = $taskCode;
        return $payload;
    }

    public static function postJson(string $url, array $payload, array $errors): array
    {
        $body = json_encode($payload, JSON_UNESCAPED_UNICODE);
        if ($body === false) {
            throw new AppException(500, 'INTERNAL_ERROR', 'Failed to encode submission payload');
        }

        if (function_exists('curl_init')) {
            return self::postWithCurl($url, $body, $errors);
        }

        return self::postWithStreamContext($url, $body, $errors);
    }

    private static function postWithCurl(string $url, string $body, array $errors): array
    {
        $ch = curl_init($url);
        curl_setopt_array($ch, [
            CURLOPT_POST => true,
            CURLOPT_RETURNTRANSFER => true,
            CURLOPT_HTTPHEADER => [
                'Content-Type: application/json',
                'Content-Length: ' . strlen($body)
            ],
            CURLOPT_POSTFIELDS => $body,
            CURLOPT_TIMEOUT => 10,
            CURLOPT_CONNECTTIMEOUT => 5,
        ]);

        $responseBody = curl_exec($ch);
        $curlError = curl_error($ch);
        $responseCode = (int)curl_getinfo($ch, CURLINFO_RESPONSE_CODE);
        curl_close($ch);

        $responseBody = is_string($responseBody) ? $responseBody : null;
        if ($curlError !== '') {
            return [
                'success' => false,
                'response_code' => $responseCode > 0 ? $responseCode : null,
                'response_body' => $responseBody,
                'error_code' => $errors['network_code'],
                'error_message' => $errors['network_message_prefix'] . $curlError,
            ];
        }

        if ($responseCode < 200 || $responseCode >= 300) {
            return [
                'success' => false,
                'response_code' => $responseCode,
                'response_body' => $responseBody,
                'error_code' => $errors['rejected_code'],
                'error_message' => $errors['rejected_message'],
            ];
        }

        return [
            'success' => true,
            'response_code' => $responseCode,
            'response_body' => $responseBody,
            'error_code' => null,
            'error_message' => null,
        ];
    }

    private static function postWithStreamContext(string $url, string $body, array $errors): array
    {
        $context = stream_context_create([
            'http' => [
                'method' => 'POST',
                'header' => "Content-Type: application/json\r\nContent-Length: " . strlen($body) . "\r\n",
                'content' => $body,
                'timeout' => 10,
                'ignore_errors' => true,
            ],
        ]);

        $responseBody = @file_get_contents($url, false, $context);
        $responseCode = null;
        if (isset($http_response_header[0]) && preg_match('/\s(\d{3})\s/', $http_response_header[0], $matches)) {
            $responseCode = (int)$matches[1];
        }

        $responseBody = is_string($responseBody) ? $responseBody : null;
        if ($responseBody === null && $responseCode === null) {
            return [
                'success' => false,
                'response_code' => null,
                'response_body' => null,
                'error_code' => $errors['network_code'],
                'error_message' => $errors['network_message'],
            ];
        }

        if ($responseCode === null || $responseCode < 200 || $responseCode >= 300) {
            return [
                'success' => false,
                'response_code' => $responseCode,
                'response_body' => $responseBody,
                'error_code' => $errors['rejected_code'],
                'error_message' => $errors['rejected_message'],
            ];
        }

        return [
            'success' => true,
            'response_code' => $responseCode,
            'response_body' => $responseBody,
            'error_code' => null,
            'error_message' => null,
        ];
    }
}
