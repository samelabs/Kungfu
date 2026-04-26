<?php

require_once dirname(__DIR__) . '/repositories/BotRepository.php';
require_once dirname(__DIR__) . '/presenters/AccountPresenter.php';
require_once dirname(__DIR__) . '/exceptions/AppException.php';
require_once dirname(__DIR__) . '/core/Logger.php';

class KeyService
{
    public static function currentOwnerKey(int $botId): array
    {
        $bot = BotRepository::findActiveBotKeyById($botId);
        if (!$bot) {
            throw new AppException(401, 'OWNER_LOGIN_REQUIRED', 'Owner login required');
        }

        Logger::log((int)$bot['id'], 'key_get', null, null, null, null, true, null, null, [
            'bot_name' => $bot['bot_name'],
            'source' => 'owner_session'
        ]);

        return AccountPresenter::ownerKey($bot);
    }
}
