<?php
/**
 * Database - PDO Database Wrapper
 * Kungfu Platform Core Component
 */

class Database {
    private static $instance = null;
    private $pdo;
    private $config;
    
    private function __construct() {
        $this->config = require __DIR__ . '/../config/config.php';
        $this->connect();
    }
    
    public static function getInstance(): self {
        if (self::$instance === null) {
            self::$instance = new self();
        }
        return self::$instance;
    }
    
    private function connect(): void {
        try {
            $dsn = "mysql:host={$this->config['db_host']};" .
                   "dbname={$this->config['db_name']};" .
                   "charset={$this->config['db_charset']}";
            
            $this->pdo = new PDO($dsn, $this->config['db_user'], $this->config['db_pass'], [
                PDO::ATTR_ERRMODE => PDO::ERRMODE_EXCEPTION,
                PDO::ATTR_DEFAULT_FETCH_MODE => PDO::FETCH_ASSOC,
                PDO::ATTR_EMULATE_PREPARES => false,
            ]);
        } catch (PDOException $e) {
            throw new Exception("Database connection failed: " . $e->getMessage());
        }
    }
    
    /**
     * Execute query (SELECT)
     */
    public function query(string $sql, array $params = []): array {
        try {
            $stmt = $this->pdo->prepare($sql);
            $stmt->execute($params);
            return $stmt->fetchAll();
        } catch (PDOException $e) {
            throw new Exception("Query failed: " . $e->getMessage());
        }
    }
    
    /**
     * Get single record
     */
    public function fetchOne(string $sql, array $params = []): ?array {
        try {
            $stmt = $this->pdo->prepare($sql);
            $stmt->execute($params);
            $result = $stmt->fetch();
            return $result ?: null;
        } catch (PDOException $e) {
            throw new Exception("Query failed: " . $e->getMessage());
        }
    }
    
    /**
     * Insert data
     */
    public function insert(string $table, array $data): int {
        try {
            $columns = implode(', ', array_keys($data));
            $placeholders = ':' . implode(', :', array_keys($data));
            $sql = "INSERT INTO {$table} ({$columns}) VALUES ({$placeholders})";
            
            $stmt = $this->pdo->prepare($sql);
            $stmt->execute($data);
            
            return (int) $this->pdo->lastInsertId();
        } catch (PDOException $e) {
            if ($e->getCode() == 23000) {
                throw new Exception("Data already exists", 409);
            }
            throw new Exception("Insert failed: " . $e->getMessage());
        }
    }
    
    /**
     * Update data
     */
    public function update(string $table, array $data, string $where, array $whereParams = []): int {
        try {
            $sets = [];
            foreach ($data as $key => $value) {
                $sets[] = "{$key} = :{$key}";
            }
            $setStr = implode(', ', $sets);
            
            $sql = "UPDATE {$table} SET {$setStr} WHERE {$where}";
            
            $stmt = $this->pdo->prepare($sql);
            $stmt->execute(array_merge($data, $whereParams));
            
            return $stmt->rowCount();
        } catch (PDOException $e) {
            throw new Exception("Update failed: " . $e->getMessage());
        }
    }
    
    /**
     * Execute raw SQL
     */
    public function exec(string $sql, array $params = []): int {
        try {
            $stmt = $this->pdo->prepare($sql);
            $stmt->execute($params);
            return $stmt->rowCount();
        } catch (PDOException $e) {
            throw new Exception("Execution failed: " . $e->getMessage());
        }
    }
    
    /**
     * Begin transaction
     */
    public function beginTransaction(): void {
        $this->pdo->beginTransaction();
    }
    
    /**
     * Commit transaction
     */
    public function commit(): void {
        $this->pdo->commit();
    }
    
    /**
     * Rollback transaction
     */
    public function rollback(): void {
        $this->pdo->rollback();
    }

    /**
     * Check if a transaction is currently active.
     */
    public function inTransaction(): bool {
        return $this->pdo->inTransaction();
    }
    
    /**
     * Check if connected
     */
    public function isConnected(): bool {
        try {
            $this->pdo->query('SELECT 1');
            return true;
        } catch (PDOException $e) {
            return false;
        }
    }
}
