package main

// ---- 数据库防御式迁移回归测试 ----
// 场景：旧版本创建的库（video_tasks 无 source / pinned_account_id 列）用新
// 二进制打开时，migrate() 必须幂等补列，且旧行读出时 source 兜底为 'api'。

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// oldVideoTasksSchema 升级前的 video_tasks 建表语句（26 列，无 source/pinned_account_id）
const oldVideoTasksSchema = `
CREATE TABLE video_tasks (
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
	created_at              TEXT DEFAULT '',
	submitted_at            TEXT DEFAULT '',
	started_at              TEXT DEFAULT '',
	completed_at            TEXT DEFAULT '',
	updated_at              TEXT DEFAULT ''
);`

func TestMigrateAddsTaskColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")

	// 1) 构造旧版 schema 库并插入一行旧任务
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open old db: %v", err)
	}
	if _, err := old.Exec(oldVideoTasksSchema); err != nil {
		t.Fatalf("create old schema: %v", err)
	}
	if _, err := old.Exec(`INSERT INTO video_tasks (id, model, status, created_at) VALUES ('task_old1','MiniMax-H3','SUCCEEDED','2026-01-01T00:00:00+08:00')`); err != nil {
		t.Fatalf("insert old row: %v", err)
	}
	if err := old.Close(); err != nil {
		t.Fatalf("close old db: %v", err)
	}

	// 2) NewDB 触发 migrate：幂等补列
	db, err := NewDB(path)
	if err != nil {
		t.Fatalf("NewDB on old schema: %v", err)
	}
	defer db.Close()
	cols, err := db.tableColumns("video_tasks")
	if err != nil {
		t.Fatalf("tableColumns: %v", err)
	}
	if !cols["source"] || !cols["pinned_account_id"] {
		t.Fatalf("迁移未补列: source=%v pinned_account_id=%v", cols["source"], cols["pinned_account_id"])
	}
	if !cols["media_type"] || !cols["width"] || !cols["height"] {
		t.Fatalf("迁移未补图片列: media_type=%v width=%v height=%v",
			cols["media_type"], cols["width"], cols["height"])
	}

	// 3) 再开一次验证幂等（重复 migrate 不报错）
	db2, err := NewDB(path)
	if err != nil {
		t.Fatalf("NewDB twice (idempotent migrate): %v", err)
	}
	db2.Close()

	// 4) 旧行经 scanTask 读出：source 兜底 'api'，media_type 兜底 'video'
	row := db.conn.QueryRow(`SELECT ` + taskCols + ` FROM video_tasks WHERE id='task_old1'`)
	tk, err := scanTask(row)
	if err != nil {
		t.Fatalf("scan old row: %v", err)
	}
	if tk.Source != TaskSourceAPI && tk.Source != "" {
		t.Fatalf("旧行 source 异常: %q", tk.Source)
	}
	if tk.MediaType != TaskMediaVideo {
		t.Fatalf("旧行 media_type 应为 video: %q", tk.MediaType)
	}
	if tk.PinnedAccountID != "" {
		t.Fatalf("旧行 pinned_account_id 应为空: %q", tk.PinnedAccountID)
	}

	// 5) 新插入任务默认 source=api（InsertTask 兜底）
	tm := &TaskManager{db: db}
	nt := &VideoTask{ID: "task_new1", Model: ModelH3, Status: TaskQueued, CreatedAt: nowStr(), UpdatedAt: nowStr()}
	if err := tm.InsertTask(nt); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}
	var src, pinned string
	if err := db.conn.QueryRow(`SELECT source, pinned_account_id FROM video_tasks WHERE id='task_new1'`).Scan(&src, &pinned); err != nil {
		t.Fatalf("query new row: %v", err)
	}
	if src != TaskSourceAPI {
		t.Fatalf("新任务 source 默认应为 api: %q", src)
	}
	if pinned != "" {
		t.Fatalf("新任务 pinned_account_id 应为空: %q", pinned)
	}
}
