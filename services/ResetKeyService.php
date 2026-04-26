<?php

require_once dirname(__DIR__) . '/core/Auth.php';
require_once dirname(__DIR__) . '/core/RateLimiter.php';
require_once dirname(__DIR__) . '/core/Logger.php';
require_once dirname(__DIR__) . '/repositories/BotRepository.php';
require_once dirname(__DIR__) . '/presenters/IdentityPresenter.php';
require_once dirname(__DIR__) . '/exceptions/AppException.php';
require_once dirname(__DIR__) . '/exceptions/RateLimitException.php';

class ResetKeyService
{
    public static function reset(int $botId, string $currentKey): array
    {
        $bot = BotRepository::findActiveBotKeyById($botId);
        if (!$bot) {
            throw new AppException(401, 'OWNER_LOGIN_REQUIRED', 'Owner login required');
        }

        $currentKey = trim($currentKey);
        if ($currentKey === '') {
            throw new AppException(400, 'MISSING_FIELD', 'Missing required field: current_key');
        }

        if (!preg_match('/^kf_live_[a-f0-9]{64}$/i', $currentKey)) {
            throw new AppException(400, 'INVALID_KEY', 'Current key format is invalid');
        }

        if (!hash_equals((string)$bot['api_key'], $currentKey)) {
            throw new AppException(401, 'INVALID_KEY', 'Current key is incorrect');
        }

        if (!RateLimiter::checkApi($botId, 'reset_key')) {
            $details = RateLimiter::checkApiWithDetails($botId, 'reset_key');
            throw new RateLimitException((int)$details['retry_after'], (int)$details['limit'], (int)$details['window']);
        }

        $newKey = Auth::generateKey();
        BotRepository::updateApiKeyById($botId, $newKey);

        Logger::log(
            $botId,
            'reset_key',
            null,
            null,
            null,
            null,
            true,
            null,
            null,
            ['new_key_masked' => Logger::maskKey($newKey)]
        );

        return IdentityPresenter::keyReset($bot['bot_name'], $newKey);
    }
}
