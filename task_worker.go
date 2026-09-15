package main

// ---- 异步视频任务管道 ----
// 职责：任务状态机唯一推进者。
//   状态机：QUEUED → PRECHECK → UPLOADING → SUBMITTING → POLLING → RETRIEVING
//           → DOWNLOADING → SUCCEEDED；失败分支 FAILED / CANCELLED / SUBMIT_UNKNOWN。
//   - SUBMIT_UNKNOWN：提交阶段结果不明（超时/连接中断）时的保守终态，绝不自动重试（防重复扣费）；
//   - 401/403 → 先续期一次重放，仍失败标 token_expired 换号（account_tries≤3）；
//   - 1033/5xx → 账号冷却 60s 换号；余额不足 → 标 empty 换号，全部不足 → FAILED；
//   - worker 池：固定启动 maxPoolWorkers 个 goroutine，仅前 rt.WorkerCount() 个消费队列
//     （实现 worker_count 热更）；每账号并发上限由 AccountManager 槽位控制；
//   - 重启恢复：非终态任务按阶段恢复（SUBMITTING → SUBMIT_UNKNOWN）；
//   - reconcile：每 60s 把 updated_at 超时且无人持有的非终态任务重新入队。

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// 任务内部状态（阶段）
const (
	TaskQueued        = "QUEUED"
	TaskPrecheck      = "PRECHECK"
	TaskUploading     = "UPLOADING"
	TaskSubmitting    = "SUBMITTING"
	TaskPolling       = "POLLING"
	TaskRetrieving    = "RETRIEVING"
	TaskDownloading   = "DOWNLOADING"
	TaskSucceeded     = "SUCCEEDED"
	TaskFailed        = "FAILED"
	TaskCancelled     = "CANCELLED"
	TaskSubmitUnknown = "SUBMIT_UNKNOWN"
)

// 任务来源（video_tasks.source 列）
const (
	TaskSourceAPI       = "api"        // 下游经 /v1/videos 提交
	TaskSourceAdminTest = "admin_test" // 管理台「在线测试」提交
)

// 任务媒体类型（video_tasks.media_type 列）
const (
	TaskMediaVideo = "video"
	TaskMediaImage = "image"
)

// AdminTestKeyID 管理台在线测试任务的 api_key_id 标记值（非真实 Key）
const AdminTestKeyID = "__admin_test__"

// 钉账号任务的失败错误码
const (
	ErrPinnedUnavailable = "pinned_account_unavailable" // 选号时发现钉住账号不可用（不存在/禁用/失效/冷却/耗尽）
	ErrPinnedFailed      = "pinned_account_failed"      // 执行中账号级失败（钉账号不换号，直接终态）
)

// maxPoolWorkers worker 池物理上限（rt.WorkerCount 热更在此范围内伸缩）
const maxPoolWorkers = 32

// isTerminalStatus 是否终态
func isTerminalStatus(s string) bool {
	switch s {
	case TaskSucceeded, TaskFailed, TaskCancelled, TaskSubmitUnknown:
		return true
	}
	return false
}

// ExternalStatus 内部阶段 → 对外粗粒度状态（2API 契约）
func ExternalStatus(s string) string {
	switch s {
	case TaskQueued, TaskPrecheck, TaskUploading, TaskSubmitting:
		return "queued"
	case TaskPolling, TaskRetrieving, TaskDownloading:
		return "processing"
	case TaskSucceeded:
		return "succeeded"
	case TaskCancelled:
		return "cancelled"
	case TaskFailed, TaskSubmitUnknown:
		return "failed"
	}
	return "queued"
}

// VideoTask video_tasks 表行
type VideoTask struct {
	ID                    string  `json:"id"`
	IdempotencyKey        string  `json:"-"`
	APIKeyID              string  `json:"api_key_id"`
	AccountID             string  `json:"account_id"`
	Model                 string  `json:"model"`
	Prompt                string  `json:"prompt"`
	RequestJSON           string  `json:"request_json"`
	Status                string  `json:"status"`
	CloudTaskID           string  `json:"cloud_task_id"`
	ProviderTaskID        string  `json:"provider_task_id"`
	FileID                string  `json:"file_id"`
	VideoPath             string  `json:"video_path"`
	VideoURL              string  `json:"video_url"`
	ErrorCode             string  `json:"error_code"`
	ErrorMsg              string  `json:"error_msg"`
	CreditsEstimated      float64 `json:"credits_estimated"`
	Progress              int     `json:"progress"`
	EstimatedRemainingSec int     `json:"estimated_remaining_sec"`
	CancelRequested       bool    `json:"cancel_requested"`
	Duration              int     `json:"duration"`
	Resolution            string  `json:"resolution"`
	Source                string  `json:"source"`            // api | admin_test
	PinnedAccountID       string  `json:"pinned_account_id"` // 钉住的账号（空=自动选号）
	MediaType             string  `json:"media_type"`        // video | image
	Width                 int     `json:"width"`             // 图片宽度（image 任务成功后回填）
	Height                int     `json:"height"`            // 图片高度
	CreatedAt             string  `json:"created_at"`
	SubmittedAt           string  `json:"submitted_at"`
	StartedAt             string  `json:"started_at"`
	CompletedAt           string  `json:"completed_at"`
	UpdatedAt             string  `json:"updated_at"`
}

// newTaskID 任务 ID：task_<20hex>
func newTaskID() string { return "task_" + randomHex(10) }

// TaskManager 任务管道管理器
type TaskManager struct {
	db     *DB
	am     *AccountManager
	client *MiniMaxClient
	cfg    *AppConfig
	rt     *RuntimeSettings

	queue   chan string
	mu      sync.Mutex
	pending map[string]bool // 已入队或正在处理（reconcile 防重复入队）

	costMu    sync.Mutex
	costCache map[string]costCacheEntry // calculate-cost 参数指纹 → 最近预估（预选号用，命中零云调用）

	stop chan struct{}
	wg   sync.WaitGroup
}

// NewTaskManager 创建任务管理器
func NewTaskManager(db *DB, am *AccountManager, client *MiniMaxClient, cfg *AppConfig, rt *RuntimeSettings) *TaskManager {
	return &TaskManager{
		db:      db,
		am:      am,
		client:  client,
		cfg:     cfg,
		rt:      rt,
		queue:   make(chan string, 1024),
		pending: make(map[string]bool),
		stop:    make(chan struct{}),
	}
}

// Start 启动 worker 池
func (tm *TaskManager) Start() {
	for i := 0; i < maxPoolWorkers; i++ {
		tm.wg.Add(1)
		go tm.workerLoop(i)
	}
	log.Printf("[worker] pool started (active=%d, max=%d)", tm.rt.WorkerCount(), maxPoolWorkers)
}

// Stop 停止全部 worker（等待当前任务阶段退出）
func (tm *TaskManager) Stop() {
	close(tm.stop)
	tm.wg.Wait()
	log.Printf("[worker] pool stopped")
}

// Enqueue 任务入队（幂等：pending 中的任务不重复入队）
func (tm *TaskManager) Enqueue(taskID string) {
	tm.mu.Lock()
	if tm.pending[taskID] {
		tm.mu.Unlock()
		return
	}
	tm.pending[taskID] = true
	tm.mu.Unlock()

	select {
	case tm.queue <- taskID:
	default:
		// 队列满：后台阻塞推送（reconcile 兜底，绝不丢任务）
		go func() {
			select {
			case tm.queue <- taskID:
			case <-tm.stop:
			}
		}()
	}
}

// releasePending 处理结束后清除 pending 标记
func (tm *TaskManager) releasePending(taskID string) {
	tm.mu.Lock()
	delete(tm.pending, taskID)
	tm.mu.Unlock()
}

// workerLoop 单个 worker：只有序号 < 当前 worker_count 的 worker 消费队列（热更）
func (tm *TaskManager) workerLoop(idx int) {
	defer tm.wg.Done()
	for {
		if idx >= tm.rt.WorkerCount() {
			select {
			case <-tm.stop:
				return
			case <-time.After(time.Second):
				continue
			}
		}
		select {
		case <-tm.stop:
			return
		case taskID := <-tm.queue:
			tm.processTask(taskID)
		case <-time.After(time.Second):
			// 空转一秒后回到循环头，重新检查 worker_count（支持缩容）
		}
	}
}

// processTask worker 取到任务后的入口（panic 安全）
func (tm *TaskManager) processTask(taskID string) {
	defer tm.releasePending(taskID)
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("[worker] PANIC processing %s: %v", taskID, rec)
			t, err := tm.GetTask(taskID)
			if err == nil && !isTerminalStatus(t.Status) {
				if t.Status == TaskSubmitting {
					// panic 发生在提交窗口内，结果不明
					tm.markSubmitUnknown(taskID, fmt.Sprintf("worker panic during submit: %v", rec))
				} else {
					tm.failTask(t, "internal_error", fmt.Sprintf("worker panic: %v", rec))
				}
			}
		}
	}()

	t, err := tm.GetTask(taskID)
	if err != nil {
		log.Printf("[worker] task %s gone: %v", taskID, err)
		return
	}
	if isTerminalStatus(t.Status) {
		return
	}
	// 入队期间被取消：直接置终态
	if t.CancelRequested && t.CloudTaskID == "" {
		tm.finishCancelled(t, "任务在排队中被取消")
		return
	}
	tm.runPipeline(t)
}

// ---- 选号前计价（预计算额度） ----
// 目标：首次选号即带 needCredit 走 PickAccount 的余额过滤，
// 避免"先选中 → PRECHECK 发现余额不足 → 换号"的浪费轮次。
// 计价结果按参数指纹缓存（costCacheTTL），同规格任务连续提交时零额外云调用。

const costCacheTTL = 10 * time.Minute

type costCacheEntry struct {
	est float64
	at  time.Time
}

func costCacheKeyVideo(model, resolution string, duration int) string {
	return fmt.Sprintf("video|%s|%s|%d", model, resolution, duration)
}

func costCacheKeyImage(model, resolution string, charCount, refCount int) string {
	return fmt.Sprintf("image|%s|%s|%d|%d", model, resolution, charCount, refCount)
}

func (tm *TaskManager) cachedCost(key string) (float64, bool) {
	tm.costMu.Lock()
	defer tm.costMu.Unlock()
	e, ok := tm.costCache[key]
	if !ok || e.est <= 0 || time.Since(e.at) > costCacheTTL {
		return 0, false
	}
	return e.est, true
}

func (tm *TaskManager) putCostCache(key string, est float64) {
	if est <= 0 {
		return
	}
	tm.costMu.Lock()
	defer tm.costMu.Unlock()
	if tm.costCache == nil {
		tm.costCache = map[string]costCacheEntry{}
	}
	tm.costCache[key] = costCacheEntry{est: est, at: time.Now()}
}

// probeAccount 预估用账号：优先钉住账号，否则任一 active 启用账号。
// 只借 token 计价，不占并发槽、不改变账号状态。
func (tm *TaskManager) probeAccount(t *VideoTask) *Account {
	if t.PinnedAccountID != "" {
		if acc, err := tm.am.GetAccount(t.PinnedAccountID); err == nil &&
			acc.IsEnabled && acc.Status == AcctActive && acc.Token != "" {
			return acc
		}
		return nil
	}
	all, err := tm.am.ListAccounts()
	if err != nil {
		return nil
	}
	for _, a := range all {
		if a.IsEnabled && a.Status == AcctActive && a.Token != "" {
			return a
		}
	}
	return nil
}

// estimateCostUpfront 选号前预估本任务所需积分。
// 缓存命中 → 零云调用；未命中 → 用探测账号调一次 calculate-cost。
// 计价是模型级价格（跨账号基本一致，会员促销差异由 PRECHECK 实时余额兜底）。
// 失败返回 0：不阻断任务，退回"选中后 PRECHECK 计价"的原路径。
func (tm *TaskManager) estimateCostUpfront(ctx context.Context, t *VideoTask) float64 {
	var (
		key string
		est float64
	)
	if t.MediaType == TaskMediaImage {
		var req ImageRequest
		if err := unmarshalImageRequest(t.RequestJSON, &req); err != nil {
			return 0
		}
		charCount := utf8.RuneCountInString(req.Prompt)
		key = costCacheKeyImage(req.Model, req.Resolution, charCount, len(req.Images))
		if v, ok := tm.cachedCost(key); ok {
			est = v
		} else {
			acc := tm.probeAccount(t)
			if acc == nil {
				return 0
			}
			v, cerr := tm.client.CalculateImageCost(ctx, acc.Profile(), req.Model, req.Resolution, 1, charCount, len(req.Images))
			if cerr != nil {
				log.Printf("[worker] %s upfront image estimate failed: %v", t.ID, cerr.Msg)
				return 0
			}
			est = v
			tm.putCostCache(key, est)
		}
	} else {
		key = costCacheKeyVideo(t.Model, t.Resolution, t.Duration)
		if v, ok := tm.cachedCost(key); ok {
			est = v
		} else {
			acc := tm.probeAccount(t)
			if acc == nil {
				return 0
			}
			v, cerr := tm.client.CalculateCost(ctx, acc.Profile(), t.Model, t.Resolution, t.Duration, true)
			if cerr != nil {
				log.Printf("[worker] %s upfront estimate failed: %v", t.ID, cerr.Msg)
				return 0
			}
			est = v
			tm.putCostCache(key, est)
		}
	}
	if est > 0 {
		tm.setField(t.ID, "credits_estimated", est)
		t.CreditsEstimated = est
		tm.addEvent(t.ID, t.Status, t.Status, fmt.Sprintf("选号前预估：本任务约消耗 %g 积分", est))
	}
	return est
}

// ---- 管道主循环 ----

// runPipeline 驱动任务从当前阶段走到终态；账号级失败换号重跑（≤3 次）
func (tm *TaskManager) runPipeline(t *VideoTask) {
	ctx := context.Background()
	excluded := map[string]bool{}
	needCredit := 0.0
	// 选号前预估：让 PickAccount 的余额过滤从第一次选号就生效。
	// 重启恢复/重试的任务直接复用已持久化的 credits_estimated；
	// 预估失败退回 0（PRECHECK 实时计价照旧兜底）。
	if t.CreditsEstimated > 0 {
		needCredit = t.CreditsEstimated
	} else if est := tm.estimateCostUpfront(ctx, t); est > 0 {
		needCredit = est
	}
	tries := 0

	for tries < 3 {
		select {
		case <-tm.stop:
			return // 进程退出：任务留在 DB，重启后恢复
		default:
		}

		// 选取账号：POLLING/RETRIEVING/DOWNLOADING 恢复时沿用原账号（云任务归属账号）
		acct, reason := tm.pickForAttempt(t, excluded, needCredit)
		if acct == nil {
			if reason == PickPinnedUnavailable {
				return // 钉账号不可用：pickPinned 内已置终态 FAILED
			}
			if reason == PickInsufficient && needCredit > 0 {
				// 所有账号余额都不够：终态失败
				tm.failTask(t, "billing_insufficient_balance",
					fmt.Sprintf("账号池余额不足（预估需要 %g 积分）", needCredit))
				return
			}
			if reason == PickNoAccount {
				tm.failTask(t, "no_account", "账号池中没有可用账号，请导入/启用账号")
				return
			}
			// 冷却/并发满：延迟重新入队等待
			tm.addEvent(t.ID, t.Status, t.Status, "无可用账号("+reason+")，5s 后重新排队")
			if !tm.sleep(5 * time.Second) {
				return
			}
			tm.Enqueue(t.ID)
			return
		}

		outcome := tm.runAttempt(ctx, t, acct, &needCredit)
		tm.am.ReleaseSlot(acct.ID)

		// 重新加载任务（阶段推进后字段已变化）
		fresh, err := tm.GetTask(t.ID)
		if err == nil {
			*t = *fresh
		}
		if isTerminalStatus(t.Status) {
			return
		}
		switch outcome {
		case outcomeSuccess, outcomeTerminal:
			return
		case outcomeRequeue:
			return
		case outcomeSwitch:
			if t.PinnedAccountID != "" {
				// 钉账号任务绝不换号：账号级失败直接终态（细节见事件时间线）
				tm.failTask(t, ErrPinnedFailed, "钉住账号执行失败（在线测试任务不换号），详见事件时间线")
				return
			}
			excluded[acct.ID] = true
			tries++
			continue
		case outcomeFail:
			return
		}
		return
	}
	tm.failTask(t, "account_tries_exhausted", "连续 3 个账号执行失败，最后错误: "+t.LastErrorMsg())
}

// attemptOutcome 单次账号尝试的结果
type attemptOutcome int

const (
	outcomeSuccess attemptOutcome = iota // 任务到达终态（成功/取消/失败已记录）
	outcomeSwitch                        // 账号问题，换号重试
	outcomeFail                          // 任务终态失败（已记录）
	outcomeRequeue                       // 已重新入队（等待恢复）
	outcomeTerminal                      // 任务已置终态（如取消）：立即结束整条管道，绝不推进后续阶段
)

// pickForAttempt 为本次尝试选账号：
//   - 恢复中的云阶段任务必须沿用原账号（云任务归属账号）；
//   - 钉账号任务（pinned_account_id 非空，管理台在线测试指定）跳过过滤/打分
//     直接用该账号：并发槽满则等待（不换号），账号不可用则终态失败（不换号）；
//   - 其余走选号器（过滤+打分+加权随机）。
func (tm *TaskManager) pickForAttempt(t *VideoTask, excluded map[string]bool, needCredit float64) (*Account, string) {
	resumeCloud := t.CloudTaskID != "" &&
		(t.Status == TaskPolling || t.Status == TaskRetrieving || t.Status == TaskDownloading)
	if resumeCloud {
		acc, err := tm.am.GetAccount(t.AccountID)
		if err == nil && acc.IsEnabled && (acc.Status == AcctActive || acc.Status == AcctCooldown) {
			if tm.am.AcquireSlot(acc.ID) {
				return acc, PickOK
			}
		}
		// 原账号并发满或暂不可用：检查总时限后延迟重排
		if tm.pollDeadlineExceeded(t) {
			tm.failTask(t, "timeout", "任务轮询超过总时限且原账号不可用")
			return nil, PickNoAccount
		}
		return nil, PickConcurrencyFull
	}
	if t.PinnedAccountID != "" {
		return tm.pickPinned(t)
	}
	return tm.am.PickAccount(needCredit, excluded)
}

// pickPinned 钉账号选取：不做过滤/打分，账号不可用直接终态失败（不换号）。
// 返回 (nil, PickPinnedUnavailable) 时任务已在内部置 FAILED，调用方直接结束。
func (tm *TaskManager) pickPinned(t *VideoTask) (*Account, string) {
	acc, err := tm.am.GetAccount(t.PinnedAccountID)
	if err != nil {
		tm.failTask(t, ErrPinnedUnavailable, "指定的测试账号不存在: "+t.PinnedAccountID)
		return nil, PickPinnedUnavailable
	}
	// 冷却到期自动恢复（与选号器同语义）
	now := time.Now()
	if acc.Status == AcctCooldown {
		if ct := parseTimeOrZero(acc.CooldownUntil); !ct.IsZero() && now.After(ct) {
			acc.Status = AcctActive
			tm.am.markStatus(acc.ID, AcctActive, "")
		}
	}
	if !acc.IsEnabled || acc.Status == AcctDisabled {
		tm.failTask(t, ErrPinnedUnavailable, "指定的测试账号已禁用: "+acc.ID)
		return nil, PickPinnedUnavailable
	}
	switch acc.Status {
	case AcctActive:
		// 可用
	case AcctTokenExpired:
		tm.failTask(t, ErrPinnedUnavailable, "指定的测试账号 token 已失效，请重新导入后再测: "+acc.ID)
		return nil, PickPinnedUnavailable
	case AcctEmpty:
		tm.failTask(t, ErrPinnedUnavailable, "指定的测试账号余额耗尽: "+acc.ID)
		return nil, PickPinnedUnavailable
	case AcctCooldown:
		tm.failTask(t, ErrPinnedUnavailable, fmt.Sprintf("指定的测试账号冷却中（至 %s）: %s", acc.CooldownUntil, acc.ID))
		return nil, PickPinnedUnavailable
	default:
		tm.failTask(t, ErrPinnedUnavailable, "指定的测试账号状态异常("+acc.Status+"): "+acc.ID)
		return nil, PickPinnedUnavailable
	}
	// 并发槽满：等待（延迟重排，绝不换号）
	if !tm.am.AcquireSlot(acc.ID) {
		return nil, PickConcurrencyFull
	}
	return acc, PickOK
}

// runAttempt 用指定账号从任务当前阶段推进；返回尝试结果
func (tm *TaskManager) runAttempt(ctx context.Context, t *VideoTask, acct *Account, needCredit *float64) attemptOutcome {
	tm.addEvent(t.ID, "", t.Status, fmt.Sprintf("账号 %s(%s) 开始执行", acct.Label, acct.ID))
	if _, err := tm.db.conn.Exec(`UPDATE video_tasks SET account_id=?, updated_at=? WHERE id=?`,
		acct.ID, nowStr(), t.ID); err != nil {
		log.Printf("[worker] bind account failed: %v", err)
	}
	t.AccountID = acct.ID
	if t.StartedAt == "" {
		tm.setField(t.ID, "started_at", nowStr())
		t.StartedAt = nowStr()
	}

	// 图片任务走独立阶段管道（同状态机骨架，跳过 RETRIEVING；见 task_worker_image.go）
	if t.MediaType == TaskMediaImage {
		return tm.runImageStages(ctx, t, acct, needCredit)
	}

	// 解析规范化请求
	var req VideoRequest
	if err := unmarshalRequest(t.RequestJSON, &req); err != nil {
		tm.failTask(t, "invalid_request", "request_json 解析失败: "+err.Error())
		return outcomeFail
	}

	stage := t.Status
	if stage == TaskQueued {
		stage = TaskPrecheck
	}

	// ---- PRECHECK：calculate-cost + balance 预检 ----
	if stage == TaskPrecheck {
		tm.setStage(t, TaskPrecheck, "预检：计价 + 余额")
		est, bal := tm.stagePrecheck(ctx, t, acct)
		if est > 0 {
			*needCredit = est
			safety := tm.rt.BalanceSafetyFactor()
			if bal >= 0 && bal < est*safety {
				// 分级处置：余额真正耗尽才标 empty（全局摘除直到充值/刷新）；
				// "跑不起本任务"只刷新余额并为本任务排除该账号——它仍能接更便宜的任务，
				// 且刷新后的缓存余额会让后续任务的选号过滤直接生效。
				if bal <= 0 {
					tm.am.MarkEmpty(acct.ID, fmt.Sprintf("余额 %g，已耗尽", bal))
				} else {
					tm.am.UpdateBalance(acct.ID, bal)
				}
				if t.PinnedAccountID != "" {
					tm.failTask(t, ErrPinnedFailed,
						fmt.Sprintf("钉住账号余额不足（余额 %g < 预估 %g×安全系数 %g），在线测试任务不换号", bal, est, safety))
					return outcomeFail
				}
				tm.addEvent(t.ID, TaskPrecheck, TaskPrecheck,
					fmt.Sprintf("账号余额 %g 不足以执行本任务（需 ≥%g），换号重试", bal, est*safety))
				return outcomeSwitch
			}
		}
		stage = TaskUploading
	}

	// 阶段边界取消检查
	if tm.cancelIfRequested(t, "任务在上传前被取消") {
		return outcomeSuccess
	}

	// ---- UPLOADING：素材上传换 CDN URL ----
	var submitReq VideoSubmitRequest
	if stage == TaskUploading {
		tm.setStage(t, TaskUploading, "上传参考素材")
		if res := tm.stageUpload(ctx, t, acct, &req, &submitReq); res != outcomeSuccess {
			return res
		}
		stage = TaskSubmitting
	} else {
		// 恢复路径（SUBMITTING 之后）：从 request_json 重建提交体（素材已是云 URL 时直接用）
		buildSubmitFromRequest(&req, &submitReq)
	}

	// ---- SUBMITTING：提交生成 ----
	if stage == TaskSubmitting {
		tm.setStage(t, TaskSubmitting, "提交云端生成")
		res := tm.stageSubmit(ctx, t, acct, &submitReq)
		if res != outcomeSuccess {
			return res
		}
		stage = TaskPolling
	}

	// ---- POLLING：轮询直到云端终态 ----
	if stage == TaskPolling {
		res := tm.stagePoll(ctx, t, acct)
		if res != outcomeSuccess {
			return res
		}
		stage = TaskRetrieving
	}

	// ---- RETRIEVING：取成片直链 ----
	if stage == TaskRetrieving {
		tm.setStage(t, TaskRetrieving, "获取成片直链")
		res := tm.stageRetrieve(ctx, t, acct)
		if res != outcomeSuccess {
			return res
		}
		stage = TaskDownloading
	}

	// ---- DOWNLOADING：流式下载落盘 ----
	if stage == TaskDownloading {
		tm.setStage(t, TaskDownloading, "下载成片到本地")
		res := tm.stageDownload(ctx, t, acct)
		if res != outcomeSuccess {
			return res
		}
	}
	return outcomeSuccess
}

// stagePrecheck 预检：预估已知（选号前完成）则只查实时余额；
// 未知时并行 calculate-cost + balance（原路径），计价结果回填缓存。
// 计价失败不阻断（云端自身会拦余额不足）；
// 余额查询失败时用账号缓存余额兜底（bal<0 表示未知，放行提交）。
func (tm *TaskManager) stagePrecheck(ctx context.Context, t *VideoTask, acct *Account) (float64, float64) {
	profile := acct.Profile()
	bal := acct.Balance

	if t.CreditsEstimated > 0 {
		if v, cerr := tm.client.GetBalance(ctx, profile); cerr == nil {
			bal = v
			tm.am.UpdateBalance(acct.ID, v)
		} else {
			log.Printf("[worker] %s balance failed: %v（沿用缓存余额 %g）", t.ID, cerr.Msg, bal)
		}
		tm.addEvent(t.ID, TaskPrecheck, TaskPrecheck,
			fmt.Sprintf("预估消耗 %g 积分，账号余额 %g", t.CreditsEstimated, bal))
		return t.CreditsEstimated, bal
	}

	var (
		est float64
		wg  sync.WaitGroup
		mu  sync.Mutex
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		v, cerr := tm.client.CalculateCost(ctx, profile, t.Model, t.Resolution, t.Duration, true)
		mu.Lock()
		defer mu.Unlock()
		if cerr == nil {
			est = v
			tm.putCostCache(costCacheKeyVideo(t.Model, t.Resolution, t.Duration), v)
		} else {
			log.Printf("[worker] %s calculate-cost failed: %v", t.ID, cerr.Msg)
		}
	}()
	go func() {
		defer wg.Done()
		v, cerr := tm.client.GetBalance(ctx, profile)
		mu.Lock()
		defer mu.Unlock()
		if cerr == nil {
			bal = v
			tm.am.UpdateBalance(acct.ID, v)
		}
	}()
	wg.Wait()

	if est > 0 {
		tm.setField(t.ID, "credits_estimated", est)
		t.CreditsEstimated = est
		tm.addEvent(t.ID, TaskPrecheck, TaskPrecheck, fmt.Sprintf("预估消耗 %g 积分，账号余额 %g", est, bal))
	}
	return est, bal
}

// stageUpload 上传全部参考素材并构造提交体
func (tm *TaskManager) stageUpload(ctx context.Context, t *VideoTask, acct *Account, req *VideoRequest, out *VideoSubmitRequest) attemptOutcome {
	profile := acct.Profile()

	uploadOne := func(spec, prefix string) (string, attemptOutcome, bool) {
		dataURI, cerr := tm.resolveAsset(ctx, spec, prefix)
		if cerr != nil {
			if cerr.Kind == KindClient {
				tm.failTask(t, "invalid_asset", cerr.Msg)
				return "", outcomeFail, false
			}
			tm.addEvent(t.ID, TaskUploading, TaskUploading, "素材获取失败("+prefix+"): "+cerr.Msg)
			return "", outcomeSwitch, false
		}
		// 上传（带账号级错误处理）
		var cloudURL string
		res, errOut := tm.callCloud(ctx, t, acct, &profile, "upload "+prefix, func(p DeviceProfile) *CloudError {
			u, e := tm.client.UploadFile(ctx, p, prefix, dataURI)
			cloudURL = u
			return e
		})
		if res != nil {
			return "", errOut, false
		}
		return cloudURL, outcomeSuccess, true
	}

	buildSubmitFromRequest(req, out)

	if req.FirstFrameImage != "" {
		u, outcome, ok := uploadOne(req.FirstFrameImage, "image")
		if !ok {
			return outcome
		}
		out.FirstFrameImage = u
	}
	if req.LastFrameImage != "" {
		u, outcome, ok := uploadOne(req.LastFrameImage, "image")
		if !ok {
			return outcome
		}
		out.LastFrameImage = u
	}
	for i, spec := range req.ReferenceImages {
		u, outcome, ok := uploadOne(spec, "image")
		if !ok {
			return outcome
		}
		out.ReferenceImages[i] = u
	}
	for i, spec := range req.ReferenceVideos {
		u, outcome, ok := uploadOne(spec, "video")
		if !ok {
			return outcome
		}
		out.ReferenceVideos[i] = u
	}
	for i, spec := range req.ReferenceAudios {
		u, outcome, ok := uploadOne(spec, "audio")
		if !ok {
			return outcome
		}
		out.ReferenceAudios[i] = u
	}
	tm.addEvent(t.ID, TaskUploading, TaskUploading, "素材上传完成")
	return outcomeSuccess
}

// buildSubmitFromRequest 由规范化请求构造云端提交体（素材槽位留待上传后填充）
func buildSubmitFromRequest(req *VideoRequest, out *VideoSubmitRequest) {
	out.Model = req.Model
	out.Prompt = req.Prompt
	out.GenerateAudio = req.GenerateAudio
	out.Ratio = req.Ratio
	out.Duration = req.Duration
	out.Resolution = req.Resolution
	if len(req.ReferenceImages) > 0 {
		out.ReferenceImages = make([]string, len(req.ReferenceImages))
		copy(out.ReferenceImages, req.ReferenceImages)
	}
	if len(req.ReferenceVideos) > 0 {
		out.ReferenceVideos = make([]string, len(req.ReferenceVideos))
		copy(out.ReferenceVideos, req.ReferenceVideos)
	}
	if len(req.ReferenceAudios) > 0 {
		out.ReferenceAudios = make([]string, len(req.ReferenceAudios))
		copy(out.ReferenceAudios, req.ReferenceAudios)
	}
}

// resolveAsset 把调用方给的素材（http(s) URL 或 data: URI）解析成 data URI，
// 并按桌面端限制校验大小：图 jpeg/png≤10MB、视频≤50MB、音频≤15MB（报告 §7.3/§9.1）
func (tm *TaskManager) resolveAsset(ctx context.Context, spec, prefix string) (string, *CloudError) {
	sizeLimit := map[string]int64{"image": 10 << 20, "video": 50 << 20, "audio": 15 << 20}[prefix]
	return tm.resolveAssetWithLimit(ctx, spec, prefix, sizeLimit)
}

// resolveAssetWithLimit 同 resolveAsset，但显式指定大小上限
// （图片生成参考图用官方 nano_banana 的 7MB 限制）
func (tm *TaskManager) resolveAssetWithLimit(ctx context.Context, spec, prefix string, sizeLimit int64) (string, *CloudError) {
	var data []byte
	var mime string
	if strings.HasPrefix(spec, "data:") {
		// data:<mime>;base64,<...>
		comma := strings.Index(spec, ",")
		if comma < 0 || !strings.Contains(spec[:comma], ";base64") {
			return "", &CloudError{Kind: KindClient, Msg: "data URI 格式非法（需要 data:<mime>;base64,<...>）"}
		}
		mime = strings.TrimPrefix(spec[5:comma], ";base64")
		mime = strings.TrimSuffix(mime, ";base64")
		dec, err := base64.StdEncoding.DecodeString(spec[comma+1:])
		if err != nil {
			dec, err = base64.RawStdEncoding.DecodeString(spec[comma+1:])
			if err != nil {
				return "", &CloudError{Kind: KindClient, Msg: "data URI base64 解码失败"}
			}
		}
		data = dec
	} else if strings.HasPrefix(spec, "http://") || strings.HasPrefix(spec, "https://") {
		dlCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(dlCtx, http.MethodGet, spec, nil)
		if err != nil {
			return "", &CloudError{Kind: KindClient, Msg: "素材 URL 非法: " + err.Error()}
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", &CloudError{Kind: KindTransient, Msg: "下载素材失败: " + err.Error()}
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return "", &CloudError{Kind: KindClient, Msg: fmt.Sprintf("下载素材 http %d", resp.StatusCode)}
		}
		mime = resp.Header.Get("Content-Type")
		data, err = io.ReadAll(io.LimitReader(resp.Body, sizeLimit+1))
		if err != nil {
			return "", &CloudError{Kind: KindTransient, Msg: "读取素材失败: " + err.Error()}
		}
	} else {
		return "", &CloudError{Kind: KindClient, Msg: "素材必须是 http(s) URL 或 data: URI"}
	}

	if int64(len(data)) > sizeLimit {
		return "", &CloudError{Kind: KindClient,
			Msg: fmt.Sprintf("%s 素材超过大小限制 %dMB", prefix, sizeLimit>>20)}
	}
	if len(data) == 0 {
		return "", &CloudError{Kind: KindClient, Msg: prefix + " 素材为空"}
	}
	// mime 以 magic bytes 为准（云端也按 magic bytes 定扩展名）
	detected := http.DetectContentType(data)
	if i := strings.Index(detected, ";"); i > 0 {
		detected = detected[:i]
	}
	if detected != "application/octet-stream" {
		mime = detected
	}
	if !strings.HasPrefix(mime, prefix+"/") && !(prefix == "image" && strings.HasPrefix(mime, "image/")) {
		return "", &CloudError{Kind: KindClient,
			Msg: fmt.Sprintf("素材类型 %s 与声明的 %s 不符", mime, prefix)}
	}
	if prefix == "image" && mime != "image/jpeg" && mime != "image/png" {
		return "", &CloudError{Kind: KindClient, Msg: "图片仅支持 jpeg/png，实际为 " + mime}
	}
	return fmt.Sprintf("data:%s;base64,%s", mime, base64.StdEncoding.EncodeToString(data)), nil
}

// stageSubmit 提交生成任务。结果不明的传输错误 → SUBMIT_UNKNOWN（绝不自动重试）。
func (tm *TaskManager) stageSubmit(ctx context.Context, t *VideoTask, acct *Account, req *VideoSubmitRequest) attemptOutcome {
	var cloudTaskID string
	profile := acct.Profile()

	submitOnce := func(p DeviceProfile) *CloudError {
		id, cerr := tm.client.SubmitVideo(ctx, p, *req)
		cloudTaskID = id
		return cerr
	}
	cerr := submitOnce(profile)
	if cerr != nil {
		if cerr.Uncertain {
			// HTTP 层错误且无法确认云端是否受理 → 保守终态，人工复核
			tm.markSubmitUnknown(t.ID, "提交结果不明: "+cerr.Msg)
			return outcomeFail
		}
		// 认证失败：续期一次重放
		if cerr.Kind == KindAuth {
			if _, rerr := tm.am.TryRenew(ctx, acct.ID); rerr == nil {
				if fresh, err := tm.am.GetAccount(acct.ID); err == nil {
					cerr = submitOnce(fresh.Profile())
				}
			}
			if cerr != nil && cerr.Kind == KindAuth {
				tm.am.MarkTokenExpired(acct.ID, cerr.Msg)
				tm.addEvent(t.ID, TaskSubmitting, TaskSubmitting, "token 失效且续期失败，换号重试")
				return outcomeSwitch
			}
		}
		if cerr != nil {
			switch cerr.Kind {
			case KindBusy:
				tm.am.ApplyCooldown(acct.ID, 60*time.Second, cerr.Msg)
				return outcomeSwitch
			case KindNoCredit:
				tm.am.MarkEmpty(acct.ID, cerr.Msg)
				return outcomeSwitch
			case KindTransient:
				if cerr.Uncertain {
					tm.markSubmitUnknown(t.ID, "提交结果不明: "+cerr.Msg)
					return outcomeFail
				}
				tm.am.ApplyCooldown(acct.ID, 60*time.Second, cerr.Msg)
				return outcomeSwitch
			default: // KindClient / KindUnknown：参数或未知云端错误，保守终态
				code := fmt.Sprintf("upstream_%d", cerr.Code)
				if cerr.Kind == KindClient {
					code = "invalid_params"
				}
				tm.failTask(t, code, cerr.Msg)
				return outcomeFail
			}
		}
	}

	// 提交成功
	now := nowStr()
	if _, err := tm.db.conn.Exec(`UPDATE video_tasks SET cloud_task_id=?, status=?, submitted_at=?, updated_at=? WHERE id=?`,
		cloudTaskID, TaskPolling, now, now, t.ID); err != nil {
		log.Printf("[worker] persist cloud_task_id failed: %v", err)
	}
	t.CloudTaskID = cloudTaskID
	t.SubmittedAt = now
	t.Status = TaskPolling
	tm.addEvent(t.ID, TaskSubmitting, TaskPolling, "云端受理 task_id="+cloudTaskID)
	return outcomeSuccess
}

// pollDeadlineExceeded 轮询总时限（poll_max_wait_min，从 submitted_at 起算）是否已超
func (tm *TaskManager) pollDeadlineExceeded(t *VideoTask) bool {
	base := parseTimeOrZero(t.SubmittedAt)
	if base.IsZero() {
		base = parseTimeOrZero(t.CreatedAt)
	}
	limit := time.Duration(tm.cfg.Get().PollMaxWaitMin) * time.Minute
	return time.Since(base) > limit
}

// stagePoll 轮询云端任务：初始 poll_interval_sec，×backoff 至上限；
// 用 estimated_remaining_wait_seconds 修正 progress 与间隔。
func (tm *TaskManager) stagePoll(ctx context.Context, t *VideoTask, acct *Account) attemptOutcome {
	cfg := tm.cfg.Get()
	interval := tm.rt.PollInterval()
	maxInterval := time.Duration(cfg.PollMaxIntervalSec) * time.Second
	backoff := cfg.PollBackoffFactor
	profile := acct.Profile()
	failStreak := 0

	for {
		// 取消请求：调云端 cancel 后置 CANCELLED（404=云端任务已不存在，视为取消成功）
		if tm.isCancelRequested(t.ID) {
			tm.addEvent(t.ID, TaskPolling, TaskPolling, "收到取消请求，向云端发送 cancel")
			if cerr := tm.client.CancelTask(ctx, profile, t.CloudTaskID); cerr != nil {
				if cerr.HTTPStatus == http.StatusNotFound {
					tm.addEvent(t.ID, TaskPolling, TaskPolling, "云端返回任务不存在(404)，已终止，视为取消成功")
				} else {
					tm.addEvent(t.ID, TaskPolling, TaskPolling, "云端 cancel 返回: "+cerr.Msg+"（继续等待云端终态）")
				}
			}
			// 再查一次确认；无论确认与否都置 CANCELLED（云端 cancel 是尽力而为）
			tm.finishCancelled(t, "用户取消（云端 task "+t.CloudTaskID+"）")
			return outcomeTerminal
		}
		if tm.pollDeadlineExceeded(t) {
			_ = tm.client.CancelTask(ctx, profile, t.CloudTaskID) // 超时尝试止损，失败忽略
			tm.failTask(t, "timeout", fmt.Sprintf("轮询超过总时限 %d 分钟", cfg.PollMaxWaitMin))
			return outcomeFail
		}
		if !tm.sleep(interval) {
			return outcomeRequeue // 进程退出
		}

		st, cerr := tm.client.QueryTask(ctx, profile, t.CloudTaskID)
		if cerr != nil {
			switch cerr.Kind {
			case KindAuth:
				if fresh, rerr := tm.am.TryRenew(ctx, acct.ID); rerr == nil {
					profile = fresh.Profile()
					continue // 立即重查
				}
				tm.am.MarkTokenExpired(acct.ID, cerr.Msg)
				tm.addEvent(t.ID, TaskPolling, TaskPolling, "token 失效且续期失败（保留 cloud_task_id）")
				return outcomeSwitch
			case KindBusy, KindTransient:
				failStreak++
				tm.addEvent(t.ID, TaskPolling, TaskPolling, fmt.Sprintf("查询失败(%d/10): %s", failStreak, cerr.Msg))
				if failStreak >= 10 {
					tm.failTask(t, "poll_unreachable", "连续 10 次查询失败: "+cerr.Msg)
					return outcomeFail
				}
				interval = tm.nextInterval(interval, backoff, maxInterval, 0)
				continue
			default:
				failStreak++
				if failStreak >= 10 {
					tm.failTask(t, "poll_error", cerr.Msg)
					return outcomeFail
				}
				continue
			}
		}
		failStreak = 0

		// 刷新进度与心跳（updated_at 供 reconcile 判断）
		// estRemaining：云端 estimated_remaining_wait_seconds 经 flexInt 宽容解析后转 int
		estRemaining := int(st.EstRemainingSec)
		elapsed := time.Since(parseTimeOrZero(t.SubmittedAt)).Seconds()
		progress := t.Progress
		if estRemaining > 0 && elapsed > 0 {
			p := int(elapsed / (elapsed + float64(estRemaining)) * 100)
			progress = clampInt(p, 5, 95)
		} else if st.Status == "processing" {
			progress = clampInt(maxInt(t.Progress, 50), 50, 95)
		} else {
			progress = clampInt(maxInt(t.Progress, 10), 10, 95)
		}
		if _, err := tm.db.conn.Exec(`UPDATE video_tasks SET progress=?, estimated_remaining_sec=?, provider_task_id=?, updated_at=? WHERE id=?`,
			progress, estRemaining, st.ProviderTaskID, nowStr(), t.ID); err != nil {
			log.Printf("[worker] update poll progress failed: %v", err)
		}
		t.Progress = progress
		t.EstimatedRemainingSec = estRemaining
		t.ProviderTaskID = st.ProviderTaskID

		switch st.Status {
		case "queue", "processing":
			interval = tm.nextInterval(interval, backoff, maxInterval, estRemaining)
		case "success":
			if st.FileID == "" {
				tm.failTask(t, "no_file_id", "云端 success 但未返回 file_id")
				return outcomeFail
			}
			now := nowStr()
			if _, err := tm.db.conn.Exec(`UPDATE video_tasks SET status=?, file_id=?, progress=96, updated_at=? WHERE id=?`,
				TaskRetrieving, st.FileID, now, t.ID); err != nil {
				log.Printf("[worker] set retrieving failed: %v", err)
			}
			t.Status = TaskRetrieving
			t.FileID = st.FileID
			tm.addEvent(t.ID, TaskPolling, TaskRetrieving, "云端生成完成 file_id="+st.FileID)
			return outcomeSuccess
		case "cancelled":
			tm.finishCancelled(t, "云端任务已取消")
			return outcomeTerminal
		case "failed", "fail":
			// 用户已请求取消而云端先终止任务：按取消收敛，避免误标失败
			// （云端 cancel 为异步尽力而为，任务可能先被终止；未扣费场景同样适用）
			if tm.isCancelRequested(t.ID) {
				tm.finishCancelled(t, "用户取消（云端任务已终止: "+st.BaseResp.StatusMsg+"）")
				return outcomeTerminal
			}
			msg := st.BaseResp.StatusMsg
			if msg == "" {
				msg = "云端生成失败"
			}
			code := "task_failed"
			if st.BaseResp.StatusCode != 0 {
				code = fmt.Sprintf("upstream_%d", int(st.BaseResp.StatusCode))
			}
			tm.failTask(t, code, msg)
			return outcomeFail
		default:
			tm.addEvent(t.ID, TaskPolling, TaskPolling, "未知云端状态: "+st.Status)
			interval = tm.nextInterval(interval, backoff, maxInterval, estRemaining)
		}
	}
}

// nextInterval 计算下一次轮询间隔：×backoff 封顶；预计剩余时间长时放宽（拟人 + 省请求）
func (tm *TaskManager) nextInterval(cur time.Duration, backoff float64, max time.Duration, estRemaining int) time.Duration {
	next := time.Duration(float64(cur) * backoff)
	if estRemaining > 300 {
		next = maxDuration(next, 15*time.Second)
	} else if estRemaining > 120 {
		next = maxDuration(next, 10*time.Second)
	}
	if next > max {
		next = max
	}
	return next
}

// stageRetrieve 取成片直链
func (tm *TaskManager) stageRetrieve(ctx context.Context, t *VideoTask, acct *Account) attemptOutcome {
	var downloadURL string
	_, outcome := tm.callCloud(ctx, t, acct, nil, "retrieve file", func(p DeviceProfile) *CloudError {
		u, cerr := tm.client.GetFileURL(ctx, p, t.FileID)
		downloadURL = u
		return cerr
	})
	if outcome != outcomeSuccess {
		return outcome
	}
	now := nowStr()
	if _, err := tm.db.conn.Exec(`UPDATE video_tasks SET video_url=?, status=?, progress=98, updated_at=? WHERE id=?`,
		downloadURL, TaskDownloading, now, t.ID); err != nil {
		log.Printf("[worker] set downloading failed: %v", err)
	}
	t.VideoURL = downloadURL
	t.Status = TaskDownloading
	tm.addEvent(t.ID, TaskRetrieving, TaskDownloading, "拿到 CDN 直链")
	return outcomeSuccess
}

// stageDownload 流式下载成片到 data/videos/{task_id}.mp4（完整性校验失败重试 ≤3）
func (tm *TaskManager) stageDownload(ctx context.Context, t *VideoTask, acct *Account) attemptOutcome {
	// 恢复路径兜底：直链缺失但有 file_id 时先补一次 RETRIEVING
	if t.VideoURL == "" && t.FileID != "" {
		if res := tm.stageRetrieve(ctx, t, acct); res != outcomeSuccess {
			return res
		}
	}
	if t.VideoURL == "" {
		tm.failTask(t, "download_failed", "无可用下载直链")
		return outcomeFail
	}
	videoDir := tm.cfg.Get().VideoDir
	if err := os.MkdirAll(videoDir, 0755); err != nil {
		tm.failTask(t, "download_failed", "创建视频目录失败: "+err.Error())
		return outcomeFail
	}
	dest := filepath.Join(videoDir, t.ID+".mp4")
	timeout := time.Duration(tm.cfg.Get().DownloadTimeoutSec) * time.Second

	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		dlCtx, cancel := context.WithTimeout(ctx, timeout)
		lastErr = tm.client.DownloadFile(dlCtx, t.VideoURL, dest)
		cancel()
		if lastErr == nil {
			break
		}
		tm.addEvent(t.ID, TaskDownloading, TaskDownloading, fmt.Sprintf("下载失败(%d/3): %v", attempt, lastErr))
		if !tm.sleep(time.Duration(attempt*5) * time.Second) {
			return outcomeRequeue
		}
	}
	if lastErr != nil {
		// 本地下载失败但 CDN 直链仍在：降级为"仅直链"成功（content 端点会 302）
		tm.addEvent(t.ID, TaskDownloading, TaskDownloading, "本地落盘失败，降级为 CDN 直链交付: "+lastErr.Error())
	} else {
		tm.setField(t.ID, "video_path", dest)
		t.VideoPath = dest
	}

	// 终态：成功
	now := nowStr()
	if _, err := tm.db.conn.Exec(`UPDATE video_tasks SET status=?, progress=100, completed_at=?, updated_at=? WHERE id=?`,
		TaskSucceeded, now, now, t.ID); err != nil {
		log.Printf("[worker] set succeeded failed: %v", err)
	}
	t.Status = TaskSucceeded
	t.CompletedAt = now
	tm.am.MarkSuccess(acct.ID)
	tm.writeUsageLog(t, TaskSucceeded)
	tm.addEvent(t.ID, TaskDownloading, TaskSucceeded, "任务完成")
	log.Printf("[worker] task %s SUCCEEDED (account=%s credits=%g)", t.ID, acct.ID, t.CreditsEstimated)
	return outcomeSuccess
}

// ---- 云调用统一错误处理 ----

// callCloud 执行一次云调用，统一处理 401 续期重放 / 1033·5xx 冷却换号 / 网络退避。
// fn 返回 nil 表示成功；否则返回分类错误。profile 可为 nil（用 acct 当前档案）。
// 返回值：(最终错误, 建议的 attemptOutcome)。最终错误为 nil 时 outcome 无意义。
func (tm *TaskManager) callCloud(ctx context.Context, t *VideoTask, acct *Account, profile *DeviceProfile,
	desc string, fn func(p DeviceProfile) *CloudError) (*CloudError, attemptOutcome) {

	p := acct.Profile()
	if profile != nil {
		p = *profile
	}
	authRetried := false
	var lastErr *CloudError
	for attempt := 0; attempt < 3; attempt++ {
		cerr := fn(p)
		if cerr == nil {
			return nil, outcomeSuccess
		}
		lastErr = cerr
		tm.addEvent(t.ID, t.Status, t.Status, fmt.Sprintf("%s 失败(%d): [%d] %s", desc, attempt+1, cerr.Code, cerr.Msg))
		switch cerr.Kind {
		case KindAuth:
			if !authRetried {
				authRetried = true
				if fresh, rerr := tm.am.TryRenew(ctx, acct.ID); rerr == nil {
					p = fresh.Profile()
					continue // 续期成功，重放一次
				}
			}
			tm.am.MarkTokenExpired(acct.ID, cerr.Msg)
			return cerr, outcomeSwitch
		case KindBusy:
			tm.am.ApplyCooldown(acct.ID, 60*time.Second, cerr.Msg)
			return cerr, outcomeSwitch
		case KindNoCredit:
			tm.am.MarkEmpty(acct.ID, cerr.Msg)
			return cerr, outcomeSwitch
		case KindTransient:
			if cerr.Uncertain {
				return cerr, outcomeSwitch // 调用方决定语义（提交阶段→SUBMIT_UNKNOWN）
			}
			if !tm.sleep(time.Duration((attempt+1)*3) * time.Second) {
				return cerr, outcomeRequeue
			}
			continue
		default: // KindClient / KindUnknown：保守终态，不自动重试
			code := fmt.Sprintf("upstream_%d", cerr.Code)
			if cerr.Kind == KindClient {
				code = "invalid_params"
			}
			tm.failTask(t, code, cerr.Msg)
			return cerr, outcomeFail
		}
	}
	// 网络类重试耗尽：冷却换号
	tm.am.ApplyCooldown(acct.ID, 60*time.Second, lastErr.Msg)
	return lastErr, outcomeSwitch
}

// ---- 终态与状态落库 ----

// setStage 推进阶段：更新 status/updated_at 并追加事件
func (tm *TaskManager) setStage(t *VideoTask, to, msg string) {
	from := t.Status
	if from == to {
		tm.addEvent(t.ID, from, to, msg)
		return
	}
	if _, err := tm.db.conn.Exec(`UPDATE video_tasks SET status=?, updated_at=? WHERE id=?`, to, nowStr(), t.ID); err != nil {
		log.Printf("[worker] set stage %s failed: %v", to, err)
		return
	}
	t.Status = to
	tm.addEvent(t.ID, from, to, msg)
}

// setField 更新单字段
func (tm *TaskManager) setField(id, col string, val any) {
	// col 仅来自代码内常量，无注入风险
	query := fmt.Sprintf(`UPDATE video_tasks SET %s=?, updated_at=? WHERE id=?`, col)
	if _, err := tm.db.conn.Exec(query, val, nowStr(), id); err != nil {
		log.Printf("[worker] set field %s failed: %v", col, err)
	}
}

// addEvent 追加任务事件
func (tm *TaskManager) addEvent(taskID, from, to, msg string) {
	if _, err := tm.db.conn.Exec(`INSERT INTO task_events (task_id, ts, from_status, to_status, message) VALUES (?,?,?,?,?)`,
		taskID, nowStr(), from, to, truncate(msg, 500)); err != nil {
		log.Printf("[worker] add event failed: %v", err)
	}
	// 心跳：任何事件都刷新 updated_at，供 reconcile 判断
	if _, err := tm.db.conn.Exec(`UPDATE video_tasks SET updated_at=? WHERE id=?`, nowStr(), taskID); err != nil {
		log.Printf("[worker] touch task failed: %v", err)
	}
}

// failTask 终态 FAILED（记录错误 + 用量 + 账号计数）
func (tm *TaskManager) failTask(t *VideoTask, code, msg string) {
	now := nowStr()
	if _, err := tm.db.conn.Exec(`UPDATE video_tasks SET status=?, error_code=?, error_msg=?, completed_at=?, updated_at=? WHERE id=?`,
		TaskFailed, code, truncate(msg, 500), now, now, t.ID); err != nil {
		log.Printf("[worker] fail task failed: %v", err)
	}
	from := t.Status
	t.Status = TaskFailed
	t.ErrorCode = code
	t.ErrorMsg = msg
	t.CompletedAt = now
	tm.am.MarkTaskDone(t.AccountID)
	tm.writeUsageLog(t, TaskFailed)
	tm.addEvent(t.ID, from, TaskFailed, fmt.Sprintf("[%s] %s", code, msg))
	log.Printf("[worker] task %s FAILED: [%s] %s", t.ID, code, truncate(msg, 120))
}

// markSubmitUnknown SUBMIT_UNKNOWN 保守终态（绝不自动重试，防重复扣费）
func (tm *TaskManager) markSubmitUnknown(taskID, msg string) {
	now := nowStr()
	if _, err := tm.db.conn.Exec(`UPDATE video_tasks SET status=?, error_code=?, error_msg=?, completed_at=?, updated_at=? WHERE id=?`,
		TaskSubmitUnknown, "submit_unknown", truncate(msg, 500), now, now, taskID); err != nil {
		log.Printf("[worker] mark submit_unknown failed: %v", err)
	}
	tm.addEvent(taskID, TaskSubmitting, TaskSubmitUnknown, msg+"（需人工在管理端复核：可能已扣费）")
	log.Printf("[worker] task %s SUBMIT_UNKNOWN: %s", taskID, truncate(msg, 120))
}

// finishCancelled 终态 CANCELLED
func (tm *TaskManager) finishCancelled(t *VideoTask, msg string) {
	now := nowStr()
	if _, err := tm.db.conn.Exec(`UPDATE video_tasks SET status=?, completed_at=?, updated_at=? WHERE id=?`,
		TaskCancelled, now, now, t.ID); err != nil {
		log.Printf("[worker] cancel task failed: %v", err)
	}
	from := t.Status
	t.Status = TaskCancelled
	t.CompletedAt = now
	tm.am.MarkTaskDone(t.AccountID)
	tm.writeUsageLog(t, TaskCancelled)
	tm.addEvent(t.ID, from, TaskCancelled, msg)
	log.Printf("[worker] task %s CANCELLED: %s", t.ID, msg)
}

// writeUsageLog 任务终态时写用量明细
func (tm *TaskManager) writeUsageLog(t *VideoTask, status string) {
	durationMs := 0
	start := parseTimeOrZero(t.StartedAt)
	end := parseTimeOrZero(t.CompletedAt)
	if !start.IsZero() && !end.IsZero() {
		durationMs = int(end.Sub(start).Milliseconds())
	}
	credits := 0.0
	if status == TaskSucceeded {
		credits = t.CreditsEstimated
	}
	if _, err := tm.db.conn.Exec(`
		INSERT INTO usage_logs (ts, api_key_id, task_id, account_id, model, credits, status, duration_ms)
		VALUES (?,?,?,?,?,?,?,?)
	`, nowStr(), t.APIKeyID, t.ID, t.AccountID, t.Model, credits, ExternalStatus(status), durationMs); err != nil {
		log.Printf("[worker] write usage log failed: %v", err)
	}
}

// isCancelRequested 读库检查取消标记
func (tm *TaskManager) isCancelRequested(taskID string) bool {
	var v int
	if err := tm.db.conn.QueryRow(`SELECT cancel_requested FROM video_tasks WHERE id=?`, taskID).Scan(&v); err != nil {
		return false
	}
	return v != 0
}

// cancelIfRequested 阶段边界取消检查：置 CANCELLED 并返回 true
func (tm *TaskManager) cancelIfRequested(t *VideoTask, msg string) bool {
	if tm.isCancelRequested(t.ID) {
		tm.finishCancelled(t, msg)
		return true
	}
	return false
}

// sleep 可被 Stop 中断的睡眠；返回 false 表示进程正在退出
func (tm *TaskManager) sleep(d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-tm.stop:
		return false
	case <-timer.C:
		return true
	}
}

// ---- 任务 CRUD ----

// InsertTask 创建任务行
func (tm *TaskManager) InsertTask(t *VideoTask) error {
	if t.Source == "" {
		t.Source = TaskSourceAPI
	}
	if t.MediaType == "" {
		t.MediaType = TaskMediaVideo
	}
	// 28 列：8 个入参占位 + 7 个空串默认 + 4 个数值 0 + duration/resolution/source/
	// pinned_account_id/created_at 占位 + submitted/started/completed 空串 + updated_at 占位
	_, err := tm.db.conn.Exec(`
		INSERT INTO video_tasks (id, idempotency_key, api_key_id, account_id, model, prompt, request_json,
			status, cloud_task_id, provider_task_id, file_id, video_path, video_url,
			error_code, error_msg, credits_estimated, progress, estimated_remaining_sec,
			cancel_requested, duration, resolution, source, pinned_account_id, media_type,
			created_at, submitted_at, started_at, completed_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?, '','','','','', '','', 0,0,0,0, ?,?,?,?,?,?, '','','', ?)
	`, t.ID, t.IdempotencyKey, t.APIKeyID, t.AccountID, t.Model, t.Prompt, t.RequestJSON,
		t.Status, t.Duration, t.Resolution, t.Source, t.PinnedAccountID, t.MediaType, t.CreatedAt, t.UpdatedAt)
	if err != nil {
		return fmt.Errorf("insert task: %w", err)
	}
	tm.addEvent(t.ID, "", TaskQueued, "任务创建")
	return nil
}

const taskCols = `id, idempotency_key, api_key_id, account_id, model, prompt, request_json, status,
	cloud_task_id, provider_task_id, file_id, video_path, video_url, error_code, error_msg,
	credits_estimated, progress, estimated_remaining_sec, cancel_requested, duration, resolution,
	source, pinned_account_id, media_type, width, height,
	created_at, submitted_at, started_at, completed_at, updated_at`

func scanTask(scanner interface {
	Scan(dest ...any) error
}) (*VideoTask, error) {
	var t VideoTask
	var cancel int
	err := scanner.Scan(&t.ID, &t.IdempotencyKey, &t.APIKeyID, &t.AccountID, &t.Model, &t.Prompt,
		&t.RequestJSON, &t.Status, &t.CloudTaskID, &t.ProviderTaskID, &t.FileID, &t.VideoPath,
		&t.VideoURL, &t.ErrorCode, &t.ErrorMsg, &t.CreditsEstimated, &t.Progress,
		&t.EstimatedRemainingSec, &cancel, &t.Duration, &t.Resolution,
		&t.Source, &t.PinnedAccountID, &t.MediaType, &t.Width, &t.Height,
		&t.CreatedAt, &t.SubmittedAt, &t.StartedAt, &t.CompletedAt, &t.UpdatedAt)
	if err != nil {
		return nil, err
	}
	t.CancelRequested = cancel != 0
	if t.Source == "" {
		t.Source = TaskSourceAPI // 迁移前的旧行按 api 处理
	}
	if t.MediaType == "" {
		t.MediaType = TaskMediaVideo // 迁移前的旧行按 video 处理
	}
	return &t, nil
}

// GetTask 按 ID 取任务
func (tm *TaskManager) GetTask(id string) (*VideoTask, error) {
	row := tm.db.conn.QueryRow(`SELECT `+taskCols+` FROM video_tasks WHERE id=?`, id)
	t, err := scanTask(row)
	if err != nil {
		return nil, fmt.Errorf("task %s not found: %w", id, err)
	}
	return t, nil
}

// FindTaskByIdempotencyKey 幂等查找
func (tm *TaskManager) FindTaskByIdempotencyKey(apiKeyID, key string) (*VideoTask, error) {
	row := tm.db.conn.QueryRow(`SELECT `+taskCols+` FROM video_tasks WHERE api_key_id=? AND idempotency_key=?`,
		apiKeyID, key)
	return scanTask(row)
}

// RequestCancel 置取消标记；已终态返回 false
func (tm *TaskManager) RequestCancel(id string) (bool, error) {
	t, err := tm.GetTask(id)
	if err != nil {
		return false, err
	}
	if isTerminalStatus(t.Status) {
		return false, nil
	}
	if _, err := tm.db.conn.Exec(`UPDATE video_tasks SET cancel_requested=1, updated_at=? WHERE id=?`, nowStr(), id); err != nil {
		return false, err
	}
	tm.addEvent(id, t.Status, t.Status, "收到取消请求")
	return true, nil
}

// ---- 重启恢复 / reconcile ----

// RecoverOnStartup 启动时扫描非终态任务：
//   - SUBMITTING → SUBMIT_UNKNOWN（提交结果不明，绝不自动续跑）；
//   - POLLING/RETRIEVING/DOWNLOADING（有 cloud_task_id）→ 按阶段恢复入队；
//   - QUEUED/PRECHECK/UPLOADING → 重置 QUEUED 重新入队。
func (tm *TaskManager) RecoverOnStartup() error {
	rows, err := tm.db.conn.Query(`SELECT id, status, cloud_task_id FROM video_tasks
		WHERE status NOT IN (?,?,?,?)`, TaskSucceeded, TaskFailed, TaskCancelled, TaskSubmitUnknown)
	if err != nil {
		return fmt.Errorf("query non-terminal tasks: %w", err)
	}
	defer rows.Close()
	type rec struct{ id, status, cloudTaskID string }
	var recs []rec
	for rows.Next() {
		var r rec
		if err := rows.Scan(&r.id, &r.status, &r.cloudTaskID); err != nil {
			return err
		}
		recs = append(recs, r)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, r := range recs {
		switch {
		case r.status == TaskSubmitting:
			tm.markSubmitUnknown(r.id, "服务重启于提交阶段，云端受理结果不明")
		case (r.status == TaskPolling || r.status == TaskRetrieving || r.status == TaskDownloading) && r.cloudTaskID != "":
			tm.addEvent(r.id, r.status, r.status, "服务重启，从 "+r.status+" 阶段恢复")
			tm.Enqueue(r.id)
		default:
			if _, err := tm.db.conn.Exec(`UPDATE video_tasks SET status=?, updated_at=? WHERE id=?`,
				TaskQueued, nowStr(), r.id); err != nil {
				log.Printf("[worker] reset task %s failed: %v", r.id, err)
			}
			tm.addEvent(r.id, r.status, TaskQueued, "服务重启，重新排队")
			tm.Enqueue(r.id)
		}
	}
	if len(recs) > 0 {
		log.Printf("[worker] startup recovery: %d tasks re-queued/marked", len(recs))
	}
	return nil
}

// Reconcile 兜底扫描：updated_at 超过 2×轮询间隔仍非终态、且无 worker 持有的任务重新入队
func (tm *TaskManager) Reconcile() {
	poll := tm.rt.PollInterval()
	cutoff := time.Now().Add(-2 * poll)
	if twoMin := time.Now().Add(-2 * time.Minute); cutoff.After(twoMin) {
		cutoff = twoMin // 至少 2 分钟，避免排队中的任务被误判
	}
	rows, err := tm.db.conn.Query(`SELECT id, status, updated_at, cancel_requested FROM video_tasks
		WHERE status NOT IN (?,?,?,?)`, TaskSucceeded, TaskFailed, TaskCancelled, TaskSubmitUnknown)
	if err != nil {
		log.Printf("[worker] reconcile query failed: %v", err)
		return
	}
	defer rows.Close()
	type rec struct {
		id, status, updatedAt string
		cancel                int
	}
	var recs []rec
	for rows.Next() {
		var r rec
		if err := rows.Scan(&r.id, &r.status, &r.updatedAt, &r.cancel); err != nil {
			log.Printf("[worker] reconcile scan failed: %v", err)
			return
		}
		recs = append(recs, r)
	}
	for _, r := range recs {
		tm.mu.Lock()
		held := tm.pending[r.id]
		tm.mu.Unlock()
		if held {
			continue
		}
		// 排队中被取消且无人处理：直接置终态
		if r.cancel != 0 && r.status == TaskQueued {
			if t, err := tm.GetTask(r.id); err == nil {
				tm.finishCancelled(t, "任务在排队中被取消（reconcile）")
			}
			continue
		}
		if t := parseTimeOrZero(r.updatedAt); !t.IsZero() && t.Before(cutoff) {
			log.Printf("[worker] reconcile: re-queue stuck task %s (status=%s updated=%s)", r.id, r.status, r.updatedAt)
			tm.Enqueue(r.id)
		}
	}
}

// ---- 小工具 ----

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

// LastErrorMsg 最近错误信息（换号耗尽时展示）
func (t *VideoTask) LastErrorMsg() string {
	if t.ErrorMsg != "" {
		return t.ErrorMsg
	}
	return "无详细信息"
}
