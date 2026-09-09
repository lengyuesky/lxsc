// lxsc：支持洛雪音源的 Subsonic 服务端
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"lxsc/internal/admin"
	"lxsc/internal/assets"
	"lxsc/internal/backup"
	"lxsc/internal/config"
	"lxsc/internal/db"
	"lxsc/internal/diagnostics"
	"lxsc/internal/js"
	"lxsc/internal/logbuf"
	"lxsc/internal/music"
	"lxsc/internal/portal"
	"lxsc/internal/secret"
	"lxsc/internal/settings"
	"lxsc/internal/subsonic"
	"lxsc/internal/webauth"
)

var version = "0.1.0"

func main() {
	cfgPath := flag.String("config", envOr("LXSC_CONFIG", ""), "配置文件路径（yaml）")
	flag.Parse()
	if err := run(*cfgPath); err != nil {
		fmt.Fprintln(os.Stderr, "启动失败:", err)
		os.Exit(1)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func run(cfgPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	if cfgPath == "" {
		// 数据目录下的 config.yaml 也会被读取
		if c2, err := config.Load(filepath.Join(cfg.DataDir, "config.yaml")); err == nil {
			cfg = c2
		}
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return err
	}
	appliedRestore, err := backup.ApplyPending(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("应用待恢复备份失败: %w", err)
	}
	restoreCommitted := false
	defer func() {
		if appliedRestore != nil && !restoreCommitted {
			_ = appliedRestore.Rollback()
		}
	}()
	if appliedRestore != nil && cfgPath == "" {
		if restored, loadErr := config.Load(filepath.Join(cfg.DataDir, "config.yaml")); loadErr == nil {
			cfg = restored
		}
	}

	// 日志
	var level slog.Level
	switch strings.ToLower(cfg.LogLevel) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	logBuf := logbuf.New(1000)
	base := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	log := slog.New(logbuf.NewHandler(base, logBuf))
	slog.SetDefault(log)
	subsonic.ServerVersion = version

	// 数据库与密钥
	database, err := db.Open(filepath.Join(cfg.DataDir, "lxsc.db"))
	if err != nil {
		return err
	}
	defer database.Close()
	box, err := secret.Load(cfg.DataDir, cfg.SecretKey)
	if err != nil {
		return err
	}
	ctx := context.Background()
	if n, _ := database.CountUsers(ctx); n == 0 {
		enc, _ := box.Encrypt(cfg.AdminPassword)
		if _, err := database.CreateUser(ctx, cfg.AdminUser, enc, true, "320k"); err != nil {
			return fmt.Errorf("创建管理员失败: %w", err)
		}
		log.Info("已创建管理员账号", "user", cfg.AdminUser)
	}
	st, err := settings.New(ctx, database)
	if err != nil {
		return err
	}

	// JS 运行时
	prelude, _ := assets.JS.ReadFile("js/prelude.js")
	bundle, _ := assets.JS.ReadFile("js/sdk.bundle.js")
	httpSecure, httpInsecure, err := js.NewHTTPClients(cfg.Proxy)
	if err != nil {
		return err
	}
	sdkPool, err := js.NewSDKPool(cfg.SDKWorkers, string(prelude), string(bundle), httpSecure, httpInsecure, log)
	if err != nil {
		return err
	}
	defer sdkPool.Close()
	sources := js.NewSourceManager(string(prelude), httpSecure, httpInsecure, log)
	defer sources.UnloadAll()
	catalog := music.NewCatalog(database, sdkPool, sources, st, log)

	// 加载已保存的音源脚本
	srcs, _ := database.ListSources(ctx)
	for _, s := range srcs {
		if !s.Enabled {
			continue
		}
		if _, err := sources.Load(ctx, s.ID, s.Priority, s.Script); err != nil {
			log.Warn("音源加载失败", "name", s.Name, "err", err)
		}
	}
	// 数据目录 sources/ 下的 .js 文件自动导入（便于 Docker 挂载）
	authManager := &webauth.Manager{DB: database, Secret: box}
	authSrv := &webauth.Server{Auth: authManager, Settings: st}
	backupSrv := backup.New(database, box, cfg.DataDir, version, httpSecure, log)
	backupCtx, stopBackups := context.WithCancel(context.Background())
	defer stopBackups()
	backupSrv.StartScheduler(backupCtx)
	defer backupSrv.StopScheduler()
	debugSrv := diagnostics.New(database, authManager, catalog, sources, st, version, time.Now())
	defer debugSrv.Tokens.Close()
	adminSrv := &admin.Server{DB: database, Sources: sources, Catalog: catalog, Settings: st, Secret: box, Logs: logBuf, Log: log, HTTP: httpSecure, Version: version, StartAt: time.Now(), Auth: authManager, Backup: backupSrv, Debug: debugSrv}
	importDir(ctx, adminSrv, srcs, filepath.Join(cfg.DataDir, "sources"), log)
	if err := catalog.EnableURLCache(cfg.DataDir, cfg.Proxy); err != nil {
		log.Warn("直链持久缓存初始化失败，改用内存缓存", "err", err)
	}
	defer catalog.CloseURLCache()

	sub := &subsonic.Server{Diagnostics: debugSrv.Events, DB: database, Catalog: catalog, Settings: st, Secret: box, Log: log, HTTP: httpSecure}
	portalSrv := &portal.Server{DB: database, Catalog: catalog, Settings: st, Secret: box, Log: log, Auth: authManager, Stream: sub.ServeWebStream}
	webFS, err := fs.Sub(assets.Web, "web")
	if err != nil {
		return err
	}
	webHandler := http.FileServer(http.FS(webFS))

	r := chi.NewRouter()
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(requestLogger(log))
	r.Use(cors)
	r.Mount("/rest", sub.Routes())
	r.Mount("/api/auth", authSrv.Routes())
	r.Mount("/api/app", portalSrv.Routes())
	r.Mount("/api/admin", adminSrv.Routes())
	r.Mount("/api/debug", debugSrv.Routes())
	r.Handle("/app", http.NotFoundHandler())
	r.Handle("/app/*", http.NotFoundHandler())
	r.Handle("/admin", http.NotFoundHandler())
	r.Handle("/admin/*", http.NotFoundHandler())
	r.Handle("/", webHandler)
	r.Handle("/*", webHandler)

	srv := &http.Server{Addr: cfg.Listen, Handler: r, ReadHeaderTimeout: 15 * time.Second}
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("监听 %s 失败: %w", cfg.Listen, err)
	}
	if appliedRestore != nil {
		if err := appliedRestore.Commit(); err != nil {
			_ = listener.Close()
			return fmt.Errorf("确认备份恢复失败: %w", err)
		}
	}
	restoreCommitted = true
	go func() {
		log.Info("lxsc 已启动", "version", version, "listen", cfg.Listen, "data", cfg.DataDir, "sdkWorkers", cfg.SDKWorkers, "platforms", sources.SupportedPlatforms())
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("HTTP 服务异常退出", "err", err)
			os.Exit(1)
		}
	}()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Info("正在退出…")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// importDir 导入数据目录 sources/ 下尚未入库的脚本（按文件名判重）
func importDir(ctx context.Context, a *admin.Server, existing []*db.Source, dir string, log *slog.Logger) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	names := map[string]bool{}
	for _, s := range existing {
		names[s.Name] = true
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".js") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		meta := js.ParseScriptMeta(string(b))
		name := meta.Name
		if name == "" {
			name = strings.TrimSuffix(e.Name(), ".js")
		}
		if names[name] {
			continue
		}
		if _, _, err := a.AddSource(ctx, string(b), name, 100); err != nil {
			log.Warn("导入音源失败", "file", e.Name(), "err", err)
		} else {
			log.Info("已从目录导入音源", "file", e.Name(), "name", name)
			names[name] = true
		}
	}
}

func requestLogger(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)
			if strings.HasPrefix(r.URL.Path, "/admin/") && ww.Status() < 400 {
				return
			}
			log.Debug("http", "method", r.Method, "path", r.URL.Path, "status", ww.Status(), "ms", time.Since(t).Milliseconds(), "ip", r.RemoteAddr)
		})
	}
}

func cors(next http.Handler) http.Handler {
	return diagnostics.SensitiveIO(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if diagnostics.SensitivePath(r.URL.Path) {
			diagnostics.SecurityHeaders(w)
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	}))
}
