package main

// ---- 图片任务管道（nano_banana v2 异步） ----
// 与视频共用状态机骨架（QUEUED → PRECHECK → UPLOADING → SUBMITTING → POLLING
// → DOWNLOADING → SUCCEEDED），差异点：
//   - UPLOADING 不走 files/upload：参考图压缩上限 7MB，直接以 data URI 内嵌提交体
//     （官方 encodeImagePathsCompressed 语义）；
//   - 计价用 media_type=image（char_count/ref_count）；
//   - POLLING 的 success 响应直接携带 image_url/width/height，跳过 RETRIEVING；
//   - 落盘 data/images/{task_id}.{ext}（magic bytes 判扩展名，供 Content-Type）；
//   - 提交体带固定 idempotency_key（建任务时生成、存 request_json），
//     重放由云端幂等去重，防重复扣费。
// 选号/钉账号/换号/取消/重启恢复/reconcile 全部复用视频管道既有逻辑。

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"
)

// runImageStages 图片任务阶段推进（runAttempt 在 media_type=image 时转入）
func (tm *TaskManager) runImageStages(ctx context.Context, t *VideoTask, acct *Account, needCredit *float64) attemptOutcome {
	var req ImageRequest
	if err := unmarshalImageRequest(t.RequestJSON, &req); err != nil {
		tm.failTask(t, "invalid_request", "request_json 解析失败: "+err.Error())
		return outcomeFail
	}
	backend, ok := ImageBackendForModel(req.Model)
	if !ok {
		tm.failTask(t, "unsupported_image_model", "未注册的图片模型后端: "+req.Model)
		return outcomeFail
	}

	stage := t.Status
	if stage == TaskQueued {
		stage = TaskPrecheck
	}

	// ---- PRECHECK：图片计价 + 余额预检 ----
	if stage == TaskPrecheck {
		tm.setStage(t, TaskPrecheck, "预检：图片计价 + 余额")
		est, bal := tm.stageImagePrecheck(ctx, t, acct, &req)
		if est > 0 {
			*needCredit = est
			safety := tm.rt.BalanceSafetyFactor()
			if bal >= 0 && bal < est*safety {
				// 分级处置（与视频管道一致）：耗尽才标 empty，跑不起本任务仅本任务排除
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
	if tm.cancelIfRequested(t, "任务在准备参考图前被取消") {
		return outcomeSuccess
	}

	// ---- UPLOADING：参考图解析为 data URI（SUBMITTING 恢复时重走，重放有幂等键保护） ----
	var submitReq ImageSubmitRequest
	if stage == TaskUploading || stage == TaskSubmitting {
		tm.setStage(t, TaskUploading, "准备参考图")
		if res := tm.stageImageUpload(ctx, t, &req, &submitReq); res != outcomeSuccess {
			return res
		}
		stage = TaskSubmitting
	}

	// ---- SUBMITTING：v2 异步提交 ----
	if stage == TaskSubmitting {
		tm.setStage(t, TaskSubmitting, "提交图片生成")
		res := tm.stageImageSubmit(ctx, t, acct, backend, &submitReq)
		if res != outcomeSuccess {
			return res
		}
		stage = TaskPolling
	}

	// ---- POLLING：轮询直到云端终态（success 直接带 image_url，跳过 RETRIEVING） ----
	if stage == TaskPolling {
		res := tm.stageImagePoll(ctx, t, acct, backend)
		if res != outcomeSuccess {
			return res
		}
		stage = TaskDownloading
	}

	// ---- DOWNLOADING：图片落盘 ----
	if stage == TaskDownloading {
		tm.setStage(t, TaskDownloading, "下载图片到本地")
		res := tm.stageImageDownload(ctx, t, acct)
		if res != outcomeSuccess {
			return res
		}
	}
	return outcomeSuccess
}

// stageImagePrecheck 图片预检：预估已知（选号前完成）则只查实时余额；
// 未知时计价（优先参数指纹缓存）+ 余额查询。
// 计价失败不阻断——云端自身会拦余额不足，语义与视频 stagePrecheck 一致。
func (tm *TaskManager) stageImagePrecheck(ctx context.Context, t *VideoTask, acct *Account, req *ImageRequest) (float64, float64) {
	profile := acct.Profile()
	bal := acct.Balance
	est := t.CreditsEstimated
	if est <= 0 {
		charCount := utf8.RuneCountInString(req.Prompt) // 对齐官方 prompt.length 语义（BMP 内近似）
		key := costCacheKeyImage(req.Model, req.Resolution, charCount, len(req.Images))
		if v, ok := tm.cachedCost(key); ok {
			est = v
		} else if v, cerr := tm.client.CalculateImageCost(ctx, profile, req.Model, req.Resolution, 1, charCount, len(req.Images)); cerr == nil {
			est = v
			tm.putCostCache(key, v)
		} else {
			log.Printf("[worker] %s image calculate-cost failed: %v", t.ID, cerr.Msg)
		}
	}
	if v, berr := tm.client.GetBalance(ctx, profile); berr == nil {
		bal = v
		tm.am.UpdateBalance(acct.ID, v)
	}
	if est > 0 {
		tm.setField(t.ID, "credits_estimated", est)
		t.CreditsEstimated = est
		tm.addEvent(t.ID, TaskPrecheck, TaskPrecheck, fmt.Sprintf("预估消耗 %g 积分，账号余额 %g", est, bal))
	}
	return est, bal
}

// stageImageUpload 参考图解析为 data URI（单图 ≤7MB，官方 nano_banana 输入限制）；
// 不走 files/upload，直接内嵌提交体。
func (tm *TaskManager) stageImageUpload(ctx context.Context, t *VideoTask, req *ImageRequest, out *ImageSubmitRequest) attemptOutcome {
	tm.buildImageSubmitFromRequest(req, out)
	for i, spec := range req.Images {
		dataURI, cerr := tm.resolveAssetWithLimit(ctx, spec, "image", imageRefMaxBytes)
		if cerr != nil {
			if cerr.Kind == KindClient {
				tm.failTask(t, "invalid_asset", cerr.Msg)
				return outcomeFail
			}
			tm.addEvent(t.ID, TaskUploading, TaskUploading, "参考图获取失败: "+cerr.Msg)
			return outcomeSwitch
		}
		out.ImagePaths[i] = dataURI
	}
	if len(req.Images) > 0 {
		tm.addEvent(t.ID, TaskUploading, TaskUploading, fmt.Sprintf("参考图就绪（%d 张，data URI 内嵌）", len(req.Images)))
	} else {
		tm.addEvent(t.ID, TaskUploading, TaskUploading, "无参考图（文生图）")
	}
	return outcomeSuccess
}

// buildImageSubmitFromRequest 由规范化请求构造 v2 提交体（参考图槽位留待上传阶段填充）
func (tm *TaskManager) buildImageSubmitFromRequest(req *ImageRequest, out *ImageSubmitRequest) {
	out.Prompt = req.Prompt
	out.ModelName = req.Model
	out.AspectRatio = req.AspectRatio
	out.Resolution = req.Resolution
	out.IdempotencyKey = req.CloudIdempotencyKey
	if out.IdempotencyKey == "" {
		out.IdempotencyKey = newDeviceUUID() // 兜底：正常路径在建任务时已生成并固化
	}
	out.ImagePaths = make([]string, len(req.Images))
	copy(out.ImagePaths, req.Images)
}

// stageImageSubmit v2 提交（错误语义与视频 stageSubmit 同构）：
// 结果不明 → SUBMIT_UNKNOWN 绝不自动重试；401 → 续期重放一次；
// 1033/5xx → 冷却换号；余额不足 → empty 换号；参数/未知 → 终态。
func (tm *TaskManager) stageImageSubmit(ctx context.Context, t *VideoTask, acct *Account, backend ImageBackendPaths, req *ImageSubmitRequest) attemptOutcome {
	var cloudTaskID string
	profile := acct.Profile()

	submitOnce := func(p DeviceProfile) *CloudError {
		id, cerr := tm.client.SubmitImage(ctx, p, backend, *req)
		cloudTaskID = id
		return cerr
	}
	cerr := submitOnce(profile)
	if cerr != nil {
		if cerr.Uncertain {
			tm.markSubmitUnknown(t.ID, "图片提交结果不明: "+cerr.Msg)
			return outcomeFail
		}
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
					tm.markSubmitUnknown(t.ID, "图片提交结果不明: "+cerr.Msg)
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

	now := nowStr()
	if _, err := tm.db.conn.Exec(`UPDATE video_tasks SET cloud_task_id=?, status=?, submitted_at=?, updated_at=? WHERE id=?`,
		cloudTaskID, TaskPolling, now, now, t.ID); err != nil {
		log.Printf("[worker] persist image cloud_task_id failed: %v", err)
	}
	t.CloudTaskID = cloudTaskID
	t.SubmittedAt = now
	t.Status = TaskPolling
	tm.addEvent(t.ID, TaskSubmitting, TaskPolling, "云端受理 task_id="+cloudTaskID)
	return outcomeSuccess
}

// stageImagePoll 轮询 v2 图片任务：节奏/退避/取消/超时/续期语义与视频 stagePoll 一致。
// success 响应直接带 image_url/width/height（跳过 RETRIEVING）；
// 失败包 base.message 优先（v2 图片），base_resp.status_msg 兜底（双容忍）。
func (tm *TaskManager) stageImagePoll(ctx context.Context, t *VideoTask, acct *Account, backend ImageBackendPaths) attemptOutcome {
	cfg := tm.cfg.Get()
	interval := tm.rt.PollInterval()
	maxInterval := time.Duration(cfg.PollMaxIntervalSec) * time.Second
	backoff := cfg.PollBackoffFactor
	profile := acct.Profile()
	failStreak := 0

	for {
		// 取消请求：调云端 cancel 后置 CANCELLED（404=任务已不存在，视为取消成功）
		if tm.isCancelRequested(t.ID) {
			tm.addEvent(t.ID, TaskPolling, TaskPolling, "收到取消请求，向云端发送 cancel")
			if cerr := tm.client.CancelTask(ctx, profile, t.CloudTaskID); cerr != nil {
				if cerr.HTTPStatus == http.StatusNotFound {
					tm.addEvent(t.ID, TaskPolling, TaskPolling, "云端返回任务不存在(404)，已终止，视为取消成功")
				} else {
					tm.addEvent(t.ID, TaskPolling, TaskPolling, "云端 cancel 返回: "+cerr.Msg+"（继续置取消终态）")
				}
			}
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

		st, cerr := tm.client.QueryImageTask(ctx, profile, backend, t.CloudTaskID)
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

		// 进度：图片查询无预计剩余秒字段，按阶段给保守进度
		progress := t.Progress
		if st.Status == "processing" {
			progress = clampInt(maxInt(t.Progress, 50), 50, 95)
		} else {
			progress = clampInt(maxInt(t.Progress, 10), 10, 95)
		}
		if _, err := tm.db.conn.Exec(`UPDATE video_tasks SET progress=?, updated_at=? WHERE id=?`,
			progress, nowStr(), t.ID); err != nil {
			log.Printf("[worker] update image poll progress failed: %v", err)
		}
		t.Progress = progress

		switch st.Status {
		case "", "pending", "processing", "queue":
			interval = tm.nextInterval(interval, backoff, maxInterval, 0)
		case "success":
			if st.ImageURL == "" {
				tm.failTask(t, "no_image_url", "云端 success 但未返回 image_url")
				return outcomeFail
			}
			now := nowStr()
			if _, err := tm.db.conn.Exec(`UPDATE video_tasks SET video_url=?, width=?, height=?, status=?, progress=96, updated_at=? WHERE id=?`,
				st.ImageURL, int(st.Width), int(st.Height), TaskDownloading, now, t.ID); err != nil {
				log.Printf("[worker] set image downloading failed: %v", err)
			}
			t.VideoURL = st.ImageURL
			t.Width = int(st.Width)
			t.Height = int(st.Height)
			t.Status = TaskDownloading
			tm.addEvent(t.ID, TaskPolling, TaskDownloading, fmt.Sprintf("图片生成完成（%dx%d），转下载", t.Width, t.Height))
			return outcomeSuccess
		case "cancelled":
			tm.finishCancelled(t, "云端任务已取消")
			return outcomeTerminal
		case "failed", "fail":
			// 用户已请求取消而云端先终止：按取消收敛，避免误标失败
			if tm.isCancelRequested(t.ID) {
				tm.finishCancelled(t, "用户取消（云端任务已终止: "+st.ErrMessage()+"）")
				return outcomeTerminal
			}
			code := "task_failed"
			if st.ErrCode() != 0 {
				code = fmt.Sprintf("upstream_%d", st.ErrCode())
			}
			msg := st.ErrMessage()
			if st.Base.UserMessage != "" {
				msg += "（" + st.Base.UserMessage + "）"
			}
			tm.failTask(t, code, msg)
			return outcomeFail
		default:
			tm.addEvent(t.ID, TaskPolling, TaskPolling, "未知云端状态: "+st.Status)
			interval = tm.nextInterval(interval, backoff, maxInterval, 0)
		}
	}
}

// stageImageDownload 图片落盘 data/images/{task_id}.{ext}（完整性校验失败重试 ≤3；
// 扩展名按 magic bytes 判定，供 content 端点推断 Content-Type）
func (tm *TaskManager) stageImageDownload(ctx context.Context, t *VideoTask, acct *Account) attemptOutcome {
	if t.VideoURL == "" {
		tm.failTask(t, "download_failed", "无可用图片直链")
		return outcomeFail
	}
	imageDir := tm.cfg.Get().ImageDir
	if err := os.MkdirAll(imageDir, 0755); err != nil {
		tm.failTask(t, "download_failed", "创建图片目录失败: "+err.Error())
		return outcomeFail
	}
	tmpDest := filepath.Join(imageDir, t.ID+".img")
	timeout := time.Duration(tm.cfg.Get().DownloadTimeoutSec) * time.Second

	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		dlCtx, cancel := context.WithTimeout(ctx, timeout)
		lastErr = tm.client.DownloadFile(dlCtx, t.VideoURL, tmpDest)
		cancel()
		if lastErr == nil {
			break
		}
		tm.addEvent(t.ID, TaskDownloading, TaskDownloading, fmt.Sprintf("下载失败(%d/3): %v", attempt, lastErr))
		if !tm.sleep(time.Duration(attempt*5) * time.Second) {
			return outcomeRequeue
		}
	}
	dest := tmpDest
	if lastErr != nil {
		// 本地下载失败但 CDN 直链仍在：降级为"仅直链"交付（content 端点会 302）
		tm.addEvent(t.ID, TaskDownloading, TaskDownloading, "本地落盘失败，降级为 CDN 直链交付: "+lastErr.Error())
		dest = ""
	} else if ext := detectImageExt(tmpDest); ext != "" {
		final := filepath.Join(imageDir, t.ID+ext)
		if err := os.Rename(tmpDest, final); err == nil {
			dest = final
		}
	}
	if dest != "" {
		tm.setField(t.ID, "video_path", dest)
		t.VideoPath = dest
	}

	now := nowStr()
	if _, err := tm.db.conn.Exec(`UPDATE video_tasks SET status=?, progress=100, completed_at=?, updated_at=? WHERE id=?`,
		TaskSucceeded, now, now, t.ID); err != nil {
		log.Printf("[worker] set image succeeded failed: %v", err)
	}
	t.Status = TaskSucceeded
	t.CompletedAt = now
	tm.am.MarkSuccess(acct.ID)
	tm.writeUsageLog(t, TaskSucceeded)
	tm.addEvent(t.ID, TaskDownloading, TaskSucceeded, "图片任务完成")
	log.Printf("[worker] image task %s SUCCEEDED (account=%s credits=%g)", t.ID, acct.ID, t.CreditsEstimated)
	return outcomeSuccess
}

// detectImageExt 按 magic bytes 判图片扩展名（未知返回空串，保留 .img）
func detectImageExt(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	buf := make([]byte, 512)
	n, _ := f.Read(buf)
	switch http.DetectContentType(buf[:n]) {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	}
	return ""
}
