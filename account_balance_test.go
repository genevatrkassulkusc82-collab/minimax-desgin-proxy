package main

// ---- UpdateBalance 持久化回归测试 ----
// 背景：balance SQL 有 5 个占位符但旧代码只传 4 个参数（missing argument with
// index 5），此前因 GetBalance 解析必败而被掩盖；flexFloat 修复后暴露。
// 本测试锁定参数顺序与 empty→active 恢复语义，防回归。

import (
	"path/filepath"
	"testing"
)

func setupBalanceTestDB(t *testing.T) (*DB, *AccountManager) {
	t.Helper()
	db, err := NewDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.conn.Exec(`INSERT INTO accounts (id, token, device_id, balance) VALUES ('acct_t1','tok','dev_t1',-1)`); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	// UpdateBalance 只依赖 db，其余组件传 nil 即可
	return db, NewAccountManager(db, nil, nil, nil)
}

func TestUpdateBalancePersists(t *testing.T) {
	db, am := setupBalanceTestDB(t)

	am.UpdateBalance("acct_t1", 123.5)

	var bal float64
	var ts string
	if err := db.conn.QueryRow(`SELECT balance, balance_updated_at FROM accounts WHERE id='acct_t1'`).Scan(&bal, &ts); err != nil {
		t.Fatalf("query account: %v", err)
	}
	if bal != 123.5 {
		t.Fatalf("balance 未正确持久化: got %v, want 123.5", bal)
	}
	if ts == "" {
		t.Fatal("balance_updated_at 未写入（参数错位会把时间写到别处）")
	}
	// 快照同步写入
	var snaps int
	if err := db.conn.QueryRow(`SELECT COUNT(*) FROM credit_snapshots WHERE account_id='acct_t1'`).Scan(&snaps); err != nil || snaps != 1 {
		t.Fatalf("credit_snapshots 未写入: count=%d err=%v", snaps, err)
	}
}

func TestUpdateBalanceEmptyRecovery(t *testing.T) {
	db, am := setupBalanceTestDB(t)

	// empty + 余额回升 → 自动恢复 active
	if _, err := db.conn.Exec(`UPDATE accounts SET status='empty' WHERE id='acct_t1'`); err != nil {
		t.Fatal(err)
	}
	am.UpdateBalance("acct_t1", 10)
	var status string
	if err := db.conn.QueryRow(`SELECT status FROM accounts WHERE id='acct_t1'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != AcctActive {
		t.Fatalf("余额回升未恢复 active: got %q", status)
	}

	// empty + 余额仍为 0 → 保持 empty（CASE 比较值必须是 balance 而非时间串）
	if _, err := db.conn.Exec(`UPDATE accounts SET status='empty' WHERE id='acct_t1'`); err != nil {
		t.Fatal(err)
	}
	am.UpdateBalance("acct_t1", 0)
	if err := db.conn.QueryRow(`SELECT status FROM accounts WHERE id='acct_t1'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != AcctEmpty {
		t.Fatalf("余额为 0 不应恢复 active: got %q", status)
	}
}
