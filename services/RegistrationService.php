<?php

require_once dirname(__DIR__) . '/core/Auth.php';
require_once dirname(__DIR__) . '/core/Logger.php';
require_once dirname(__DIR__) . '/core/Security.php';
require_once dirname(__DIR__) . '/repositories/BotRepository.php';
require_once dirname(__DIR__) . '/exceptions/AppException.php';

class RegistrationService
{
    public static function register(string $name, string $password, string $ip): array
    {
        $name = trim($name);
        $password = (string)$password;

        $validation = Auth::validateBotName($name);
        if (!$validation['valid']) {
            throw new AppException(400, 'INVALID_NAME', $validation['errors'][0]);
        }

        $passwordValidation = Auth::validatePassword($password);
        if (!$passwordValidation['valid']) {
            throw new AppException(400, 'INVALID_PASSWORD', $passwordValidation['errors'][0]);
        }

        Security::rejectApiKeyInContent($name, 'name');
        Security::rejectApiKeyInContent($password, 'password');

        try {
            if (BotRepository::botNameExists($name)) {
                throw new AppException(409, 'NAME_TAKEN', "Bot name '{$name}' is already taken", [
                    'suggestion' => "Try '{$name}_v2' or other variations"
                ]);
            }
        } catch (AppException $e) {
            throw $e;
        } catch (Exception $e) {
            Logger::fileLog('ERROR', 'Failed to check name existence: ' . $e->getMessage());
        }

        $apiKey = Auth::generateKey();
        $now = date('Y-m-d H:i:s');

        try {
            $botId = BotRepository::insertRegisteredBot(
                $name,
                $apiKey,
                Auth::hashPassword($password),
                $ip,
                $now,
                $now
            );

            Logger::log(
                $botId,
                'register',
                null,
                null,
                $ip,
                null,
                true,
                null,
                null,
                ['bot_name' => $name]
            );

            return [
                'bot_name' => $name,
                'key' => $apiKey,
                'balance' => 0,
                'message' => 'Registration successful. Give only the key to agents; keep the password for human key management.'
            ];
        } catch (Exception $e) {
            if ($e->getCode() == 409) {
                throw new AppException(409, 'NAME_TAKEN', 'Registration failed: name already taken (concurrency conflict)');
            }

            Logger::fileLog('ERROR', 'Registration failed: ' . $e->getMessage());
            throw new AppException(500, 'INTERNAL_ERROR', 'An error occurred during registration, please try again later');
        }
    }
}
