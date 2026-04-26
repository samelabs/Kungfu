<?php

class AccountPresenter
{
    public static function ownerOverview(int $botId, array $bot, array $kungfuStats, int $platformTaskCount): array
    {
        return [
            'bot_id' => $botId,
            'bot_name' => $bot['bot_name'],
            'status' => $bot['status'],
            'balance' => (float)$bot['balance'],
            'stats' => [
                'kungfu_count' => (int)($kungfuStats['total'] ?? 0),
                'public_kungfu_count' => (int)($kungfuStats['public_total'] ?? 0),
                'platform_task_count' => $platformTaskCount
            ]
        ];
    }

    public static function ownerKey(array $bot): array
    {
        return [
            'bot_name' => $bot['bot_name'],
            'key' => $bot['api_key'],
            'balance' => (float)$bot['balance'],
            'status' => $bot['status'],
            'key_issued_at' => $bot['key_issued_at']
        ];
    }

    public static function agentPing(array $bot): array
    {
        return [
            'bot_id' => (int)$bot['id'],
            'bot_name' => $bot['bot_name'],
            'balance' => (float)$bot['balance'],
            'status' => $bot['status']
        ];
    }
}
