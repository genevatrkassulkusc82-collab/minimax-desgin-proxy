package main

// ---- 对外 2API：图片生成（/v1/images/*，API Key 认证） ----
// OpenAI Images 兼容接口（nano_banana 后端，v2 异步任务管道承载）：
//   POST   /v1/images/generations    提交生成（n 1-4 逐张独立云任务；同步等待
//                                    ≤5min，成功返回 OpenAI 格式，超时 202+task_ids）
//   POST   /v1/images/edits          参考图编辑（multipart 上传，OpenAI edits 兼容，
//                                    infinite-canvas 等前端使用；复用 generations 管道）
//   GET    /v1/images/{id}           任务状态（object:"image"，成功带 url）
//   GET    /v1/images/{id}/content   图片二进制（本地文件流式 / 302 CDN 直链）
//   DELETE /v1/images/{id}           取消任务
// 入参对齐官方 nano_banana 硬限制：参考图 ≤10 张、单图 ≤7MB（data URI 内嵌提交，
// 不走 files/upload）、aspect_ratio 十档 + 自动、resolution 1K/2K/4K。

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// 图片模型常量与限制（官方 nano-banana.service / aspect-ratio.util 逆向证据）
const (
	ImageModelNanoBanana2Flash = "nano_banana_2_flash"
	imageMaxRefs               = 10      // 官方 max_refs=10
	imageRefMaxBytes           = 7 << 20 // 官方 NANO_BANANA_INPUT_LIMITS.imageMaxBytes=7MB
	imageSyncWaitMax           = 5 * time.Minute
)

// imageAspectRatios nano_banana 支持的比例档位（官方 SUPPORTED_ASPECT_RATIOS；空串=自动）
var imageAspectRatios = []string{"16:9", "9:16", "1:1", "4:3", "3:4", "3:2", "2:3", "5:4", "4:5", "21:9"}

// imageResolutions nano_banana 分辨率档位（官方 BANANA_RESOLUTIONS）
var imageResolutions = []string{"1K", "2K", "4K"}

// imageSizeToAspect OpenAI size → aspect_ratio 映射（规格书三档 + 常见兼容档）
var imageSizeToAspect = map[string]string{
	"1024x1024": "1:1",
	"1536x1024": "3:2",
	"1024x1536": "2:3",
	"256x256":   "1:1",
	"512x512":   "1:1",
	"1792x1024": "16:9",
	"1024x1792": "9:16",
	"auto":      "",
}

// ImageRequest 规范化后的图片生成请求（image 任务 request_json 的存储结构；
// 每任务恒为 1 张，n>1 由 API 层拆分）
type ImageRequest struct {
	Model               string   `json:"model"`
	Prompt              string   `json:"prompt"`
	AspectRatio         string   `json:"aspect_ratio"`          // 空串=自动
	Resolution          string   `json:"resolution"`            // 1K/2K/4K
	ResponseFormat      string   `json:"response_format"`       // url | b64_json
	Images              []string `json:"images,omitempty"`      // 图生图参考（https/data URI）
	CloudIdempotencyKey string   `json:"cloud_idempotency_key"` // 云端提交幂等键（建任务时固化，重放复用防重复扣费）
}

// unmarshalImageRequest 解析 image 任务的 request_json
func unmarshalImageRequest(raw string, out *ImageRequest) error {
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("request_json 为空")
	}
	return json.Unmarshal([]byte(raw), out)
}

// normalizeImageModel 图片模型别名归一（general-image-2 / gpt-image-nano 等均映射到
// nano_banana_2_flash；空串取默认）
func normalizeImageModel(m string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(m)) {
	case "",
		ImageModelNanoBanana2Flash,
		"nano-banana-2-flash", "nanobanana2flash", "nano_banana_2flash", "nano banana 2 flash",
		"general-image-2", "general_image_2", "general image 2",
		"gpt-image-nano":
		return ImageModelNanoBanana2Flash, true
	}
	return "", false
}

// createImageBody POST /v1/images/generations 请求体（OpenAI Images 兼容 + 扩展字段）
type createImageBody struct {
	Model          string   `json:"model"`
	Prompt         string   `json:"prompt"`
	N              *int     `json:"n"`
	Size           string   `json:"size"`            // OpenAI 尺寸（1024x1024 等）→ aspect_ratio
	AspectRatio    string   `json:"aspect_ratio"`    // 直接指定比例（优先于 size）
	Resolution     string   `json:"resolution"`      // 1K(默认)/2K/4K
	ResponseFormat string   `json:"response_format"` // url(默认) | b64_json
	Image          string   `json:"image"`           // 单张参考图（等价 images[0]）
	Images         []string `json:"images"`          // 图生图参考（≤10 张）
}

// validateAndNormalizeImage 入参校验 + 规范化；返回 (请求, n, 错误消息)
func validateAndNormalizeImage(body *createImageBody) (*ImageRequest, int, string) {
	model, ok := normalizeImageModel(body.Model)
	if !ok {
		return nil, 0, fmt.Sprintf("不支持的图片模型 %q（可用: nano_banana_2_flash，别名 general-image-2 / gpt-image-nano）", body.Model)
	}
	if strings.TrimSpace(body.Prompt) == "" {
		return nil, 0, "prompt 不能为空"
	}
	if len(body.Prompt) > 10000 {
		return nil, 0, "prompt 过长（≤10000 字符）"
	}
	n := 1
	if body.N != nil {
		n = *body.N
	}
	if n < 1 || n > 4 {
		return nil, 0, "n 必须在 1-4 之间（每张图独立云任务）"
	}

	req := &ImageRequest{Model: model, Prompt: strings.TrimSpace(body.Prompt)}

	// response_format
	switch rf := strings.ToLower(strings.TrimSpace(body.ResponseFormat)); rf {
	case "", "url":
		req.ResponseFormat = "url"
	case "b64_json":
		req.ResponseFormat = "b64_json"
	default:
		return nil, 0, "response_format 只支持 url 或 b64_json"
	}

	// aspect_ratio 优先；否则由 size 映射
	ar := strings.TrimSpace(body.AspectRatio)
	if ar == "" && strings.TrimSpace(body.Size) != "" {
		size := strings.ToLower(strings.TrimSpace(body.Size))
		mapped, ok := imageSizeToAspect[size]
		if !ok {
			return nil, 0, fmt.Sprintf("不支持的 size %q（可用: 1024x1024→1:1, 1536x1024→3:2, 1024x1536→2:3；或直接传 aspect_ratio）", body.Size)
		}
		ar = mapped
	}
	if strings.EqualFold(ar, "auto") {
		ar = ""
	}
	if ar != "" && !containsString(imageAspectRatios, ar) {
		return nil, 0, "aspect_ratio 只支持 " + strings.Join(imageAspectRatios, " / ") + "（或 auto/空=自动）"
	}
	req.AspectRatio = ar

	// resolution
	switch res := strings.ToUpper(strings.TrimSpace(body.Resolution)); res {
	case "", "AUTO", "auto":
		req.Resolution = "1K"
	case "1K", "2K", "4K":
		req.Resolution = res
	default:
		return nil, 0, "resolution 只支持 1K / 2K / 4K"
	}

	// 参考图（image 单张与 images 数组合并）
	imgs := append([]string{}, body.Images...)
	if s := strings.TrimSpace(body.Image); s != "" {
		imgs = append([]string{s}, imgs...)
	}
	cleaned := []string{}
	for _, s := range imgs {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") && !strings.HasPrefix(s, "data:") {
			return nil, 0, "image/images 元素必须是 http(s) URL 或 data: URI"
		}
		cleaned = append(cleaned, s)
	}
	if len(cleaned) > imageMaxRefs {
		return nil, 0, fmt.Sprintf("参考图最多 %d 张（当前 %d）", imageMaxRefs, len(cleaned))
	}
	req.Images = cleaned
	return req, n, ""
}

// containsString 字符串切片包含判断
func containsString(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// ---- 路由 ----

// routeImages /v1/images 与 /v1/images/{id}[/content] 的方法分发
func (v *VideoAPI) routeImages(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/images")
	rest = strings.TrimPrefix(rest, "/")
	key := APIKeyFromContext(r.Context())
	if key == nil {
		writeV1Error(w, http.StatusUnauthorized, "invalid_api_key", "missing api key")
		return
	}
	switch {
	case rest == "generations" && r.Method == http.MethodPost:
		v.handleImageGenerations(w, r, key)
	case rest == "edits" && r.Method == http.MethodPost:
		v.handleImageEdits(w, r, key)
	case rest != "" && !strings.Contains(rest, "/") && r.Method == http.MethodGet:
		v.handleGetImage(w, r, key, rest)
	case strings.HasSuffix(rest, "/content") && r.Method == http.MethodGet:
		v.handleImageContent(w, r, key, strings.TrimSuffix(rest, "/content"))
	case rest != "" && !strings.Contains(rest, "/") && r.Method == http.MethodDelete:
		v.handleCancelImage(w, r, key, rest)
	default:
		writeV1Error(w, http.StatusMethodNotAllowed, "method_not_allowed", "不支持的方法或路径")
	}
}

// ---- 提交（同步等待语义） ----

// handleImageGenerations POST /v1/images/generations
// n 张图逐张建任务入队，同步等待全部终态（上限 5 分钟）：
//   - 全部成功 → 200 OpenAI 格式 {created,data:[{url|b64_json}]}；
//   - 任一失败/取消 → 500 error（message 逐任务说明，error.task_ids 可继续查询）；
//   - 超时未完成 → 202 {status:"processing",task_ids}，用 GET /v1/images/{id} 轮询。
func (v *VideoAPI) handleImageGenerations(w http.ResponseWriter, r *http.Request, key *APIKey) {
	var body createImageBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeV1Error(w, http.StatusBadRequest, "invalid_params", "请求体 JSON 解析失败: "+err.Error())
		return
	}
	v.submitImagesAndWait(w, r, key, &body)
}

// handleImageEdits POST /v1/images/edits（OpenAI edits 兼容，multipart/form-data；
// infinite-canvas 等前端的「参考图编辑」走此端点）。
// 表单字段：image（可多值，也接受 image[]/images）、prompt、model、n、size、
// aspect_ratio、resolution、response_format。上传文件转 data URI 后
// 复用 generations 的图生图管道（nano_banana image_paths）。
func (v *VideoAPI) handleImageEdits(w http.ResponseWriter, r *http.Request, key *APIKey) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeV1Error(w, http.StatusBadRequest, "invalid_params", "multipart 表单解析失败: "+err.Error())
		return
	}
	form := r.MultipartForm
	val := func(k string) string {
		if vs := form.Value[k]; len(vs) > 0 {
			return strings.TrimSpace(vs[0])
		}
		return ""
	}
	prompt := val("prompt")
	if prompt == "" {
		writeV1Error(w, http.StatusBadRequest, "invalid_params", "prompt 不能为空")
		return
	}
	var files []*multipart.FileHeader
	for _, field := range []string{"image", "image[]", "images", "images[]"} {
		files = append(files, form.File[field]...)
	}
	if len(files) == 0 {
		writeV1Error(w, http.StatusBadRequest, "invalid_params", "缺少参考图（multipart 字段名 image，可多值）")
		return
	}
	if len(files) > imageMaxRefs {
		writeV1Error(w, http.StatusBadRequest, "invalid_params",
			fmt.Sprintf("参考图最多 %d 张（当前 %d）", imageMaxRefs, len(files)))
		return
	}
	images := make([]string, 0, len(files))
	for i, fh := range files {
		uri, err := readFileAsImageDataURI(fh)
		if err != nil {
			writeV1Error(w, http.StatusBadRequest, "invalid_params", fmt.Sprintf("参考图第 %d 张: %v", i+1, err))
			return
		}
		images = append(images, uri)
	}
	var nPtr *int
	if s := val("n"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			nPtr = &n
		}
	}
	v.submitImagesAndWait(w, r, key, &createImageBody{
		Model:          val("model"),
		Prompt:         prompt,
		N:              nPtr,
		Size:           val("size"),
		AspectRatio:    val("aspect_ratio"),
		Resolution:     val("resolution"),
		ResponseFormat: val("response_format"),
		Images:         images,
	})
}

// readFileAsImageDataURI 读取上传文件转 data URI。magic bytes 判型（扩展名兜底），
// 仅收 jpeg/png/webp、单图 ≤7MB（官方 nano_banana 输入限制）。
func readFileAsImageDataURI(fh *multipart.FileHeader) (string, error) {
	if fh.Size > imageRefMaxBytes {
		return "", fmt.Errorf("大小 %dMB 超过单图 7MB 上限", fh.Size>>20)
	}
	f, err := fh.Open()
	if err != nil {
		return "", err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, imageRefMaxBytes+1))
	if err != nil {
		return "", err
	}
	if int64(len(data)) > imageRefMaxBytes {
		return "", fmt.Errorf("大小超过单图 7MB 上限")
	}
	mime := http.DetectContentType(data)
	switch mime {
	case "image/jpeg", "image/png", "image/webp":
	default:
		// magic bytes 判型兜底：按扩展名纠正（云端最终按内容嗅探，此处只影响 data URI 头）
		name := strings.ToLower(fh.Filename)
		switch {
		case strings.HasSuffix(name, ".jpg"), strings.HasSuffix(name, ".jpeg"):
			mime = "image/jpeg"
		case strings.HasSuffix(name, ".png"):
			mime = "image/png"
		case strings.HasSuffix(name, ".webp"):
			mime = "image/webp"
		default:
			return "", fmt.Errorf("不支持的图片格式 %q（仅 jpeg/png/webp）", mime)
		}
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

// submitImagesAndWait generations/edits 共用：校验 → n 张逐张建任务入队 → 同步等待终态
func (v *VideoAPI) submitImagesAndWait(w http.ResponseWriter, r *http.Request, key *APIKey, body *createImageBody) {
	req, n, errMsg := validateAndNormalizeImage(body)
	if req == nil {
		writeV1Error(w, http.StatusBadRequest, "invalid_params", errMsg)
		return
	}

	now := nowStr()
	tasks := make([]*VideoTask, 0, n)
	for i := 0; i < n; i++ {
		per := *req
		per.CloudIdempotencyKey = newDeviceUUID() // 每张一个幂等键，重试/重放复用防重复扣费
		if len(req.Images) > 0 {
			per.Images = append([]string{}, req.Images...)
		}
		reqJSON, err := json.Marshal(&per)
		if err != nil {
			writeV1Error(w, http.StatusInternalServerError, "internal_error", "序列化请求失败")
			return
		}
		task := &VideoTask{
			ID:          newTaskID(),
			APIKeyID:    key.ID,
			Model:       per.Model,
			Prompt:      per.Prompt,
			RequestJSON: string(reqJSON),
			Status:      TaskQueued,
			MediaType:   TaskMediaImage,
			Resolution:  per.Resolution,
			Source:      TaskSourceAPI,
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		if err := v.tm.InsertTask(task); err != nil {
			writeV1Error(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
		consumeAPIKeyTask(v.tm, key.ID)
		v.tm.Enqueue(task.ID)
		tasks = append(tasks, task)
	}
	ids := make([]string, len(tasks))
	for i, t := range tasks {
		ids[i] = t.ID
	}
	log.Printf("[2api] image generation: %d task(s) created for key %s", n, key.ID)

	// 同步等待全部终态（上限 imageSyncWaitMax；客户端断开则放弃等待，任务继续跑）
	deadline := time.Now().Add(imageSyncWaitMax)
	for time.Now().Before(deadline) {
		allDone := true
		anyBad := false
		fresh := make([]*VideoTask, len(tasks))
		for i, tk := range tasks {
			cur, err := v.tm.GetTask(tk.ID)
			if err != nil {
				cur = tk
			}
			fresh[i] = cur
			if !isTerminalStatus(cur.Status) {
				allDone = false
			} else if cur.Status != TaskSucceeded {
				anyBad = true
			}
		}
		if allDone {
			if anyBad {
				v.writeImageFailure(w, fresh, ids)
			} else {
				v.writeImageSuccess(w, r, fresh, req.ResponseFormat)
			}
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(1500 * time.Millisecond):
		}
	}

	// 超时：202 + task_ids 供轮询
	writeJSON(w, http.StatusAccepted, map[string]any{
		"object":   "image.list",
		"status":   "processing",
		"created":  time.Now().Unix(),
		"task_ids": ids,
		"message":  fmt.Sprintf("同步等待超时（%d 分钟），任务仍在生成，请轮询 GET /v1/images/{task_id}", int(imageSyncWaitMax.Minutes())),
	})
}

// writeImageSuccess 全部成功 → OpenAI Images 响应
func (v *VideoAPI) writeImageSuccess(w http.ResponseWriter, r *http.Request, tasks []*VideoTask, format string) {
	data := make([]map[string]any, 0, len(tasks))
	for _, t := range tasks {
		item := map[string]any{}
		if format == "b64_json" {
			b64, err := readTaskImageB64(t)
			if err != nil {
				writeV1Error(w, http.StatusBadGateway, "image_encode_failed",
					fmt.Sprintf("任务 %s 图片读取失败: %v", t.ID, err))
				return
			}
			item["b64_json"] = b64
		} else {
			item["url"] = absoluteURL(r, "/v1/images/"+t.ID+"/content")
		}
		data = append(data, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"created": time.Now().Unix(), "data": data})
}

// writeImageFailure 任一任务失败/取消 → 500 error（逐任务说明 + task_ids）
func (v *VideoAPI) writeImageFailure(w http.ResponseWriter, tasks []*VideoTask, ids []string) {
	msgs := make([]string, 0, len(tasks))
	for _, t := range tasks {
		switch t.Status {
		case TaskSucceeded:
			msgs = append(msgs, t.ID+": succeeded（可经 content 端点获取）")
		case TaskCancelled:
			msgs = append(msgs, t.ID+": cancelled")
		default:
			msgs = append(msgs, fmt.Sprintf("%s: [%s] %s", t.ID, orDefault(t.ErrorCode, "task_failed"), t.ErrorMsg))
		}
	}
	writeJSON(w, http.StatusInternalServerError, map[string]any{
		"error": map[string]any{
			"code":     "image_generation_failed",
			"message":  strings.Join(msgs, "; "),
			"type":     "server_error",
			"task_ids": ids,
		},
	})
}

// readTaskImageB64 读取任务图片并 base64（本地文件优先，缺失时拉 CDN 直链）
func readTaskImageB64(t *VideoTask) (string, error) {
	if t.VideoPath != "" {
		if b, err := os.ReadFile(t.VideoPath); err == nil && len(b) > 0 {
			return base64.StdEncoding.EncodeToString(b), nil
		}
	}
	if t.VideoURL == "" {
		return "", fmt.Errorf("本地文件与 CDN 直链均不可用")
	}
	resp, err := http.Get(t.VideoURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("CDN http %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return "", err
	}
	if len(b) == 0 {
		return "", fmt.Errorf("CDN 返回空内容")
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// ---- 查询 / 内容 / 取消 ----

// taskResponseImage 图片任务对外对象（object:"image"）
func (v *VideoAPI) taskResponseImage(r *http.Request, t *VideoTask) map[string]any {
	resp := map[string]any{
		"id":         t.ID,
		"object":     "image",
		"status":     ExternalStatus(t.Status),
		"model":      t.Model,
		"progress":   t.Progress,
		"created_at": toUnixSeconds(t.CreatedAt),
		"resolution": t.Resolution,
	}
	if t.Width > 0 {
		resp["width"] = t.Width
	}
	if t.Height > 0 {
		resp["height"] = t.Height
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
	case TaskSucceeded:
		resp["url"] = absoluteURL(r, "/v1/images/"+t.ID+"/content")
		resp["credits_used"] = t.CreditsEstimated
	}
	return resp
}

// getImageTask 取任务并校验归属 + 媒体类型（图片任务专用，视频任务 404）
func (v *VideoAPI) getImageTask(key *APIKey, id string) (*VideoTask, bool) {
	t, err := v.tm.GetTask(id)
	if err != nil || t.APIKeyID != key.ID || t.MediaType != TaskMediaImage {
		return nil, false
	}
	return t, true
}

// handleGetImage GET /v1/images/{id}
func (v *VideoAPI) handleGetImage(w http.ResponseWriter, r *http.Request, key *APIKey, id string) {
	t, ok := v.getImageTask(key, id)
	if !ok {
		writeV1Error(w, http.StatusNotFound, "task_not_found", "图片任务不存在")
		return
	}
	writeJSON(w, http.StatusOK, v.taskResponseImage(r, t))
}

// handleImageContent GET /v1/images/{id}/content
// 本地文件存在 → 200 流式（Content-Type 按扩展名）；否则 302 CDN 直链；都无 → 404
func (v *VideoAPI) handleImageContent(w http.ResponseWriter, r *http.Request, key *APIKey, id string) {
	t, ok := v.getImageTask(key, id)
	if !ok {
		writeV1Error(w, http.StatusNotFound, "task_not_found", "图片任务不存在")
		return
	}
	if t.VideoPath != "" {
		if fi, err := os.Stat(t.VideoPath); err == nil && !fi.IsDir() && fi.Size() > 0 {
			http.ServeFile(w, r, t.VideoPath) // Content-Type 按扩展名（.png/.jpg/.webp）
			return
		}
	}
	if t.VideoURL != "" {
		http.Redirect(w, r, t.VideoURL, http.StatusFound)
		return
	}
	writeV1Error(w, http.StatusNotFound, "content_not_ready", "图片尚未就绪或本地文件与直链均不可用")
}

// handleCancelImage DELETE /v1/images/{id}
func (v *VideoAPI) handleCancelImage(w http.ResponseWriter, r *http.Request, key *APIKey, id string) {
	t, ok := v.getImageTask(key, id)
	if !ok {
		writeV1Error(w, http.StatusNotFound, "task_not_found", "图片任务不存在")
		return
	}
	okCancel, err := v.tm.RequestCancel(id)
	if err != nil {
		writeV1Error(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if !okCancel {
		writeV1Error(w, http.StatusConflict, "already_terminal", "任务已处于终态 "+t.Status)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": "cancelling"})
}

// absoluteURL 构造绝对地址（反代场景优先 X-Forwarded-Proto）
func absoluteURL(r *http.Request, path string) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if fp := r.Header.Get("X-Forwarded-Proto"); fp != "" {
		scheme = fp
	}
	return scheme + "://" + r.Host + path
}
