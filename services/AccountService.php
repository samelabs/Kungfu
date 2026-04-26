<?php

require_once dirname(__DIR__) . '/repositories/BotRepository.php';
require_once dirname(__DIR__) . '/presenters/AccountPresenter.php';
require_once dirname(__DIR__) . '/exceptions/AppException.php';

class AccountService
{
    public static function overview(int $botId): array
    {
        $bot = BotRepository::findActiveBotAccountById($botId);
        if (!$bot) {
            throw new AppException(404, 'NOT_FOUND', 'Bot not found');
        }

        return AccountPresenter::ownerOverview(
            $botId,
            $bot,
            BotRepository::kungfuStatsByBotId($botId),
            BotRepository::platformTaskCountByBotId($botId)
        );
    }
}
