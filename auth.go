package main

// ---- 认证：Web session + 下游 API Key ----
// 双认证体系（对齐 dumate-proxy 模式）：
//   - /api/*（除 /api/login）：session cookie 认证。会话 token 只存 sha256，
//     DB 持久化（重启不掉线），登录失败限流 5 次/15min 锁定；
//     密码 bcrypt 存 settings 表，无哈希时回退配置文件默认 admin/admin123。
//   - /v1/*：API Key 认证（Authorization: Bearer sk-xxx）。
//     Key 生成 sk-<40hex>，DB 只存 sha256 哈希 + 前缀（展示/查找用），
//     校验用 subtle.ConstantTimeCompare 常量时间比较。

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	sessionCookieName = "minimax2api_session"
	sessionExpiry     = 24 * time.Hour

	// 登录限流参数（同 dumate-proxy：5 次失败锁 15 分钟）
	maxLoginFailures  = 5
	loginLockDuration = 15 * time.Minute
	loginFailWindow   = 15 * time.Minute
	loginFailDelay    = 500 * time.Millisecond

	// settings 表键名
	settingPasswordHash = "admin_password_hash"
	settingAdminUser    = "admin_username"
)

// sha256Hex 计算 sha256 十六进制串
func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// ---- session ----

type sessionEntry struct {
	username  string
	createdAt time.Time
	expiresAt time.Time
}

// loginAttempt 登录失败计数（按 客户端IP+用户名）
type loginAttempt struct {
	failures  int
	firstFail time.Time
	lockUntil time.Time
}

// AuthManager 认证管理器（session + API Key）
type AuthManager struct {
	mu       sync.RWMutex
	sessions map[string]*sessionEntry // key = sha256(token)
	db       *DB
	cfg      *AppConfig

	attemptMu sync.Mutex
	attempts  map[string]*loginAttempt
}

// NewAuthManager 创建认证管理器（从 DB 恢复未过期会话）
func NewAuthManager(db *DB, cfg *AppConfig) *AuthManager {
	am := &AuthManager{
		sessions: make(map[string]*sessionEntry),
		db:       db,
		cfg:      cfg,
		attempts: make(map[string]*loginAttempt),
	}
	if entries, err := am.loadValidSessions(); err != nil {
		log.Printf("[auth] warning: load sessions from db: %v", err)
	} else if len(entries) > 0 {
		am.sessions = entries
		log.Printf("[auth] restored %d valid sessions from db", len(entries))
	}
	return am
}

// loadValidSessions 从 DB 恢复未过期会话（token_hash 为主键）
func (am *AuthManager) loadValidSessions() (map[string]*sessionEntry, error) {
	rows, err := am.db.conn.Query(`SELECT token_hash, username, created_at, expires_at FROM sessions WHERE expires_at > ?`,
		time.Now().Format(time.RFC3339))
	if err != nil {
		return nil, fmt.Errorf("query sessions: %w", err)
	}
	defer rows.Close()
	out := make(map[string]*sessionEntry)
	for rows.Next() {
		var hash, username, createdS, expiresS string
		if err := rows.Scan(&hash, &username, &createdS, &expiresS); err != nil {
			return nil, err
		}
		created := parseTimeOrZero(createdS)
		expires := parseTimeOrZero(expiresS)
		if expires.IsZero() {
			continue
		}
		out[hash] = &sessionEntry{username: username, createdAt: created, expiresAt: expires}
	}
	return out, rows.Err()
}

// saveSession 持久化会话（存哈希）
func (am *AuthManager) saveSession(tokenHash string, s *sessionEntry) error {
	_, err := am.db.conn.Exec(`
		INSERT OR REPLACE INTO sessions (token_hash, username, created_at, expires_at) VALUES (?,?,?,?)
	`, tokenHash, s.username, s.createdAt.Format(time.RFC3339), s.expiresAt.Format(time.RFC3339))
	return err
}

// deleteSession 删除会话（登出）
func (am *AuthManager) deleteSession(tokenHash string) {
	if _, err := am.db.conn.Exec(`DELETE FROM sessions WHERE token_hash=?`, tokenHash); err != nil {
		log.Printf("[auth] warning: delete session: %v", err)
	}
}

// deleteExpiredSessions 清理过期会话
func (am *AuthManager) deleteExpiredSessions() {
	if _, err := am.db.conn.Exec(`DELETE FROM sessions WHERE expires_at <= ?`,
		time.Now().Format(time.RFC3339)); err != nil {
		log.Printf("[auth] warning: clean expired sessions: %v", err)
	}
}

// generateToken 生成随机 token（session 用 32 字节 hex）
func generateToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// username 当前管理用户名（settings 优先，回退配置）
func (am *AuthManager) username() string {
	if v, ok := am.db.GetSetting(settingAdminUser); ok && v != "" {
		return v
	}
	return am.cfg.Get().AdminUsername
}

// verifyPassword 校验密码：settings 中有 bcrypt 哈希则比对哈希；否则回退配置明文（常量时间）
func (am *AuthManager) verifyPassword(password string) bool {
	if hash, ok := am.db.GetSetting(settingPasswordHash); ok && hash != "" {
		return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
	}
	return subtle.ConstantTimeCompare([]byte(password), []byte(am.cfg.Get().AdminPassword)) == 1
}

// isDefaultPassword 是否仍在用默认密码（未设置 DB 哈希且配置为默认值）
func (am *AuthManager) isDefaultPassword() bool {
	if hash, ok := am.db.GetSetting(settingPasswordHash); ok && hash != "" {
		return false
	}
	return am.cfg.Get().AdminPassword == "admin123"
}

// loginAttemptKey 限流键：客户端 IP + 用户名（RemoteAddr 不可伪造）
func loginAttemptKey(r *http.Request, username string) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return host + "|" + username
}

// loginLockRemaining 剩余锁定时间（<=0 未锁定）
func (am *AuthManager) loginLockRemaining(key string) time.Duration {
	am.attemptMu.Lock()
	defer am.attemptMu.Unlock()
	a, ok := am.attempts[key]
	if !ok {
		return 0
	}
	now := time.Now()
	if now.Before(a.lockUntil) {
		return a.lockUntil.Sub(now)
	}
	if !a.lockUntil.IsZero() {
		delete(am.attempts, key) // 锁定期已过，清零重新计数
	}
	return 0
}

// recordLoginFailure 记录登录失败，达上限锁定
func (am *AuthManager) recordLoginFailure(key string) {
	am.attemptMu.Lock()
	defer am.attemptMu.Unlock()
	now := time.Now()
	a, ok := am.attempts[key]
	if !ok || now.Sub(a.firstFail) > loginFailWindow {
		a = &loginAttempt{firstFail: now}
		am.attempts[key] = a
	}
	a.failures++
	if a.failures >= maxLoginFailures {
		a.lockUntil = now.Add(loginLockDuration)
		log.Printf("[auth] login locked %v after %d failures (key=%s)", loginLockDuration, a.failures, key)
	}
}

// clearLoginFailures 登录成功清零
func (am *AuthManager) clearLoginFailures(key string) {
	am.attemptMu.Lock()
	defer am.attemptMu.Unlock()
	delete(am.attempts, key)
}

// Login 验证用户名密码，创建会话，返回明文 token（仅此次返回）
func (am *AuthManager) Login(username, password string) (string, bool) {
	am.mu.Lock()
	defer am.mu.Unlock()

	if subtle.ConstantTimeCompare([]byte(username), []byte(am.username())) != 1 {
		return "", false
	}
	if !am.verifyPassword(password) {
		return "", false
	}

	// 清理过期会话（内存 + DB）
	now := time.Now()
	for hash, s := range am.sessions {
		if now.After(s.expiresAt) {
			delete(am.sessions, hash)
		}
	}
	am.deleteExpiredSessions()

	token := generateToken()
	entry := &sessionEntry{username: username, createdAt: now, expiresAt: now.Add(sessionExpiry)}
	tokenHash := sha256Hex(token)
	am.sessions[tokenHash] = entry
	if err := am.saveSession(tokenHash, entry); err != nil {
		log.Printf("[auth] warning: persist session: %v", err)
	}
	log.Printf("[auth] login success: user=%s token=%s...%s", username, token[:8], token[len(token)-4:])
	return token, true
}

// Logout 注销会话
func (am *AuthManager) Logout(token string) {
	hash := sha256Hex(token)
	am.mu.Lock()
	delete(am.sessions, hash)
	am.mu.Unlock()
	am.deleteSession(hash)
}

// IsValidSession 检查会话有效性
func (am *AuthManager) IsValidSession(token string) bool {
	if token == "" {
		return false
	}
	am.mu.RLock()
	defer am.mu.RUnlock()
	s, ok := am.sessions[sha256Hex(token)]
	if !ok {
		return false
	}
	return !time.Now().After(s.expiresAt)
}

// extractSessionToken 从 Cookie 或 Bearer 头提取 session token
func extractSessionToken(r *http.Request) string {
	if cookie, err := r.Cookie(sessionCookieName); err == nil && cookie.Value != "" {
		return cookie.Value
	}
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return ""
}

// HandleLogin POST /api/login
func (am *AuthManager) HandleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	attemptKey := loginAttemptKey(r, body.Username)
	if remaining := am.loginLockRemaining(attemptKey); remaining > 0 {
		retryAfter := int(remaining.Seconds()) + 1
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		writeAPIError(w, http.StatusTooManyRequests,
			fmt.Sprintf("失败次数过多，请 %d 秒后再试", retryAfter))
		return
	}
	token, ok := am.Login(body.Username, body.Password)
	if !ok {
		am.recordLoginFailure(attemptKey)
		time.Sleep(loginFailDelay) // 拖慢在线爆破节奏
		writeAPIError(w, http.StatusUnauthorized, "用户名或密码错误")
		return
	}
	am.clearLoginFailures(attemptKey)

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		MaxAge:   int(sessionExpiry.Seconds()),
		SameSite: http.SameSiteStrictMode,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"success":             true,
		"token":               token,
		"username":            body.Username,
		"is_default_password": am.isDefaultPassword(),
	})
}

// HandleLogout POST /api/logout
func (am *AuthManager) HandleLogout(w http.ResponseWriter, r *http.Request) {
	token := extractSessionToken(r)
	if token != "" {
		am.Logout(token)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: "", Path: "/", HttpOnly: true, MaxAge: -1})
	writeJSON(w, http.StatusOK, map[string]string{"message": "logout success"})
}

// ---- API Key ----

// APIKey api_keys 表行
type APIKey struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	KeyHash    string `json:"-"`
	KeyPrefix  string `json:"key_prefix"`
	IsEnabled  bool   `json:"is_enabled"`
	MaxTasks   int    `json:"max_tasks"`
	UsedTasks  int    `json:"used_tasks"`
	ExpiresAt  string `json:"expires_at"`
	LastUsedAt string `json:"last_used_at"`
	CreatedAt  string `json:"created_at"`
}

// generateAPIKey 生成 sk-<40hex>
func generateAPIKey() string {
	return "sk-" + randomHex(20)
}

// keyPrefixOf 展示用前缀：sk- + 前 8 位 hex（共 11 字符）
func keyPrefixOf(key string) string {
	if len(key) >= 11 {
		return key[:11]
	}
	return key
}

// CreateAPIKey 创建 Key，返回明文（仅此一次）与行记录
func (am *AuthManager) CreateAPIKey(name string, maxTasks int, expiresDays int) (string, *APIKey, error) {
	if strings.TrimSpace(name) == "" {
		return "", nil, fmt.Errorf("name 不能为空")
	}
	plain := generateAPIKey()
	row := &APIKey{
		ID:        "key_" + randomHex(6),
		Name:      strings.TrimSpace(name),
		KeyHash:   sha256Hex(plain),
		KeyPrefix: keyPrefixOf(plain),
		IsEnabled: true,
		MaxTasks:  maxTasks,
		CreatedAt: nowStr(),
	}
	if expiresDays > 0 {
		row.ExpiresAt = time.Now().Add(time.Duration(expiresDays) * 24 * time.Hour).Format(time.RFC3339)
	}
	if _, err := am.db.conn.Exec(`
		INSERT INTO api_keys (id, name, key_hash, key_prefix, is_enabled, max_tasks, used_tasks, expires_at, last_used_at, created_at)
		VALUES (?,?,?,?,?,?,0,?, '', ?)
	`, row.ID, row.Name, row.KeyHash, row.KeyPrefix, boolToInt(row.IsEnabled), row.MaxTasks, row.ExpiresAt, row.CreatedAt); err != nil {
		return "", nil, fmt.Errorf("insert api key: %w", err)
	}
	log.Printf("[auth] api key created: id=%s prefix=%s name=%s", row.ID, row.KeyPrefix, row.Name)
	return plain, row, nil
}

// ListAPIKeys 列出全部 Key（不含哈希）
func (am *AuthManager) ListAPIKeys() ([]*APIKey, error) {
	rows, err := am.db.conn.Query(`
		SELECT id, name, key_hash, key_prefix, is_enabled, max_tasks, used_tasks, expires_at, last_used_at, created_at
		FROM api_keys ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("query api keys: %w", err)
	}
	defer rows.Close()
	var out []*APIKey
	for rows.Next() {
		var k APIKey
		var enabled int
		if err := rows.Scan(&k.ID, &k.Name, &k.KeyHash, &k.KeyPrefix, &enabled, &k.MaxTasks,
			&k.UsedTasks, &k.ExpiresAt, &k.LastUsedAt, &k.CreatedAt); err != nil {
			return nil, err
		}
		k.IsEnabled = enabled != 0
		out = append(out, &k)
	}
	return out, rows.Err()
}

// DeleteAPIKey 删除 Key
func (am *AuthManager) DeleteAPIKey(id string) error {
	_, err := am.db.conn.Exec(`DELETE FROM api_keys WHERE id=?`, id)
	return err
}

// ToggleAPIKey 启停 Key
func (am *AuthManager) ToggleAPIKey(id string, enabled bool) error {
	_, err := am.db.conn.Exec(`UPDATE api_keys SET is_enabled=? WHERE id=?`, boolToInt(enabled), id)
	return err
}

// ValidateAPIKey 校验明文 Key：按前缀查候选行 → sha256 常量时间比较 →
// 检查启用/过期/配额。通过返回行记录。
func (am *AuthManager) ValidateAPIKey(plain string) (*APIKey, error) {
	if !strings.HasPrefix(plain, "sk-") {
		return nil, fmt.Errorf("invalid api key")
	}
	rows, err := am.db.conn.Query(`
		SELECT id, name, key_hash, key_prefix, is_enabled, max_tasks, used_tasks, expires_at, last_used_at, created_at
		FROM api_keys WHERE key_prefix=?`, keyPrefixOf(plain))
	if err != nil {
		return nil, fmt.Errorf("query api key: %w", err)
	}
	defer rows.Close()
	hash := sha256Hex(plain)
	for rows.Next() {
		var k APIKey
		var enabled int
		if err := rows.Scan(&k.ID, &k.Name, &k.KeyHash, &k.KeyPrefix, &enabled, &k.MaxTasks,
			&k.UsedTasks, &k.ExpiresAt, &k.LastUsedAt, &k.CreatedAt); err != nil {
			return nil, err
		}
		k.IsEnabled = enabled != 0
		if subtle.ConstantTimeCompare([]byte(hash), []byte(k.KeyHash)) != 1 {
			continue
		}
		if !k.IsEnabled {
			return nil, fmt.Errorf("api key disabled")
		}
		if t := parseTimeOrZero(k.ExpiresAt); !t.IsZero() && time.Now().After(t) {
			return nil, fmt.Errorf("api key expired")
		}
		if k.MaxTasks > 0 && k.UsedTasks >= k.MaxTasks {
			return nil, fmt.Errorf("api key quota exceeded")
		}
		return &k, nil
	}
	return nil, fmt.Errorf("invalid api key")
}

// ConsumeKeyTask 受理任务时 used_tasks+1 并刷新 last_used_at
func (am *AuthManager) ConsumeKeyTask(keyID string) {
	if _, err := am.db.conn.Exec(`UPDATE api_keys SET used_tasks=used_tasks+1, last_used_at=? WHERE id=?`,
		nowStr(), keyID); err != nil {
		log.Printf("[auth] consume key task failed: %v", err)
	}
}

// TouchKey 只刷新 last_used_at（查询类请求）
func (am *AuthManager) TouchKey(keyID string) {
	if _, err := am.db.conn.Exec(`UPDATE api_keys SET last_used_at=? WHERE id=?`, nowStr(), keyID); err != nil {
		log.Printf("[auth] touch key failed: %v", err)
	}
}

// ---- 中间件 ----

// ctxKeyIDKey context 键：已认证的 API Key 行
type ctxKeyIDType struct{}

var ctxAPIKey = ctxKeyIDType{}

// APIKeyFromContext 取出中间件放入的 API Key
func APIKeyFromContext(ctx context.Context) *APIKey {
	k, _ := ctx.Value(ctxAPIKey).(*APIKey)
	return k
}

// Middleware 认证中间件：
//   - /api/login、/health、/web*、/ 放行；
//   - /v1/* 走 API Key；
//   - /api/* 走 session。
func (am *AuthManager) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path == "/api/login" || path == "/api/logout" || path == "/health" ||
			strings.HasPrefix(path, "/web") || path == "/" || path == "/favicon.ico" {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(path, "/v1/") {
			var apiKey string
			if authHeader := r.Header.Get("Authorization"); strings.HasPrefix(authHeader, "Bearer ") {
				apiKey = strings.TrimPrefix(authHeader, "Bearer ")
			} else if xKey := r.Header.Get("x-api-key"); xKey != "" {
				apiKey = xKey
			}
			if apiKey == "" {
				writeV1Error(w, http.StatusUnauthorized, "invalid_api_key",
					"缺少 API Key，请使用 Authorization: Bearer sk-xxx")
				return
			}
			key, err := am.ValidateAPIKey(apiKey)
			if err != nil {
				writeV1Error(w, http.StatusUnauthorized, "invalid_api_key", err.Error())
				return
			}
			ctx := context.WithValue(r.Context(), ctxAPIKey, key)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		if strings.HasPrefix(path, "/api/") {
			token := extractSessionToken(r)
			if !am.IsValidSession(token) {
				writeAPIError(w, http.StatusUnauthorized, "unauthorized")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
