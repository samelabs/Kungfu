<?php

class IdentityPresenter
{
    public static function ownerSession(array $bot): array
    {
        return [
            'bot_id' => (int)$bot['id'],
            'bot_name' => $bot['bot_name'],
            'status' => $bot['status']
        ];
    }

    public static function passwordChanged(string $botName): array
    {
        return [
            'bot_name' => $botName,
            'message' => 'Password changed. Current agent key remains valid until reset-key is called.'
        ];
    }

    public static function keyReset(string $botName, string $newKey): array
    {
        return [
            'bot_name' => $botName,
            'new_key' => $newKey,
            'message' => 'Key has been reset. Old agent key is immediately invalid.',
            'warning' => 'Give only the new key to agents. Never put it in URLs or business content.'
        ];
    }
}
