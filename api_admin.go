package main

// ---- 管理 API（/api/*，session 认证，/api/login 除外） ----
// 路由风格对齐 dumate-proxy：单入口 /api/ 按 segments 分发。
// 端点清单：
//   accounts: 列表/导入/刷新/启停/领试用/删除/健康测试(单个+批量)
//   tasks:    列表/详情(含事件)/内容流/取消/重试
//   keys:     创建(返回一次明文)/列表/启停/删除
//   models:   远端模型目录(带账号级缓存+内置降级)/管道支持模型清单
//   test:     在线测试生成(复用任务管道，钉账号)/estimate-cost 计价
//   stats:    仪表盘统计
//   settings: 运行期参数读取/热更

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// APIServer 管理 API 依赖集合
type APIServer struct {
	db   *DB
	am   *AccountManager
	tm   *TaskManager
	auth *AuthManager
	rt   *RuntimeSettings
	cfg  *AppConfig

	// 远端模型目录内存缓存（账号级，TTL 10 分钟；refresh=1 强制拉新）
	modelsMu    sync.Mutex
	modelsCache map[string]modelsCacheEntry
}

// modelsCacheEntry 模型目录缓存条目
type modelsCacheEntry struct {
	at  time.Time
	cfg *ModelsConfig
}

// modelsCacheTTL 远端模型目录缓存时长
const modelsCacheTTL = 10 * time.Minute

// NewAPIServer 创建管理 API 服务器
func NewAPIServer(db *DB, am *AccountManager, tm *TaskManager, auth *AuthManager, rt *RuntimeSettings, cfg *AppConfig) *APIServer {
	return &APIServer{db: db, am: am, tm: tm, auth: auth, rt: rt, cfg: cfg,
		modelsCache: make(map[string]modelsCacheEntry)}
}

// ---- 通用响应助手 ----

// writeJSON 写 JSON 响应
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("[api] write json failed: %v", err)
	}
}

// writeAPIError 管理 API 错误响应
func writeAPIError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": message})
}

// RegisterRoutes 注册管理路由（/api/login、/api/logout 由 main 直接挂 auth handler）
func (s *APIServer) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/", s.handleAPI)
}

// handleAPI 统一路由分发。
// 安全：管理 API 仅供同源 Web 界面使用，不发送 CORS 头，
// 配合 Host 校验中间件阻止外部网页跨源读取（防 DNS Rebinding）。
func (s *APIServer) handleAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusOK)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/")
	segments := strings.Split(path, "/")

	switch {
	// ---- 账号 ----
	case len(segments) == 1 && segments[0] == "accounts" && r.Method == http.MethodGet:
		s.handleListAccounts(w, r)
	case len(segments) == 2 && segments[0] == "accounts" && segments[1] == "import" && r.Method == http.MethodPost:
		s.handleImportAccounts(w, r)
	case len(segments) == 3 && segments[0] == "accounts" && segments[2] == "refresh" && r.Method == http.MethodPost:
		s.handleRefreshAccount(w, r, segments[1])
	case len(segments) == 3 && segments[0] == "accounts" && segments[2] == "toggle" && r.Method == http.MethodPost:
		s.handleToggleAccount(w, r, segments[1])
	case len(segments) == 3 && segments[0] == "accounts" && segments[2] == "claim-trial" && r.Method == http.MethodPost:
		s.handleClaimTrial(w, r, segments[1])
	case len(segments) == 3 && segments[0] == "accounts" && segments[2] == "restore-link" && r.Method == http.MethodGet:
		s.handleRestoreLink(w, r, segments[1])
	case len(segments) == 2 && segments[0] == "accounts" && segments[1] == "test-all" && r.Method == http.MethodPost:
		s.handleTestAllAccounts(w, r)
	case len(segments) == 3 && segments[0] == "accounts" && segments[2] == "test" && r.Method == http.MethodPost:
		s.handleTestAccount(w, r, segments[1])
	case len(segments) == 2 && segments[0] == "accounts" && r.Method == http.MethodDelete:
		s.handleDeleteAccount(w, r, segments[1])

	// ---- 任务 ----
	case len(segments) == 1 && segments[0] == "tasks" && r.Method == http.MethodGet:
		s.handleListTasks(w, r)
	case len(segments) == 2 && segments[0] == "tasks" && r.Method == http.MethodGet:
		s.handleGetTask(w, r, segments[1])
	case len(segments) == 3 && segments[0] == "tasks" && segments[2] == "content" && r.Method == http.MethodGet:
		s.handleTaskContent(w, r, segments[1])
	case len(segments) == 3 && segments[0] == "tasks" && segments[2] == "cancel" && r.Method == http.MethodPost:
		s.handleCancelTask(w, r, segments[1])
	case len(segments) == 3 && segments[0] == "tasks" && segments[2] == "retry" && r.Method == http.MethodPost:
		s.handleRetryTask(w, r, segments[1])

	// ---- API Key ----
	case len(segments) == 1 && segments[0] == "keys" && r.Method == http.MethodGet:
		s.handleListKeys(w, r)
	case len(segments) == 1 && segments[0] == "keys" && r.Method == http.MethodPost:
		s.handleCreateKey(w, r)
	case len(segments) == 2 && segments[0] == "keys" && r.Method == http.MethodDelete:
		s.handleDeleteKey(w, r, segments[1])
	case len(segments) == 3 && segments[0] == "keys" && segments[2] == "toggle" && r.Method == http.MethodPost:
		s.handleToggleKey(w, r, segments[1])

	// ---- 统计 / 设置 ----
	case len(segments) == 1 && segments[0] == "stats" && r.Method == http.MethodGet:
		s.handleStats(w, r)
	case len(segments) == 1 && segments[0] == "settings" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, s.rt.Get())
	case len(segments) == 1 && segments[0] == "settings" && r.Method == http.MethodPut:
		s.handleUpdateSettings(w, r)

	// ---- 模型目录 ----
	case len(segments) == 2 && segments[0] == "models" && segments[1] == "remote" && r.Method == http.MethodGet:
		s.handleRemoteModels(w, r)
	case len(segments) == 2 && segments[0] == "models" && segments[1] == "supported" && r.Method == http.MethodGet:
		s.handleSupportedModels(w, r)

	// ---- 在线测试 ----
	case len(segments) == 2 && segments[0] == "test" && segments[1] == "generate" && r.Method == http.MethodPost:
		s.handleTestGenerate(w, r)
	case len(segments) == 1 && segments[0] == "estimate-cost" && r.Method == http.MethodPost:
		s.handleEstimateCost(w, r)

	default:
		writeAPIError(w, http.StatusNotFound, "not found: /api/"+path)
	}
}

// ---- 账号管理 ----

// handleListAccounts GET /api/accounts（token 字段 json:"-" 绝不外泄）
func (s *APIServer) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	accounts, err := s.am.ListAccounts()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if accounts == nil {
		accounts = []*Account{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": accounts})
}

// handleImportAccounts POST /api/accounts/import {content, label_prefix}
func (s *APIServer) handleImportAccounts(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Content     string `json:"content"`
		LabelPrefix string `json:"label_prefix"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if strings.TrimSpace(body.Content) == "" {
		writeAPIError(w, http.StatusBadRequest, "content 不能为空（粘贴深链 URL 或裸 JWT，每行一个）")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	result := s.am.ImportAccounts(ctx, body.Content, body.LabelPrefix)
	writeJSON(w, http.StatusOK, result)
}

// handleRefreshAccount POST /api/accounts/{id}/refresh（续期 + 用户信息 + 余额）
func (s *APIServer) handleRefreshAccount(w http.ResponseWriter, r *http.Request, id string) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	acc, err := s.am.RefreshAccount(ctx, id)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"account": acc, "warning": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"account": acc})
}

// handleToggleAccount POST /api/accounts/{id}/toggle {is_enabled}
func (s *APIServer) handleToggleAccount(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		IsEnabled bool `json:"is_enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := s.am.SetEnabled(id, body.IsEnabled); err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "id": id, "is_enabled": body.IsEnabled})
}

// handleClaimTrial POST /api/accounts/{id}/claim-trial
func (s *APIServer) handleClaimTrial(w http.ResponseWriter, r *http.Request, id string) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	st, err := s.am.ClaimTrial(ctx, id)
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// handleRestoreLink GET /api/accounts/{id}/restore-link[?renew=1]
// 生成官方桌面端恢复深链（minimax-hub-cn://auth-callback?accessToken=<JWT>），
// 在装有 MiniMax Design 的机器上打开即可把账号恢复到官方客户端（导入的逆过程）。
// renew=1 时先续期 token 再生成（旧 token 临期/验证不过时用）。
// 安全：响应含完整 JWT，仅 session 认证可达；日志只打 token 前缀。
func (s *APIServer) handleRestoreLink(w http.ResponseWriter, r *http.Request, id string) {
	renewed := false
	renewWarn := ""
	if r.URL.Query().Get("renew") == "1" {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		if _, err := s.am.TryRenew(ctx, id); err != nil {
			renewWarn = "续期失败，已用当前 token 生成: " + err.Error()
		} else {
			renewed = true
		}
		cancel()
	}
	acc, err := s.am.GetAccount(id)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "账号不存在")
		return
	}
	if acc.Token == "" {
		writeAPIError(w, http.StatusConflict, "账号无 token")
		return
	}
	region := s.cfg.Get().Region
	link, exp, userID := BuildRestoreLink(region, acc.Token)
	log.Printf("[admin] restore-link generated: account=%s user=%s token=%s renewed=%v",
		id, userID, tokenPrefix(acc.Token), renewed)
	resp := map[string]any{
		"account_id": id,
		"url":        link,
		"scheme":     DeepLinkScheme(region),
		"user_id":    orDefault(userID, acc.UserID),
		"token_exp":  exp,
		"renewed":    renewed,
	}
	if exp > 0 {
		resp["token_expired"] = time.Now().Unix() >= exp
		resp["token_exp_human"] = time.Unix(exp, 0).Local().Format("2006-01-02 15:04:05")
	}
	if renewWarn != "" {
		resp["warning"] = renewWarn
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleDeleteAccount DELETE /api/accounts/{id}
func (s *APIServer) handleDeleteAccount(w http.ResponseWriter, r *http.Request, id string) {
	if err := s.am.DeleteAccount(id); err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "id": id})
}

// ---- 任务管理 ----

// handleListTasks GET /api/tasks?status=&account_id=&page=&page_size=
func (s *APIServer) handleListTasks(w http.ResponseWriter, r *http.Request) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	pageSize, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
	if pageSize < 1 || pageSize > 200 {
		pageSize = 20
	}
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	accountID := strings.TrimSpace(r.URL.Query().Get("account_id"))

	where := "WHERE 1=1"
	var args []any
	if status != "" {
		if internal := internalStatuses(status); len(internal) > 0 {
			where += fmt.Sprintf(" AND status IN (%s)", strings.TrimSuffix(strings.Repeat("?,", len(internal)), ","))
			for _, st := range internal {
				args = append(args, st)
			}
		} else {
			where += " AND status=?"
			args = append(args, strings.ToUpper(status))
		}
	}
	if accountID != "" {
		where += " AND account_id=?"
		args = append(args, accountID)
	}

	var total int
	if err := s.db.conn.QueryRow(`SELECT COUNT(*) FROM video_tasks `+where, args...).Scan(&total); err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	query := `SELECT ` + taskCols + ` FROM video_tasks ` + where + ` ORDER BY created_at DESC LIMIT ? OFFSET ?`
	args = append(args, pageSize, (page-1)*pageSize)
	rows, err := s.db.conn.Query(query, args...)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	tasks := []*VideoTask{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			continue
		}
		tasks = append(tasks, t)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tasks": tasks, "total": total, "page": page, "page_size": pageSize,
	})
}

// handleGetTask GET /api/tasks/{id}（含事件时间线）
func (s *APIServer) handleGetTask(w http.ResponseWriter, r *http.Request, id string) {
	t, err := s.tm.GetTask(id)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "任务不存在")
		return
	}
	rows, err := s.db.conn.Query(`SELECT id, task_id, ts, from_status, to_status, message FROM task_events WHERE task_id=? ORDER BY id ASC`, id)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	type taskEvent struct {
		ID         int64  `json:"id"`
		TaskID     string `json:"task_id"`
		Ts         string `json:"ts"`
		FromStatus string `json:"from_status"`
		ToStatus   string `json:"to_status"`
		Message    string `json:"message"`
	}
	events := []taskEvent{}
	for rows.Next() {
		var e taskEvent
		if err := rows.Scan(&e.ID, &e.TaskID, &e.Ts, &e.FromStatus, &e.ToStatus, &e.Message); err != nil {
			continue
		}
		events = append(events, e)
	}
	writeJSON(w, http.StatusOK, map[string]any{"task": t, "events": events})
}

// handleTaskContent GET /api/tasks/{id}/content（session 认证的媒体预览流：视频/图片）
func (s *APIServer) handleTaskContent(w http.ResponseWriter, r *http.Request, id string) {
	t, err := s.tm.GetTask(id)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "任务不存在")
		return
	}
	if t.VideoPath != "" {
		if fi, err := os.Stat(t.VideoPath); err == nil && !fi.IsDir() && fi.Size() > 0 {
			if t.MediaType != TaskMediaImage {
				w.Header().Set("Content-Type", "video/mp4")
			} // 图片任务：ServeFile 按扩展名（.png/.jpg/.webp）推断 Content-Type
			http.ServeFile(w, r, t.VideoPath)
			return
		}
	}
	if t.VideoURL != "" {
		http.Redirect(w, r, t.VideoURL, http.StatusFound)
		return
	}
	writeAPIError(w, http.StatusNotFound, "媒体文件不存在且无 CDN 直链")
}

// handleCancelTask POST /api/tasks/{id}/cancel
func (s *APIServer) handleCancelTask(w http.ResponseWriter, r *http.Request, id string) {
	ok, err := s.tm.RequestCancel(id)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "任务不存在")
		return
	}
	if !ok {
		writeAPIError(w, http.StatusConflict, "任务已处于终态，无法取消")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": "cancelling"})
}

// handleRetryTask POST /api/tasks/{id}/retry
// 仅 FAILED / SUBMIT_UNKNOWN 可重试：复制请求新建任务（旧任务保留供审计）
func (s *APIServer) handleRetryTask(w http.ResponseWriter, r *http.Request, id string) {
	old, err := s.tm.GetTask(id)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "任务不存在")
		return
	}
	if old.Status != TaskFailed && old.Status != TaskSubmitUnknown {
		writeAPIError(w, http.StatusConflict, "仅 FAILED / SUBMIT_UNKNOWN 状态的任务可重试，当前: "+old.Status)
		return
	}
	now := nowStr()
	nt := &VideoTask{
		ID:              newTaskID(),
		APIKeyID:        old.APIKeyID,
		Model:           old.Model,
		Prompt:          old.Prompt,
		RequestJSON:     old.RequestJSON,
		Status:          TaskQueued,
		Duration:        old.Duration,
		Resolution:      old.Resolution,
		MediaType:       old.MediaType,       // 重试保留媒体类型（视频/图片）
		Source:          old.Source,          // 重试保留任务来源
		PinnedAccountID: old.PinnedAccountID, // 重试保留钉住的测试账号
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := s.tm.InsertTask(nt); err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.tm.addEvent(nt.ID, "", TaskQueued, "由任务 "+old.ID+" 人工重试创建")
	s.tm.Enqueue(nt.ID)
	writeJSON(w, http.StatusOK, map[string]any{"task": nt, "from": old.ID})
}

// ---- API Key 管理 ----

// handleListKeys GET /api/keys
func (s *APIServer) handleListKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.auth.ListAPIKeys()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if keys == nil {
		keys = []*APIKey{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": keys})
}

// handleCreateKey POST /api/keys {name, max_tasks, expires_days}（明文只返回一次）
func (s *APIServer) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name        string `json:"name"`
		MaxTasks    int    `json:"max_tasks"`
		ExpiresDays int    `json:"expires_days"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	plain, row, err := s.auth.CreateAPIKey(body.Name, body.MaxTasks, body.ExpiresDays)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"key": plain, "record": row,
		"warning": "明文 Key 仅此一次返回，请立即保存"})
}

// handleDeleteKey DELETE /api/keys/{id}
func (s *APIServer) handleDeleteKey(w http.ResponseWriter, r *http.Request, id string) {
	if err := s.auth.DeleteAPIKey(id); err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "id": id})
}

// handleToggleKey POST /api/keys/{id}/toggle {is_enabled}
func (s *APIServer) handleToggleKey(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		IsEnabled bool `json:"is_enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := s.auth.ToggleAPIKey(id, body.IsEnabled); err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "id": id, "is_enabled": body.IsEnabled})
}

// ---- 统计 ----

// handleStats GET /api/stats
func (s *APIServer) handleStats(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{}
	// 账号统计
	var total, active int
	var balanceTotal float64
	_ = s.db.conn.QueryRow(`SELECT COUNT(*) FROM accounts`).Scan(&total)
	_ = s.db.conn.QueryRow(`SELECT COUNT(*) FROM accounts WHERE is_enabled=1 AND status='active'`).Scan(&active)
	_ = s.db.conn.QueryRow(`SELECT COALESCE(SUM(balance),0) FROM accounts WHERE balance > 0`).Scan(&balanceTotal)
	out["accounts_total"] = total
	out["accounts_active"] = active
	out["balance_total"] = balanceTotal

	// 今日任务（本地时区当天 0 点起）
	now := time.Now()
	midnightStr := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Format(time.RFC3339)
	var tasksToday, succToday, failToday int
	_ = s.db.conn.QueryRow(`SELECT COUNT(*) FROM video_tasks WHERE created_at >= ?`, midnightStr).Scan(&tasksToday)
	_ = s.db.conn.QueryRow(`SELECT COUNT(*) FROM video_tasks WHERE created_at >= ? AND status='SUCCEEDED'`, midnightStr).Scan(&succToday)
	_ = s.db.conn.QueryRow(`SELECT COUNT(*) FROM video_tasks WHERE created_at >= ? AND status IN ('FAILED','SUBMIT_UNKNOWN')`, midnightStr).Scan(&failToday)
	out["tasks_today"] = tasksToday
	out["tasks_succeeded"] = succToday
	out["tasks_failed"] = failToday

	// 累计消耗积分
	var creditsTotal float64
	_ = s.db.conn.QueryRow(`SELECT COALESCE(SUM(credits),0) FROM usage_logs`).Scan(&creditsTotal)
	out["credits_used_total"] = creditsTotal

	// 近 7 天每日任务/积分
	cutoff := time.Now().AddDate(0, 0, -6)
	cutoffStr := time.Date(cutoff.Year(), cutoff.Month(), cutoff.Day(), 0, 0, 0, 0, cutoff.Location()).Format(time.RFC3339)
	rows, err := s.db.conn.Query(`
		SELECT substr(created_at,1,10) AS d, COUNT(*),
		       COALESCE(SUM(CASE WHEN status='SUCCEEDED' THEN credits_estimated ELSE 0 END),0)
		FROM video_tasks WHERE created_at >= ? GROUP BY d ORDER BY d ASC`, cutoffStr)
	daily := []map[string]any{}
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var d string
			var n int
			var credits float64
			if err := rows.Scan(&d, &n, &credits); err == nil {
				daily = append(daily, map[string]any{"date": d, "tasks": n, "credits": credits})
			}
		}
	}
	out["daily"] = daily

	writeJSON(w, http.StatusOK, out)
}

// ---- 设置 ----

// handleUpdateSettings PUT /api/settings（热更 poll_interval/worker_count/并发/安全系数）
func (s *APIServer) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var in SettingsSnapshot
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := s.rt.Update(in); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	log.Printf("[api] settings updated: %+v", s.rt.Get())
	writeJSON(w, http.StatusOK, s.rt.Get())
}

// ---- 模型目录 ----

// supportedModelSpec 任务管道真正支持的模型规格（与 api_video.go validateAndNormalize 保持一致）
type supportedModelSpec struct {
	ID                string   `json:"id"`
	DisplayName       string   `json:"display_name"`
	Description       string   `json:"description"`
	Aliases           []string `json:"aliases"`
	DurationMin       int      `json:"duration_min"`
	DurationMax       int      `json:"duration_max"`
	DurationDefault   int      `json:"duration_default"`
	Resolutions       []string `json:"resolutions"`
	DefaultResolution string   `json:"default_resolution"`
}

// supportedRatios 画幅比例取值（与提交校验一致）
var supportedRatios = []string{"adaptive", "16:9", "9:16", "1:1", "4:3", "3:4", "21:9"}

// supportedModels 内置支持模型清单（H3: duration 4-15 / 768P|2K；Max: 5-15 / 480P|768P）
func supportedModels() []supportedModelSpec {
	return []supportedModelSpec{
		{
			ID: ModelH3, DisplayName: "海螺 03（MiniMax-H3）",
			Description: "MiniMax 海螺 03 视频生成：文生视频/首尾帧/参考素材，duration 4-15s",
			Aliases:     []string{"hailuo-03", "hailuo03", "hailuo_03", "minimax-hailuo-03"},
			DurationMin: 4, DurationMax: 15, DurationDefault: 5,
			Resolutions: []string{"768P", "2K"}, DefaultResolution: "768P",
		},
		{
			ID: ModelH3Max, DisplayName: "海螺 03 Max（MiniMax-H3-Max）",
			Description: "MiniMax 海螺 03 Max 增强版：duration 5-15s，支持 480P/768P",
			Aliases:     []string{"hailuo-03-max", "hailuo03-max", "hailuo_03_max"},
			DurationMin: 5, DurationMax: 15, DurationDefault: 5,
			Resolutions: []string{"480P", "768P"}, DefaultResolution: "768P",
		},
	}
}

// builtinVideoModels 内置模型的目录形态（远端拉取失败时的降级数据，字段对齐 videoModels 条目）
func builtinVideoModels() []map[string]any {
	out := []map[string]any{}
	for _, m := range supportedModels() {
		out = append(out, map[string]any{
			"id":              m.ID,
			"model_name":      m.ID,
			"display_name":    m.DisplayName,
			"description":     m.Description,
			"type":            "video",
			"promptMaxLength": 10000,
			"hot":             false,
			"builtin":         true,
		})
	}
	return out
}

// firstActiveAccount 第一个启用且状态正常的账号（管理端缺省用号）
func (s *APIServer) firstActiveAccount() *Account {
	accounts, err := s.am.ListAccounts()
	if err != nil {
		log.Printf("[api] first active account: list failed: %v", err)
		return nil
	}
	for _, a := range accounts {
		if a.IsEnabled && a.Status == AcctActive {
			return a
		}
	}
	return nil
}

// handleSupportedModels GET /api/models/supported：管道支持模型 + 参数取值范围（供测试页下拉/联动）
func (s *APIServer) handleSupportedModels(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"models": supportedModels(),
		"ratios": supportedRatios,
		// 图片模型（nano_banana 后端）：aspect_ratios 中 auto 提交时映射为空串
		"image_models": []map[string]any{{
			"id":                   ImageModelNanoBanana2Flash,
			"display_name":         "通用图片 2（nano_banana_2_flash）",
			"description":          "Nano Banana 2 Flash 通用图片生成：文生图 / 图生图（参考图 ≤10 张、单图 ≤7MB）",
			"aliases":              []string{"general-image-2", "gpt-image-nano"},
			"aspect_ratios":        append([]string{"auto"}, imageAspectRatios...),
			"resolutions":          imageResolutions,
			"default_resolution":   "1K",
			"max_reference_images": imageMaxRefs,
			"ref_max_bytes":        imageRefMaxBytes,
		}},
		"prompt_max_length":    10000,
		"max_reference_images": 9,
		"max_reference_videos": 3,
		"max_reference_audios": 3,
	})
}

// handleRemoteModels GET /api/models/remote?account_id=&refresh=1
// 拉云端模型目录（按账号内存缓存 10 分钟）；无账号/拉取失败时优雅降级到
// 内置模型清单（source=builtin + error 说明原因），绝不 500。
func (s *APIServer) handleRemoteModels(w http.ResponseWriter, r *http.Request) {
	refresh := r.URL.Query().Get("refresh") == "1"
	accountID := strings.TrimSpace(r.URL.Query().Get("account_id"))

	builtinFallback := func(reason string) {
		writeJSON(w, http.StatusOK, map[string]any{
			"models": builtinVideoModels(), "source": "builtin",
			"error": reason,
		})
	}

	var acc *Account
	if accountID != "" {
		a, err := s.am.GetAccount(accountID)
		if err != nil {
			builtinFallback("指定账号不存在: " + accountID)
			return
		}
		acc = a
	} else {
		acc = s.firstActiveAccount()
		if acc == nil {
			builtinFallback("无可用账号（需要至少一个启用且状态正常的账号才能拉取远端目录）")
			return
		}
	}

	// 账号级缓存（10 分钟）；refresh=1 强制拉新
	if !refresh {
		s.modelsMu.Lock()
		e, ok := s.modelsCache[acc.ID]
		s.modelsMu.Unlock()
		if ok && time.Since(e.at) < modelsCacheTTL {
			s.writeRemoteModels(w, e.cfg, e.at, acc.ID, true)
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	models, cerr := s.am.client.FetchModelsConfig(ctx, acc.Profile())
	if cerr != nil {
		log.Printf("[api] fetch remote models failed (account=%s): %v", acc.ID, cerr.Msg)
		builtinFallback("拉取远端目录失败: " + cerr.Msg)
		return
	}
	now := time.Now()
	s.modelsMu.Lock()
	s.modelsCache[acc.ID] = modelsCacheEntry{at: now, cfg: models}
	s.modelsMu.Unlock()
	s.writeRemoteModels(w, models, now, acc.ID, false)
}

// writeRemoteModels 远端目录成功响应（models=videoModels 全量条目，未识别字段在 raw 内）
func (s *APIServer) writeRemoteModels(w http.ResponseWriter, models *ModelsConfig, at time.Time, accountID string, cached bool) {
	video := models.VideoModels
	if video == nil {
		video = []VideoModelEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"models":                video,
		"source":                "remote",
		"fetched_at":            at.Format(time.RFC3339),
		"account_id":            accountID,
		"cached":                cached,
		"default_text_model_id": models.DefaultTextModelID,
		"counts": map[string]int{
			"video": len(models.VideoModels),
			"image": len(models.ImageModels),
			"audio": len(models.AudioModels),
			"text":  len(models.TextModels),
		},
	})
}

// ---- 在线测试生成 ----

// testGenerateBody POST /api/test/generate 请求体：与 POST /v1/videos 相同，
// 另加可选 account_id（钉账号）与 media_type（video 默认 / image）。
// 图片模式复用同名字段：model/prompt/ratio(=aspect_ratio)/resolution/reference_images。
type testGenerateBody struct {
	createVideoBody
	AccountID string `json:"account_id"` // 钉住指定账号测试（空=自动选号）
	MediaType string `json:"media_type"` // ""|video=视频；image=图片
}

// handleTestGenerate POST /api/test/generate：管理台在线测试提交视频/图片任务。
// 复用 2API 的校验与任务管道；任务标记 source=admin_test、api_key_id 用管理端标记值。
// 钉住账号不存在时不在此拦截：照常建任务，由 worker 置 FAILED(pinned_account_unavailable)。
func (s *APIServer) handleTestGenerate(w http.ResponseWriter, r *http.Request) {
	var body testGenerateBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "请求体 JSON 解析失败: "+err.Error())
		return
	}
	pinned := strings.TrimSpace(body.AccountID)
	mediaType := strings.ToLower(strings.TrimSpace(body.MediaType))

	var (
		task *VideoTask
		err  error
	)
	now := nowStr()
	switch mediaType {
	case "", TaskMediaVideo:
		var req *VideoRequest
		var errMsg string
		if req, errMsg = validateAndNormalize(&body.createVideoBody); req == nil {
			writeAPIError(w, http.StatusBadRequest, errMsg)
			return
		}
		if task, err = s.newAdminTestTask(req.Model, req.Prompt, req, pinned, TaskMediaVideo, req.Resolution, req.Duration, now); err != nil {
			writeAPIError(w, http.StatusInternalServerError, err.Error())
			return
		}
	case TaskMediaImage:
		imgBody := &createImageBody{
			Model:       body.Model,
			Prompt:      body.Prompt,
			AspectRatio: body.Ratio, // 管理台「比例」字段即图片 aspect_ratio
			Size:        "",         // 管理台不传 OpenAI size
			Resolution:  body.Resolution,
			Image:       body.Image,
			Images:      body.ReferenceImages,
		}
		imgReq, _, errMsg := validateAndNormalizeImage(imgBody)
		if imgReq == nil {
			writeAPIError(w, http.StatusBadRequest, errMsg)
			return
		}
		imgReq.ResponseFormat = "url"
		imgReq.CloudIdempotencyKey = newDeviceUUID() // 幂等键建任务时固化，重放复用
		if task, err = s.newAdminTestTask(imgReq.Model, imgReq.Prompt, imgReq, pinned, TaskMediaImage, imgReq.Resolution, 0, now); err != nil {
			writeAPIError(w, http.StatusInternalServerError, err.Error())
			return
		}
	default:
		writeAPIError(w, http.StatusBadRequest, "media_type 只支持 video 或 image")
		return
	}

	if pinned != "" {
		s.tm.addEvent(task.ID, "", TaskQueued, "管理台在线测试创建（钉住账号 "+pinned+"）")
	} else {
		s.tm.addEvent(task.ID, "", TaskQueued, "管理台在线测试创建（自动选号）")
	}
	s.tm.Enqueue(task.ID)
	log.Printf("[api] admin test task created: %s media=%s model=%s pinned=%q", task.ID, task.MediaType, task.Model, pinned)

	writeJSON(w, http.StatusAccepted, map[string]any{
		"id":         task.ID,
		"object":     task.MediaType,
		"status":     "queued",
		"model":      task.Model,
		"source":     task.Source,
		"media_type": task.MediaType,
		"account_id": pinned,
		"created_at": toUnixSeconds(task.CreatedAt),
	})
}

// newAdminTestTask 构造并落库一条管理台测试任务（视频/图片共用；图片任务 duration=0）
func (s *APIServer) newAdminTestTask(model, prompt string, req any, pinned, mediaType, resolution string, duration int, now string) (*VideoTask, error) {
	reqJSON, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("序列化请求失败: %w", err)
	}
	task := &VideoTask{
		ID:              newTaskID(),
		APIKeyID:        AdminTestKeyID,
		AccountID:       pinned,
		PinnedAccountID: pinned,
		Source:          TaskSourceAdminTest,
		MediaType:       mediaType,
		Model:           model,
		Prompt:          prompt,
		RequestJSON:     string(reqJSON),
		Status:          TaskQueued,
		Duration:        duration,
		Resolution:      resolution,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := s.tm.InsertTask(task); err != nil {
		return nil, err
	}
	return task, nil
}

// handleEstimateCost POST /api/estimate-cost
// 视频：{model,duration,resolution,ratio,generate_audio,account_id}
// 图片：{media_type:"image",model,resolution,prompt,images,account_id}
// 用指定或第一个 active 账号调云端 calculate-cost；无可用账号 409。
func (s *APIServer) handleEstimateCost(w http.ResponseWriter, r *http.Request) {
	var body struct {
		MediaType     string   `json:"media_type"` // ""|video=视频；image=图片
		Model         string   `json:"model"`
		Duration      *int     `json:"duration"`
		Resolution    string   `json:"resolution"`
		Ratio         string   `json:"ratio"` // 视频计价不消费 ratio，接受但仅回显
		GenerateAudio *bool    `json:"generate_audio"`
		Prompt        string   `json:"prompt"` // 图片计价用（char_count）
		Images        []string `json:"images"` // 图片计价用（ref_count）
		AccountID     string   `json:"account_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "请求体 JSON 解析失败: "+err.Error())
		return
	}

	isImage := strings.ToLower(strings.TrimSpace(body.MediaType)) == TaskMediaImage

	// —— 参数校验（fail-fast，先于账号选择，避免无账号时把校验错误误报为 409）——
	var (
		vModel, vResolution string
		vDuration           int
		vGenerateAudio      bool
		vCharCount, vRefCnt int
	)
	if isImage {
		m, ok := normalizeImageModel(body.Model)
		if !ok {
			writeAPIError(w, http.StatusBadRequest, fmt.Sprintf("不支持的图片模型 %q（可用: nano_banana_2_flash / general-image-2 / gpt-image-nano）", body.Model))
			return
		}
		vModel = m
		res := strings.ToUpper(strings.TrimSpace(body.Resolution))
		if res == "" || res == "AUTO" {
			res = "1K"
		}
		if !containsString(imageResolutions, res) {
			writeAPIError(w, http.StatusBadRequest, "图片 resolution 只支持 1K / 2K / 4K")
			return
		}
		vResolution = res
		vCharCount = len([]rune(body.Prompt))
		vRefCnt = len(body.Images)
	} else {
		m, ok := normalizeModel(body.Model)
		if !ok {
			writeAPIError(w, http.StatusBadRequest, fmt.Sprintf("不支持的模型 %q（可用: MiniMax-H3, MiniMax-H3-Max, hailuo-03）", body.Model))
			return
		}
		vModel = m
		vDuration = 5
		if body.Duration != nil {
			vDuration = *body.Duration
		}
		vGenerateAudio = true
		if body.GenerateAudio != nil {
			vGenerateAudio = *body.GenerateAudio
		}
		// 与提交管道同规则校验，避免云端 400（range 与 api_video.go validateAndNormalize 一致）
		minDur, maxDur := 4, 15
		if vModel == ModelH3Max {
			minDur = 5
		}
		if vDuration < minDur || vDuration > maxDur {
			writeAPIError(w, http.StatusBadRequest, fmt.Sprintf("duration 必须在 %d-%d 秒之间（%s）", minDur, maxDur, vModel))
			return
		}
		res := strings.ToUpper(strings.TrimSpace(body.Resolution))
		if res == "" {
			res = "768P"
		}
		switch vModel {
		case ModelH3:
			if res != "768P" && res != "2K" {
				writeAPIError(w, http.StatusBadRequest, "MiniMax-H3 的 resolution 只支持 768P 或 2K")
				return
			}
		case ModelH3Max:
			if res != "480P" && res != "768P" {
				writeAPIError(w, http.StatusBadRequest, "MiniMax-H3-Max 的 resolution 只支持 480P 或 768P")
				return
			}
		}
		vResolution = res
	}

	// —— 账号选择：显式指定（须存在）或缺省第一个 active；都没有 → 409 ——
	var acc *Account
	if aid := strings.TrimSpace(body.AccountID); aid != "" {
		a, err := s.am.GetAccount(aid)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "指定账号不存在: "+aid)
			return
		}
		acc = a
	} else {
		acc = s.firstActiveAccount()
		if acc == nil {
			writeAPIError(w, http.StatusConflict, "无可用账号（需要至少一个启用且状态正常的账号才能计价）")
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()

	// —— 云端计价 + 余额 ——
	var est float64
	var cerr *CloudError
	if isImage {
		est, cerr = s.am.client.CalculateImageCost(ctx, acc.Profile(), vModel, vResolution, 1, vCharCount, vRefCnt)
	} else {
		est, cerr = s.am.client.CalculateCost(ctx, acc.Profile(), vModel, vResolution, vDuration, vGenerateAudio)
	}
	if cerr != nil {
		writeAPIError(w, http.StatusBadGateway, "云端计价失败: "+cerr.Msg)
		return
	}
	bal := acc.Balance
	if v, berr := s.am.client.GetBalance(ctx, acc.Profile()); berr == nil {
		bal = v
		s.am.UpdateBalance(acc.ID, v)
	}
	resp := map[string]any{
		"estimated_credits": est,
		"balance":           bal,
		"account_id":        acc.ID,
		"model":             vModel,
		"resolution":        vResolution,
	}
	if isImage {
		resp["media_type"] = TaskMediaImage
	} else {
		resp["media_type"] = TaskMediaVideo
		resp["duration"] = vDuration
		resp["ratio"] = body.Ratio
	}
	writeJSON(w, http.StatusOK, resp)
}

// ---- 账号健康测试 ----

// handleTestAccount POST /api/accounts/{id}/test：user/info + balance + trial 三连
func (s *APIServer) handleTestAccount(w http.ResponseWriter, r *http.Request, id string) {
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	res, err := s.am.TestAccount(ctx, id)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "账号不存在: "+id)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleTestAllAccounts POST /api/accounts/test-all：串行批量测试全部 enabled 账号
// （账号间 500ms 间隔防风控，token 失效自动禁用）
func (s *APIServer) handleTestAllAccounts(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	results := s.am.TestAllAccounts(ctx)
	okCount, failCount, disabledCount := 0, 0, 0
	for i := range results {
		if results[i].OK {
			okCount++
		} else {
			failCount++
		}
		if results[i].Disabled {
			disabledCount++
		}
	}
	out := map[string]any{
		"total":          len(results),
		"ok_count":       okCount,
		"fail_count":     failCount,
		"disabled_count": disabledCount,
		"results":        results,
	}
	if len(results) == 0 {
		out["message"] = "没有已启用的账号可测试，请先导入并启用账号"
	}
	writeJSON(w, http.StatusOK, out)
}
