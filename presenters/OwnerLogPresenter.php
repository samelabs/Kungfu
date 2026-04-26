<?php

require_once __DIR__ . '/TaskLogPresenter.php';

class OwnerLogPresenter
{
    public static function pagination(int $page, int $pageSize, int $total): array
    {
        return [
            'page' => $page,
            'page_size' => $pageSize,
            'total' => $total,
            'total_pages' => $total > 0 ? (int)ceil($total / $pageSize) : 1
        ];
    }

    public static function creditLogRow(array $row): array
    {
        return [
            'id' => (int)$row['id'],
            'type' => $row['type'],
            'amount' => (float)$row['amount'],
            'balance_after' => (float)$row['balance_after'],
            'ref_type' => $row['ref_type'],
            'ref_id' => $row['ref_id'],
            'created_at' => $row['created_at']
        ];
    }

    public static function agentLogRow(array $row): array
    {
        $requestData = null;
        if (!empty($row['request_data'])) {
            $decoded = json_decode((string)$row['request_data'], true);
            if (is_array($decoded)) {
                $requestData = $decoded;
            }
        }

        return [
            'id' => (int)$row['id'],
            'action' => $row['action'],
            'target_type' => $row['target_type'],
            'target_id' => $row['target_id'],
            'ip_address' => $row['ip_address'],
            'user_agent' => $row['user_agent'],
            'request_data' => $requestData,
            'success' => (bool)$row['success'],
            'error_code' => $row['error_code'],
            'error_msg' => $row['error_msg'],
            'created_at' => $row['created_at']
        ];
    }

    public static function taskFilterItem(array $task): array
    {
        return [
            'code' => $task['code'],
            'title' => $task['title']
        ];
    }

    public static function taskLogRow(array $row): array
    {
        $payload = null;
        if (!empty($row['payload_json'])) {
            $decoded = json_decode((string)$row['payload_json'], true);
            if (is_array($decoded)) {
                $payload = $decoded;
            }
        }

        return [
            'id' => (int)$row['id'],
            'task_code' => $row['task_code'],
            'bot_id' => $row['bot_id'] !== null ? (int)$row['bot_id'] : null,
            'action' => TaskLogPresenter::actionLabel((string)$row['action']),
            'payload_json' => $payload,
            'response_code' => $row['response_code'] !== null ? (int)$row['response_code'] : null,
            'response_body' => $row['response_body'],
            'success' => (bool)$row['success'],
            'error_code' => $row['error_code'],
            'error_message' => $row['error_message'],
            'created_at' => $row['created_at']
        ];
    }
}
