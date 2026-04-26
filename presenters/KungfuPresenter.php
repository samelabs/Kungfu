<?php

class KungfuPresenter
{
    public static function listItem(array $row): array
    {
        return [
            'code' => $row['code'],
            'title' => $row['title'],
            'tags' => json_decode((string)$row['tags_json'], true) ?: [],
            'description' => $row['description'],
            'visibility' => $row['visibility'],
            'created_at' => $row['created_at'],
            'updated_at' => $row['updated_at']
        ];
    }

    public static function listResponse(array $items, float $balance, int $offset, int $total): array
    {
        return [
            'kungfus' => $items,
            'balance' => $balance,
            'meta' => [
                'total' => $total,
                'returned' => count($items),
                'offset' => $offset,
                'has_more' => ($offset + count($items)) < $total
            ]
        ];
    }

    public static function detail(array $kungfu, float $balance): array
    {
        return [
            'code' => $kungfu['code'],
            'title' => $kungfu['title'],
            'tags' => json_decode((string)$kungfu['tags_json'], true) ?: [],
            'description' => $kungfu['description'],
            'content' => $kungfu['content'],
            'checksum' => $kungfu['checksum'],
            'visibility' => $kungfu['visibility'],
            'created_at' => $kungfu['created_at'],
            'updated_at' => $kungfu['updated_at'],
            'balance' => $balance
        ];
    }

    public static function shareStatus(string $code, string $visibility, string $message): array
    {
        return [
            'code' => $code,
            'visibility' => $visibility,
            'message' => $message
        ];
    }

    public static function deletion(string $code, string $title): array
    {
        return [
            'code' => $code,
            'title' => $title,
            'message' => 'Deleted (bots that already acquired can still use normally)'
        ];
    }
}
