package main

// ---- 选号前计价（预计算额度）回归测试 ----
// 覆盖：计价缓存 put/get/TTL/非法值、缓存键构造、
// PickAccount 的 needCredit 余额过滤（预选号的核心保障）。

import (
	"path/filepath"
	"testing"
	"time"
)

func newTestAccountManager(t *testing.T) *AccountManager {
	t.Helper()
	db, err := NewDB(filepath.Join(t.TempDir(), "preflight.db"))
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	rt := NewRuntimeSettings(db, FileConfig{
		PollIntervalSec:       5,
		WorkerCount:           4,
		PerAccountConcurrency: 2,
		BalanceSafetyFactor:   1.2,
	})
	return NewAccountManager(db, nil, nil, rt)
}

func TestCostCachePutGetTTL(t *testing.T) {
	tm := &TaskManager{}

	// 命中
	tm.putCostCache("k1", 280)
	if v, ok := tm.cachedCost("k1"); !ok || v != 280 {
		t.Fatalf("cachedCost(k1) = (%v,%v), want (280,true)", v, ok)
	}
	// 未写入的键
	if _, ok := tm.cachedCost("nope"); ok {
		t.Fatal("未知键应 miss")
	}
	// 非正数不入缓存
	tm.putCostCache("zero", 0)
	tm.putCostCache("neg", -5)
	if _, ok := tm.cachedCost("zero"); ok {
		t.Fatal("est=0 不应入缓存")
	}
	if _, ok := tm.cachedCost("neg"); ok {
		t.Fatal("est<0 不应入缓存")
	}
	// TTL 过期
	tm.costMu.Lock()
	e := tm.costCache["k1"]
	e.at = time.Now().Add(-costCacheTTL - time.Minute)
	tm.costCache["k1"] = e
	tm.costMu.Unlock()
	if _, ok := tm.cachedCost("k1"); ok {
		t.Fatal("过期条目应 miss")
	}
}

func TestCostCacheKeys(t *testing.T) {
	if got := costCacheKeyVideo("MiniMax-H3", "768P", 5); got != "video|MiniMax-H3|768P|5" {
		t.Errorf("video key = %q", got)
	}
	if got := costCacheKeyImage("nano_banana_2_flash", "1K", 10, 2); got != "image|nano_banana_2_flash|1K|10|2" {
		t.Errorf("image key = %q", got)
	}
	// 参数不同键必须不同（防串价）
	if costCacheKeyVideo("MiniMax-H3", "768P", 5) == costCacheKeyVideo("MiniMax-H3", "2K", 5) {
		t.Error("不同 resolution 的键冲突")
	}
	if costCacheKeyVideo("MiniMax-H3", "768P", 5) == costCacheKeyVideo("MiniMax-H3-Max", "768P", 5) {
		t.Error("不同 model 的键冲突")
	}
}

func TestPickAccountBalanceFilter(t *testing.T) {
	am := newTestAccountManager(t)

	accA, _, err := am.upsertAccount("tok-a", "dev-aaaa", "t", 1)
	if err != nil {
		t.Fatalf("insert A: %v", err)
	}
	accB, _, err := am.upsertAccount("tok-b", "dev-bbbb", "t", 2)
	if err != nil {
		t.Fatalf("insert B: %v", err)
	}
	am.UpdateBalance(accA.ID, 100)  // 穷账号
	am.UpdateBalance(accB.ID, 5000) // 富账号

	// needCredit=1000（阈值 1000×1.2=1200）：A(100) 被过滤，必选 B
	acc, reason := am.PickAccount(1000, nil)
	if acc == nil || acc.ID != accB.ID {
		t.Fatalf("应选中余额充足的 B，got acc=%v reason=%s", acc, reason)
	}

	// needCredit=10000（阈值 12000）：全部不足 → PickInsufficient
	acc2, reason2 := am.PickAccount(10000, nil)
	if acc2 != nil || reason2 != PickInsufficient {
		t.Fatalf("应返回 PickInsufficient，got acc=%v reason=%s", acc2, reason2)
	}

	// needCredit=50（阈值 60）：两个都够，返回其一
	acc3, _ := am.PickAccount(50, nil)
	if acc3 == nil {
		t.Fatal("低额度任务应有账号可选")
	}

	// 余额未知(-1)的账号放行（PRECHECK 实时兜底）
	accC, _, err := am.upsertAccount("tok-c", "dev-cccc", "t", 3)
	if err != nil {
		t.Fatalf("insert C: %v", err)
	}
	acc4, reason4 := am.PickAccount(1000, map[string]bool{accA.ID: true, accB.ID: true})
	if acc4 == nil || acc4.ID != accC.ID {
		t.Fatalf("余额未知账号应放行，got acc=%v reason=%s", acc4, reason4)
	}

	// 排除集生效：排除 B 后 needCredit=1000 应 Insufficient（A 不够、C 被穷余额过滤？C 未知放行）
	// 注意 C 余额未知会放行，所以再排除 C 才触发 Insufficient
	acc5, reason5 := am.PickAccount(1000, map[string]bool{accB.ID: true, accC.ID: true})
	if acc5 != nil && acc5.ID != accA.ID {
		t.Fatalf("排除集异常: acc=%v", acc5)
	}
	if acc5 != nil {
		// A 余额 100 < 1200 必须被过滤 → 不应返回 A
		t.Fatalf("A 余额不足应被过滤，却选中了 %s", acc5.ID)
	}
	if reason5 != PickInsufficient {
		t.Fatalf("应返回 PickInsufficient，got %s", reason5)
	}
}
