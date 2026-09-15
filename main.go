package main

// ---- 入口 ----
// 启动顺序：配置 → SQLite → 运行期设置 → 认证 → 云客户端 → 账号管理器 →
// 任务管道（worker 池 + 重启恢复）→ 定时调度 → HTTP 服务。
// 中间件链：hostGuard（防 DNS Rebinding）→ bodyLimit → auth → mux。
// Web 管理界面经 go:embed 打进单二进制。

import (
	"context"
	"embed"
	"flag"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

//go:embed web
var webFS embed.FS

func main() {
	configDir := flag.String("config", "config", "配置目录")
	dbPath := flag.String("db", "", "SQLite 数据库路径（默认取配置 db_path）")
	flag.Parse()

	absDir, err := filepath.Abs(*configDir)
	if err != nil {
		log.Fatalf("resolve config path: %v", err)
	}
	log.Printf("[main] config dir: %s", absDir)

	// 加载配置（无文件时生成默认配置）
	cfg, err := LoadConfig(absDir)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	c := cfg.Get()
	log.Printf("[main] region=%s gateway=%s account_api=%s", c.Region, c.CloudGateway(), c.AccountAPI())

	// 数据库路径与视频目录
	dbRel := c.DBPath
	if *dbPath != "" {
		dbRel = *dbPath
	}
	absDBPath, err := filepath.Abs(dbRel)
	if err != nil {
		log.Fatalf("resolve db path: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(absDBPath), 0755); err != nil {
		log.Fatalf("create db dir: %v", err)
	}
	videoDir, err := filepath.Abs(c.VideoDir)
	if err != nil {
		log.Fatalf("resolve video dir: %v", err)
	}
	if err := os.MkdirAll(videoDir, 0755); err != nil {
		log.Fatalf("create video dir: %v", err)
	}

	db, err := NewDB(absDBPath)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer db.Close()
	log.Printf("[main] database: %s", absDBPath)

	// 配置热加载（30s）
	cfg.StartHotReload(30 * time.Second)

	// 运行期设置（settings 表，管理界面可热更）
	rt := NewRuntimeSettings(db, c)

	// 云协议客户端 + 账号管理器
	client := NewMiniMaxClient(cfg)
	am := NewAccountManager(db, client, cfg, rt)

	// 任务管道：worker 池 + 重启恢复
	tm := NewTaskManager(db, am, client, cfg, rt)
	tm.Start()
	if err := tm.RecoverOnStartup(); err != nil {
		log.Printf("[main] WARNING: task recovery failed: %v", err)
	}

	// 定时调度：续期/余额巡检/reconcile
	sched := NewScheduler(db, am, tm, cfg)
	sched.Start()

	// 认证（session + API Key）
	auth := NewAuthManager(db, cfg)
	if auth.isDefaultPassword() {
		log.Printf("[main] web auth: 使用默认密码 admin/admin123，请尽快在设置中修改")
	}

	// 路由
	mux := http.NewServeMux()
	apiServer := NewAPIServer(db, am, tm, auth, rt, cfg)
	apiServer.RegisterRoutes(mux)
	mux.HandleFunc("/api/login", auth.HandleLogin)
	mux.HandleFunc("/api/logout", auth.HandleLogout)

	videoAPI := NewVideoAPI(tm, am)
	videoAPI.RegisterRoutes(mux)

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "service": "minimax-2api", "time": nowStr()})
	})

	// Web 管理界面（go:embed）
	webContent, err := fs.ReadFile(webFS, "web/index.html")
	if err != nil {
		log.Fatalf("read embedded web/index.html: %v", err)
	}
	webSub, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatalf("sub web fs: %v", err)
	}
	staticHandler := http.StripPrefix("/web/", http.FileServer(http.FS(webSub)))
	mux.HandleFunc("/web", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(webContent)
	})
	mux.HandleFunc("/web/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/web/static/") {
			staticHandler.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(webContent)
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/web", http.StatusFound)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"service":   "minimax-2api",
			"version":   "1.0",
			"endpoints": []string{"/v1/videos", "/v1/images/generations", "/v1/models", "/api/", "/web", "/health"},
		})
	})

	// 中间件链：Host 校验 → Body 限制 → 认证 → mux
	listenAddr := cfg.Get().ListenAddr
	protected := hostGuardMiddleware(listenAddr, bodyLimitMiddleware(auth.Middleware(mux)))

	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           protected,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// 不设 ReadTimeout/WriteTimeout：需要支持大素材上传与视频流式下载
	}

	// 优雅退出
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		sig := <-sigCh
		log.Printf("[main] received %v, shutting down...", sig)
		sched.Stop()
		tm.Stop()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		os.Exit(0)
	}()

	log.Printf("[main] minimax-2api listening on http://%s", listenAddr)
	log.Printf("[main] web UI: http://%s/web  (默认账号 admin / admin123)", listenAddr)
	log.Printf("[main] 2API base: http://%s/v1/  (Bearer sk-xxx)", listenAddr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server error: %v", err)
	}
}

// hostGuardMiddleware 防 DNS Rebinding：仅允许 Host 为 IP 字面量、localhost
// 或与监听地址一致的 host。攻击者域名即使解析到 127.0.0.1，其 Host 头也无法通过校验。
func hostGuardMiddleware(listenAddr string, next http.Handler) http.Handler {
	allowed := map[string]bool{"localhost": true}
	if h, _, err := net.SplitHostPort(listenAddr); err == nil {
		if h != "" && h != "0.0.0.0" && h != "::" {
			allowed[h] = true
		}
	} else if listenAddr != "" {
		allowed[listenAddr] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
			host = host[1 : len(host)-1]
		}
		if allowed[host] || net.ParseIP(host) != nil {
			next.ServeHTTP(w, r)
			return
		}
		http.Error(w, "invalid Host header", http.StatusMisdirectedRequest)
	})
}

// bodyLimitMiddleware 限制请求体大小：
// /api/* 32MB（导入含 base64 的可能大）、/v1/videos 与 /v1/images 64MB（素材 data URI）、其余 8MB
func bodyLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit := int64(8 << 20)
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/"):
			limit = 32 << 20
		case strings.HasPrefix(r.URL.Path, "/v1/videos"), strings.HasPrefix(r.URL.Path, "/v1/images"):
			limit = 64 << 20
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		next.ServeHTTP(w, r)
	})
}
