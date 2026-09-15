package main

// ---- 账号池管理 ----
// 职责：
//  1. 导入：解析深链 URL（minimax-hub-cn://auth-callback?accessToken=<JWT>）或裸 JWT，
//     base64 解码 JWT 第二段提取 user.deviceID，生成终身不变的设备指纹档案
//     （os_name/cpu_core_num/device_memory 随机定格），立即调 user/info 验证并拉余额；
//     重复 device_id 更新 token 而非重复插入；
//  2. 生命周期：验证/续期/余额刷新/免费试用领取/启停/删除；
//  3. 选号器：过滤（启用+状态+冷却+余额+并发槽）→ 打分 → 加权随机，输出可用账号；
//  4. 账号处置：冷却熔断、标记 empty/token_expired、成功失败计数。

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math"
	mrand "math/rand"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 账号状态常量
const (
	AcctActive       = "active"        // 可调度
	AcctTokenExpired = "token_expired" // token 失效且续期失败，需人工重新导入
	AcctEmpty        = "empty"         // 余额耗尽摘除（巡检回升后自动恢复）
	AcctCooldown     = "cooldown"      // 冷却中（cooldown_until 到期后自动恢复 active）
	AcctDisabled     = "disabled"      // 人工禁用
)

// Account 账号行（token 明文入库但 json:"-"，绝不返回前端）
type Account struct {
	ID               string  `json:"id"`
	Label            string  `json:"label"`
	Token            string  `json:"-"`
	DeviceID         string  `json:"device_id"`
	UUID             string  `json:"uuid"`
	OsName           string  `json:"os_name"`
	CPUCoreNum       int     `json:"cpu_core_num"`
	DeviceMemory     int     `json:"device_memory"`
	UserID           string  `json:"user_id"`
	Username         string  `json:"username"`
	GroupID          string  `json:"group_id"`
	Status           string  `json:"status"`
	IsEnabled        bool    `json:"is_enabled"`
	Balance          float64 `json:"balance"` // -1=未知
	BalanceUpdatedAt string  `json:"balance_updated_at"`
	LastRenewAt      string  `json:"last_renew_at"`
	LastCheckAt      string  `json:"last_check_at"`
	CooldownUntil    string  `json:"cooldown_until"`
	FailCount        int     `json:"fail_count"`
	SuccessCount     int     `json:"success_count"`
	TotalTasks       int     `json:"total_tasks"`
	LastError        string  `json:"last_error"`
	CreatedAt        string  `json:"created_at"`
	UpdatedAt        string  `json:"updated_at"`
}

// Profile 导出该账号的设备档案+凭据（供云客户端构造请求）
func (a *Account) Profile() DeviceProfile {
	return DeviceProfile{
		Token:    a.Token,
		DeviceID: a.DeviceID,
		UUID:     a.UUID,
		OsName:   a.OsName,
		CPUCores: a.CPUCoreNum,
		MemoryGB: a.DeviceMemory,
		GroupID:  a.GroupID,
		Lang:     "zh",
	}
}

// AccountManager 账号池管理器
type AccountManager struct {
	db     *DB
	client *MiniMaxClient
	cfg    *AppConfig
	rt     *RuntimeSettings

	mu       sync.Mutex
	inflight map[string]int // accountID → 正在执行的并发任务数（本地计数）
	renewMu  sync.Mutex     // 同账号续期串行化（防止并发 renewal 互相作废）
}

// NewAccountManager 创建账号管理器
func NewAccountManager(db *DB, client *MiniMaxClient, cfg *AppConfig, rt *RuntimeSettings) *AccountManager {
	return &AccountManager{
		db:       db,
		client:   client,
		cfg:      cfg,
		rt:       rt,
		inflight: make(map[string]int),
	}
}

// ---- JWT / 深链解析 ----

// parseImportLine 从一行文本解析 accessToken：
// 支持三种形态——完整深链 URL（minimax-hub[-cn]://auth-callback?accessToken=...）、
// https 回调 URL（?accessToken=...）、裸 JWT（xxx.yyy.zzz）
func parseImportLine(line string) (string, error) {
	s := strings.TrimSpace(line)
	if s == "" {
		return "", fmt.Errorf("empty line")
	}
	// 形态1/2：带 accessToken= 参数的 URL（深链或 https）
	if idx := strings.Index(s, "accessToken="); idx >= 0 {
		v := s[idx+len("accessToken="):]
		if end := strings.IndexAny(v, "&# \t"); end >= 0 {
			v = v[:end]
		}
		v = strings.Trim(v, `"'`)
		if isJWT(v) {
			return v, nil
		}
		return "", fmt.Errorf("accessToken 参数不是合法 JWT")
	}
	// 形态3：裸 JWT
	if isJWT(s) {
		return s, nil
	}
	return "", fmt.Errorf("无法识别（需要深链 URL 或裸 JWT）")
}

// isJWT 粗略校验 JWT 形态：三段 base64url，以 . 分隔
func isJWT(s string) bool {
	parts := strings.Split(s, ".")
	return len(parts) == 3 && len(parts[0]) > 0 && len(parts[1]) > 0 && len(parts[2]) > 0
}

// jwtPayload 解码 JWT 第二段（base64url，无签名校验——只用于提取 deviceID）
func jwtPayload(token string) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("not a JWT")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// 部分实现带 padding，兜底再试一次
		raw, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return nil, fmt.Errorf("decode JWT payload: %w", err)
		}
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("parse JWT payload: %w", err)
	}
	return payload, nil
}

// extractDeviceID 从 JWT payload 提取设备 ID（逆向报告 §2.3：主路径 user.deviceID，
// 兼容 user.device_id / 顶层 deviceID / device_id / did 等变体字段）。
// ok=false 且 err=nil 表示 token 可正常解码但未携带 deviceID——网页端签发的
// token 常见此情况。官方桌面端此时直接省略 device_id 参数继续工作（报告 §2.2
// buildCloudCommonParams 的 if(deviceId) 分支），导入侧不视为硬错误，
// 由调用方生成设备档案兜底。仅 JWT 本身无法解码时返回 err。
func extractDeviceID(token string) (id string, ok bool, err error) {
	payload, err := jwtPayload(token)
	if err != nil {
		return "", false, err
	}
	lookup := func(m map[string]any, keys ...string) (string, bool) {
		if m == nil {
			return "", false
		}
		for _, k := range keys {
			if s, isStr := m[k].(string); isStr && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s), true
			}
		}
		return "", false
	}
	user, _ := payload["user"].(map[string]any)
	if v, found := lookup(user, "deviceID", "device_id"); found {
		return v, true, nil
	}
	if v, found := lookup(payload, "deviceID", "device_id", "did"); found {
		return v, true, nil
	}
	return "", false, nil
}

// newDeviceUUID 生成 UUID v4，格式与桌面端首启 generateDeviceID
// （crypto.randomUUID()）一致，用作无 deviceID token 的设备档案兜底。
func newDeviceUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10xx
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// randomHex 生成 n 字节随机 hex 串
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 失败极罕见，退化到时间戳（保证可用性）
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// newAccountID 生成账号 ID：acct_<12hex>
func newAccountID() string { return "acct_" + randomHex(6) }

// generateFingerprint 生成设备指纹档案（导入时定格，终身不变）：
// os_name ∈ {Windows, macOS}（Windows 占比拟真 80%），cpu ∈ {8,12,16,20,24}，内存 ∈ {16,32,64}GB
func generateFingerprint() (osName string, cpu, mem int) {
	r := mrand.Intn(100)
	switch {
	case r < 80:
		osName = "Windows"
	default:
		osName = "macOS"
	}
	cpus := []int{8, 12, 16, 20, 24}
	cpu = cpus[mrand.Intn(len(cpus))]
	mems := []int{16, 32, 64}
	mem = mems[mrand.Intn(len(mems))]
	return osName, cpu, mem
}

// ---- 导入 ----

// ImportLineResult 单行导入结果
type ImportLineResult struct {
	Line    int    `json:"line"`
	Summary string `json:"summary"` // 脱敏摘要（device_id 前 8 位等）
	Status  string `json:"status"`  // created | updated | failed
	Message string `json:"message"`
}

// ImportResult 批量导入汇总
type ImportResult struct {
	Total   int                `json:"total"`
	Created int                `json:"created"`
	Updated int                `json:"updated"`
	Failed  int                `json:"failed"`
	Results []ImportLineResult `json:"results"`
}

// ImportAccounts 逐行解析导入。每行：解析 token → 提取 deviceID →
// 建档/更新 → user/info 验证（取 user_id/username）→ balance 拉余额。
// 验证失败不阻断入库（账号标记相应状态），方便先导入后排查。
func (am *AccountManager) ImportAccounts(ctx context.Context, content, labelPrefix string) *ImportResult {
	result := &ImportResult{}
	lines := strings.Split(content, "\n")
	lineNo := 0
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		lineNo++
		result.Total++
		token, err := parseImportLine(line)
		if err != nil {
			result.Failed++
			result.Results = append(result.Results, ImportLineResult{
				Line: lineNo, Summary: summarizeLine(line), Status: "failed", Message: err.Error(),
			})
			continue
		}
		deviceID, hasDeviceID, err := extractDeviceID(token)
		if err != nil {
			result.Failed++
			result.Results = append(result.Results, ImportLineResult{
				Line: lineNo, Summary: "jwt(" + tokenPrefix(token) + ")", Status: "failed", Message: err.Error(),
			})
			continue
		}

		var acc *Account
		var created bool
		var summary string
		if hasDeviceID {
			summary = "device=" + tokenPrefix(deviceID)
			acc, created, err = am.upsertAccount(token, deviceID, labelPrefix, lineNo)
		} else {
			// 网页端签发的 token 常见不携带 deviceID：官方桌面端此时省略
			// device_id 参数继续工作，这里先验证取 user_id 去重，
			// 全新账号生成独立设备档案兜底（等价桌面端首启行为）
			summary = "jwt(" + tokenPrefix(token) + ")"
			acc, created, err = am.upsertWebTokenAccount(ctx, token, labelPrefix, lineNo)
		}
		if err != nil {
			result.Failed++
			result.Results = append(result.Results, ImportLineResult{
				Line: lineNo, Summary: summary, Status: "failed", Message: err.Error(),
			})
			continue
		}

		// 验证 token 并回填 user_id/username + 余额
		msg := am.verifyAndFill(ctx, acc)
		if !hasDeviceID && created {
			msg += "（JWT 未携带 deviceID，已生成设备档案）"
		}
		if created {
			result.Created++
			result.Results = append(result.Results, ImportLineResult{Line: lineNo, Summary: summary, Status: "created", Message: msg})
		} else {
			result.Updated++
			result.Results = append(result.Results, ImportLineResult{Line: lineNo, Summary: summary, Status: "updated", Message: msg})
		}
	}
	log.Printf("[accounts] import done: total=%d created=%d updated=%d failed=%d",
		result.Total, result.Created, result.Updated, result.Failed)
	return result
}

// upsertAccount 按 device_id 建档或更新 token（重复导入=换 token，不重复插入）
func (am *AccountManager) upsertAccount(token, deviceID, labelPrefix string, lineNo int) (*Account, bool, error) {
	now := nowStr()
	existing, err := am.GetAccountByDeviceID(deviceID)
	if err == nil && existing != nil {
		// 更新 token，重置失效状态（重新导入即人工确认凭据有效）
		newStatus := existing.Status
		if newStatus == AcctTokenExpired {
			newStatus = AcctActive
		}
		if _, err := am.db.conn.Exec(`
			UPDATE accounts SET token=?, status=?, last_error='', is_enabled=1, updated_at=? WHERE id=?
		`, token, newStatus, now, existing.ID); err != nil {
			return nil, false, fmt.Errorf("update account: %w", err)
		}
		existing.Token = token
		existing.Status = newStatus
		existing.IsEnabled = true
		return existing, false, nil
	}

	osName, cpu, mem := generateFingerprint()
	label := ""
	if labelPrefix != "" {
		label = fmt.Sprintf("%s-%02d", labelPrefix, lineNo)
	}
	acc := &Account{
		ID:           newAccountID(),
		Label:        label,
		Token:        token,
		DeviceID:     deviceID,
		UUID:         deviceID, // 桌面端语义：uuid 与 device_id 同源（设计方案 §4.1.3）
		OsName:       osName,
		CPUCoreNum:   cpu,
		DeviceMemory: mem,
		Status:       AcctActive,
		IsEnabled:    true,
		Balance:      -1,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if _, err := am.db.conn.Exec(`
		INSERT INTO accounts (id, label, token, device_id, uuid, os_name, cpu_core_num, device_memory,
			user_id, username, group_id, status, is_enabled, balance, balance_updated_at,
			last_renew_at, last_check_at, cooldown_until, fail_count, success_count, total_tasks,
			last_error, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?, '', 0,0,0, '',?,?)
	`, acc.ID, acc.Label, acc.Token, acc.DeviceID, acc.UUID, acc.OsName, acc.CPUCoreNum, acc.DeviceMemory,
		acc.UserID, acc.Username, acc.GroupID, acc.Status, boolToInt(acc.IsEnabled), acc.Balance,
		acc.BalanceUpdatedAt, acc.LastRenewAt, acc.LastCheckAt, acc.CreatedAt, acc.UpdatedAt); err != nil {
		return nil, false, fmt.Errorf("insert account: %w", err)
	}
	return acc, true, nil
}

// upsertWebTokenAccount 处理未携带 deviceID 的 token（网页端签发）：
//  1. 先用临时档案调 user/info 验证，成功则按 realUserID 去重（同一账号
//     重复导入只更新 token，设备档案保持不变）；
//  2. 验证失败（网络/无效 token）退化按 token 全文去重；
//  3. 都查不到 → 生成 UUID v4 设备档案建档（等价桌面端首启 generateDeviceID）。
//
// 无效 token 不在这里拒绝：与桌面路径一致"先入库后排查"，
// 后续 verifyAndFill 会把账号标记为 token_expired。
func (am *AccountManager) upsertWebTokenAccount(ctx context.Context, token, labelPrefix string, lineNo int) (*Account, bool, error) {
	now := nowStr()
	osName, cpu, mem := generateFingerprint()
	probe := &Account{Token: token, OsName: osName, CPUCoreNum: cpu, DeviceMemory: mem}

	var existing *Account
	if info, cerr := am.client.GetUserInfo(ctx, probe.Profile()); cerr == nil && info.RealUserID != "" {
		if acc, err := am.GetAccountByUserID(info.RealUserID); err == nil && acc != nil {
			existing = acc
		}
	}
	if existing == nil {
		if acc, err := am.GetAccountByToken(token); err == nil && acc != nil {
			existing = acc
		}
	}
	if existing != nil {
		// 更新 token，重置失效状态（与 upsertAccount 更新分支语义一致）
		newStatus := existing.Status
		if newStatus == AcctTokenExpired {
			newStatus = AcctActive
		}
		if _, err := am.db.conn.Exec(`
			UPDATE accounts SET token=?, status=?, last_error='', is_enabled=1, updated_at=? WHERE id=?
		`, token, newStatus, now, existing.ID); err != nil {
			return nil, false, fmt.Errorf("update account: %w", err)
		}
		existing.Token = token
		existing.Status = newStatus
		existing.IsEnabled = true
		return existing, false, nil
	}
	return am.upsertAccount(token, newDeviceUUID(), labelPrefix, lineNo)
}

// GetAccountByUserID 按 user_id 取账号（web token 导入去重用）
func (am *AccountManager) GetAccountByUserID(userID string) (*Account, error) {
	if userID == "" {
		return nil, fmt.Errorf("empty user_id")
	}
	row := am.db.conn.QueryRow(`SELECT `+accountCols+` FROM accounts WHERE user_id=? LIMIT 1`, userID)
	return scanAccount(row)
}

// GetAccountByToken 按完整 token 取账号（web token 导入兜底去重）
func (am *AccountManager) GetAccountByToken(token string) (*Account, error) {
	row := am.db.conn.QueryRow(`SELECT `+accountCols+` FROM accounts WHERE token=? LIMIT 1`, token)
	return scanAccount(row)
}

// ---- 恢复链接（导入的逆过程：把池内账号还原回官方桌面端） ----

// DeepLinkScheme 官方桌面端深链协议名（逆向报告 §3.1：domestic prod =
// minimax-hub-cn，overseas prod = minimax-hub；本服务仅对接官方 prod 域名）
func DeepLinkScheme(region string) string {
	if strings.EqualFold(strings.TrimSpace(region), "overseas") {
		return "minimax-hub"
	}
	return "minimax-hub-cn"
}

// BuildRestoreLink 生成账号恢复深链：<scheme>://auth-callback?accessToken=<JWT>。
// 在装有 MiniMax Design 的机器上打开，桌面端拦截该深链（handleAuthRedirect 要求
// action=auth-callback 且携带 accessToken）→ user/info 验证 → 完成登录，
// 即把池内账号"恢复"到官方客户端。
// 同时解析 JWT 的 exp（有效期）与 user.id 供前端展示。
// JWT 字符集为 base64url + 点号，天然 URL 安全，与官方深链一致不做转义。
func BuildRestoreLink(region, token string) (link string, exp int64, userID string) {
	link = DeepLinkScheme(region) + "://auth-callback?accessToken=" + token
	if payload, err := jwtPayload(token); err == nil {
		switch v := payload["exp"].(type) {
		case float64:
			exp = int64(v)
		case string:
			if n, e := strconv.ParseInt(v, 10, 64); e == nil {
				exp = n
			}
		}
		if u, ok := payload["user"].(map[string]any); ok {
			switch v := u["id"].(type) {
			case string:
				userID = v
			case float64:
				userID = strconv.FormatInt(int64(v), 10)
			}
		}
	}
	return link, exp, userID
}

// verifyAndFill 调 user/info 验证 + 拉余额，回填 DB；返回人类可读结果消息
func (am *AccountManager) verifyAndFill(ctx context.Context, acc *Account) string {
	info, cerr := am.client.GetUserInfo(ctx, acc.Profile())
	if cerr != nil {
		am.markStatus(acc.ID, AcctTokenExpired, cerr.Msg)
		return "token 验证失败: " + cerr.Msg
	}
	msg := fmt.Sprintf("验证通过 user=%s", info.Name)
	bal, cerr2 := am.client.GetBalance(ctx, acc.Profile())
	if cerr2 == nil {
		am.UpdateBalance(acc.ID, bal)
		msg += fmt.Sprintf(", 余额=%g", bal)
	} else {
		msg += ", 余额查询失败: " + cerr2.Msg
	}
	if _, err := am.db.conn.Exec(`
		UPDATE accounts SET user_id=?, username=?, last_check_at=?, updated_at=? WHERE id=?
	`, info.RealUserID, info.Name, nowStr(), nowStr(), acc.ID); err != nil {
		log.Printf("[accounts] fill user info failed: %v", err)
	}
	return msg
}

// summarizeLine 导入行脱敏摘要（不打完整 token）
func summarizeLine(line string) string {
	s := strings.TrimSpace(line)
	if len(s) > 40 {
		s = s[:40] + "..."
	}
	return s
}

// tokenPrefix 日志脱敏：只保留前 8 位
func tokenPrefix(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// boolToInt SQLite 布尔存储
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---- 查询 / 单行操作 ----

// scanAccount 行扫描（列顺序与查询一致）
func scanAccount(scanner interface {
	Scan(dest ...any) error
}) (*Account, error) {
	var a Account
	var enabled int
	err := scanner.Scan(&a.ID, &a.Label, &a.Token, &a.DeviceID, &a.UUID, &a.OsName,
		&a.CPUCoreNum, &a.DeviceMemory, &a.UserID, &a.Username, &a.GroupID, &a.Status,
		&enabled, &a.Balance, &a.BalanceUpdatedAt, &a.LastRenewAt, &a.LastCheckAt,
		&a.CooldownUntil, &a.FailCount, &a.SuccessCount, &a.TotalTasks, &a.LastError,
		&a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return nil, err
	}
	a.IsEnabled = enabled != 0
	return &a, nil
}

const accountCols = `id, label, token, device_id, uuid, os_name, cpu_core_num, device_memory,
	user_id, username, group_id, status, is_enabled, balance, balance_updated_at,
	last_renew_at, last_check_at, cooldown_until, fail_count, success_count, total_tasks,
	last_error, created_at, updated_at`

// ListAccounts 全量账号列表（按创建时间）
func (am *AccountManager) ListAccounts() ([]*Account, error) {
	rows, err := am.db.conn.Query(`SELECT ` + accountCols + ` FROM accounts ORDER BY created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("query accounts: %w", err)
	}
	defer rows.Close()
	var out []*Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetAccount 按 ID 取账号
func (am *AccountManager) GetAccount(id string) (*Account, error) {
	row := am.db.conn.QueryRow(`SELECT `+accountCols+` FROM accounts WHERE id=?`, id)
	a, err := scanAccount(row)
	if err != nil {
		return nil, fmt.Errorf("account %s not found: %w", id, err)
	}
	return a, nil
}

// GetAccountByDeviceID 按 device_id 取账号（导入去重用）
func (am *AccountManager) GetAccountByDeviceID(deviceID string) (*Account, error) {
	row := am.db.conn.QueryRow(`SELECT `+accountCols+` FROM accounts WHERE device_id=?`, deviceID)
	return scanAccount(row)
}

// markStatus 更新账号状态与最后错误
func (am *AccountManager) markStatus(id, status, lastErr string) {
	if _, err := am.db.conn.Exec(`UPDATE accounts SET status=?, last_error=?, updated_at=? WHERE id=?`,
		status, truncate(lastErr, 300), nowStr(), id); err != nil {
		log.Printf("[accounts] mark status %s failed: %v", status, err)
	}
}

// SetEnabled 启用/禁用账号
func (am *AccountManager) SetEnabled(id string, enabled bool) error {
	status := AcctActive
	if !enabled {
		status = AcctDisabled
	}
	_, err := am.db.conn.Exec(`UPDATE accounts SET is_enabled=?, status=?, updated_at=? WHERE id=?`,
		boolToInt(enabled), status, nowStr(), id)
	return err
}

// DeleteAccount 删除账号
func (am *AccountManager) DeleteAccount(id string) error {
	_, err := am.db.conn.Exec(`DELETE FROM accounts WHERE id=?`, id)
	return err
}

// UpdateBalance 更新余额缓存 + 写快照；余额回升时自动从 empty 恢复 active
func (am *AccountManager) UpdateBalance(id string, balance float64) {
	now := nowStr()
	// 5 个占位符依次：balance、balance_updated_at、CASE 比较值、updated_at、id
	if _, err := am.db.conn.Exec(`
		UPDATE accounts SET balance=?, balance_updated_at=?,
			status = CASE WHEN status='empty' AND ?>0 THEN 'active' ELSE status END,
			updated_at=? WHERE id=?
	`, balance, now, balance, now, id); err != nil {
		log.Printf("[accounts] update balance failed: %v", err)
		return
	}
	if _, err := am.db.conn.Exec(`INSERT INTO credit_snapshots (account_id, ts, balance) VALUES (?,?,?)`,
		id, now, balance); err != nil {
		log.Printf("[accounts] insert credit snapshot failed: %v", err)
	}
}

// MarkEmpty 余额耗尽摘除
func (am *AccountManager) MarkEmpty(id, reason string) {
	am.markStatus(id, AcctEmpty, reason)
	log.Printf("[accounts] %s marked empty: %s", id, truncate(reason, 100))
}

// MarkTokenExpired token 失效（续期也失败），需人工重新导入
func (am *AccountManager) MarkTokenExpired(id, reason string) {
	am.markStatus(id, AcctTokenExpired, reason)
	log.Printf("[accounts] %s marked token_expired: %s", id, truncate(reason, 100))
}

// ApplyCooldown 冷却账号（1033/5xx 熔断）：status=cooldown + cooldown_until + fail_count++
func (am *AccountManager) ApplyCooldown(id string, d time.Duration, reason string) {
	until := time.Now().Add(d).Format(time.RFC3339)
	if _, err := am.db.conn.Exec(`
		UPDATE accounts SET status='cooldown', cooldown_until=?, fail_count=fail_count+1,
			last_error=?, updated_at=? WHERE id=?
	`, until, truncate(reason, 300), nowStr(), id); err != nil {
		log.Printf("[accounts] apply cooldown failed: %v", err)
	}
	log.Printf("[accounts] %s cooldown %v: %s", id, d, truncate(reason, 100))
}

// MarkSuccess 任务成功计数（fail_count 清零）
func (am *AccountManager) MarkSuccess(id string) {
	if _, err := am.db.conn.Exec(`
		UPDATE accounts SET success_count=success_count+1, total_tasks=total_tasks+1,
			fail_count=0, updated_at=? WHERE id=?
	`, nowStr(), id); err != nil {
		log.Printf("[accounts] mark success failed: %v", err)
	}
}

// MarkTaskDone 任务终态（失败/取消）时 total_tasks 递增
func (am *AccountManager) MarkTaskDone(id string) {
	if id == "" {
		return
	}
	if _, err := am.db.conn.Exec(`UPDATE accounts SET total_tasks=total_tasks+1, updated_at=? WHERE id=?`,
		nowStr(), id); err != nil {
		log.Printf("[accounts] mark task done failed: %v", err)
	}
}

// ---- 续期 / 刷新 ----

// TryRenew 续期 token（同账号串行）。成功则替换存储并返回新档案；失败返回错误。
func (am *AccountManager) TryRenew(ctx context.Context, id string) (*Account, error) {
	am.renewMu.Lock()
	defer am.renewMu.Unlock()

	acc, err := am.GetAccount(id)
	if err != nil {
		return nil, err
	}
	newToken, cerr := am.client.RenewToken(ctx, acc.Profile())
	if cerr != nil {
		am.markStatus(id, AcctTokenExpired, "renewal failed: "+cerr.Msg)
		return nil, fmt.Errorf("renewal failed (kind=%d): %s", cerr.Kind, cerr.Msg)
	}
	// 续期后 deviceID 理论上不变；仍重新解码做一致性校验（防指纹漂移，设计方案 §4.7.3）。
	// 新 token 未携带 deviceID（网页端续签）时跳过比对，档案保持不变。
	newDeviceID, hasNewDeviceID, derr := extractDeviceID(newToken)
	if derr == nil && hasNewDeviceID && newDeviceID != acc.DeviceID {
		log.Printf("[accounts] WARNING: %s renewed token deviceID changed %s -> %s (keep profile)",
			id, tokenPrefix(acc.DeviceID), tokenPrefix(newDeviceID))
	}
	now := nowStr()
	if _, err := am.db.conn.Exec(`UPDATE accounts SET token=?, last_renew_at=?, last_error='', updated_at=? WHERE id=?`,
		newToken, now, now, id); err != nil {
		return nil, fmt.Errorf("persist renewed token: %w", err)
	}
	log.Printf("[accounts] %s token renewed (old=%s new=%s)", id, tokenPrefix(acc.Token), tokenPrefix(newToken))
	acc.Token = newToken
	acc.LastRenewAt = now
	return acc, nil
}

// RefreshAccount 手动刷新：续期 + user/info + 余额
func (am *AccountManager) RefreshAccount(ctx context.Context, id string) (*Account, error) {
	acc, err := am.GetAccount(id)
	if err != nil {
		return nil, err
	}
	// 先续期（失败不阻断，继续用旧 token 验证）
	if _, rerr := am.TryRenew(ctx, id); rerr != nil {
		log.Printf("[accounts] refresh %s renew failed: %v", id, rerr)
	}
	acc, err = am.GetAccount(id)
	if err != nil {
		return nil, err
	}
	msg := am.verifyAndFill(ctx, acc)
	acc, err = am.GetAccount(id)
	if err != nil {
		return nil, err
	}
	if acc.Status == AcctTokenExpired {
		return acc, fmt.Errorf("%s", msg)
	}
	return acc, nil
}

// ClaimTrial 领取海螺03视频免费试用次数
func (am *AccountManager) ClaimTrial(ctx context.Context, id string) (*TrialStatus, error) {
	acc, err := am.GetAccount(id)
	if err != nil {
		return nil, err
	}
	st, cerr := am.client.ClaimTrial(ctx, acc.Profile())
	if cerr != nil {
		return nil, fmt.Errorf("claim failed: %s", cerr.Msg)
	}
	// 领取后刷新余额
	if bal, berr := am.client.GetBalance(ctx, acc.Profile()); berr == nil {
		am.UpdateBalance(id, bal)
	}
	return st, nil
}

// ---- 选号器 ----

// 选号失败原因码
const (
	PickOK                = ""
	PickNoAccount         = "no_account"          // 池内无启用账号
	PickInsufficient      = "insufficient_credit" // 全部余额不足
	PickAllCooling        = "all_cooling"         // 全部冷却/失效中
	PickConcurrencyFull   = "concurrency_full"    // 并发槽全满
	PickPinnedUnavailable = "pinned_unavailable"  // 钉住的账号不可用（任务已置终态）
)

// PickAccount 选号：过滤 → 打分 → 加权随机 → 抢占并发槽。
// exclude 为本任务已试过（需换号）的账号集合。
// 成功返回账号（调用方用完必须 ReleaseSlot）；失败返回 reason 码。
func (am *AccountManager) PickAccount(needCredit float64, exclude map[string]bool) (*Account, string) {
	all, err := am.ListAccounts()
	if err != nil {
		log.Printf("[accounts] pick: list failed: %v", err)
		return nil, PickNoAccount
	}
	now := time.Now()
	safety := am.rt.BalanceSafetyFactor()
	limit := am.rt.PerAccountConcurrency()

	var (
		candidates     []*Account
		anyEnabled     bool
		anySchedulable bool
		balanceBlock   bool
		concBlock      bool
	)
	for _, a := range all {
		if !a.IsEnabled || exclude[a.ID] {
			continue
		}
		anyEnabled = true
		// 冷却到期自动恢复
		if a.Status == AcctCooldown {
			if t := parseTimeOrZero(a.CooldownUntil); !t.IsZero() && now.After(t) {
				a.Status = AcctActive
				am.markStatus(a.ID, AcctActive, "")
			}
		}
		if a.Status != AcctActive {
			continue // token_expired / empty / disabled / cooldown 未到期
		}
		anySchedulable = true
		// 余额过滤：balance<0 视为未知放行（PRECHECK 阶段兜底）
		if a.Balance >= 0 && needCredit > 0 && a.Balance < needCredit*safety {
			balanceBlock = true
			continue
		}
		// 并发槽过滤（内存计数）
		am.mu.Lock()
		full := am.inflight[a.ID] >= limit
		am.mu.Unlock()
		if full {
			concBlock = true
			continue
		}
		candidates = append(candidates, a)
	}

	if len(candidates) == 0 {
		switch {
		case !anyEnabled:
			return nil, PickNoAccount
		case !anySchedulable && balanceBlock:
			return nil, PickInsufficient
		case !anySchedulable:
			return nil, PickAllCooling
		case balanceBlock && !concBlock:
			return nil, PickInsufficient
		default:
			return nil, PickConcurrencyFull
		}
	}

	// 打分 + 加权随机（打散热点，避免全队列涌向同一账号）
	type scored struct {
		acc *Account
		w   float64
	}
	var scoredList []scored
	total := 0.0
	for _, a := range candidates {
		s := scoreAccount(a)
		w := math.Pow(s, 3) // 拉大分差但保留随机性
		if w < 0.01 {
			w = 0.01
		}
		scoredList = append(scoredList, scored{a, w})
		total += w
	}
	pick := mrand.Float64() * total
	chosen := scoredList[len(scoredList)-1].acc
	for _, sc := range scoredList {
		pick -= sc.w
		if pick <= 0 {
			chosen = sc.acc
			break
		}
	}

	// 抢占并发槽（双检，防止并发 Pick 超额）
	am.mu.Lock()
	if am.inflight[chosen.ID] >= limit {
		am.mu.Unlock()
		return nil, PickConcurrencyFull
	}
	am.inflight[chosen.ID]++
	am.mu.Unlock()
	return chosen, PickOK
}

// scoreAccount 打分：余额充足度 + 成功率 + 轮转新鲜度 - 失败惩罚（设计方案 §4.2.2 简化版）
func scoreAccount(a *Account) float64 {
	balNorm := 0.5 // 余额未知给中性分
	if a.Balance >= 0 {
		balNorm = math.Min(a.Balance/1000.0, 1.0)
	}
	total := a.SuccessCount + a.FailCount
	succRate := 0.5
	if total > 0 {
		succRate = float64(a.SuccessCount) / float64(total)
	}
	// 轮转新鲜度：累计任务越少分越高（自然轮转新账号）
	fresh := 1.0 / (1.0 + float64(a.TotalTasks)/20.0)
	risk := math.Min(float64(a.FailCount)*0.1, 0.5)
	score := 0.35*balNorm + 0.3*succRate + 0.35*fresh - risk
	if score < 0.01 {
		score = 0.01
	}
	return score
}

// ReleaseSlot 释放账号并发槽
func (am *AccountManager) ReleaseSlot(accountID string) {
	if accountID == "" {
		return
	}
	am.mu.Lock()
	defer am.mu.Unlock()
	if n, ok := am.inflight[accountID]; ok {
		if n <= 1 {
			delete(am.inflight, accountID)
		} else {
			am.inflight[accountID] = n - 1
		}
	}
}

// AcquireSlot 为指定账号抢占并发槽（任务恢复时沿用原账号）
func (am *AccountManager) AcquireSlot(accountID string) bool {
	am.mu.Lock()
	defer am.mu.Unlock()
	if am.inflight[accountID] >= am.rt.PerAccountConcurrency() {
		return false
	}
	am.inflight[accountID]++
	return true
}

// ---- 账号健康测试（管理台「测试 / 批量测试」） ----

// AccountTestResult 单账号健康测试结果（三连：user/info + balance + trial status）
type AccountTestResult struct {
	AccountID    string       `json:"account_id"`
	Label        string       `json:"label"`
	OK           bool         `json:"ok"`
	TokenValid   bool         `json:"token_valid"`
	TokenExpired bool         `json:"token_expired"` // true=云端明确 401/403（区别于网络故障）
	UserID       string       `json:"user_id"`
	Username     string       `json:"username"`
	Balance      float64      `json:"balance"` // -1=查询失败或未知
	Trial        *TrialStatus `json:"trial"`   // nil=试用状态查询失败
	Message      string       `json:"message"`
	Disabled     bool         `json:"disabled"` // 批量测试中因 token 失效被自动禁用
	LatencyMs    int64        `json:"latency_ms"`
}

// TestAccount 健康测试：user/info 验证 + credit/balance + trial status 三连，
// 刷新账号状态/余额/用户名。token 失效（401/403）标 token_expired；
// 是否自动禁用由调用方决定（单测不禁用，批量禁用）。网络类失败不误标 token。
func (am *AccountManager) TestAccount(ctx context.Context, id string) (*AccountTestResult, error) {
	acc, err := am.GetAccount(id)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	res := &AccountTestResult{
		AccountID: acc.ID,
		Label:     acc.Label,
		UserID:    acc.UserID,
		Username:  acc.Username,
		Balance:   -1,
	}

	// 1) user/info 验证 token（同时取 user_id/username）
	info, cerr := am.client.GetUserInfo(ctx, acc.Profile())
	res.LatencyMs = time.Since(start).Milliseconds()
	if cerr != nil {
		res.TokenValid = false
		res.Message = "token 验证失败: " + cerr.Msg
		if cerr.Kind == KindAuth {
			res.TokenExpired = true
			am.MarkTokenExpired(acc.ID, cerr.Msg)
		}
		return res, nil
	}
	res.TokenValid = true
	res.UserID = info.RealUserID
	res.Username = info.Name

	// 2) credit/balance（失败不阻断，保留 -1）
	bal, berr := am.client.GetBalance(ctx, acc.Profile())
	if berr == nil {
		res.Balance = bal
		am.UpdateBalance(acc.ID, bal)
	}

	// 3) trial status（尽力而为：活动接口可能不存在/未开放）
	if st, terr := am.client.GetTrialStatus(ctx, acc.Profile()); terr == nil {
		res.Trial = st
	}

	// 刷新用户信息与检查时间
	if _, err := am.db.conn.Exec(`
		UPDATE accounts SET user_id=?, username=?, last_check_at=?, updated_at=? WHERE id=?
	`, info.RealUserID, info.Name, nowStr(), nowStr(), acc.ID); err != nil {
		log.Printf("[accounts] test %s fill user info failed: %v", acc.ID, err)
	}

	res.OK = true
	res.Message = "测试通过"
	if berr != nil {
		res.Message += "（余额查询失败: " + berr.Msg + "）"
	}
	res.LatencyMs = time.Since(start).Milliseconds()
	return res, nil
}

// TestAllAccounts 批量测试所有 enabled 账号：串行执行、账号间 500ms 间隔（防风控），
// token 明确失效（401/403）的账号自动禁用（is_enabled=0，状态保留 token_expired）。
// ctx 取消时返回已完成部分。
func (am *AccountManager) TestAllAccounts(ctx context.Context) []AccountTestResult {
	accounts, err := am.ListAccounts()
	if err != nil {
		log.Printf("[accounts] test-all: list failed: %v", err)
		return []AccountTestResult{}
	}
	results := []AccountTestResult{}
	first := true
	for _, a := range accounts {
		if !a.IsEnabled {
			continue
		}
		if !first {
			select {
			case <-ctx.Done():
				return results
			case <-time.After(500 * time.Millisecond): // 串行 + 间隔防风控
			}
		}
		first = false

		actx, cancel := context.WithTimeout(ctx, 30*time.Second)
		r, err := am.TestAccount(actx, a.ID)
		cancel()
		if err != nil {
			results = append(results, AccountTestResult{
				AccountID: a.ID, Label: a.Label, Balance: -1,
				Message: "测试失败: " + err.Error(),
			})
			continue
		}
		if r.TokenExpired {
			// 自动禁用：is_enabled=0，状态保留 token_expired（last_error 有详情）
			if _, derr := am.db.conn.Exec(`UPDATE accounts SET is_enabled=0, updated_at=? WHERE id=?`,
				nowStr(), a.ID); derr != nil {
				log.Printf("[accounts] test-all disable %s failed: %v", a.ID, derr)
			} else {
				r.Disabled = true
				r.Message += "（token 失效，已自动禁用）"
			}
		}
		results = append(results, *r)
	}
	log.Printf("[accounts] test-all done: %d accounts tested", len(results))
	return results
}
