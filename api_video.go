package main

// ---- 对外 2API（/v1/*，API Key 认证） ----
// new-api / OpenAI Sora 风格的视频生成接口：
//   POST   /v1/videos            提交任务（202，支持 Idempotency-Key 幂等）
//   GET    /v1/videos            任务列表（仅本 Key 的任务）
//   GET    /v1/videos/{id}       任务详情（内部阶段映射为粗粒度状态）
//   GET    /v1/videos/{id}/content  成片下载（本地文件流式+Range / 302 CDN 直链）
//   DELETE /v1/videos/{id}       取消任务
//   GET    /v1/models            模型列表
// 入参校验对齐 MiniMax-H3 硬限制（报告 §7.3）：duration H3:4-15 / Max:5-15，
// resolution H3:768P|2K / Max:480P|768P，参考图≤9、参考视频≤3、参考音频≤3，
// 首尾帧模式与参考素材互斥。

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// VideoRequest 规范化后的视频生成请求（request_json 的存储结构，也是重试的复制源）
type VideoRequest struct {
	Model           string   `json:"model"`
	Prompt          string   `json:"prompt"`
	Duration        int      `json:"duration"`
	Resolution      string   `json:"resolution"`
	Ratio           string   `json:"ratio"`
	GenerateAudio   bool     `json:"generate_audio"`
	FirstFrameImage string   `json:"first_frame_image,omitempty"`
	LastFrameImage  string   `json:"last_frame_image,omitempty"`
	ReferenceImages []string `json:"reference_images,omitempty"`
	ReferenceVideos []string `json:"reference_videos,omitempty"`
	ReferenceAudios []string `json:"reference_audios,omitempty"`
}

// 模型常量与别名映射
const (
	ModelH3    = "MiniMax-H3"
	ModelH3Max = "MiniMax-H3-Max"
)

// normalizeModel 模型别名归一（hailuo-03 系别名 → 官方模型名）
func normalizeModel(m string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(m)) {
	case strings.ToLower(ModelH3), "hailuo-03", "hailuo03", "hailuo_03", "minimax-hailuo-03":
		return ModelH3, true
	case strings.ToLower(ModelH3Max), "hailuo-03-max", "hailuo03-max", "hailuo_03_max":
		return ModelH3Max, true
	}
	return "", false
}

// unmarshalRequest 解析 request_json（worker 管道用）
func unmarshalRequest(raw string, out *VideoRequest) error {
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("request_json 为空")
	}
	return json.Unmarshal([]byte(raw), out)
}

// VideoAPI /v1/* 端点处理器
type VideoAPI struct {
	tm *TaskManager
	am *AccountManager
}

// NewVideoAPI 创建 2API 处理器
func NewVideoAPI(tm *TaskManager, am *AccountManager) *VideoAPI {
	return &VideoAPI{tm: tm, am: am}
}

// RegisterRoutes 注册 /v1/* 路由
func (v *VideoAPI) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/v1/videos", v.routeVideos)
	mux.HandleFunc("/v1/videos/", v.routeVideos)
	mux.HandleFunc("/v1/images", v.routeImages)
	mux.HandleFunc("/v1/images/", v.routeImages)
	mux.HandleFunc("/v1/models", v.handleModels)
}

// routeVideos /v1/videos 与 /v1/videos/{id}[/content] 的方法分发
func (v *VideoAPI) routeVideos(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/videos")
	rest = strings.TrimPrefix(rest, "/")
	key := APIKeyFromContext(r.Context())
	if key == nil {
		writeV1Error(w, http.StatusUnauthorized, "invalid_api_key", "missing api key")
		return
	}

	switch {
	case rest == "" && r.Method == http.MethodPost:
		v.handleCreateVideo(w, r, key)
	case rest == "" && r.Method == http.MethodGet:
		v.handleListVideos(w, r, key)
	case rest != "" && !strings.Contains(rest, "/") && r.Method == http.MethodGet:
		v.handleGetVideo(w, r, key, rest)
	case strings.HasSuffix(rest, "/content") && r.Method == http.MethodGet:
		v.handleVideoContent(w, r, key, strings.TrimSuffix(rest, "/content"))
	case rest != "" && !strings.Contains(rest, "/") && r.Method == http.MethodDelete:
		v.handleCancelVideo(w, r, key, rest)
	default:
		writeV1Error(w, http.StatusMethodNotAllowed, "method_not_allowed", "不支持的方法或路径")
	}
}

// ---- 提交任务 ----

// createVideoBody POST /v1/videos 请求体
// 兼容三种字段约定：本服务原生（duration/resolution/ratio）、
// OpenAI Sora 风格（seconds/size，infinite-canvas 等前端使用）、
// new-api 变体（n_seconds）。显式原生字段优先于别名。
type createVideoBody struct {
	Model           string   `json:"model"`
	Prompt          string   `json:"prompt"`
	Duration        *int     `json:"duration"`
	Seconds         *flexInt `json:"seconds"`     // Sora 风格别名（number 或字符串）
	NSeconds        *flexInt `json:"n_seconds"`   // new-api 变体别名
	Resolution      string   `json:"resolution"`
	Size            string   `json:"size"`   // Sora 风格别名："1280x720" / "720p" / "2k" 等
	Ratio           string   `json:"ratio"`
	GenerateAudio   *bool    `json:"generate_audio"`
	Image           string   `json:"image"` // 单图 = 首帧
	FirstFrameImage string   `json:"first_frame_image"`
	LastFrameImage  string   `json:"last_frame_image"`
	ReferenceImages []string `json:"reference_images"`
	ReferenceVideos []string `json:"reference_videos"`
	ReferenceAudios []string `json:"reference_audios"`
}

// videoRatioSnaps size(WxH) 吸附用的比例档位（与官方 nano-banana snapAspectRatio 同集，
// H3 视频另含 adaptive；此处不含 adaptive——显式给了宽高就应确定比例）
var videoRatioSnaps = []struct {
	label string
	value float64
}{
	{"16:9", 16.0 / 9.0}, {"9:16", 9.0 / 16.0}, {"1:1", 1},
	{"4:3", 4.0 / 3.0}, {"3:4", 3.0 / 4.0}, {"3:2", 3.0 / 2.0},
	{"2:3", 2.0 / 3.0}, {"5:4", 5.0 / 4.0}, {"4:5", 4.0 / 5.0},
	{"21:9", 21.0 / 9.0},
}

// snapVideoRatio 按对数距离吸附最近比例档位（对齐官方 snapAspectRatio 算法）
func snapVideoRatio(ratio float64) string {
	if !(ratio > 0) {
		return ""
	}
	best, bestDiff := "1:1", math.Inf(1)
	for _, c := range videoRatioSnaps {
		diff := math.Abs(math.Log(c.value / ratio))
		if diff < bestDiff {
			bestDiff, best = diff, c.label
		}
	}
	return best
}

// parseVideoSizeAlias 解析 Sora 风格 size 字段 → (resolution, ratio)。
// 支持 "WxH"（如 1280x720，比例吸附+按长边定档）与 "720p"/"1080p"/"2k"/"4k" 简写。
// 解析失败返回空串（调用方保持默认值，不报错——size 是可选别名）。
func parseVideoSizeAlias(size string) (resolution, ratio string) {
	s := strings.ToLower(strings.TrimSpace(size))
	if s == "" {
		return "", ""
	}
	resFromHeight := func(maxSide int) string {
		// H3 档位只有 768P/2K：长边 ≤1600 归 768P，否则 2K（H3-Max 的 480P/768P
		// 由模型校验层兜底纠正，这里给出 H3 语义的最近档位）
		if maxSide <= 1600 {
			return "768P"
		}
		return "2K"
	}
	// 形态1：WxH
	if m := regexp.MustCompile(`^(\d{2,5})\s*[x×*]\s*(\d{2,5})$`).FindStringSubmatch(s); m != nil {
		w, _ := strconv.Atoi(m[1])
		h, _ := strconv.Atoi(m[2])
		if w > 0 && h > 0 {
			maxSide := w
			if h > maxSide {
				maxSide = h
			}
			return resFromHeight(maxSide), snapVideoRatio(float64(w) / float64(h))
		}
		return "", ""
	}
	// 形态2：720p/1080p/2k/4k（可带尾缀 p，数字与字母间可有空格）
	switch strings.TrimSuffix(strings.TrimSpace(s), "p") {
	case "480":
		return "480P", ""
	case "720", "1080":
		return "768P", ""
	case "2k", "1440":
		return "2K", ""
	case "4k", "2160":
		return "2K", "" // H3 无 4K 档，向上取 2K
	}
	return "", ""
}

// validateAndNormalize 入参校验 + 规范化（对齐 H3 硬限制）
func validateAndNormalize(body *createVideoBody) (*VideoRequest, string) {
	model, ok := normalizeModel(body.Model)
	if !ok {
		return nil, fmt.Sprintf("不支持的模型 %q（可用: MiniMax-H3, MiniMax-H3-Max, hailuo-03）", body.Model)
	}
	if strings.TrimSpace(body.Prompt) == "" {
		return nil, "prompt 不能为空"
	}
	if len(body.Prompt) > 10000 {
		return nil, "prompt 过长（≤10000 字符）"
	}

	req := &VideoRequest{
		Model:         model,
		Prompt:        strings.TrimSpace(body.Prompt),
		Duration:      5, // 默认 5s（两个模型都合法）
		GenerateAudio: true,
	}
	if body.Duration != nil {
		req.Duration = *body.Duration
	} else if body.Seconds != nil && int(*body.Seconds) > 0 {
		req.Duration = int(*body.Seconds) // Sora 风格别名
	} else if body.NSeconds != nil && int(*body.NSeconds) > 0 {
		req.Duration = int(*body.NSeconds) // new-api 变体别名
	}
	minDur, maxDur := 4, 15
	if model == ModelH3Max {
		minDur = 5
	}
	if req.Duration < minDur || req.Duration > maxDur {
		return nil, fmt.Sprintf("duration 必须在 %d-%d 秒之间（%s）", minDur, maxDur, model)
	}

	// size 别名（Sora 风格）：显式 resolution/ratio 优先，别名只补空缺
	sizeRes, sizeRatio := parseVideoSizeAlias(body.Size)

	req.Resolution = strings.ToUpper(strings.TrimSpace(body.Resolution))
	fromAlias := false
	if req.Resolution == "" && sizeRes != "" {
		req.Resolution = sizeRes
		fromAlias = true
	}
	if req.Resolution == "" {
		req.Resolution = "768P"
	}
	// 别名来源的档位做跨模型钳制（兼容层语义：尽量成交而不是报错）；
	// 显式 resolution 保持严格校验不变
	if fromAlias {
		if model == ModelH3Max && req.Resolution == "2K" {
			req.Resolution = "768P"
		}
		if model == ModelH3 && req.Resolution == "480P" {
			req.Resolution = "768P"
		}
	}
	switch model {
	case ModelH3:
		if req.Resolution != "768P" && req.Resolution != "2K" {
			return nil, "MiniMax-H3 的 resolution 只支持 768P 或 2K"
		}
	case ModelH3Max:
		if req.Resolution != "480P" && req.Resolution != "768P" {
			return nil, "MiniMax-H3-Max 的 resolution 只支持 480P 或 768P"
		}
	}

	req.Ratio = strings.TrimSpace(body.Ratio)
	if req.Ratio == "" && sizeRatio != "" {
		req.Ratio = sizeRatio
	}
	if req.Ratio == "" {
		req.Ratio = "adaptive"
	}
	if body.GenerateAudio != nil {
		req.GenerateAudio = *body.GenerateAudio
	}

	// 首帧：image 与 first_frame_image 二选一
	req.FirstFrameImage = strings.TrimSpace(body.FirstFrameImage)
	if body.Image != "" {
		if req.FirstFrameImage != "" && body.Image != req.FirstFrameImage {
			return nil, "image 与 first_frame_image 冲突（image 即首帧，只能提供一个）"
		}
		req.FirstFrameImage = strings.TrimSpace(body.Image)
	}
	req.LastFrameImage = strings.TrimSpace(body.LastFrameImage)

	// 素材格式校验：http(s) URL 或 data: URI
	for _, item := range []struct {
		val  string
		what string
	}{
		{req.FirstFrameImage, "first_frame_image"},
		{req.LastFrameImage, "last_frame_image"},
	} {
		if item.val != "" && !strings.HasPrefix(item.val, "http://") &&
			!strings.HasPrefix(item.val, "https://") && !strings.HasPrefix(item.val, "data:") {
			return nil, fmt.Sprintf("%s 必须是 http(s) URL 或 data: URI", item.what)
		}
	}

	validateList := func(list []string, what string, maxN int) ([]string, string) {
		var out []string
		for _, s := range list {
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") && !strings.HasPrefix(s, "data:") {
				return nil, fmt.Sprintf("%s 元素必须是 http(s) URL 或 data: URI", what)
			}
			out = append(out, s)
		}
		if len(out) > maxN {
			return nil, fmt.Sprintf("%s 最多 %d 个（当前 %d）", what, maxN, len(out))
		}
		return out, ""
	}
	var errMsg string
	if req.ReferenceImages, errMsg = validateList(body.ReferenceImages, "reference_images", 9); errMsg != "" {
		return nil, errMsg
	}
	if req.ReferenceVideos, errMsg = validateList(body.ReferenceVideos, "reference_videos", 3); errMsg != "" {
		return nil, errMsg
	}
	if req.ReferenceAudios, errMsg = validateList(body.ReferenceAudios, "reference_audios", 3); errMsg != "" {
		return nil, errMsg
	}

	// 首尾帧模式与任何参考素材互斥（报告 §7.3）
	if (req.FirstFrameImage != "" || req.LastFrameImage != "") &&
		(len(req.ReferenceImages) > 0 || len(req.ReferenceVideos) > 0 || len(req.ReferenceAudios) > 0) {
		return nil, "首尾帧模式（first/last_frame_image）不可与参考素材（reference_*）混用"
	}
	// 只有尾帧没有首帧不合法
	if req.FirstFrameImage == "" && req.LastFrameImage != "" {
		return nil, "提供 last_frame_image 时必须同时提供首帧（image 或 first_frame_image）"
	}
	// 首尾帧模式 ratio 强制 adaptive（报告 §7.3）
	if req.FirstFrameImage != "" || req.LastFrameImage != "" {
		req.Ratio = "adaptive"
	}
	return req, ""
}

// handleCreateVideo POST /v1/videos → 202
func (v *VideoAPI) handleCreateVideo(w http.ResponseWriter, r *http.Request, key *APIKey) {
	var body createVideoBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeV1Error(w, http.StatusBadRequest, "invalid_params", "请求体 JSON 解析失败: "+err.Error())
		return
	}
	req, errMsg := validateAndNormalize(&body)
	if req == nil {
		writeV1Error(w, http.StatusBadRequest, "invalid_params", errMsg)
		return
	}

	// 幂等：Idempotency-Key 命中已有任务直接返回原任务
	idemKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idemKey != "" {
		if len(idemKey) > 128 {
			writeV1Error(w, http.StatusBadRequest, "invalid_params", "Idempotency-Key 过长（≤128 字符）")
			return
		}
		if existing, err := v.tm.FindTaskByIdempotencyKey(key.ID, idemKey); err == nil && existing != nil {
			w.Header().Set("Idempotent-Replay", "true")
			writeJSON(w, http.StatusOK, v.taskResponse(existing))
			return
		}
	}

	reqJSON, err := json.Marshal(req)
	if err != nil {
		writeV1Error(w, http.StatusInternalServerError, "internal_error", "序列化请求失败")
		return
	}
	now := nowStr()
	task := &VideoTask{
		ID:             newTaskID(),
		IdempotencyKey: idemKey,
		APIKeyID:       key.ID,
		Model:          req.Model,
		Prompt:         req.Prompt,
		RequestJSON:    string(reqJSON),
		Status:         TaskQueued,
		Duration:       req.Duration,
		Resolution:     req.Resolution,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := v.tm.InsertTask(task); err != nil {
		writeV1Error(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	consumeAPIKeyTask(v.tm, key.ID)
	v.tm.Enqueue(task.ID)

	writeJSON(w, http.StatusAccepted, map[string]any{
		"id":         task.ID,
		"object":     "video",
		"status":     "queued",
		"model":      task.Model,
		"created_at": toUnixSeconds(task.CreatedAt),
	})
}

// consumeAPIKeyTask Key 配额 +1（通过 TaskManager 持有的 db 直接更新，避免循环依赖）
func consumeAPIKeyTask(tm *TaskManager, keyID string) {
	if _, err := tm.db.conn.Exec(`UPDATE api_keys SET used_tasks=used_tasks+1, last_used_at=? WHERE id=?`,
		nowStr(), keyID); err != nil {
		log.Printf("[2api] consume key task failed: %v", err)
	}
}

// ---- 查询 ----

// taskResponse 对外任务对象（内部阶段 → 粗粒度状态映射）
func (v *VideoAPI) taskResponse(t *VideoTask) map[string]any {
	resp := map[string]any{
		"id":         t.ID,
		"object":     "video",
		"status":     ExternalStatus(t.Status),
		"model":      t.Model,
		"progress":   t.Progress,
		"created_at": toUnixSeconds(t.CreatedAt),
		"seconds":    t.Duration,
		"resolution": t.Resolution,
	}
	// 尺寸信息（infinite-canvas 等下游会读 size/width/height 做展示；
	// 视频任务由下载后探测回填，未回填时省略）
	if t.Width > 0 && t.Height > 0 {
		resp["width"] = t.Width
		resp["height"] = t.Height
		resp["size"] = fmt.Sprintf("%dx%d", t.Width, t.Height)
	}
	if t.StartedAt != "" {
		resp["started_at"] = toUnixSeconds(t.StartedAt)
	}
	if t.CompletedAt != "" {
		resp["completed_at"] = toUnixSeconds(t.CompletedAt)
	}
	switch t.Status {
	case TaskFailed:
		resp["error"] = map[string]string{"code": orDefault(t.ErrorCode, "task_failed"), "message": t.ErrorMsg}
	case TaskSubmitUnknown:
		resp["error"] = map[string]string{
			"code":    "submit_unknown",
			"message": "提交结果不明（可能已扣费），请联系管理员复核。" + t.ErrorMsg,
		}
	}
	if t.Status == TaskSucceeded {
		resp["credits_used"] = t.CreditsEstimated
	} else if t.CreditsEstimated > 0 {
		resp["estimated_credits"] = t.CreditsEstimated
	}
	if t.EstimatedRemainingSec > 0 && ExternalStatus(t.Status) == "processing" {
		resp["estimated_remaining_seconds"] = t.EstimatedRemainingSec
	}
	return resp
}

// handleGetVideo GET /v1/videos/{id}（图片任务不在此端点，走 /v1/images/{id}）
func (v *VideoAPI) handleGetVideo(w http.ResponseWriter, r *http.Request, key *APIKey, id string) {
	t, err := v.tm.GetTask(id)
	if err != nil || t.APIKeyID != key.ID || t.MediaType == TaskMediaImage {
		writeV1Error(w, http.StatusNotFound, "task_not_found", "任务不存在")
		return
	}
	writeJSON(w, http.StatusOK, v.taskResponse(t))
}

// handleListVideos GET /v1/videos?page=&limit=&status=（仅返回本 Key 的任务）
func (v *VideoAPI) handleListVideos(w http.ResponseWriter, r *http.Request, key *APIKey) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit < 1 || limit > 100 {
		limit = 20
	}
	status := strings.TrimSpace(r.URL.Query().Get("status"))

	query := `SELECT ` + taskCols + ` FROM video_tasks WHERE api_key_id=?
		AND (media_type IS NULL OR media_type IN ('', 'video'))`
	args := []any{key.ID}
	if status != "" {
		// 对外粗粒度状态 → 内部阶段集合
		internal := internalStatuses(status)
		if len(internal) > 0 {
			query += fmt.Sprintf(" AND status IN (%s)", strings.TrimSuffix(strings.Repeat("?,", len(internal)), ","))
			for _, s := range internal {
				args = append(args, s)
			}
		} else {
			query += " AND status=?"
			args = append(args, strings.ToUpper(status))
		}
	}
	query += " ORDER BY created_at DESC LIMIT ? OFFSET ?"
	args = append(args, limit+1, (page-1)*limit) // 多取一条判断 has_more

	rows, err := v.tm.db.conn.Query(query, args...)
	if err != nil {
		writeV1Error(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	defer rows.Close()
	var data []map[string]any
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			continue
		}
		data = append(data, v.taskResponse(t))
	}
	hasMore := len(data) > limit
	if hasMore {
		data = data[:limit]
	}
	if data == nil {
		data = []map[string]any{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object":   "list",
		"data":     data,
		"page":     page,
		"has_more": hasMore,
	})
}

// internalStatuses 对外状态 → 内部阶段集合
func internalStatuses(external string) []string {
	switch strings.ToLower(external) {
	case "queued":
		return []string{TaskQueued, TaskPrecheck, TaskUploading, TaskSubmitting}
	case "processing":
		return []string{TaskPolling, TaskRetrieving, TaskDownloading}
	case "succeeded":
		return []string{TaskSucceeded}
	case "failed":
		return []string{TaskFailed, TaskSubmitUnknown}
	case "cancelled":
		return []string{TaskCancelled}
	}
	return nil
}

// handleVideoContent GET /v1/videos/{id}/content
// 本地文件存在 → 200 video/mp4 流式（http.ServeFile 原生支持 Range）；
// 否则有 CDN 直链 → 302；都没有 → 404
func (v *VideoAPI) handleVideoContent(w http.ResponseWriter, r *http.Request, key *APIKey, id string) {
	t, err := v.tm.GetTask(id)
	if err != nil || t.APIKeyID != key.ID || t.MediaType == TaskMediaImage {
		writeV1Error(w, http.StatusNotFound, "task_not_found", "任务不存在")
		return
	}
	if t.VideoPath != "" {
		if fi, err := os.Stat(t.VideoPath); err == nil && !fi.IsDir() && fi.Size() > 0 {
			w.Header().Set("Content-Type", "video/mp4")
			http.ServeFile(w, r, t.VideoPath)
			return
		}
	}
	if t.VideoURL != "" {
		http.Redirect(w, r, t.VideoURL, http.StatusFound)
		return
	}
	writeV1Error(w, http.StatusNotFound, "content_not_ready", "视频尚未就绪或本地文件与直链均不可用")
}

// handleCancelVideo DELETE /v1/videos/{id} → 置 cancel_requested
func (v *VideoAPI) handleCancelVideo(w http.ResponseWriter, r *http.Request, key *APIKey, id string) {
	t, err := v.tm.GetTask(id)
	if err != nil || t.APIKeyID != key.ID || t.MediaType == TaskMediaImage {
		writeV1Error(w, http.StatusNotFound, "task_not_found", "任务不存在")
		return
	}
	ok, err := v.tm.RequestCancel(id)
	if err != nil {
		writeV1Error(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if !ok {
		writeV1Error(w, http.StatusConflict, "already_terminal", "任务已处于终态 "+t.Status)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": "cancelling"})
}

// handleModels GET /v1/models（视频 + 图片模型）
func (v *VideoAPI) handleModels(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data": []map[string]string{
			{"id": ModelH3, "object": "model", "owned_by": "minimax"},
			{"id": ModelH3Max, "object": "model", "owned_by": "minimax"},
			{"id": ImageModelNanoBanana2Flash, "object": "model", "owned_by": "minimax"},
		},
	})
}

// ---- 小工具 ----

// toUnixSeconds RFC3339 → unix 秒（解析失败返回 0）
func toUnixSeconds(s string) int64 {
	t := parseTimeOrZero(s)
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// orDefault 空串兜底
func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// writeV1Error 2API 统一错误包裹
func writeV1Error(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"code":    code,
			"message": message,
			"type":    v1ErrorType(status),
		},
	})
}

// v1ErrorType HTTP 状态 → 错误类型
func v1ErrorType(status int) string {
	switch {
	case status == http.StatusBadRequest || status == http.StatusNotFound || status == http.StatusConflict:
		return "invalid_request_error"
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return "authentication_error"
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	default:
		return "server_error"
	}
}
