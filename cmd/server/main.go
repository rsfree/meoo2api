// Command server 启动 Meoo OpenAI 兼容网关与控制台。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // 把时区库编进二进制：运行镜像无需 tzdata 包

	"meoo2api/internal/account"
	"meoo2api/internal/admin"
	"meoo2api/internal/catalog"
	"meoo2api/internal/gateway"
	"meoo2api/internal/model"
	"meoo2api/internal/pool"
	"meoo2api/internal/reqlog"
	"meoo2api/internal/store"
	"meoo2api/internal/upstream"
	"meoo2api/internal/webui"
)

// version 由 -ldflags "-X main.version=…" 注入，本地构建时为 dev。
var version = "dev"

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	port := flag.Int("port", atoi(env("PORT", "8791")), "监听端口")
	bind := flag.String("bind", env("BIND", "127.0.0.1"), "监听地址")
	dataDir := flag.String("data", env("DATA_DIR", "data"), "数据目录")
	reset := flag.Bool("reset-password", false, "重置管理密码并退出（先停止服务）")
	flag.Parse()
	if *port < 1 || *port > 65535 {
		return fmt.Errorf("端口必须在 1..65535 之间")
	}

	base := strings.TrimRight(env("MEOO_AI_BASE", "https://api.meoo.host/meoo-ai/compatible-mode/v1"), "/")
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("MEOO_AI_BASE 不是合法的 HTTP(S) 地址")
	}

	st, generated, err := store.Load(*dataDir)
	if err != nil {
		return fmt.Errorf("加载状态: %w", err)
	}
	defer st.Close()
	if *reset {
		pw := model.RandID("", 8)
		// 用同样的随机源生成可读密码
		pw = strings.ToLower(pw)
		st.Mu.Lock()
		st.Data.Config.AdminPass = store.HashPassword(pw)
		st.Mu.Unlock()
		if err := st.Save(); err != nil {
			return err
		}
		fmt.Printf("新的管理密码（只显示这一次）：%s\n", pw)
		return nil
	}
	if generated != "" {
		fmt.Printf("管理密码（只显示这一次）：%s\n", generated)
	}

	// 预置环境变量凭证（可选）
	if ak := strings.TrimSpace(os.Getenv("MEOO_PROJECT_AK")); ak != "" {
		st.Mu.RLock()
		exists := false
		for _, k := range st.Data.Keys {
			if k.Key == ak {
				exists = true
			}
		}
		st.Mu.RUnlock()
		if !exists && model.IsProjectAK(ak) {
			st.Mu.Lock()
			st.Data.Keys = append(st.Data.Keys, &model.Key{
				ID: model.RandID("k_", 6), Key: ak, Enabled: true,
				Health: model.Health{State: "ok"}, CreatedAt: model.NowMilli(), Label: "env",
			})
			st.Mu.Unlock()
			_ = st.Save()
		}
	}
	for _, k := range splitList(os.Getenv("MEOO2API_KEYS")) {
		st.Mu.RLock()
		dup := false
		for _, c := range st.Data.ClientKeys {
			if c.Key == k {
				dup = true
			}
		}
		st.Mu.RUnlock()
		if dup {
			continue
		}
		st.Mu.Lock()
		st.Data.ClientKeys = append(st.Data.ClientKeys, &model.ClientKey{
			ID: model.RandID("c_", 6), Key: k, Label: "env", Enabled: true, CreatedAt: model.NowMilli(),
		})
		st.Mu.Unlock()
		_ = st.Save()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	up := upstream.New(base)
	defer up.HTTP.CloseIdleConnections()
	lg, err := reqlog.Open(*dataDir)
	if err != nil {
		return fmt.Errorf("打开请求日志: %w", err)
	}
	defer lg.Close()

	acc := &account.Service{St: st, Up: up}
	pk := &pool.Picker{St: st, Up: up}
	cat := catalog.New(st, pk, up)
	// 富元数据目录：用项目 AK 读 Meoo 站点 /api/v1/agent/models，只给已上架模型补
	// context_window / max_tokens / multimodal / label；不扩集合（硬白名单之外的调不通）。
	// MEOO_METADATA=off 可整体关闭，MEOO_WEB_BASE 可指向镜像站点。
	if !strings.EqualFold(env("MEOO_METADATA", "on"), "off") {
		cat.WebBase = strings.TrimRight(env("MEOO_WEB_BASE", "https://meoo.com"), "/")
	}
	// 目录自动刷新周期（热更新）：默认 300 秒，改这个值不用重新编译。
	if sec, cerr := strconv.Atoi(env("MEOO_CATALOG_REFRESH_SEC", "")); cerr == nil && sec > 0 {
		cat.RefreshEvery = time.Duration(sec) * time.Second
	}
	gw := &gateway.Handler{St: st, Picker: pk, Catalog: cat, Up: up, Log: lg}
	adm := admin.New(st, pk, acc, cat, up, lg)

	// 先把模型目录同步一次再开监听：客户端第一眼拿到的就是真实目录。
	syncCtx, syncCancel := context.WithTimeout(ctx, 15*time.Second)
	cat.Refresh(syncCtx)
	syncCancel()
	catalogDone := cat.Start(ctx)
	healthDone := acc.HealthLoop(ctx, time.Hour)
	costDone := acc.CostLoop(ctx, 5*time.Minute)
	saveDone := make(chan struct{})
	go func() {
		defer close(saveDone)
		st.SaveLoop(ctx.Done())
	}()

	srv := &http.Server{
		Addr:              net.JoinHostPort(*bind, strconv.Itoa(*port)),
		Handler:           newHandler(gw, adm),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()
	log.Printf("meoo2api 已启动：http://%s/  （上游 %s）", srv.Addr, base)

	select {
	case <-ctx.Done():
	case err = <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		stop()
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if serr := srv.Shutdown(shutdownCtx); serr != nil {
		_ = srv.Close()
		log.Printf("HTTP 关闭: %v", serr)
	}
	<-catalogDone
	<-healthDone
	<-costDone
	<-saveDone
	if serr := st.Save(); serr != nil {
		return fmt.Errorf("保存最终状态: %w", serr)
	}
	return err
}

// stateWriter 记录响应头是否已写出，供换号重试判断。
type stateWriter struct {
	http.ResponseWriter
	wrote bool
}

func (w *stateWriter) Written() bool { return w.wrote }

func (w *stateWriter) WriteHeader(code int) {
	if w.wrote {
		return
	}
	w.wrote = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *stateWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

func (w *stateWriter) Flush() {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *stateWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

const maxBodyBytes = 16 << 20

func newHandler(gw *gateway.Handler, adm *admin.Handler) http.Handler {
	html := webui.Index()
	return http.HandlerFunc(func(raw http.ResponseWriter, r *http.Request) {
		w := &stateWriter{ResponseWriter: raw}
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("panic: %v", rec)
				if w.wrote {
					panic(http.ErrAbortHandler)
				}
				writeErr(w, 500, "internal_error", "内部错误")
			}
		}()
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")

		path := strings.TrimRight(r.URL.Path, "/")
		if path == "" {
			path = "/"
		}
		// 面板本身不带 CORS；/v1 允许跨域，方便前端直连。
		if !strings.HasPrefix(path, "/admin") && !strings.HasPrefix(path, "/api/") &&
			path != "/" && path != "/index.html" && path != "/panel" {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Request-Id")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}

		switch {
		// 面板 HTML 里所有接口都是 /admin/api/... 绝对路径，所以换任意别名路径直接吐同一份
		// HTML 都不会失效；/panel 是用户最容易手输的地址，别再回 404。
		case path == "/" || path == "/index.html" || path == "/panel":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Frame-Options", "DENY")
			w.Header().Set("Content-Security-Policy",
				"default-src 'none'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
			_, _ = w.Write(html)
		case path == "/healthz":
			writeJSON(w, 200, map[string]any{
				"ok": true, "service": "meoo2api", "version": version,
				"usable": len(gw.Picker.Usable()), "upstream": gw.Up.Base,
			})
		default:
			if adm.ServeHTTP(w, r) {
				return
			}
			if strings.HasPrefix(path, "/v1/") || path == "/models" || strings.HasPrefix(path, "/models/") {
				gw.ServeHTTP(w, r)
				return
			}
			writeErr(w, 404, "not_found", "未知路径")
		}
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"message": msg, "type": "invalid_request_error", "code": code}})
}

func splitList(v string) []string {
	out := []string{}
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func atoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 8791
	}
	return n
}
