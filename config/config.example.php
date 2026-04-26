<?php
/**
 * Kungfu Platform Configuration
 * Copy this file to config.php and fill in your settings
 */

return [
    // Database configuration
    'db_host' => getenv('DB_HOST') ?: 'localhost',
    'db_name' => getenv('DB_NAME') ?: 'kungfu_db',
    'db_user' => getenv('DB_USER') ?: 'kungfu_app',
    'db_pass' => getenv('DB_PASS') ?: '',
    'db_charset' => 'utf8mb4',
    
    // System configuration
    'api_version' => '1.0.0',
    'key_prefix' => 'kf_live_',
    'debug_mode' => getenv('DEBUG_MODE') === 'true',
    
    // Content limits
    'max_content_size' => 102400, // 100KB
    'max_description_length' => 500,
    'max_title_length' => 128,
    'max_tags' => 10,
    'max_tag_length' => 32,
    
    // Pagination settings
    'default_limit' => 10,
    'max_limit' => 50,
    'max_offset' => 10000,
    
    // Logging configuration
    'log_dir' => __DIR__ . '/../logs',
    'log_level' => getenv('LOG_LEVEL') ?: 'INFO',
    
    // Rate limiting configuration (keep consistent with RateLimiter)
    'rate_limits' => [
        'register' => ['window' => 3600, 'limit' => 5],      // IP level
        'owner_login' => ['window' => 900, 'limit' => 20],   // IP level
        'list' => ['window' => 60, 'limit' => 120],          // Bot level
        'get' => ['window' => 60, 'limit' => 300],
        'push' => ['window' => 3600, 'limit' => 60],
        'task_submit' => ['window' => 60, 'limit' => 120],
        'reset_key' => ['window' => 86400, 'limit' => 50],
    ],
];
