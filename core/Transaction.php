<?php
/**
 * Transaction Manager
 * Credit flow: platform task reward, push cost, get cost
 */

require_once __DIR__ . '/Database.php';

class Transaction {

    const AMOUNT_TASK = 1.00;
    const AMOUNT_PUSH = -1.00;
    const AMOUNT_GET = -1.00;

    /**
     * Record a transaction and update balance
     * @return float New balance
     */
    public static function record(int $botId, string $type, float $amount, ?string $refType = null, ?string $refId = null): float {
        $db = Database::getInstance();
        $startedTransaction = !$db->inTransaction();

        try {
            if ($startedTransaction) {
                $db->beginTransaction();
            }

            $bot = $db->fetchOne(
                "SELECT balance FROM tb_bots WHERE id = :id FOR UPDATE",
                [':id' => $botId]
            );

            if (!$bot) {
                throw new Exception("Bot not found");
            }

            $currentBalance = (float)$bot['balance'];
            $newBalance = $currentBalance + $amount;

            if ($newBalance < 0) {
                throw new Exception("Insufficient credits. Need " . abs($amount) . ", have " . $currentBalance, 402);
            }

            $db->exec(
                "UPDATE tb_bots SET balance = :balance WHERE id = :id",
                [':balance' => $newBalance, ':id' => $botId]
            );

            $db->insert('tb_transactions', [
                'bot_id' => $botId,
                'type' => $type,
                'amount' => $amount,
                'balance_after' => $newBalance,
                'ref_type' => $refType,
                'ref_id' => $refId
            ]);

            if ($startedTransaction) {
                $db->commit();
            }

            return $newBalance;

        } catch (Exception $e) {
            if ($startedTransaction && $db->isConnected() && $db->inTransaction()) {
                $db->rollback();
            }
            throw $e;
        }
    }

    /**
     * Get current balance
     */
    public static function getBalance(int $botId): float {
        $db = Database::getInstance();
        $res = $db->fetchOne("SELECT balance FROM tb_bots WHERE id = :id", [':id' => $botId]);
        return $res ? (float)$res['balance'] : 0.00;
    }
}
