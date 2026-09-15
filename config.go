package main

// ---- 配置加载与运行期设置 ----
// 职责：
//  1. 加载 config/config.json（缺省文件时用内置默认值），提供线程安全读取与 30s 热加载；
//  2. RuntimeSettings：poll_interval_sec / worker_count / per_account_concurrency /
//     balance_safety_factor 四个运行期可调项，持久化在 settings 表，管理界面可热更，
//     优先级高于配置文件（配置文件值仅作为 settings 表的初始种子）。

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// 域名矩阵（逆向报告 §1.1：国内/海外 prod）
const (
	cloudGatewayDomestic = "https://design.minimaxi.com"
	cloudGatewayOverseas = "https://design.minimax.io"
	accountAPIDomestic   = "https://hailuoai.com"
	accountAPIOverseas   = "https://hailuoai.video"

	defaultVersionCode = "3.0.11" // 与逆向的桌面端版本一致，全池统一
	defaultListenAddr  = "127.0.0.1:8787"
)

// FileConfig config/config.json 的结构
type FileConfig struct {
	ListenAddr      string `json:"listen_addr"`       // 监听地址，默认 127.0.0.1:8787
	Region          string `json:"region"`            // domestic(默认) | overseas
	CloudGatewayURL string `json:"cloud_gateway_url"` // 覆盖云网关域名（留空按 region 推导）
	AccountAPIURL   string `json:"account_api_url"`   // 覆盖账号域域名（留空按 region 推导）
	VersionCode     string `json:"version_code"`      // 公共参数 version_code

	DBPath   string `json:"db_path"`   // SQLite 路径，默认 data/minimax-2api.db
	VideoDir string `json:"video_dir"` // 成片落盘目录，默认 data/videos
	ImageDir string `json:"image_dir"` // 图片成片落盘目录，默认 data/images

	AdminUsername string `json:"admin_username"` // 管理界面默认用户名 admin
	AdminPassword string `json:"admin_password"` // 管理界面默认密码 admin123（首次登录后可改）

	WorkerCount           int     `json:"worker_count"`            // 任务 worker 数，默认 4
	PerAccountConcurrency int     `json:"per_account_concurrency"` // 每账号并发上限，默认 2
	PollIntervalSec       int     `json:"poll_interval_sec"`       // 轮询初始间隔，默认 5
	PollBackoffFactor     float64 `json:"poll_backoff_factor"`     // 轮询退避系数，默认 1.3
	PollMaxIntervalSec    int     `json:"poll_max_interval_sec"`   // 轮询间隔上限，默认 20
	PollMaxWaitMin        int     `json:"poll_max_wait_min"`       // 单任务轮询总时长上限(分钟)，默认 90
	BalanceSafetyFactor   float64 `json:"balance_safety_factor"`   // 余额安全系数，默认 1.2

	RenewIntervalHour    int `json:"renew_interval_hour"`    // token 续期周期(小时)，默认 12
	BalanceIntervalMin   int `json:"balance_interval_min"`   // 余额巡检周期(分钟)，默认 15
	ReconcileIntervalSec int `json:"reconcile_interval_sec"` // 任务 reconcile 周期(秒)，默认 60

	UploadTimeoutSec   int `json:"upload_timeout_sec"`   // 素材上传超时，默认 120（同桌面端）
	DownloadTimeoutSec int `json:"download_timeout_sec"` // 成片下载超时，默认 300
}

// applyDefaults 给零值字段填充默认值
func (c *FileConfig) applyDefaults() {
	if c.ListenAddr == "" {
		c.ListenAddr = defaultListenAddr
	}
	if c.Region != "overseas" {
		c.Region = "domestic"
	}
	if c.VersionCode == "" {
		c.VersionCode = defaultVersionCode
	}
	if c.DBPath == "" {
		c.DBPath = filepath.Join("data", "minimax-2api.db")
	}
	if c.VideoDir == "" {
		c.VideoDir = filepath.Join("data", "videos")
	}
	if c.ImageDir == "" {
		c.ImageDir = filepath.Join("data", "images")
	}
	if c.AdminUsername == "" {
		c.AdminUsername = "admin"
	}
	if c.AdminPassword == "" {
		c.AdminPassword = "admin123"
	}
	if c.WorkerCount <= 0 {
		c.WorkerCount = 4
	}
	if c.PerAccountConcurrency <= 0 {
		c.PerAccountConcurrency = 2
	}
	if c.PollIntervalSec <= 0 {
		c.PollIntervalSec = 5
	}
	if c.PollBackoffFactor <= 1 {
		c.PollBackoffFactor = 1.3
	}
	if c.PollMaxIntervalSec <= 0 {
		c.PollMaxIntervalSec = 20
	}
	if c.PollMaxWaitMin <= 0 {
		c.PollMaxWaitMin = 90
	}
	if c.BalanceSafetyFactor <= 0 {
		c.BalanceSafetyFactor = 1.2
	}
	if c.RenewIntervalHour <= 0 {
		c.RenewIntervalHour = 12
	}
	if c.BalanceIntervalMin <= 0 {
		c.BalanceIntervalMin = 15
	}
	if c.ReconcileIntervalSec <= 0 {
		c.ReconcileIntervalSec = 60
	}
	if c.UploadTimeoutSec <= 0 {
		c.UploadTimeoutSec = 120
	}
	if c.DownloadTimeoutSec <= 0 {
		c.DownloadTimeoutSec = 300
	}
}

// CloudGateway 返回云网关基础 URL（显式配置优先，其次按 region）
func (c *FileConfig) CloudGateway() string {
	if c.CloudGatewayURL != "" {
		return c.CloudGatewayURL
	}
	if c.Region == "overseas" {
		return cloudGatewayOverseas
	}
	return cloudGatewayDomestic
}

// AccountAPI 返回账号域基础 URL
func (c *FileConfig) AccountAPI() string {
	if c.AccountAPIURL != "" {
		return c.AccountAPIURL
	}
	if c.Region == "overseas" {
		return accountAPIOverseas
	}
	return accountAPIDomestic
}

// AppConfig 配置管理器（热加载安全）
type AppConfig struct {
	mu         sync.RWMutex
	cfg        FileConfig
	configPath string
}

// LoadConfig 从目录加载 config.json；文件不存在时使用全默认值并落盘一份示例
func LoadConfig(configDir string) (*AppConfig, error) {
	ac := &AppConfig{configPath: filepath.Join(configDir, "config.json")}
	data, err := os.ReadFile(ac.configPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("read config.json: %w", err)
		}
		// 无配置文件：生成默认配置，方便运维直接改
		ac.cfg = FileConfig{}
		ac.cfg.applyDefaults()
		if mkErr := os.MkdirAll(configDir, 0755); mkErr == nil {
			if b, jErr := json.MarshalIndent(ac.cfg, "", "  "); jErr == nil {
				if wErr := os.WriteFile(ac.configPath, b, 0644); wErr != nil {
					log.Printf("[config] write default config.json failed: %v", wErr)
				}
			}
		}
		log.Printf("[config] config.json not found, using defaults (written to %s)", ac.configPath)
		return ac, nil
	}
	var fc FileConfig
	if err := json.Unmarshal(data, &fc); err != nil {
		return nil, fmt.Errorf("parse config.json: %w", err)
	}
	fc.applyDefaults()
	ac.cfg = fc
	return ac, nil
}

// Get 返回当前配置副本（线程安全）
func (ac *AppConfig) Get() FileConfig {
	ac.mu.RLock()
	defer ac.mu.RUnlock()
	return ac.cfg
}

// StartHotReload 定时重载配置文件；解析失败保持旧配置不变
func (ac *AppConfig) StartHotReload(interval time.Duration) {
	ticker := time.NewTicker(interval)
	go func() {
		for range ticker.C {
			data, err := os.ReadFile(ac.configPath)
			if err != nil {
				continue // 文件被删除/占用时保持现状
			}
			var fc FileConfig
			if err := json.Unmarshal(data, &fc); err != nil {
				log.Printf("[config] hot reload parse failed (keep old): %v", err)
				continue
			}
			fc.applyDefaults()
			ac.mu.Lock()
			changed := ac.cfg != fc
			ac.cfg = fc
			ac.mu.Unlock()
			if changed {
				log.Printf("[config] hot reload OK: region=%s gateway=%s", fc.Region, fc.CloudGateway())
			}
		}
	}()
}

// ---- RuntimeSettings：settings 表中的运行期可调项 ----

// RuntimeSettings 四个可在管理界面热更的参数。
// 值持久化在 settings 表；首次启动用配置文件值做种子。
type RuntimeSettings struct {
	mu                    sync.RWMutex
	db                    *DB
	pollIntervalSec       int
	workerCount           int
	perAccountConcurrency int
	balanceSafetyFactor   float64
}

// NewRuntimeSettings 从 settings 表加载（缺失键用 seed 值写入）
func NewRuntimeSettings(db *DB, seed FileConfig) *RuntimeSettings {
	rs := &RuntimeSettings{
		db:                    db,
		pollIntervalSec:       seed.PollIntervalSec,
		workerCount:           seed.WorkerCount,
		perAccountConcurrency: seed.PerAccountConcurrency,
		balanceSafetyFactor:   seed.BalanceSafetyFactor,
	}
	rs.pollIntervalSec = rs.loadInt("poll_interval_sec", rs.pollIntervalSec)
	rs.workerCount = rs.loadInt("worker_count", rs.workerCount)
	rs.perAccountConcurrency = rs.loadInt("per_account_concurrency", rs.perAccountConcurrency)
	rs.balanceSafetyFactor = rs.loadFloat("balance_safety_factor", rs.balanceSafetyFactor)
	return rs
}

func (rs *RuntimeSettings) loadInt(key string, def int) int {
	if v, ok := rs.db.GetSetting(key); ok {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			return n
		}
	}
	_ = rs.db.SetSetting(key, fmt.Sprintf("%d", def)) // 种子写入失败不影响运行
	return def
}

func (rs *RuntimeSettings) loadFloat(key string, def float64) float64 {
	if v, ok := rs.db.GetSetting(key); ok {
		var f float64
		if _, err := fmt.Sscanf(v, "%f", &f); err == nil && f > 0 {
			return f
		}
	}
	_ = rs.db.SetSetting(key, fmt.Sprintf("%g", def))
	return def
}

// Snapshot 运行期设置快照（对外展示/应用）
type SettingsSnapshot struct {
	PollIntervalSec       int     `json:"poll_interval_sec"`
	WorkerCount           int     `json:"worker_count"`
	PerAccountConcurrency int     `json:"per_account_concurrency"`
	BalanceSafetyFactor   float64 `json:"balance_safety_factor"`
}

// Get 返回当前快照
func (rs *RuntimeSettings) Get() SettingsSnapshot {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	return SettingsSnapshot{
		PollIntervalSec:       rs.pollIntervalSec,
		WorkerCount:           rs.workerCount,
		PerAccountConcurrency: rs.perAccountConcurrency,
		BalanceSafetyFactor:   rs.balanceSafetyFactor,
	}
}

// PollInterval 轮询初始间隔
func (rs *RuntimeSettings) PollInterval() time.Duration {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	return time.Duration(rs.pollIntervalSec) * time.Second
}

// WorkerCount 当前 worker 数
func (rs *RuntimeSettings) WorkerCount() int {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	return rs.workerCount
}

// PerAccountConcurrency 每账号并发上限
func (rs *RuntimeSettings) PerAccountConcurrency() int {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	return rs.perAccountConcurrency
}

// BalanceSafetyFactor 余额安全系数
func (rs *RuntimeSettings) BalanceSafetyFactor() float64 {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	return rs.balanceSafetyFactor
}

// Update 校验并写入新值（0 值表示不修改），持久化到 settings 表
func (rs *RuntimeSettings) Update(in SettingsSnapshot) error {
	if in.PollIntervalSec < 0 || in.PollIntervalSec > 300 {
		return fmt.Errorf("poll_interval_sec 需在 1-300 之间")
	}
	if in.WorkerCount < 0 || in.WorkerCount > maxPoolWorkers {
		return fmt.Errorf("worker_count 需在 1-%d 之间", maxPoolWorkers)
	}
	if in.PerAccountConcurrency < 0 || in.PerAccountConcurrency > 16 {
		return fmt.Errorf("per_account_concurrency 需在 1-16 之间")
	}
	if in.BalanceSafetyFactor < 1 || in.BalanceSafetyFactor > 10 {
		return fmt.Errorf("balance_safety_factor 需在 1.0-10.0 之间")
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if in.PollIntervalSec > 0 {
		rs.pollIntervalSec = in.PollIntervalSec
		_ = rs.db.SetSetting("poll_interval_sec", fmt.Sprintf("%d", in.PollIntervalSec))
	}
	if in.WorkerCount > 0 {
		rs.workerCount = in.WorkerCount
		_ = rs.db.SetSetting("worker_count", fmt.Sprintf("%d", in.WorkerCount))
	}
	if in.PerAccountConcurrency > 0 {
		rs.perAccountConcurrency = in.PerAccountConcurrency
		_ = rs.db.SetSetting("per_account_concurrency", fmt.Sprintf("%d", in.PerAccountConcurrency))
	}
	if in.BalanceSafetyFactor > 0 {
		rs.balanceSafetyFactor = in.BalanceSafetyFactor
		_ = rs.db.SetSetting("balance_safety_factor", fmt.Sprintf("%g", in.BalanceSafetyFactor))
	}
	return nil
}
