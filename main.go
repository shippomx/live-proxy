// live-proxy 把直播间号翻译成可直接播放的流地址。
//
// 存在的理由：电视盒子的播放器只会播 URL，不会执行各平台的取流逻辑；
// 而公网上的同类跳板（如部署在 Vercel 上的）在国内被 DNS 污染 + IP 阻断，
// 盒子根本连不上。本服务跑在内网，全程只访问国内可直连的平台接口。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	// 嵌入时区数据库。OpenWrt 用 /etc/TZ 而非 glibc 的 tzdata 目录，
	// 静态编译的 Go 二进制找不到时区文件时会静默回退到 UTC —— 不报错，
	// 只是日志时间整体差 8 小时，排障时对不上监控和用户反馈的时间点。
	_ "time/tzdata"

	"live-proxy/sites"
	"live-proxy/store"
)

var registry = map[string]sites.Site{}

func register(s sites.Site) { registry[s.ID()] = s }

func init() {
	register(&sites.Huya{})
	register(&sites.Bilibili{})
	register(&sites.Kuaishou{})
	register(&sites.NeteaseCC{})
	register(&sites.Douyu{})
}

var reqSeq atomic.Uint64

// relayDefault FLV 线路是否默认走中转。请求里的 ?relay=1/0 优先于它。
var relayDefault bool

func main() {
	addr := flag.String("addr", ":9000", "监听地址")
	flag.BoolVar(&relayDefault, "relay", false, "FLV 线路默认走中转（上游断开自动续接），而不是 302 跳转")
	flag.StringVar(&loginDir, "data", "", "数据目录：扫码登录 Cookie 与收藏/导入频道的数据库；为空则只在本次运行期间有效")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "platforms": platformIDs()})
	})
	registerLoginRoutes(mux)
	registerBrowseRoutes(mux)
	registerWatchRoutes(mux)
	registerEPGRoutes(mux)
	mux.HandleFunc("/", handleStream)

	dbPath := ":memory:"
	if loginDir != "" {
		if err := os.MkdirAll(loginDir, 0o700); err != nil {
			slog.Error("创建数据目录失败", "dir", loginDir, "err", err)
			os.Exit(1)
		}
		dbPath = filepath.Join(loginDir, "live-proxy.db")
	} else {
		slog.Warn("未设置 -data，收藏与导入的频道只在内存中，重启后丢失")
	}
	var err error
	if db, err = store.Open(dbPath); err != nil {
		slog.Error("打开数据库失败", "path", dbPath, "err", err)
		os.Exit(1)
	}
	defer db.Close()
	startEPG(context.Background())

	srv := &http.Server{
		Addr:              *addr,
		Handler:           withTracing(mux),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      40 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	loadSavedCookies()

	go func() {
		slog.Info("启动", "addr", *addr, "platforms", platformIDs())
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("监听失败", "err", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	slog.Info("收到停止信号，优雅退出中")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

func platformIDs() []string {
	ids := make([]string, 0, len(registry))
	for id := range registry {
		ids = append(ids, id)
	}
	return ids
}

// withTracing 必须是最外层中间件：内层任何 panic 被 recover 记录时，
// 日志行需要带上 req_id 才能和访问日志对上，否则最需要排查的那条日志反而没有线索。
func withTracing(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID := fmt.Sprintf("%d-%d", time.Now().UnixNano()%1e9, reqSeq.Add(1))
		log := slog.With("req_id", reqID, "path", r.URL.Path, "remote", r.RemoteAddr)
		ctx := context.WithValue(r.Context(), logKey{}, log)

		defer func() {
			if v := recover(); v != nil {
				log.Error("handler panic", "panic", v)
				http.Error(w, "内部错误 req_id="+reqID, http.StatusInternalServerError)
			}
		}()
		w.Header().Set("X-Request-Id", reqID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

type logKey struct{}

func loggerFrom(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(logKey{}).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}

// handleStream 路由形如 /{platform}/{roomID}。
// 默认 302 跳到最佳线路（播放器直接跟随即可）；带 ?format=json 返回全部线路。
func handleStream(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	log := loggerFrom(ctx)

	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		writeErr(w, http.StatusNotFound, "路径格式应为 /{平台}/{房间号}，可用平台: "+strings.Join(platformIDs(), ", "))
		return
	}
	platform, roomID := parts[0], parts[1]

	site, ok := registry[platform]
	if !ok {
		writeErr(w, http.StatusNotFound, "不支持的平台 "+platform+"，可用: "+strings.Join(platformIDs(), ", "))
		return
	}

	// 给上游留出的时间必须短于 WriteTimeout，否则超时后客户端已断开、
	// 日志里只剩一条没有结果的请求，看不出是卡在上游还是自己。
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()

	streams, err := site.GetStreams(ctx, roomID)
	if err != nil {
		status, msg := classify(err)
		log.Warn("取流失败", "platform", platform, "room", roomID, "err", err, "status", status)
		writeErr(w, status, msg)
		return
	}
	log.Info("取流成功", "platform", platform, "room", roomID, "lines", len(streams))

	if r.URL.Query().Get("format") == "json" {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(map[string]any{"platform": platform, "room": roomID, "streams": streams})
		return
	}

	// 允许指定线路，例如 ?cdn=TX。指定了但不存在时必须报错而不是悄悄回退到默认线路 ——
	// 静默回退会让"换线路解决卡顿"这个操作看起来生效了，实际什么也没变。
	target := streams[0]
	if want := r.URL.Query().Get("cdn"); want != "" {
		found := false
		for _, s := range streams {
			if strings.EqualFold(s.CDN, want) {
				target, found = s, true
				break
			}
		}
		if !found {
			var avail []string
			for _, s := range streams {
				avail = append(avail, s.CDN)
			}
			writeErr(w, http.StatusNotFound, "无此线路 "+want+"，可用: "+strings.Join(avail, ", "))
			return
		}
	}

	if wantRelay(r) && target.Format == sites.FormatFLV {
		// 用请求本身的 ctx 而不是上面那个 25 秒的取流 ctx：中转要持续到播放器断开。
		relay(r.Context(), w, log, site, roomID, target)
		return
	}

	w.Header().Set("Cache-Control", "no-store") // 签名有时效，缓存会让盒子拿到过期地址
	http.Redirect(w, r, target.URL, http.StatusFound)
}

func wantRelay(r *http.Request) bool {
	switch r.URL.Query().Get("relay") {
	case "1", "true":
		return true
	case "0", "false":
		return false
	}
	return relayDefault
}

// classify 把内部错误映射成能指出"断在哪一层"的状态码。
// 全压成 500 会让排障失去方向：未开播、房间号写错、平台接口改版是三件完全不同的事。
func classify(err error) (int, string) {
	switch {
	case errors.Is(err, sites.ErrOffline):
		return http.StatusNotFound, "主播未开播"
	case errors.Is(err, sites.ErrRoomInvalid):
		return http.StatusNotFound, "房间号无效或不存在"
	case errors.Is(err, sites.ErrNeedAuth):
		return http.StatusUnauthorized, "该内容需要登录凭据"
	case errors.Is(err, sites.ErrUpstream):
		return http.StatusBadGateway, "上游平台接口请求失败: " + err.Error()
	case errors.Is(err, sites.ErrParse):
		return http.StatusBadGateway, "上游响应解析失败，平台接口可能已变更: " + err.Error()
	default:
		return http.StatusInternalServerError, err.Error()
	}
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	fmt.Fprintln(w, msg)
}
