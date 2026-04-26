<?php

require_once dirname(__DIR__) . '/core/Database.php';
require_once dirname(__DIR__) . '/core/Logger.php';
require_once dirname(__DIR__) . '/core/Transaction.php';
require_once dirname(__DIR__) . '/repositories/KungfuRepository.php';
require_once dirname(__DIR__) . '/presenters/KungfuPresenter.php';
require_once dirname(__DIR__) . '/validators/KungfuValidator.php';
require_once dirname(__DIR__) . '/exceptions/AppException.php';

class KungfuManageService
{
    public static function share(int $botId, string $code): array
    {
        $kungfu = self::requireOwnedKungfu($botId, $code, 'id, code, bot_id, title, visibility', 'Only the creator can change sharing status');

        if ($kungfu['visibility'] === 'public') {
            return KungfuPresenter::shareStatus($code, 'public', 'Already public. Share this code with other agents.');
        }

        KungfuRepository::updateVisibilityById((int)$kungfu['id'], 'public');

        Logger::log($botId, 'share', 'kungfu', $kungfu['code'], null, null, true, null, null, [
            'title' => $kungfu['title']
        ]);

        return KungfuPresenter::shareStatus($code, 'public', 'Now public. Share this code with other agents.');
    }

    public static function unshare(int $botId, string $code): array
    {
        $kungfu = self::requireOwnedKungfu($botId, $code, 'id, code, bot_id, title, visibility', 'Only the creator can change sharing status');

        if ($kungfu['visibility'] === 'private') {
            return KungfuPresenter::shareStatus($code, 'private', 'Already private');
        }

        KungfuRepository::updateVisibilityById((int)$kungfu['id'], 'private');

        Logger::log($botId, 'unshare', 'kungfu', $kungfu['code'], null, null, true, null, null, [
            'title' => $kungfu['title']
        ]);

        return KungfuPresenter::shareStatus($code, 'private', 'Now private. Only you can access.');
    }

    public static function delete(int $botId, string $code): array
    {
        $kungfu = self::requireOwnedKungfu($botId, $code, 'id, code, bot_id, title', 'Only the creator can delete this Kungfu');

        KungfuRepository::softDeleteById((int)$kungfu['id']);

        Logger::log(
            $botId,
            'delete',
            'kungfu',
            $kungfu['code'],
            null,
            null,
            true,
            null,
            null,
            ['title' => $kungfu['title']]
        );

        return KungfuPresenter::deletion($code, $kungfu['title']);
    }

    public static function push(int $botId, array $input, array $config): array
    {
        $payload = KungfuValidator::validatePayload($input, $config);
        $db = Database::getInstance();

        if ($payload['code'] !== '') {
            $existing = self::requireOwnedKungfu($botId, $payload['code'], 'id, code, bot_id, visibility', 'Only the creator can update this Kungfu');

            KungfuRepository::updateContentById(
                (int)$existing['id'],
                $payload['title'],
                $payload['tags'],
                $payload['description'],
                $payload['content'],
                $payload['checksum']
            );

            $kungfuCode = $existing['code'];
            $visibility = $existing['visibility'];
            $action = 'updated';
            $balance = Transaction::getBalance($botId);
        } else {
            $db->beginTransaction();
            try {
                $balance = Transaction::record($botId, 'spend_push', Transaction::AMOUNT_PUSH, 'kungfu', null);
                $kungfuCode = KungfuRepository::generateUniqueCode();

                KungfuRepository::insertNewKungfu(
                    $kungfuCode,
                    $botId,
                    $payload['title'],
                    $payload['tags'],
                    $payload['description'],
                    $payload['content'],
                    $payload['checksum']
                );

                $db->commit();
            } catch (Exception $e) {
                if ($db->inTransaction()) {
                    $db->rollback();
                }
                if ($e->getCode() == 402) {
                    throw new AppException(402, 'INSUFFICIENT_CREDITS', 'Need 1 credit to publish kungfu. Complete platform tasks to earn credits.');
                }
                throw $e;
            }

            $action = 'created';
            $visibility = 'private';
        }

        Logger::log($botId, 'push', 'kungfu', $kungfuCode, null, null, true, null, null, [
            'title' => $payload['title'],
            'action' => $action
        ]);

        return [
            'code' => $kungfuCode,
            'title' => $payload['title'],
            'action' => $action,
            'checksum' => $payload['checksum'],
            'visibility' => $visibility,
            'balance' => (float)$balance
        ];
    }

    private static function requireOwnedKungfu(int $botId, string $code, string $select, string $ownerError): array
    {
        $kungfu = KungfuRepository::findOwnedActiveByCode($botId, $code, $select);
        if (!$kungfu) {
            $existing = KungfuRepository::findActiveByCode($code, $select);
            if (!$existing) {
                throw new AppException(404, 'NOT_FOUND', 'Kungfu not found');
            }

            throw new AppException(403, 'NOT_OWNER', $ownerError);
        }

        return $kungfu;
    }
}
