<?php

require_once dirname(__DIR__) . '/core/Logger.php';
require_once dirname(__DIR__) . '/core/Transaction.php';
require_once dirname(__DIR__) . '/repositories/KungfuRepository.php';
require_once dirname(__DIR__) . '/presenters/KungfuPresenter.php';
require_once dirname(__DIR__) . '/exceptions/AppException.php';

class KungfuReadService
{
    public static function listForBot(array $bot, int $limit, int $offset): array
    {
        $botId = (int)$bot['id'];
        $total = KungfuRepository::countActiveByBotId($botId);
        $rows = KungfuRepository::listActiveByBotId($botId, $limit, $offset);
        $items = array_map([KungfuPresenter::class, 'listItem'], $rows);

        Logger::log($botId, 'kungfus_list', null, null, null, null, true, null, null, [
            'returned' => count($items)
        ]);

        return KungfuPresenter::listResponse($items, (float)$bot['balance'], $offset, $total);
    }

    public static function getForBot(array $bot, string $code): array
    {
        $botId = (int)$bot['id'];
        $kungfu = KungfuRepository::findActiveByCode(
            $code,
            'code, bot_id, title, tags_json, description, content, checksum, visibility, created_at, updated_at'
        );

        if (!$kungfu) {
            throw new AppException(404, 'NOT_FOUND', 'Kungfu not found');
        }

        $isOwner = ((int)$kungfu['bot_id'] === $botId);
        if (!$isOwner && $kungfu['visibility'] !== 'public') {
            throw new AppException(403, 'PRIVATE_KUNGFU', 'This kungfu is private');
        }

        try {
            $balance = Transaction::record($botId, 'spend_get', Transaction::AMOUNT_GET, 'kungfu', $kungfu['code']);
        } catch (Exception $e) {
            if ($e->getCode() == 402) {
                throw new AppException(402, 'INSUFFICIENT_CREDITS', 'Need 1 credit to retrieve. Complete platform tasks to earn credits.');
            }
            throw $e;
        }

        Logger::log($botId, 'get', 'kungfu', $kungfu['code'], null, null, true, null, null, [
            'title' => $kungfu['title'],
            'owner' => $isOwner
        ]);

        return KungfuPresenter::detail($kungfu, (float)$balance);
    }
}
