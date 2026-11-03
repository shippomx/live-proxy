package main

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	qrcode "github.com/skip2/go-qrcode"

	"live-proxy/sites"
)

// 扫码登录页：/login。B站、斗鱼匿名取流会被静默降级（原画→超清/高清），
// 在这里扫码后 Cookie 立即生效于取流，并保存到 -data 目录，重启后自动加载。
//
// 页面和接口都不回显 Cookie 本身——局域网内谁都能打开这个页面，
// 能看到的只应是"谁登录着"，而不是能拿去冒用账号的凭据。

// 编译期确认两个平台都实现了扫码登录；漏实现一个方法时在这里报错，
// 而不是运行时在登录页上悄悄少一个平台。
var (
	_ sites.QRLogin = (*sites.Bilibili)(nil)
	_ sites.QRLogin = (*sites.Douyu)(nil)
)

//go:embed login.html
var loginPage []byte

// loginDir Cookie 保存目录，为空则只在内存里（重启即失效）。
var loginDir string

func qrLogins() []sites.QRLogin {
	var out []sites.QRLogin
	for _, id := range []string{"bilibili", "douyu"} {
		if q, ok := registry[id].(sites.QRLogin); ok {
			out = append(out, q)
		}
	}
	return out
}

func qrLoginByID(id string) (sites.QRLogin, bool) {
	for _, q := range qrLogins() {
		if q.ID() == id {
			return q, true
		}
	}
	return nil, false
}

func cookiePath(id string) string { return filepath.Join(loginDir, id+".cookie") }

// loadSavedCookies 启动时加载已保存的 Cookie，并在后台检查是否已失效。
// 失效只记日志不删文件：可能只是启动时网络还没通，删掉就得重新扫码。
func loadSavedCookies() {
	if loginDir == "" {
		slog.Warn("未设置 -data，扫码登录只在内存中有效，重启后需重新扫码")
		return
	}
	for _, q := range qrLogins() {
		b, err := os.ReadFile(cookiePath(q.ID()))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			slog.Error("读取登录 Cookie 失败", "platform", q.ID(), "err", err)
			continue
		}
		cookie := strings.TrimSpace(string(b))
		setActiveCookie(q, cookie)
		go func(q sites.QRLogin) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			a, err := q.Account(ctx, cookie)
			switch {
			case err != nil:
				slog.Warn("已加载登录 Cookie，但校验请求失败", "platform", q.ID(), "err", err)
			case !a.LoggedIn:
				slog.Warn("已保存的登录已失效，请到 /login 重新扫码", "platform", q.ID())
			default:
				slog.Info("已加载登录", "platform", q.ID(), "user", a.Name)
			}
		}(q)
	}
}

// saveCookie 先写临时文件再改名：写到一半断电不会留下半截 Cookie。
func saveCookie(id, cookie string) error {
	if loginDir == "" {
		return nil
	}
	if err := os.MkdirAll(loginDir, 0o700); err != nil {
		return err
	}
	tmp := cookiePath(id) + ".tmp"
	if err := os.WriteFile(tmp, []byte(cookie), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, cookiePath(id))
}

func registerLoginRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(loginPage)
	})
	mux.HandleFunc("GET /login/platforms", handleLoginPlatforms)
	mux.HandleFunc("POST /login/{id}/qrcode", handleLoginQRCode)
	mux.HandleFunc("GET /login/{id}/poll", handleLoginPoll)
	mux.HandleFunc("POST /login/{id}/logout", handleLoginLogout)
}

type platformStatus struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	LoggedIn bool   `json:"logged_in"`
	User     string `json:"user,omitempty"`
	Note     string `json:"note,omitempty"`
	Error    string `json:"error,omitempty"`
	Saved    bool   `json:"saved"` // 是否会持久化（设置了 -data）
	// Methods 可用的扫码方式，页面按它出按钮。只有一种方式的平台给一个 ID 为空的默认项。
	Methods []sites.QRMethod `json:"methods"`
}

func handleLoginPlatforms(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	var out []platformStatus
	for _, q := range qrLogins() {
		st := platformStatus{ID: q.ID(), Name: q.Name(), Saved: loginDir != "",
			Methods: []sites.QRMethod{{Name: "扫码登录"}}}
		if mq, ok := q.(sites.QRMethodLogin); ok {
			st.Methods = mq.QRMethods()
		}
		if c := currentCookie(q); c != "" {
			a, err := q.Account(ctx, c)
			if err != nil {
				st.Error = "校验登录状态失败: " + err.Error()
			}
			st.LoggedIn, st.User, st.Note = a.LoggedIn, a.Name, a.Note
			if err == nil && !a.LoggedIn {
				st.Error = "登录已失效，请重新扫码"
			}
		}
		out = append(out, st)
	}
	writeJSON(w, http.StatusOK, out)
}

// activeCookies 当前生效的 Cookie，状态查询以它为准。平台实现没有导出读取接口，
// 在这里另记一份比给每个平台加只读方法简单；写入点只有加载、登录、退出三处。
var (
	cookieMu      sync.Mutex
	activeCookies = map[string]string{}
)

func setActiveCookie(q sites.QRLogin, c string) {
	cookieMu.Lock()
	defer cookieMu.Unlock()
	q.SetCookie(c)
	if c == "" {
		delete(activeCookies, q.ID())
	} else {
		activeCookies[q.ID()] = c
	}
}

func currentCookie(q sites.QRLogin) string {
	cookieMu.Lock()
	defer cookieMu.Unlock()
	return activeCookies[q.ID()]
}

func handleLoginQRCode(w http.ResponseWriter, r *http.Request) {
	q, ok := qrLoginByID(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "该平台不支持扫码登录"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	var code sites.QRCode
	var err error
	if method := r.URL.Query().Get("method"); method != "" {
		mq, ok := q.(sites.QRMethodLogin)
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "该平台只有一种扫码方式"})
			return
		}
		code, err = mq.QRStartMethod(ctx, method)
	} else {
		code, err = q.QRStart(ctx)
	}
	if err != nil {
		loggerFrom(r.Context()).Warn("申请登录二维码失败", "platform", q.ID(), "err", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	img, typ := code.Image, code.ImageType
	if len(img) == 0 {
		if img, err = qrcode.Encode(code.Content, qrcode.Medium, 280); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		typ = "image/png"
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"key": code.Key,
		"png": "data:" + typ + ";base64," + base64.StdEncoding.EncodeToString(img),
	})
}

func handleLoginPoll(w http.ResponseWriter, r *http.Request) {
	log := loggerFrom(r.Context())
	q, ok := qrLoginByID(r.PathValue("id"))
	key := r.URL.Query().Get("key")
	if !ok || key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "缺少平台或 key"})
		return
	}
	// 微信扫码是长轮询（约 15~30 秒才返回），加上回调种 Cookie，会超过服务器统一的
	// 40 秒写超时；这里单独放宽，否则成功那一次的响应恰好写不出去。
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(110 * time.Second))
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	res, err := q.QRPoll(ctx, key)
	if err != nil {
		log.Warn("查询扫码状态失败", "platform", q.ID(), "key", keyKind(key), "err", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	if res.State != sites.QRDone {
		writeJSON(w, http.StatusOK, map[string]string{"state": string(res.State)})
		return
	}

	// 先确认这份 Cookie 真能登录，再替换正在使用的那份：
	// 否则一次失败的登录会把原本有效的登录顶掉。
	// 校验单独计时：前面的长轮询和回调可能已经用掉了大半个超时，
	// 共用的话校验请求会因为超时失败，把一次成功的登录误报成"校验未通过"。
	vctx, vcancel := context.WithTimeout(context.WithoutCancel(r.Context()), 20*time.Second)
	defer vcancel()
	a, err := q.Account(vctx, res.Cookie)
	if err != nil || !a.LoggedIn {
		// 记下拿到了哪些 Cookie（只记名字）和校验接口的原始返回，才分得清
		// 是 Cookie 真无效，还是校验接口判断错了。
		log.Warn("扫码成功但登录校验未通过", "platform", q.ID(), "err", err,
			"cookies", sites.CookieNames(res.Cookie), "detail", a.Detail)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": q.Name() + "：扫码成功但登录校验未通过，请重试"})
		return
	}
	setActiveCookie(q, res.Cookie)
	if err := saveCookie(q.ID(), res.Cookie); err != nil {
		log.Error("登录成功但保存 Cookie 失败，重启后需重新扫码", "platform", q.ID(), "err", err)
	}
	log.Info("扫码登录成功", "platform", q.ID(), "user", a.Name, "note", a.Note, "cookies", sites.CookieNames(res.Cookie))
	writeJSON(w, http.StatusOK, map[string]string{"state": string(sites.QRDone), "user": a.Name, "note": a.Note})
}

func handleLoginLogout(w http.ResponseWriter, r *http.Request) {
	q, ok := qrLoginByID(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "该平台不支持扫码登录"})
		return
	}
	setActiveCookie(q, "")
	if loginDir != "" {
		if err := os.Remove(cookiePath(q.ID())); err != nil && !errors.Is(err, os.ErrNotExist) {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
	}
	loggerFrom(r.Context()).Info("已退出登录", "platform", q.ID())
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// keyKind 日志里只标出是哪种扫码流程，不记完整 key。
func keyKind(key string) string {
	if strings.HasPrefix(key, "wx:") {
		return "weixin"
	}
	return "app"
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
