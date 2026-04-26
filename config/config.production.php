<?php
/**
 * Kungfu Platform - Production Configuration
 * kungfu.md Production Environment
 */

return [
    // Database configuration (based on server info)
    'db_host' => getenv('DB_HOST') ?: 'localhost',
    'db_name' => getenv('DB_NAME') ?: 'kungfu_md',
    'db_user' => getenv('DB_USER') ?: 'kungfu_md',
    'db_pass' => getenv('DB_PASS') ?: '',
    'db_charset' => 'utf8mb4',
    
    // System configuration
    'api_version' => '1.0.0',
    'key_prefix' => 'kf_live_',
    'debug_mode' => false,  // Disable debug in production
    
    // Content limits
    'max_content_size' => 102400,
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
    'log_level' => 'INFO',
    
    // Rate limiting configuration
    'rate_limits' => [
        'register' => ['window' => 3600, 'limit' => 5],
        'owner_login' => ['window' => 900, 'limit' => 20],
        'list' => ['window' => 60, 'limit' => 120],
        'get' => ['window' => 60, 'limit' => 300],
        'push' => ['window' => 3600, 'limit' => 60],
        'task_submit' => ['window' => 60, 'limit' => 120],
        'reset_key' => ['window' => 86400, 'limit' => 50],
    ],
];
