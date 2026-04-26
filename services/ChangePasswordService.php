<?php

require_once dirname(__DIR__) . '/core/Auth.php';
require_once dirname(__DIR__) . '/core/Security.php';
require_once dirname(__DIR__) . '/core/Logger.php';
require_once dirname(__DIR__) . '/repositories/BotRepository.php';
require_once dirname(__DIR__) . '/presenters/IdentityPresenter.php';
require_once dirname(__DIR__) . '/exceptions/AppException.php';

class ChangePasswordService
{
    public static function change(string $name, string $password, string $newPassword): array
    {
        $name = trim($name);
        $password = (string)$password;
        $newPassword = (string)$newPassword;

        $nameValidation = Auth::validateBotName($name);
        if (!$nameValidation['valid']) {
            throw new AppException(400, 'INVALID_NAME', $nameValidation['errors'][0]);
        }

        $passwordValidation = Auth::validatePassword($password);
        if (!$passwordValidation['valid']) {
            throw new AppException(400, 'INVALID_PASSWORD', $passwordValidation['errors'][0]);
        }

        Security::rejectApiKeyInContent([$name, $password, $newPassword], 'human credentials');

        $validation = Auth::validatePassword($newPassword);
        if (!$validation['valid']) {
            throw new AppException(400, 'INVALID_PASSWORD', $validation['errors'][0]);
        }

        if (hash_equals($password, $newPassword)) {
            throw new AppException(400, 'PASSWORD_UNCHANGED', 'New password must be different from current password');
        }

        $bot = BotRepository::findActiveBotCredentialsByName($name);
        if (!$bot || empty($bot['password_hash']) || !Auth::verifyPassword($password, $bot['password_hash'])) {
            throw new AppException(401, 'INVALID_CREDENTIALS', 'Bot name or password is incorrect');
        }

        BotRepository::updatePasswordHashById((int)$bot['id'], Auth::hashPassword($newPassword));

        Logger::log((int)$bot['id'], 'change_password', null, null, null, null, true, null, null, [
            'bot_name' => $bot['bot_name']
        ]);

        return IdentityPresenter::passwordChanged($bot['bot_name']);
    }
}
