package main

// ---- SQLite 数据库层 ----
// 使用 modernc.org/sqlite 纯 Go 驱动（无 CGO），驱动注册名 "sqlite"。
// 本文件只负责：连接/DSN pragma、全部表 schema、settings 键值对与通用时间助手；
// 各业务表的 CRUD 分散在对应模块（accounts→account_manager.go、tasks→task_worker.go、
// api_keys/sessions→auth.go、统计→api_admin.go）。

import (
	"database/sql"
	"fmt"
	"log"
	"time"

	_ "modernc.org/sqlite"
)

// DB 持有数据库连接
type DB struct {
	conn *sql.DB
}

// nowStr 统一的落库时间格式（RFC3339，本地时区）
func nowStr() string {
	return time.Now().Format(time.RFC3339)
}

// parseTimeOrZero 解析落库时间字符串，失败返回零值（调用方用 IsZero 判断）
func parseTimeOrZero(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// NewDB 打开/创建 SQLite 数据库并初始化 schema
func NewDB(dbPath string) (*DB, error) {
	// 连接级 PRAGMA 通过 DSN 注入：它们是每条连接的属性，用 Exec 设置时，
	// 一旦驱动丢弃坏连接重新拨号，新连接不再带这些设置；DSN 方式对连接池
	// 新建的每条连接都生效。journal_mode=WAL 是文件级持久属性，单独执行一次。
	dsn := dbPath + "?_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)"
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	conn.SetMaxOpenConns(1) // SQLite 单写，避免 SQLITE_BUSY
	if _, err := conn.Exec("PRAGMA journal_mode=WAL"); err != nil {
		conn.Close()
		return nil, fmt.Errorf("exec pragma journal_mode=WAL: %w", err)
	}
	db := &DB{conn: conn}
	if err := db.initSchema(); err != nil {
		conn.Close()
		return nil, err
	}
	return db, nil
}

// Close 关闭数据库连接
func (db *DB) Close() error {
	return db.conn.Close()
}

// initSchema 创建全部表结构（IF NOT EXISTS，幂等）
func (db *DB) initSchema() error {
	schema := `
	-- 账号池：token 明文存储但结构体 json:"-"，绝不返回前端
	CREATE TABLE IF NOT EXISTS accounts (
		id               TEXT PRIMARY KEY,
		label            TEXT DEFAULT '',
		token            TEXT NOT NULL,
		device_id        TEXT NOT NULL UNIQUE,
		uuid             TEXT DEFAULT '',
		os_name          TEXT DEFAULT 'Windows',
		cpu_core_num     INTEGER DEFAULT 8,
		device_memory    INTEGER DEFAULT 16,
		user_id          TEXT DEFAULT '',
		username         TEXT DEFAULT '',
		group_id         TEXT DEFAULT '',
		status           TEXT DEFAULT 'active',
		is_enabled       INTEGER DEFAULT 1,
		balance          REAL DEFAULT -1,
		balance_updated_at TEXT DEFAULT '',
		last_renew_at    TEXT DEFAULT '',
		last_check_at    TEXT DEFAULT '',
		cooldown_until   TEXT DEFAULT '',
		fail_count       INTEGER DEFAULT 0,
		success_count    INTEGER DEFAULT 0,
		total_tasks      INTEGER DEFAULT 0,
		last_error       TEXT DEFAULT '',
		created_at       TEXT DEFAULT (datetime('now')),
		updated_at       TEXT DEFAULT (datetime('now'))
	);
	CREATE INDEX IF NOT EXISTS idx_accounts_status ON accounts(status, is_enabled);

	-- 视频任务（状态机权威）
	CREATE TABLE IF NOT EXISTS video_tasks (
		id                      TEXT PRIMARY KEY,
		idempotency_key         TEXT DEFAULT '',
		api_key_id              TEXT DEFAULT '',
		account_id              TEXT DEFAULT '',
		model                   TEXT DEFAULT '',
		prompt                  TEXT DEFAULT '',
		request_json            TEXT DEFAULT '',
		status                  TEXT DEFAULT 'QUEUED',
		cloud_task_id           TEXT DEFAULT '',
		provider_task_id        TEXT DEFAULT '',
		file_id                 TEXT DEFAULT '',
		video_path              TEXT DEFAULT '',
		video_url               TEXT DEFAULT '',
		error_code              TEXT DEFAULT '',
		error_msg               TEXT DEFAULT '',
		credits_estimated       REAL DEFAULT 0,
		progress                INTEGER DEFAULT 0,
		estimated_remaining_sec INTEGER DEFAULT 0,
		cancel_requested        INTEGER DEFAULT 0,
		duration                INTEGER DEFAULT 0,
		resolution              TEXT DEFAULT '',
		source                  TEXT DEFAULT 'api',
		pinned_account_id       TEXT DEFAULT '',
		media_type              TEXT DEFAULT 'video',
		width                   INTEGER DEFAULT 0,
		height                  INTEGER DEFAULT 0,
		created_at              TEXT DEFAULT '',
		submitted_at            TEXT DEFAULT '',
		started_at              TEXT DEFAULT '',
		completed_at            TEXT DEFAULT '',
		updated_at              TEXT DEFAULT ''
	);
	CREATE INDEX IF NOT EXISTS idx_tasks_status ON video_tasks(status);
	CREATE INDEX IF NOT EXISTS idx_tasks_apikey ON video_tasks(api_key_id, created_at DESC);
	CREATE INDEX IF NOT EXISTS idx_tasks_account ON video_tasks(account_id);
	CREATE INDEX IF NOT EXISTS idx_tasks_idem ON video_tasks(api_key_id, idempotency_key);

	-- 任务事件时间线（append-only）
	CREATE TABLE IF NOT EXISTS task_events (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		task_id     TEXT NOT NULL,
		ts          TEXT NOT NULL,
		from_status TEXT DEFAULT '',
		to_status   TEXT DEFAULT '',
		message     TEXT DEFAULT ''
	);
	CREATE INDEX IF NOT EXISTS idx_events_task ON task_events(task_id, id);

	-- 下游 API Key（sha256 存储，明文只在创建时返回一次）
	CREATE TABLE IF NOT EXISTS api_keys (
		id           TEXT PRIMARY KEY,
		name         TEXT DEFAULT '',
		key_hash     TEXT NOT NULL UNIQUE,
		key_prefix   TEXT NOT NULL,
		is_enabled   INTEGER DEFAULT 1,
		max_tasks    INTEGER DEFAULT 0,
		used_tasks   INTEGER DEFAULT 0,
		expires_at   TEXT DEFAULT '',
		last_used_at TEXT DEFAULT '',
		created_at   TEXT DEFAULT ''
	);
	CREATE INDEX IF NOT EXISTS idx_keys_prefix ON api_keys(key_prefix);

	-- 用量明细（任务终态时写入）
	CREATE TABLE IF NOT EXISTS usage_logs (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		ts          TEXT NOT NULL,
		api_key_id  TEXT DEFAULT '',
		task_id     TEXT DEFAULT '',
		account_id  TEXT DEFAULT '',
		model       TEXT DEFAULT '',
		credits     REAL DEFAULT 0,
		status      TEXT DEFAULT '',
		duration_ms INTEGER DEFAULT 0
	);
	CREATE INDEX IF NOT EXISTS idx_usage_ts ON usage_logs(ts);

	-- 余额快照（时间序列）
	CREATE TABLE IF NOT EXISTS credit_snapshots (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		account_id TEXT NOT NULL,
		ts         TEXT NOT NULL,
		balance    REAL DEFAULT 0
	);
	CREATE INDEX IF NOT EXISTS idx_snap_account ON credit_snapshots(account_id, ts DESC);

	-- Web 管理会话（token 只存 sha256）
	CREATE TABLE IF NOT EXISTS sessions (
		token_hash TEXT PRIMARY KEY,
		username   TEXT DEFAULT '',
		created_at TEXT DEFAULT '',
		expires_at TEXT DEFAULT ''
	);

	-- 运行期键值设置
	CREATE TABLE IF NOT EXISTS settings (
		key   TEXT PRIMARY KEY,
		value TEXT DEFAULT ''
	);
	`
	if _, err := db.conn.Exec(schema); err != nil {
		return fmt.Errorf("init schema: %w", err)
	}
	if err := db.migrate(); err != nil {
		return err
	}
	return nil
}

// migrate 防御式增量迁移：为旧版本创建的库补齐新增列
// （CREATE TABLE IF NOT EXISTS 不会给已存在的表加列，逐列 PRAGMA 检查后
// ALTER TABLE ADD COLUMN，幂等可重复执行；参考 dumate-proxy 的 migrate 写法）。
func (db *DB) migrate() error {
	// video_tasks：source（任务来源 api/admin_test）与 pinned_account_id（钉住的测试账号）
	existing, err := db.tableColumns("video_tasks")
	if err != nil {
		return err
	}
	if !existing["source"] {
		if _, err := db.conn.Exec(`ALTER TABLE video_tasks ADD COLUMN source TEXT DEFAULT 'api'`); err != nil {
			return fmt.Errorf("add column video_tasks.source: %w", err)
		}
		log.Printf("[db] migrated: added source column to video_tasks")
	}
	if !existing["pinned_account_id"] {
		if _, err := db.conn.Exec(`ALTER TABLE video_tasks ADD COLUMN pinned_account_id TEXT DEFAULT ''`); err != nil {
			return fmt.Errorf("add column video_tasks.pinned_account_id: %w", err)
		}
		log.Printf("[db] migrated: added pinned_account_id column to video_tasks")
	}
	// video_tasks：media_type（video/image）与图片尺寸（width/height）
	if !existing["media_type"] {
		if _, err := db.conn.Exec(`ALTER TABLE video_tasks ADD COLUMN media_type TEXT DEFAULT 'video'`); err != nil {
			return fmt.Errorf("add column video_tasks.media_type: %w", err)
		}
		log.Printf("[db] migrated: added media_type column to video_tasks")
	}
	if !existing["width"] {
		if _, err := db.conn.Exec(`ALTER TABLE video_tasks ADD COLUMN width INTEGER DEFAULT 0`); err != nil {
			return fmt.Errorf("add column video_tasks.width: %w", err)
		}
		log.Printf("[db] migrated: added width column to video_tasks")
	}
	if !existing["height"] {
		if _, err := db.conn.Exec(`ALTER TABLE video_tasks ADD COLUMN height INTEGER DEFAULT 0`); err != nil {
			return fmt.Errorf("add column video_tasks.height: %w", err)
		}
		log.Printf("[db] migrated: added height column to video_tasks")
	}
	return nil
}

// tableColumns 返回表的列名集合（PRAGMA table_info）
func (db *DB) tableColumns(table string) (map[string]bool, error) {
	rows, err := db.conn.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return nil, fmt.Errorf("pragma table_info(%s): %w", table, err)
	}
	defer rows.Close()
	cols := make(map[string]bool)
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return nil, fmt.Errorf("scan table_info(%s): %w", table, err)
		}
		cols[name] = true
	}
	return cols, rows.Err()
}

// ---- settings 键值对 ----

// GetSetting 读取设置项；ok=false 表示不存在
func (db *DB) GetSetting(key string) (string, bool) {
	var v string
	err := db.conn.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if err != nil {
		return "", false
	}
	return v, true
}

// SetSetting 写入设置项（upsert）
func (db *DB) SetSetting(key, value string) error {
	_, err := db.conn.Exec(`
		INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value
	`, key, value)
	if err != nil {
		return fmt.Errorf("set setting %s: %w", key, err)
	}
	return nil
}
