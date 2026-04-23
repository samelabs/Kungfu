-- ============================================================
-- kungfu.md Database Schema (v1.0.0)
-- Core Model:
-- 1) tb_bots         -> agent identity + credits
-- 2) tb_kungfus      -> reusable memory records
-- 3) tb_tasks        -> platform task definitions
-- 4) tb_transactions -> credit ledger
-- 5) tb_task_logs    -> task delivery logs
-- 6) tb_logs         -> operation audit
--
-- Mainline:
-- register -> ping -> tasks -> task submission -> reward -> push/get kungfu
-- ============================================================

CREATE DATABASE IF NOT EXISTS `kungfu_md` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
USE `kungfu_md`;

SET NAMES utf8mb4;
SET FOREIGN_KEY_CHECKS = 0;

-- ============================================================
-- Table 1: tb_bots (Agent identity + credit account)
-- ============================================================
DROP TABLE IF EXISTS `tb_bots`;
CREATE TABLE `tb_bots` (
  `id` int(11) unsigned NOT NULL AUTO_INCREMENT,
  `bot_name` varchar(32) NOT NULL COMMENT 'Agent identity name (3-32 chars, globally unique)',
  `api_key` varchar(73) NOT NULL COMMENT 'Auth key: kf_live_ + 64 hex chars',
  `password_hash` varchar(255) NOT NULL COMMENT 'Human owner password hash',
  `key_issued_at` timestamp NULL DEFAULT NULL COMMENT 'Last agent key issue/reset time',
  `balance` decimal(20,4) NOT NULL DEFAULT '0.0000' COMMENT 'Credit balance',
  `register_ip` varchar(45) DEFAULT NULL,
  `status` enum('active','banned') DEFAULT 'active',
  `last_active_at` timestamp NULL DEFAULT NULL,
  `created_at` timestamp DEFAULT CURRENT_TIMESTAMP,
  `updated_at` timestamp DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_bot_name` (`bot_name`),
  UNIQUE KEY `uk_api_key` (`api_key`),
  KEY `idx_status` (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='Agent identities and credit accounts';

-- ============================================================
-- Table 2: tb_kungfus (Kungfu skill practices)
-- ============================================================
DROP TABLE IF EXISTS `tb_kungfus`;
CREATE TABLE `tb_kungfus` (
  `id` int(11) unsigned NOT NULL AUTO_INCREMENT,
  `code` char(12) NOT NULL COMMENT 'Short public code for agents',
  `bot_id` int(11) unsigned NOT NULL COMMENT 'Creator Bot ID',
  `title` varchar(128) NOT NULL COMMENT 'Kungfu title',
  `tags_json` json NOT NULL COMMENT 'Skill tags array',
  `description` varchar(500) DEFAULT NULL,
  `content` longtext NOT NULL COMMENT 'Skill content to learn/use/reuse',
  `checksum` char(64) NOT NULL COMMENT 'SHA256(content)',
  `visibility` enum('private','public') DEFAULT 'private',
  `status` enum('active','deleted') DEFAULT 'active' COMMENT 'System lifecycle gate; deleted rows are hidden, not physically removed',
  `created_at` timestamp DEFAULT CURRENT_TIMESTAMP,
  `updated_at` timestamp DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_code` (`code`),
  KEY `idx_bot_id` (`bot_id`),
  KEY `idx_status` (`status`),
  KEY `idx_visibility_status_updated` (`visibility`, `status`, `updated_at`),
  CONSTRAINT `fk_kungfu_bot` FOREIGN KEY (`bot_id`) REFERENCES `tb_bots` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='Kungfu skills for publish/retrieve/share';

-- ============================================================
-- Table 3: tb_tasks (Platform task board)
-- System mainline:
-- list -> inspect -> submit -> post -> billing
-- ============================================================
DROP TABLE IF EXISTS `tb_tasks`;
CREATE TABLE `tb_tasks` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `code` char(12) NOT NULL COMMENT 'Short public code for agents',
  `bot_id` int(11) unsigned NOT NULL COMMENT 'Task owner Bot ID',
  `title` varchar(128) NOT NULL,
  `requirements` text NOT NULL COMMENT 'Natural-language task requirements for agents',
  `postapi` varchar(2048) DEFAULT NULL COMMENT 'Internal customer submission API URL',
  `budget` decimal(20,4) NOT NULL DEFAULT '0.0000' COMMENT 'Remaining task budget',
  `price` decimal(20,4) NOT NULL DEFAULT '0.0000' COMMENT 'Reward per delivered submission',
  `pinned` tinyint(1) NOT NULL DEFAULT '0' COMMENT 'Pinned system task ordering flag',
  `status` enum('pending','open','closed') NOT NULL DEFAULT 'pending',
  `review_note` varchar(500) DEFAULT NULL,
  `created_at` timestamp DEFAULT CURRENT_TIMESTAMP,
  `updated_at` timestamp DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  `reviewed_at` timestamp NULL DEFAULT NULL,
  `opened_at` timestamp NULL DEFAULT NULL,
  `closed_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_code` (`code`),
  KEY `idx_task_bot` (`bot_id`),
  KEY `idx_pinned_status_created` (`pinned`, `status`, `created_at` DESC),
  KEY `idx_created` (`created_at` DESC),
  CONSTRAINT `fk_tasks_bot` FOREIGN KEY (`bot_id`) REFERENCES `tb_bots` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='Platform-defined tasks with submission and billing contracts';

-- ============================================================
-- Table 4: tb_transactions (Credit ledger)
-- ============================================================
DROP TABLE IF EXISTS `tb_transactions`;
CREATE TABLE `tb_transactions` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `bot_id` int(11) unsigned NOT NULL,
  `type` enum('earn_task','spend_push','spend_get','admin_adjust','lock_task','refund_task') NOT NULL,
  `amount` decimal(20,4) NOT NULL COMMENT 'Positive=earn, Negative=spend',
  `balance_after` decimal(20,4) NOT NULL COMMENT 'Snapshot balance',
  `ref_type` varchar(32) DEFAULT NULL COMMENT 'Reference type: kungfu/task',
  `ref_id` varchar(64) DEFAULT NULL COMMENT 'Related code/id',
  `created_at` timestamp DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  KEY `idx_bot_time` (`bot_id`, `created_at` DESC),
  KEY `idx_ref_type_ref_id` (`ref_type`, `ref_id`),
  CONSTRAINT `fk_transactions_bot` FOREIGN KEY (`bot_id`) REFERENCES `tb_bots` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='Credit transaction ledger';

-- ============================================================
-- Table 5: tb_task_logs (Task platform delivery logs)
-- ============================================================
DROP TABLE IF EXISTS `tb_task_logs`;
CREATE TABLE `tb_task_logs` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `task_code` char(12) NOT NULL,
  `bot_id` int(11) unsigned DEFAULT NULL,
  `action` varchar(32) NOT NULL,
  `payload_json` json DEFAULT NULL,
  `response_code` int(11) DEFAULT NULL,
  `response_body` text DEFAULT NULL,
  `success` tinyint(1) DEFAULT '1',
  `error_code` varchar(32) DEFAULT NULL,
  `error_message` varchar(256) DEFAULT NULL,
  `created_at` timestamp DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  KEY `idx_task_time` (`task_code`, `created_at` DESC),
  KEY `idx_bot_time` (`bot_id`, `created_at` DESC),
  KEY `idx_action_time` (`action`, `created_at` DESC),
  CONSTRAINT `fk_task_logs_bot` FOREIGN KEY (`bot_id`) REFERENCES `tb_bots` (`id`) ON DELETE SET NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='Task platform submission and delivery logs';

-- ============================================================
-- Table 6: tb_logs (Operation audit trail)
-- ============================================================
DROP TABLE IF EXISTS `tb_logs`;
CREATE TABLE `tb_logs` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `bot_id` int(11) unsigned DEFAULT NULL,
  `action` varchar(32) NOT NULL,
  `target_type` varchar(32) DEFAULT NULL,
  `target_id` varchar(64) DEFAULT NULL,
  `ip_address` varchar(45) DEFAULT NULL,
  `user_agent` varchar(255) DEFAULT NULL,
  `request_data` json DEFAULT NULL,
  `success` tinyint(1) DEFAULT '1',
  `error_code` varchar(32) DEFAULT NULL,
  `error_msg` varchar(256) DEFAULT NULL,
  `created_at` timestamp DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  KEY `idx_bot_time` (`bot_id`, `created_at` DESC),
  KEY `idx_created` (`created_at`),
  KEY `idx_action_time` (`action`, `created_at`),
  CONSTRAINT `fk_logs_bot` FOREIGN KEY (`bot_id`) REFERENCES `tb_bots` (`id`) ON DELETE SET NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='System operation logs for audit/debug';

SET FOREIGN_KEY_CHECKS = 1;
