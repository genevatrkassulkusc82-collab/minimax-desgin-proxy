package main

// ---- 后台定时调度 ----
// 三个周期任务（全部错峰+抖动，避免整点批量请求的行为特征，设计方案 §4.7.2）：
//   1. token 续期：每账号 renew_interval_hour（默认 12h）± 按账号哈希散列的相位抖动；
//   2. 余额巡检：每账号 balance_interval_min（默认 15min）± 抖动，顺带恢复过期冷却；
//   3. 任务 reconcile：每 reconcile_interval_sec（默认 60s）调用 TaskManager.Reconcile 兜底。

import (
	"context"
	"hash/fnv"
	"log"
	"math/rand"
	"sync"
	"time"
)

// Scheduler 后台调度器
type Scheduler struct {
	db   *DB
	am   *AccountManager
	tm   *TaskManager
	cfg  *AppConfig
	stop chan struct{}
	wg   sync.WaitGroup
}

// NewScheduler 创建调度器
func NewScheduler(db *DB, am *AccountManager, tm *TaskManager, cfg *AppConfig) *Scheduler {
	return &Scheduler{db: db, am: am, tm: tm, cfg: cfg, stop: make(chan struct{})}
}

// Start 启动全部周期任务
func (s *Scheduler) Start() {
	s.wg.Add(3)
	go s.renewLoop()
	go s.balanceLoop()
	go s.reconcileLoop()
	log.Printf("[sched] started (renew=%dh balance=%dm reconcile=%ds)",
		s.cfg.Get().RenewIntervalHour, s.cfg.Get().BalanceIntervalMin, s.cfg.Get().ReconcileIntervalSec)
}

// Stop 停止调度（等待三个 loop 退出）
func (s *Scheduler) Stop() {
	close(s.stop)
	s.wg.Wait()
}

// phaseJitter 按账号 ID 哈希生成 0-1 的固定相位（错峰用，同账号相位稳定）
func phaseJitter(accountID string) float64 {
	h := fnv.New32a()
	h.Write([]byte(accountID))
	return float64(h.Sum32()%1000) / 1000.0
}

// renewLoop token 续期：每 30min 扫一遍，处理"距上次续期超过周期+相位抖动"的账号。
// 账号间随机间隔 20-60s，串行执行（同账号 renewal 由 AccountManager 内部互斥）。
func (s *Scheduler) renewLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()
	// 启动后先跑一轮（重启期间到期的账号能尽快续上）
	s.renewOnce()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			s.renewOnce()
		}
	}
}

func (s *Scheduler) renewOnce() {
	cfg := s.cfg.Get()
	period := time.Duration(cfg.RenewIntervalHour) * time.Hour
	accounts, err := s.am.ListAccounts()
	if err != nil {
		log.Printf("[sched] renew: list accounts failed: %v", err)
		return
	}
	now := time.Now()
	for _, a := range accounts {
		select {
		case <-s.stop:
			return
		default:
		}
		if !a.IsEnabled || a.Status == AcctDisabled {
			continue
		}
		// 到期判定：last_renew_at（无则 created_at）+ 周期 + 相位抖动(±20%)
		base := parseTimeOrZero(a.LastRenewAt)
		if base.IsZero() {
			base = parseTimeOrZero(a.CreatedAt)
		}
		if base.IsZero() {
			continue
		}
		jitter := time.Duration((phaseJitter(a.ID) - 0.5) * 0.4 * float64(period)) // ±20%
		if now.Sub(base) < period+jitter {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if _, err := s.am.TryRenew(ctx, a.ID); err != nil {
			log.Printf("[sched] renew %s(%s) failed: %v", a.Label, a.ID, err)
		} else {
			log.Printf("[sched] renew %s(%s) OK", a.Label, a.ID)
		}
		cancel()
		// 账号间拟人间隔
		select {
		case <-s.stop:
			return
		case <-time.After(time.Duration(20+rand.Intn(40)) * time.Second):
		}
	}
}

// balanceLoop 余额巡检：每 5min 扫一遍，处理"距上次余额更新超过周期+抖动"的账号；
// 顺带把冷却到期/余额回升的账号恢复到 active。
func (s *Scheduler) balanceLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	// 启动延迟 30s 再跑第一轮，避开启动恢复高峰
	select {
	case <-s.stop:
		return
	case <-time.After(30 * time.Second):
	}
	s.balanceOnce()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			s.balanceOnce()
		}
	}
}

func (s *Scheduler) balanceOnce() {
	cfg := s.cfg.Get()
	period := time.Duration(cfg.BalanceIntervalMin) * time.Minute
	accounts, err := s.am.ListAccounts()
	if err != nil {
		log.Printf("[sched] balance: list accounts failed: %v", err)
		return
	}
	now := time.Now()
	for _, a := range accounts {
		select {
		case <-s.stop:
			return
		default:
		}
		if !a.IsEnabled {
			continue
		}
		// 冷却到期恢复（即使不到巡检周期也要恢复调度资格）
		if a.Status == AcctCooldown {
			if t := parseTimeOrZero(a.CooldownUntil); !t.IsZero() && now.After(t) {
				if _, err := s.db.conn.Exec(`UPDATE accounts SET status='active', updated_at=? WHERE id=?`,
					nowStr(), a.ID); err == nil {
					log.Printf("[sched] account %s cooldown expired, back to active", a.ID)
				}
				a.Status = AcctActive
			}
		}
		// token 失效/禁用账号不主动触碰（最小化风控暴露）
		if a.Status == AcctTokenExpired || a.Status == AcctDisabled {
			continue
		}
		last := parseTimeOrZero(a.BalanceUpdatedAt)
		jitter := time.Duration(phaseJitter(a.ID+"bal") * 0.3 * float64(period)) // 0~30% 延后
		if !last.IsZero() && now.Sub(last) < period+jitter {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		bal, cerr := s.am.client.GetBalance(ctx, a.Profile())
		cancel()
		if cerr != nil {
			if cerr.Kind == KindAuth {
				// 巡检即探活：401 时先试一次续期
				if _, rerr := s.am.TryRenew(context.Background(), a.ID); rerr != nil {
					log.Printf("[sched] balance %s: auth failed and renew failed", a.ID)
				}
			} else {
				log.Printf("[sched] balance %s failed: %s", a.ID, cerr.Msg)
			}
		} else {
			s.am.UpdateBalance(a.ID, bal)
		}
		// 账号间小间隔
		select {
		case <-s.stop:
			return
		case <-time.After(time.Duration(3+rand.Intn(8)) * time.Second):
		}
	}
}

// reconcileLoop 任务兜底扫描
func (s *Scheduler) reconcileLoop() {
	defer s.wg.Done()
	interval := time.Duration(s.cfg.Get().ReconcileIntervalSec) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			s.tm.Reconcile()
		}
	}
}
