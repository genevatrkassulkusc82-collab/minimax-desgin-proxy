package main

// ---- MiniMax 云端协议客户端（防腐层） ----
// 唯一封装云端协议细节的模块：公共 Query 参数、双头鉴权、两套域名、
// 响应包裹解析（账号域 statusInfo / 媒体类 base_resp / 业务类裸 JSON）、
// 错误分类。协议事实全部来自逆向报告（MiniMax-Design-接口分析报告.md）：
//   - 鉴权头：token: <JWT> + Authorization: Bearer <JWT>（§2.1）
//   - 公共参数：device_platform/app_id/version_code/biz_id/unix/os_name/
//     cpu_core_num/device_memory/uuid/device_id/download_source（§2.2）
//   - 账号域：/v1/api/user/info、/v1/api/user/renewal（§3.2）
//   - 云网关：credit/promotions/files/video 各端点（§5/§7/§8/§9）

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// ---- 宽容数字类型 ----
// 云端各接口数字字段的 string/number 形态不稳定：实测 credit/balance 的
// total_credit 返回 JSON 字符串（如 "1234"）；官方网关侧对同类字段一律做
// 双形态兼容（asNonNegativeCredits 接受 number 与 /^\d+$/ 字符串，
// numberField2 对 base_resp.status_code / estimated_remaining_wait_seconds
// 等同样接受字符串）。因此客户端所有云端响应数字字段必须宽容解析：
// flexFloat/flexInt 解码时接受 JSON number、数字字符串（含小数）、
// 空串与 null（按 0 处理），仅在真正非法时报错；编码仍输出 number，
// 对前端/下游契约不变。

// flexFloat 宽容 float64（云端可能返回 number 或数字字符串）
type flexFloat float64

// flexInt 宽容 int（云端可能返回 number 或数字字符串，小数截断取整）
type flexInt int

// parseFlexNumber 统一解析 number / 数字字符串两种 JSON 形态；
// 空串与 null 视为 0（防御：云端偶发空值不应中断整个响应解析）
func parseFlexNumber(data []byte) (float64, error) {
	s := strings.TrimSpace(string(data))
	if s == "" || s == "null" {
		return 0, nil
	}
	tryParse := func(v string) (float64, bool) {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return 0, false
		}
		return f, true
	}
	// 形态1：JSON number（含科学计数法）
	if f, ok := tryParse(s); ok {
		return f, nil
	}
	// 形态2：JSON 字符串包裹的数字（官方 asNonNegativeCredits / numberField2 的字符串分支）
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		inner, err := strconv.Unquote(s)
		if err != nil {
			return 0, fmt.Errorf("invalid numeric string %s", s)
		}
		inner = strings.TrimSpace(inner)
		if inner == "" {
			return 0, nil // 空串按 0
		}
		if f, ok := tryParse(inner); ok {
			return f, nil
		}
		return 0, fmt.Errorf("cannot parse %q as number", inner)
	}
	return 0, fmt.Errorf("cannot parse %s as number", s)
}

// UnmarshalJSON 宽容解码（number / 数字字符串 / 空串 / null）
func (f *flexFloat) UnmarshalJSON(data []byte) error {
	v, err := parseFlexNumber(data)
	if err != nil {
		return err
	}
	*f = flexFloat(v)
	return nil
}

// UnmarshalJSON 宽容解码；小数值向零截断（对齐官方 Number() 转换语义）
func (i *flexInt) UnmarshalJSON(data []byte) error {
	v, err := parseFlexNumber(data)
	if err != nil {
		return err
	}
	*i = flexInt(math.Trunc(v))
	return nil
}

// DeviceProfile 单账号的设备指纹档案 + 凭据（公共参数取值来源）。
// os_name/cpu_core_num/device_memory/uuid 导入时生成后终身不变，保证指纹一致。
type DeviceProfile struct {
	Token    string // JWT accessToken
	DeviceID string // JWT payload user.deviceID
	UUID     string // 公共参数 uuid（与 DeviceID 同源）
	OsName   string // Windows | macOS
	CPUCores int
	MemoryGB int
	GroupID  string // 非空时附加 X-Group-Id（团队计费域）
	Lang     string // X-Hilo-Lang，默认 zh
}

// 错误分类（驱动上层任务状态机与账号处置）
type CloudErrorKind int

const (
	KindOK        CloudErrorKind = iota // 成功（不会出现在 error 里）
	KindAuth                            // HTTP 401/403：token 失效
	KindBusy                            // base_resp 1033：服务繁忙，可换号重试
	KindNoCredit                        // 余额不足 billing_insufficient_balance
	KindTransient                       // 网络/超时/5xx：可退避重试
	KindClient                          // 4xx 参数类：任务终态失败
	KindUnknown                         // 未知云端错误码：保守终态，不自动重试
)

// CloudError 分类后的云端错误
type CloudError struct {
	Kind       CloudErrorKind
	HTTPStatus int    // HTTP 状态码（0=传输层错误）
	Code       int    // base_resp.status_code 或 statusInfo.code
	Msg        string // 可读错误信息
	Uncertain  bool   // true=请求结果不明（超时/连接中断），提交阶段用于 SUBMIT_UNKNOWN 判定
}

func (e *CloudError) Error() string {
	return fmt.Sprintf("cloud error kind=%d http=%d code=%d: %s", e.Kind, e.HTTPStatus, e.Code, e.Msg)
}

// 结果不明的传输层错误特征（超时/连接被重置等）
func isUncertainTransport(err error) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	s := err.Error()
	for _, kw := range []string{"timeout", "connection reset", "EOF", "broken pipe", "TLS handshake"} {
		if strings.Contains(s, kw) {
			return true
		}
	}
	return false
}

// MiniMaxClient 云端协议客户端（无状态，账号凭据按调用传入）
type MiniMaxClient struct {
	cfg      *AppConfig
	http     *http.Client // 常规请求（30s）
	httpLong *http.Client // 上传/下载（长超时）
}

// NewMiniMaxClient 创建客户端
func NewMiniMaxClient(cfg *AppConfig) *MiniMaxClient {
	c := cfg.Get()
	transport := &http.Transport{
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		MaxIdleConns:        32,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     90 * time.Second,
	}
	return &MiniMaxClient{
		cfg: cfg,
		http: &http.Client{
			Timeout:   30 * time.Second, // 桌面端单次查询 30s 超时
			Transport: transport,
		},
		httpLong: &http.Client{
			Timeout:   time.Duration(c.UploadTimeoutSec) * time.Second,
			Transport: transport,
		},
	}
}

// baseURLs 返回当前 (云网关, 账号域)
func (c *MiniMaxClient) baseURLs() (string, string) {
	cfg := c.cfg.Get()
	return cfg.CloudGateway(), cfg.AccountAPI()
}

// commonQuery 构造公共 Query 参数（逆向报告 §2.2）
func (c *MiniMaxClient) commonQuery(p DeviceProfile) url.Values {
	cfg := c.cfg.Get()
	q := url.Values{}
	q.Set("device_platform", "desktop")
	q.Set("app_id", "3001")
	q.Set("version_code", cfg.VersionCode)
	q.Set("biz_id", "0")
	q.Set("unix", strconv.FormatInt(time.Now().UnixMilli(), 10))
	q.Set("os_name", p.OsName)
	q.Set("cpu_core_num", strconv.Itoa(p.CPUCores))
	q.Set("device_memory", strconv.Itoa(p.MemoryGB))
	// 桌面端语义：token 解析不出 deviceID 时整体省略 device_id/uuid 参数
	// （逆向报告 §2.2 buildCloudCommonParams 的 if(deviceId)/if(desktopDeviceId) 分支），
	// 而不是发送空值。
	if p.UUID != "" {
		q.Set("uuid", p.UUID)
	}
	if p.DeviceID != "" {
		q.Set("device_id", p.DeviceID)
	}
	q.Set("download_source", "default")
	return q
}

// setAuthHeaders 双头鉴权 + 可选头（逆向报告 §2.1）
func setAuthHeaders(req *http.Request, p DeviceProfile) {
	req.Header.Set("token", p.Token)
	req.Header.Set("Authorization", "Bearer "+p.Token)
	lang := p.Lang
	if lang == "" {
		lang = "zh"
	}
	req.Header.Set("X-Hilo-Lang", lang)
	req.Header.Set("x-hilo-source", "agent")
	if p.GroupID != "" {
		req.Header.Set("X-Group-Id", p.GroupID)
	}
	// 桌面网关是 Node 进程直连（报告 §1），云端不校验浏览器指纹；
	// 仍设置一个常规 UA，避免暴露 Go-http-client 默认标识。
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) MiniMaxDesign/3.0.11")
	req.Header.Set("Accept", "application/json")
}

// tokenTail 日志脱敏：JWT 只打前 8 位
func tokenTail(token string) string {
	if len(token) > 8 {
		return token[:8] + "..."
	}
	return token
}

// doJSON 发送 JSON 请求并解析响应。返回 *CloudError（nil=成功）。
// rawBody 非 nil 时回填原始响应体（截断 4KB），供上层记录事件。
func (c *MiniMaxClient) doJSON(ctx context.Context, hc *http.Client, method, fullURL string, p DeviceProfile, body any, out any) *CloudError {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return &CloudError{Kind: KindClient, Msg: "marshal body: " + err.Error()}
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, fullURL, rdr)
	if err != nil {
		return &CloudError{Kind: KindClient, Msg: "build request: " + err.Error()}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	setAuthHeaders(req, p)

	resp, err := hc.Do(req)
	if err != nil {
		return &CloudError{Kind: KindTransient, Msg: err.Error(), Uncertain: isUncertainTransport(err)}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return &CloudError{Kind: KindTransient, Msg: "read body: " + err.Error(), Uncertain: true}
	}

	// HTTP 层错误分类
	switch {
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		return &CloudError{Kind: KindAuth, HTTPStatus: resp.StatusCode, Msg: "token invalid (http " + strconv.Itoa(resp.StatusCode) + ")"}
	case resp.StatusCode == 429 || resp.StatusCode >= 500:
		// 云端可能用 5xx 外壳包参数类错误（如 CreateTask 内层 400：分辨率/模型不符赠送额度资格）：
		// 此类重试无意义且会触发账号冷却+退款循环，按终态参数类处理。
		if bytes.Contains(data, []byte("CreateTask failed")) || bytes.Contains(data, []byte(`"status":400`)) || bytes.Contains(data, []byte(`"status": 400`)) {
			return &CloudError{Kind: KindClient, HTTPStatus: resp.StatusCode, Msg: "http " + strconv.Itoa(resp.StatusCode) + "(inner 400): " + truncate(string(data), 300)}
		}
		return &CloudError{Kind: KindTransient, HTTPStatus: resp.StatusCode, Msg: "http " + strconv.Itoa(resp.StatusCode) + ": " + truncate(string(data), 200)}
	case resp.StatusCode >= 400:
		// 4xx：可能是余额不足（预检 error_code），其余按参数类
		if bytes.Contains(data, []byte("billing_insufficient_balance")) {
			return &CloudError{Kind: KindNoCredit, HTTPStatus: resp.StatusCode, Msg: "billing_insufficient_balance"}
		}
		return &CloudError{Kind: KindClient, HTTPStatus: resp.StatusCode, Msg: "http " + strconv.Itoa(resp.StatusCode) + ": " + truncate(string(data), 300)}
	}

	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return &CloudError{Kind: KindUnknown, HTTPStatus: resp.StatusCode, Msg: "decode json: " + err.Error()}
		}
	}
	return nil
}

// truncate 截断字符串（日志/事件用）
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// checkBaseResp 解析媒体类接口的 base_resp，非 0 时分类返回错误
func checkBaseResp(code int, msg string) *CloudError {
	if code == 0 {
		return nil
	}
	switch {
	case code == 1033:
		return &CloudError{Kind: KindBusy, Code: code, Msg: "视频生成服务暂时异常(1033): " + msg}
	case strings.Contains(strings.ToLower(msg), "balance") || strings.Contains(msg, "余额"):
		return &CloudError{Kind: KindNoCredit, Code: code, Msg: msg}
	default:
		// 未知错误码：保守处理，不自动重试（可能是风控信号）
		return &CloudError{Kind: KindUnknown, Code: code, Msg: msg}
	}
}

// ---- 账号域接口（hailuoai.com，响应包裹 {statusInfo:{code,message},data}） ----

// accountEnvelope 账号域统一响应包裹（statusInfo.code 同样宽容解析）
type accountEnvelope struct {
	StatusInfo struct {
		Code    flexInt `json:"code"`
		Message string  `json:"message"`
	} `json:"statusInfo"`
	Data json.RawMessage `json:"data"`
}

// accountRequest 发送账号域请求并拆包裹
func (c *MiniMaxClient) accountRequest(ctx context.Context, method, path string, p DeviceProfile, body any, dataOut any) *CloudError {
	_, acctAPI := c.baseURLs()
	u := acctAPI + path + "?" + c.commonQuery(p).Encode()
	var env accountEnvelope
	if cerr := c.doJSON(ctx, c.http, method, u, p, body, &env); cerr != nil {
		return cerr
	}
	if env.StatusInfo.Code != 0 {
		return &CloudError{Kind: KindUnknown, Code: int(env.StatusInfo.Code), Msg: "statusInfo: " + env.StatusInfo.Message}
	}
	if dataOut != nil && len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, dataOut); err != nil {
			return &CloudError{Kind: KindUnknown, Msg: "decode data: " + err.Error()}
		}
	}
	return nil
}

// UserInfo /v1/api/user/info 的 data.userInfo
type UserInfo struct {
	RealUserID string `json:"realUserID"`
	Name       string `json:"name"`
}

// GetUserInfo 校验 token 并拉取用户信息（GET /v1/api/user/info）
func (c *MiniMaxClient) GetUserInfo(ctx context.Context, p DeviceProfile) (*UserInfo, *CloudError) {
	var data struct {
		UserInfo UserInfo `json:"userInfo"`
	}
	if cerr := c.accountRequest(ctx, http.MethodGet, "/v1/api/user/info", p, nil, &data); cerr != nil {
		return nil, cerr
	}
	return &data.UserInfo, nil
}

// RenewToken 续期 token（POST /v1/api/user/renewal → data.token）
func (c *MiniMaxClient) RenewToken(ctx context.Context, p DeviceProfile) (string, *CloudError) {
	var data struct {
		Token string `json:"token"`
	}
	if cerr := c.accountRequest(ctx, http.MethodPost, "/v1/api/user/renewal", p, nil, &data); cerr != nil {
		return "", cerr
	}
	if data.Token == "" {
		return "", &CloudError{Kind: KindUnknown, Msg: "renewal returned empty token"}
	}
	return data.Token, nil
}

// ---- 云网关业务接口（design.minimaxi.com，裸 JSON / base_resp） ----

// cloudURL 拼云网关完整 URL（自动带公共参数）
func (c *MiniMaxClient) cloudURL(path string, p DeviceProfile) string {
	gw, _ := c.baseURLs()
	return gw + path + "?" + c.commonQuery(p).Encode()
}

// GetBalance 查询余额（GET /api/v1/credit/balance → {total_credit}）
// 实测云端 total_credit 可能返回字符串（官方网关 asNonNegativeCredits 双形态兼容），
// 用 flexFloat 宽容解析。
func (c *MiniMaxClient) GetBalance(ctx context.Context, p DeviceProfile) (float64, *CloudError) {
	// 桌面端 UI 读 wallet（含赠送/试用钱包 credit_type=2），/credit/balance 只计付费余额：
	// 新号 3000 赠送积分只在 wallet 里，故以 wallet 各钱包 total_credit 之和为准，失败回退 balance。
	var w struct {
		Wallets []struct {
			TotalCredit flexFloat `json:"total_credit"`
		} `json:"wallets"`
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if cerr := c.doJSON(ctx, c.http, http.MethodGet, c.cloudURL("/api/v1/credit/wallet", p), p, nil, &w); cerr == nil {
		var sum float64
		for _, wl := range w.Wallets {
			sum += float64(wl.TotalCredit)
		}
		return sum, nil
	}
	var out struct {
		TotalCredit flexFloat `json:"total_credit"`
	}
	if cerr := c.doJSON(ctx, c.http, http.MethodGet, c.cloudURL("/api/v1/credit/balance", p), p, nil, &out); cerr != nil {
		return 0, cerr
	}
	return float64(out.TotalCredit), nil
}

// GetWallet 钱包详情（GET /api/v1/credit/wallet），返回原始 JSON
func (c *MiniMaxClient) GetWallet(ctx context.Context, p DeviceProfile) (map[string]any, *CloudError) {
	var out map[string]any
	if cerr := c.doJSON(ctx, c.http, http.MethodGet, c.cloudURL("/api/v1/credit/wallet", p), p, nil, &out); cerr != nil {
		return nil, cerr
	}
	return out, nil
}

// CalculateCost 生成前计价（POST /api/v1/credit/calculate-cost → {estimated_credits}）
// 请求体结构见逆向报告 §5.2（media_type=video 的最小必需字段集）
func (c *MiniMaxClient) CalculateCost(ctx context.Context, p DeviceProfile, model, resolution string, duration int, generateAudio bool) (float64, *CloudError) {
	body := map[string]any{
		"media_type": "video",
		"model":      model,
		"resolution": resolution,
		"duration":   duration,
		"quantity":   1,
		"seconds":    duration,
		"has_sound":  generateAudio,
	}
	var out struct {
		EstimatedCredits flexFloat `json:"estimated_credits"`
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if cerr := c.doJSON(ctx, c.http, http.MethodPost, c.cloudURL("/api/v1/credit/calculate-cost", p), p, body, &out); cerr != nil {
		return 0, cerr
	}
	// 官方同源 asNonNegativeCredits 解析，字符串形态同样宽容
	return float64(out.EstimatedCredits), nil
}

// TrialStatus 免费试用活动状态（GET /api/v1/promotions/hailuo03-video-trial/status）
// 云端网关实际返回 snake_case 字段（free_count/remaining_count/activity_active/claim_hint，
// 旧 camelCase 字段不存在），这里统一归一化为 camelCase 输出，对齐管理前端与接口契约；
// 数字字段宽容解析（number/字符串双形态，官方 numberField 语义）。
type TrialStatus struct {
	Claimed        bool           `json:"claimed"`
	Claimable      bool           `json:"claimable"`
	FreeCount      int            `json:"freeCount"`
	RemainingCount int            `json:"remainingCount"`
	ActivityActive bool           `json:"activityActive"`
	ClaimHint      string         `json:"claimHint"`
	Raw            map[string]any `json:"-"`
}

// fillTrialFromRaw 从云端返回的原始 map 填充 TrialStatus。
// 键名同时容忍 snake_case（网关实际形态）与 camelCase（历史/占位形态），
// 数字用宽容解析，bool/string 缺失取零值，绝不 panic。
func fillTrialFromRaw(raw map[string]any) TrialStatus {
	out := TrialStatus{Raw: raw}
	if raw == nil {
		return out
	}
	pickInt := func(keys ...string) int {
		for _, k := range keys {
			if n, ok := anyToInt(raw[k]); ok {
				return n
			}
		}
		return 0
	}
	pickBool := func(keys ...string) bool {
		for _, k := range keys {
			if b, ok := anyToBool(raw[k]); ok {
				return b
			}
		}
		return false
	}
	pickStr := func(keys ...string) string {
		for _, k := range keys {
			if s := anyToString(raw[k]); s != "" {
				return s
			}
		}
		return ""
	}
	out.Claimed = pickBool("claimed")
	out.Claimable = pickBool("claimable")
	out.FreeCount = pickInt("free_count", "freeCount")
	out.RemainingCount = pickInt("remaining_count", "remainingCount")
	out.ActivityActive = pickBool("activity_active", "activityActive")
	out.ClaimHint = pickStr("claim_hint", "claimHint")
	return out
}

// GetTrialStatus 查询免费试用状态
func (c *MiniMaxClient) GetTrialStatus(ctx context.Context, p DeviceProfile) (*TrialStatus, *CloudError) {
	var raw map[string]any
	u := c.cloudURL("/api/v1/promotions/hailuo03-video-trial/status", p)
	if cerr := c.doJSON(ctx, c.http, http.MethodGet, u, p, nil, &raw); cerr != nil {
		return nil, cerr
	}
	out := fillTrialFromRaw(raw)
	// 免费次数>0 时视为活动可用（云端未显式给 activity_active/claimable 时的兜底推导）
	if out.FreeCount > 0 {
		out.ActivityActive = true
		out.Claimable = true
	}
	return &out, nil
}

// ClaimTrial 领取免费试用次数（POST /api/v1/promotions/hailuo03-video-trial/claim）
func (c *MiniMaxClient) ClaimTrial(ctx context.Context, p DeviceProfile) (*TrialStatus, *CloudError) {
	var raw map[string]any
	u := c.cloudURL("/api/v1/promotions/hailuo03-video-trial/claim", p)
	if cerr := c.doJSON(ctx, c.http, http.MethodPost, u, p, map[string]any{}, &raw); cerr != nil {
		return nil, cerr
	}
	out := fillTrialFromRaw(raw)
	if out.FreeCount > 0 {
		out.ActivityActive = true
		out.Claimed = true
	}
	return &out, nil
}

// UploadFile 上传素材（POST /api/v1/files/upload）
// dataURI 形如 data:image/png;base64,xxx；prefix ∈ image|video|audio；120s 超时（同桌面端）
func (c *MiniMaxClient) UploadFile(ctx context.Context, p DeviceProfile, prefix, dataURI string) (string, *CloudError) {
	body := map[string]string{"file_data": dataURI, "file_prefix": prefix}
	var out struct {
		URL string `json:"url"`
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(c.cfg.Get().UploadTimeoutSec)*time.Second)
	defer cancel()
	if cerr := c.doJSON(ctx, c.httpLong, http.MethodPost, c.cloudURL("/api/v1/files/upload", p), p, body, &out); cerr != nil {
		return "", cerr
	}
	if out.URL == "" {
		return "", &CloudError{Kind: KindUnknown, Msg: "upload returned empty url"}
	}
	return out.URL, nil
}

// VideoSubmitRequest 提交视频生成请求体（POST /api/v1/video/minimax-v3/generate，报告 §7.3）
type VideoSubmitRequest struct {
	Model           string   `json:"model"`
	Prompt          string   `json:"prompt"`
	GenerateAudio   bool     `json:"generate_audio"`
	Ratio           string   `json:"ratio"`
	Duration        int      `json:"duration"`
	Resolution      string   `json:"resolution"`
	FirstFrameImage string   `json:"first_frame_image,omitempty"`
	LastFrameImage  string   `json:"last_frame_image,omitempty"`
	ReferenceImages []string `json:"reference_images,omitempty"`
	ReferenceVideos []string `json:"reference_videos,omitempty"`
	ReferenceAudios []string `json:"reference_audios,omitempty"`
}

// SubmitVideo 提交视频生成任务 → cloud task_id。
// 注意：网络超时/连接中断时结果不明（Uncertain=true），上层必须置 SUBMIT_UNKNOWN，
// 绝不自动重试，防止重复扣费。
func (c *MiniMaxClient) SubmitVideo(ctx context.Context, p DeviceProfile, req VideoSubmitRequest) (string, *CloudError) {
	// t2va（纯文本+音频）场景云端强制显式 ratio 且禁 adaptive（2013）：归一化到 16:9。
	if req.GenerateAudio && req.FirstFrameImage == "" && req.LastFrameImage == "" &&
		len(req.ReferenceImages) == 0 && len(req.ReferenceVideos) == 0 && len(req.ReferenceAudios) == 0 &&
		(req.Ratio == "" || req.Ratio == "adaptive") {
		req.Ratio = "16:9"
	}
	var out struct {
		TaskID   string `json:"task_id"`
		BaseResp struct {
			StatusCode flexInt `json:"status_code"`
			StatusMsg  string  `json:"status_msg"`
		} `json:"base_resp"`
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	u := c.cloudURL("/api/v1/video/minimax-v3/generate", p)
	if cerr := c.doJSON(ctx, c.http, http.MethodPost, u, p, req, &out); cerr != nil {
		return "", cerr
	}
	if cerr := checkBaseResp(int(out.BaseResp.StatusCode), out.BaseResp.StatusMsg); cerr != nil {
		return "", cerr
	}
	if out.TaskID == "" {
		return "", &CloudError{Kind: KindUnknown, Msg: "submit returned empty task_id"}
	}
	return out.TaskID, nil
}

// CloudTaskStatus 任务查询响应（GET /api/v1/video/minimax-v3/tasks/{task_id}，报告 §8.1）
// 数字字段（estimated_remaining_wait_seconds / base_resp.status_code）经官方
// numberField2 双形态兼容解析，这里同样用 flex 类型
type CloudTaskStatus struct {
	Status          string  `json:"status"` // queue|processing|success|failed|fail|cancelled
	FileID          string  `json:"file_id"`
	ProviderTaskID  string  `json:"provider_task_id"`
	EstRemainingSec flexInt `json:"estimated_remaining_wait_seconds"`
	BaseResp        struct {
		StatusCode flexInt `json:"status_code"`
		StatusMsg  string  `json:"status_msg"`
	} `json:"base_resp"`
}

// QueryTask 查询云端任务状态
func (c *MiniMaxClient) QueryTask(ctx context.Context, p DeviceProfile, cloudTaskID string) (*CloudTaskStatus, *CloudError) {
	var out CloudTaskStatus
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second) // 单次查询 30s（同桌面端）
	defer cancel()
	u := c.cloudURL("/api/v1/video/minimax-v3/tasks/"+url.PathEscape(cloudTaskID), p)
	if cerr := c.doJSON(ctx, c.http, http.MethodGet, u, p, nil, &out); cerr != nil {
		return nil, cerr
	}
	if cerr := checkBaseResp(int(out.BaseResp.StatusCode), out.BaseResp.StatusMsg); cerr != nil {
		return nil, cerr
	}
	return &out, nil
}

// GetFileURL 取成片直链（GET /api/v1/video/minimax/files/{file_id}，H3 与 v1 共用 files 端点）
func (c *MiniMaxClient) GetFileURL(ctx context.Context, p DeviceProfile, fileID string) (string, *CloudError) {
	var out struct {
		File struct {
			DownloadURL string `json:"download_url"`
		} `json:"file"`
		BaseResp struct {
			StatusCode flexInt `json:"status_code"`
			StatusMsg  string  `json:"status_msg"`
		} `json:"base_resp"`
	}
	if cerr := c.doJSON(ctx, c.http, http.MethodGet, c.cloudURL("/api/v1/video/minimax/files/"+url.PathEscape(fileID), p), p, nil, &out); cerr != nil {
		return "", cerr
	}
	if cerr := checkBaseResp(int(out.BaseResp.StatusCode), out.BaseResp.StatusMsg); cerr != nil {
		return "", cerr
	}
	if out.File.DownloadURL == "" {
		return "", &CloudError{Kind: KindUnknown, Msg: "file response has no download_url"}
	}
	return out.File.DownloadURL, nil
}

// CancelTask 取消云端任务（POST /api/v1/task/cancel body {task_id}）
func (c *MiniMaxClient) CancelTask(ctx context.Context, p DeviceProfile, cloudTaskID string) *CloudError {
	var out struct {
		BaseResp struct {
			StatusCode flexInt `json:"status_code"`
			StatusMsg  string  `json:"status_msg"`
		} `json:"base_resp"`
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	body := map[string]string{"task_id": cloudTaskID}
	if cerr := c.doJSON(ctx, c.http, http.MethodPost, c.cloudURL("/api/v1/task/cancel", p), p, body, &out); cerr != nil {
		return cerr
	}
	// 已终态等业务性非 0 码不视为致命错误，交由调用方判断
	return checkBaseResp(int(out.BaseResp.StatusCode), out.BaseResp.StatusMsg)
}

// DownloadFile 流式下载 CDN 直链到 destPath（无需鉴权头，报告 §9.2）。
// 完整性校验：实际字节数 != Content-Length 时删除文件并报错（同桌面端语义）。
func (c *MiniMaxClient) DownloadFile(ctx context.Context, downloadURL, destPath string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return fmt.Errorf("build download request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64)")
	resp, err := c.httpLong.Do(req)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download http %d", resp.StatusCode)
	}
	f, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("create %s: %w", destPath, err)
	}
	written, err := io.Copy(f, resp.Body)
	closeErr := f.Close()
	if err != nil {
		os.Remove(destPath)
		return fmt.Errorf("download write: %w", err)
	}
	if closeErr != nil {
		os.Remove(destPath)
		return fmt.Errorf("download close: %w", closeErr)
	}
	if resp.ContentLength > 0 && written != resp.ContentLength {
		os.Remove(destPath)
		return fmt.Errorf("download integrity: got %d bytes, want %d", written, resp.ContentLength)
	}
	if written == 0 {
		os.Remove(destPath)
		return fmt.Errorf("download integrity: empty file")
	}
	log.Printf("[client] downloaded %s (%d bytes)", destPath, written)
	return nil
}

// ---- 模型目录接口（GET /api/v1/models/config，报告 §6.4） ----
// 响应形态：{imageModels[], videoModels[], audioModels[], textModels[], defaultTextModelId}。
// 云端字段来自逆向、可能随版本漂移：解析全程防御式——条目一律 optional，
// 类型不符按零值处理，未识别字段通过 Raw 原样保留，绝不 panic。

// VideoModelEntry videoModels 单条目：常用字段显式提取，Raw 承载整条目原始 JSON
type VideoModelEntry struct {
	ID               string         `json:"id"`
	ModelName        string         `json:"model_name,omitempty"`
	PricingID        string         `json:"pricingId,omitempty"`
	DisplayName      string         `json:"display_name,omitempty"`
	Description      string         `json:"description,omitempty"`
	Type             string         `json:"type,omitempty"`
	ImageMode        any            `json:"imageMode,omitempty"`
	PromptRequired   any            `json:"promptRequired,omitempty"`
	PromptMaxLength  *int           `json:"promptMaxLength,omitempty"`
	InputMediaLimits any            `json:"inputMediaLimits,omitempty"`
	ToolNames        []string       `json:"tool_names,omitempty"`
	Region           string         `json:"region,omitempty"`
	Hot              bool           `json:"hot,omitempty"`
	IconURL          string         `json:"icon_url,omitempty"`
	MentionName      string         `json:"mention_name,omitempty"`
	SeriesID         string         `json:"series_id,omitempty"`
	Raw              map[string]any `json:"raw,omitempty"`
}

// ModelsConfig 云端模型目录（videoModels 显式解析，其余类目整条目保留原始 JSON）
type ModelsConfig struct {
	ImageModels        []map[string]any  `json:"imageModels,omitempty"`
	VideoModels        []VideoModelEntry `json:"videoModels,omitempty"`
	AudioModels        []map[string]any  `json:"audioModels,omitempty"`
	TextModels         []map[string]any  `json:"textModels,omitempty"`
	DefaultTextModelID string            `json:"defaultTextModelId,omitempty"`
}

// FetchModelsConfig 拉取云端模型目录。
// 防御：顶层可能再包一层 data 包裹；数组元素非对象时整条跳过。
func (c *MiniMaxClient) FetchModelsConfig(ctx context.Context, p DeviceProfile) (*ModelsConfig, *CloudError) {
	var raw map[string]any
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	u := c.cloudURL("/api/v1/models/config", p)
	if cerr := c.doJSON(ctx, c.http, http.MethodGet, u, p, nil, &raw); cerr != nil {
		return nil, cerr
	}
	// 部分环境可能包一层 data 包裹（与其他云接口不一致时的兜底）
	if _, ok := raw["videoModels"]; !ok {
		if inner, ok2 := raw["data"].(map[string]any); ok2 {
			raw = inner
		}
	}
	out := &ModelsConfig{
		ImageModels:        rawRecordList(raw["imageModels"]),
		AudioModels:        rawRecordList(raw["audioModels"]),
		TextModels:         rawRecordList(raw["textModels"]),
		DefaultTextModelID: anyToString(raw["defaultTextModelId"]),
	}
	for _, m := range rawRecordList(raw["videoModels"]) {
		out.VideoModels = append(out.VideoModels, parseVideoModelEntry(m))
	}
	return out, nil
}

// parseVideoModelEntry 从原始条目提取常用字段（全部宽容，缺失/类型异常=零值）
func parseVideoModelEntry(m map[string]any) VideoModelEntry {
	e := VideoModelEntry{
		Raw:              m,
		ID:               anyToString(m["id"]),
		ModelName:        anyToString(m["model_name"]),
		PricingID:        anyToString(m["pricingId"]),
		DisplayName:      anyToString(m["display_name"]),
		Description:      anyToString(m["description"]),
		Type:             anyToString(m["type"]),
		ImageMode:        m["imageMode"],
		PromptRequired:   m["promptRequired"],
		InputMediaLimits: m["inputMediaLimits"],
		ToolNames:        anyToStringSlice(m["tool_names"]),
		Region:           anyToString(m["region"]),
		IconURL:          anyToString(m["icon_url"]),
		MentionName:      anyToString(m["mention_name"]),
		SeriesID:         anyToString(m["series_id"]),
	}
	if n, ok := anyToInt(m["promptMaxLength"]); ok {
		e.PromptMaxLength = &n
	}
	if b, ok := anyToBool(m["hot"]); ok {
		e.Hot = b
	}
	return e
}

// rawRecordList any → []map[string]any（非数组/元素非对象时防御性跳过）
func rawRecordList(v any) []map[string]any {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []map[string]any
	for _, item := range arr {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// anyToString 宽容取字符串：string 原样、JSON number 去尾零、bool 转字面量、nil/其他为空
func anyToString(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case float64:
		if s == math.Trunc(s) && !math.IsInf(s, 0) {
			return strconv.FormatInt(int64(s), 10)
		}
		return strconv.FormatFloat(s, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(s)
	}
	return ""
}

// anyToInt 宽容取整数：JSON number / 数字字符串；ok=false 表示缺失或非法
func anyToInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return 0, false
		}
		return int(math.Trunc(n)), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return 0, false
		}
		return int(math.Trunc(f)), true
	}
	return 0, false
}

// anyToBool 宽容取布尔：bool / "true"/"false"/"1"/"0" / JSON number；ok=false 表示无法判定
func anyToBool(v any) (bool, bool) {
	switch b := v.(type) {
	case bool:
		return b, true
	case string:
		switch strings.ToLower(strings.TrimSpace(b)) {
		case "true", "1":
			return true, true
		case "false", "0":
			return false, true
		}
	case float64:
		if b == 0 {
			return false, true
		}
		if b == 1 {
			return true, true
		}
	}
	return false, false
}

// anyToStringSlice 宽容取字符串数组（非字符串元素跳过）
func anyToStringSlice(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, item := range arr {
		if s := anyToString(item); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// ---- 图片生成接口（nano_banana 系，v2 异步主用） ----
// 协议事实来自官方网关逆向（gateway-dist/main.js）：
//   - backends/nano-banana/nano-banana.service：buildBananaBody 提交体
//     {prompt, model_name(默认 nano_banana_2_flash), image_paths(压缩后 data URI，
//     单图 ≤7MB), aspect_ratio(默认空串=自动), resolution(默认 1K)}；
//   - backends/cloud-v2.backend：stampIdempotencyKey 客户端注入 idempotency_key(UUID)；
//     提交响应必须含 task_id；轮询 GET {queryV2}/{task_id}，
//     status success → {image_url,width,height}；failed/cancelled → base.{code,message,user_message}；
//     pending/processing/其他 → 进行中；
//   - 路径注册表 image.nano_banana：generate(v1 同步)/generateV2/queryV2；
//   - v2 图片错误包是 base.message，视频 v3 是 base_resp.status_msg，
//     官方 mapBaseResp 双取（base_resp ?? base），解析同时容忍两种包。

// ImageBackendPaths 图片后端路径注册表条目（对齐官方 cloudGateway.image.* 结构）。
// 新厂商（openai/kontext/qwen/seedream/...）按同构三路径在此注册即可接入。
type ImageBackendPaths struct {
	Name       string // 后端名（model_name 归属系列）
	Generate   string // v1 同步生成（备用，本期不调用）
	GenerateV2 string // v2 异步提交
	QueryV2    string // v2 轮询前缀（完整路径 = QueryV2/{task_id}）
}

// imageBackends 图片后端注册表（本期仅 nano_banana；其他厂商留扩展位）
var imageBackends = map[string]ImageBackendPaths{
	"nano_banana": {
		Name:       "nano_banana",
		Generate:   "/api/v1/image/nano_banana/generate",
		GenerateV2: "/api/v2/image/nano_banana/generate",
		QueryV2:    "/api/v2/image/nano_banana/tasks",
	},
}

// ImageBackend 按后端名取路径（未注册返回 false）
func ImageBackend(name string) (ImageBackendPaths, bool) {
	b, ok := imageBackends[name]
	return b, ok
}

// ImageBackendForModel 模型名 → 后端（nano_banana 系模型名均以 nano_banana 开头）
func ImageBackendForModel(model string) (ImageBackendPaths, bool) {
	lower := strings.ToLower(model)
	for name := range imageBackends {
		if strings.HasPrefix(lower, name) {
			return imageBackends[name], true
		}
	}
	return ImageBackendPaths{}, false
}

// ImageSubmitRequest v2 图片生成提交体（buildBananaBody + stampIdempotencyKey）
type ImageSubmitRequest struct {
	Prompt         string   `json:"prompt"`
	ModelName      string   `json:"model_name"`
	ImagePaths     []string `json:"image_paths"`     // 图生图参考：data URI（单图 ≤7MB），无参考传空数组
	AspectRatio    string   `json:"aspect_ratio"`    // 空串=自动；或 16:9/9:16/1:1/4:3/3:4/3:2/2:3/5:4/4:5/21:9
	Resolution     string   `json:"resolution"`      // 1K(默认)/2K/4K
	IdempotencyKey string   `json:"idempotency_key"` // 客户端生成的 UUID，云端幂等去重
}

// SubmitImage 提交 v2 图片生成 → cloud task_id。
// 响应缺 task_id 视为提交失败（对齐官方 submitOnlyCloudV2）。
// Uncertain 语义与视频一致：提交窗口网络中断 → 上层置 SUBMIT_UNKNOWN，绝不自动重试。
func (c *MiniMaxClient) SubmitImage(ctx context.Context, p DeviceProfile, backend ImageBackendPaths, req ImageSubmitRequest) (string, *CloudError) {
	if req.ImagePaths == nil {
		req.ImagePaths = []string{}
	}
	var out struct {
		TaskID   string `json:"task_id"`
		BaseResp struct {
			StatusCode flexInt `json:"status_code"`
			StatusMsg  string  `json:"status_msg"`
		} `json:"base_resp"`
		Base struct {
			Code    flexInt `json:"code"`
			Message string  `json:"message"`
		} `json:"base"`
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	u := c.cloudURL(backend.GenerateV2, p)
	if cerr := c.doJSON(ctx, c.http, http.MethodPost, u, p, req, &out); cerr != nil {
		return "", cerr
	}
	// 错误包双容忍：base_resp（v3 风格）与 base（v2 风格）
	if out.BaseResp.StatusCode != 0 {
		return "", checkBaseResp(int(out.BaseResp.StatusCode), out.BaseResp.StatusMsg)
	}
	if out.Base.Code != 0 {
		return "", checkBaseResp(int(out.Base.Code), out.Base.Message)
	}
	if out.TaskID == "" {
		return "", &CloudError{Kind: KindUnknown, Msg: "image submit response missing task_id"}
	}
	return out.TaskID, nil
}

// CloudImageStatus v2 图片任务查询响应。
// status: pending|processing|success|failed|cancelled（其他值一律视为进行中）；
// 成功带 image_url/width/height；失败带 base.{code,message,user_message}
// （同时容忍 base_resp.{status_code,status_msg}）。
type CloudImageStatus struct {
	Status   string  `json:"status"`
	ImageURL string  `json:"image_url"`
	Width    flexInt `json:"width"`
	Height   flexInt `json:"height"`
	BaseResp struct {
		StatusCode flexInt `json:"status_code"`
		StatusMsg  string  `json:"status_msg"`
	} `json:"base_resp"`
	Base struct {
		Code        flexInt `json:"code"`
		Message     string  `json:"message"`
		UserMessage string  `json:"user_message"`
	} `json:"base"`
}

// ErrMessage 失败原因（base.message 优先，base_resp.status_msg 兜底）
func (s *CloudImageStatus) ErrMessage() string {
	if s.Base.Message != "" {
		return s.Base.Message
	}
	if s.BaseResp.StatusMsg != "" {
		return s.BaseResp.StatusMsg
	}
	return "云端图片生成失败"
}

// ErrCode 失败错误码（base.code / base_resp.status_code 双取）
func (s *CloudImageStatus) ErrCode() int {
	if s.Base.Code != 0 {
		return int(s.Base.Code)
	}
	return int(s.BaseResp.StatusCode)
}

// QueryImageTask 查询 v2 图片任务（GET {queryV2}/{task_id}）
func (c *MiniMaxClient) QueryImageTask(ctx context.Context, p DeviceProfile, backend ImageBackendPaths, cloudTaskID string) (*CloudImageStatus, *CloudError) {
	var out CloudImageStatus
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	u := c.cloudURL(backend.QueryV2+"/"+url.PathEscape(cloudTaskID), p)
	if cerr := c.doJSON(ctx, c.http, http.MethodGet, u, p, nil, &out); cerr != nil {
		return nil, cerr
	}
	// HTTP 200 但业务错误包非 0：按错误分类（如 1033 繁忙）
	if out.BaseResp.StatusCode != 0 {
		return nil, checkBaseResp(int(out.BaseResp.StatusCode), out.BaseResp.StatusMsg)
	}
	if out.Base.Code != 0 && out.Status == "" {
		return nil, checkBaseResp(int(out.Base.Code), out.Base.Message)
	}
	return &out, nil
}

// CalculateImageCost 图片计价（POST /api/v1/credit/calculate-cost，media_type=image）。
// 官方 creditCostRequestFor：图片消费 model/resolution/quantity/char_count/ref_count
// （duration/seconds 仅视频适用）。
func (c *MiniMaxClient) CalculateImageCost(ctx context.Context, p DeviceProfile, model, resolution string, quantity, charCount, refCount int) (float64, *CloudError) {
	body := map[string]any{
		"media_type": "image",
		"model":      model,
		"resolution": resolution,
		"quantity":   quantity,
		"char_count": charCount,
	}
	if refCount > 0 {
		body["ref_count"] = refCount
	}
	var out struct {
		EstimatedCredits flexFloat `json:"estimated_credits"`
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if cerr := c.doJSON(ctx, c.http, http.MethodPost, c.cloudURL("/api/v1/credit/calculate-cost", p), p, body, &out); cerr != nil {
		return 0, cerr
	}
	return float64(out.EstimatedCredits), nil
}
