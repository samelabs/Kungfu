<?php
/**
 * Security helpers for preventing API keys from leaking into business content.
 */

class Security {
    private const API_KEY_PATTERN = '/kf_live_[a-f0-9]{64}/i';

    public static function containsApiKey($value): bool {
        if (is_array($value)) {
            foreach ($value as $item) {
                if (self::containsApiKey($item)) {
                    return true;
                }
            }
            return false;
        }

        if (!is_scalar($value)) {
            return false;
        }

        return preg_match(self::API_KEY_PATTERN, (string)$value) === 1;
    }

    public static function rejectApiKeyInContent($value, string $field = 'content'): void {
        if (self::containsApiKey($value)) {
            Response::error(400, 'SENSITIVE_CONTENT', "{$field} must not contain API keys");
        }
    }

    public static function redactSecrets($value) {
        if (is_array($value)) {
            $redacted = [];
            foreach ($value as $key => $item) {
                $redacted[$key] = self::redactSecrets($item);
            }
            return $redacted;
        }

        if (is_string($value)) {
            return preg_replace_callback(self::API_KEY_PATTERN, function ($matches) {
                return self::maskKey($matches[0]);
            }, $value);
        }

        return $value;
    }

    public static function maskKey(string $key): string {
        if (strlen($key) <= 12) {
            return str_repeat('*', strlen($key));
        }

        return substr($key, 0, 8) . '****' . substr($key, -4);
    }
}
