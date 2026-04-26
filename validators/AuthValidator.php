<?php

class AuthValidator
{
    public static function validateKeyFormat(string $key): bool
    {
        if (strlen($key) !== 72) {
            return false;
        }

        if (strpos($key, 'kf_live_') !== 0) {
            return false;
        }

        $randomPart = substr($key, 8);
        return ctype_xdigit($randomPart);
    }

    public static function validatePassword(string $password): array
    {
        $errors = [];
        $len = strlen($password);

        if ($len < 6) {
            $errors[] = 'Password too short (minimum 6 characters)';
        }
        if ($len > 128) {
            $errors[] = 'Password too long (maximum 128 characters)';
        }
        if (preg_match('/kf_live_[a-f0-9]{64}/i', $password)) {
            $errors[] = 'Password must not contain an API key';
        }

        return [
            'valid' => empty($errors),
            'errors' => $errors
        ];
    }

    public static function validateBotName(string $name): array
    {
        $errors = [];
        $len = strlen($name);

        if ($len < 6) {
            $errors[] = 'Name too short (minimum 6 characters)';
        }
        if ($len > 32) {
            $errors[] = 'Name too long (maximum 32 characters)';
        }
        if (!preg_match('/^[a-zA-Z0-9_.-]+$/', $name)) {
            $errors[] = 'Name contains invalid characters (only letters, numbers, _, ., - allowed)';
        }

        $reserved = ['admin', 'root', 'system', 'api', 'web'];
        if (in_array(strtolower($name), $reserved, true)) {
            $errors[] = 'Name is a system reserved word';
        }

        return [
            'valid' => empty($errors),
            'errors' => $errors
        ];
    }
}
