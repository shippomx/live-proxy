package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"live-proxy/sites"
)

// fakeLogin 假平台：轮询按预设序列返回状态，Account 只认 validCookie。
type fakeLogin struct {
	id          string
	states      []sites.QRState
	cookie      string // 登录成功时下发的 Cookie
	validCookie string
	mu          sync.Mutex
	inUse       string
	polls       int
}

func (f *fakeLogin) ID() string   { return f.id }
func (f *fakeLogin) Name() string { return f.id }
func (f *fakeLogin) GetStreams(context.Context, string) ([]sites.Stream, error) {
	return nil, sites.ErrOffline
}
func (f *fakeLogin) QRStart(context.Context) (sites.QRCode, error) {
	return sites.QRCode{Key: "k1", Content: "https://example.invalid/scan?k=k1"}, nil
}
func (f *fakeLogin) QRPoll(_ context.Context, key string) (sites.QRResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.states[min(f.polls, len(f.states)-1)]
	f.polls++
	if st == sites.QRDone {
		return sites.QRResult{State: st, Cookie: f.cookie}, nil
	}
	return sites.QRResult{State: st}, nil
}
func (f *fakeLogin) Account(_ context.Context, c string) (sites.Account, error) {
	if c != "" && c == f.validCookie {
		return sites.Account{LoggedIn: true, Name: "测试用户", Note: "大会员"}, nil
	}
	return sites.Account{}, nil
}
func (f *fakeLogin) SetCookie(c string) {
	f.mu.Lock()
	f.inUse = c
	f.mu.Unlock()
}
func (f *fakeLogin) used() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inUse
}

// withFakeLogin 用假平台临时顶替 registry 里的 bilibili，并把 Cookie 目录指到临时目录。
func withFakeLogin(t *testing.T, f *fakeLogin) *httptest.Server {
	t.Helper()
	old, oldDir := registry["bilibili"], loginDir
	registry["bilibili"] = f
	loginDir = t.TempDir()
	t.Cleanup(func() {
		registry["bilibili"], loginDir = old, oldDir
		cookieMu.Lock()
		activeCookies = map[string]string{}
		cookieMu.Unlock()
	})
	mux := http.NewServeMux()
	registerLoginRoutes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func call(t *testing.T, method, url string) (int, map[string]any, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	json.Unmarshal(raw, &m)
	return resp.StatusCode, m, string(raw)
}

// TestLoginFlow 完整流程：生成二维码 → 未扫码 → 已扫码 → 成功；Cookie 立即生效、0600 落盘、
// 状态接口显示用户名但绝不回显 Cookie；退出后失效并删除文件。
func TestLoginFlow(t *testing.T) {
	f := &fakeLogin{id: "bilibili", cookie: "SESSDATA=secret-value", validCookie: "SESSDATA=secret-value",
		states: []sites.QRState{sites.QRWaiting, sites.QRScanned, sites.QRDone}}
	srv := withFakeLogin(t, f)

	code, m, _ := call(t, "POST", srv.URL+"/login/bilibili/qrcode")
	if code != 200 || m["key"] != "k1" || !strings.HasPrefix(m["png"].(string), "data:image/png;base64,") {
		t.Fatalf("生成二维码: %d %v", code, m)
	}
	for _, want := range []string{"waiting", "scanned", "done"} {
		_, m, _ = call(t, "GET", srv.URL+"/login/bilibili/poll?key=k1")
		if m["state"] != want {
			t.Fatalf("轮询应为 %s，实际 %v", want, m)
		}
	}
	if m["user"] != "测试用户" {
		t.Fatalf("成功响应应带用户名: %v", m)
	}
	if f.used() != "SESSDATA=secret-value" {
		t.Fatalf("登录成功后 Cookie 应立即生效于取流，实际 %q", f.used())
	}
	st, err := os.Stat(cookiePath("bilibili"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("Cookie 应以 0600 落盘: %v %v", err, st)
	}

	_, _, raw := call(t, "GET", srv.URL+"/login/platforms")
	if strings.Contains(raw, "secret-value") {
		t.Fatalf("状态接口泄露了 Cookie: %s", raw)
	}
	if !strings.Contains(raw, `"logged_in":true`) || !strings.Contains(raw, "测试用户") {
		t.Fatalf("状态接口应显示已登录及用户名: %s", raw)
	}

	call(t, "POST", srv.URL+"/login/bilibili/logout")
	if f.used() != "" {
		t.Fatal("退出后取流不应再带 Cookie")
	}
	if _, err := os.Stat(cookiePath("bilibili")); !os.IsNotExist(err) {
		t.Fatalf("退出后应删除 Cookie 文件: %v", err)
	}
}

// TestLoginRejectedKeepsOldCookie 扫码"成功"但下发的 Cookie 校验不通过时，
// 不能把原本有效的登录顶掉。
func TestLoginRejectedKeepsOldCookie(t *testing.T) {
	f := &fakeLogin{id: "bilibili", cookie: "SESSDATA=bad", validCookie: "SESSDATA=good",
		states: []sites.QRState{sites.QRDone}}
	srv := withFakeLogin(t, f)
	setActiveCookie(f, "SESSDATA=good")

	code, _, _ := call(t, "GET", srv.URL+"/login/bilibili/poll?key=k1")
	if code != http.StatusBadGateway {
		t.Fatalf("校验不通过应返回 502，实际 %d", code)
	}
	if f.used() != "SESSDATA=good" || currentCookie(f) != "SESSDATA=good" {
		t.Fatalf("原有登录被顶掉了: 使用中=%q", f.used())
	}
}

// TestLoadSavedCookies 重启后从 -data 目录恢复登录。
func TestLoadSavedCookies(t *testing.T) {
	f := &fakeLogin{id: "bilibili", validCookie: "SESSDATA=saved"}
	withFakeLogin(t, f)
	if err := saveCookie("bilibili", "SESSDATA=saved"); err != nil {
		t.Fatal(err)
	}
	loadSavedCookies()
	if f.used() != "SESSDATA=saved" || currentCookie(f) != "SESSDATA=saved" {
		t.Fatalf("启动时应加载已保存的 Cookie，实际 %q", f.used())
	}
	time.Sleep(50 * time.Millisecond) // 让后台校验 goroutine 跑完，避免与 Cleanup 竞争
}

// TestLoginUnknownPlatform 不支持扫码的平台（虎牙等）应明确报错。
func TestLoginUnknownPlatform(t *testing.T) {
	srv := withFakeLogin(t, &fakeLogin{id: "bilibili", states: []sites.QRState{sites.QRWaiting}})
	if code, _, _ := call(t, "POST", srv.URL+"/login/huya/qrcode"); code != http.StatusNotFound {
		t.Fatalf("虎牙不支持扫码登录，应 404，实际 %d", code)
	}
}

// TestLoginQRStartLive 联网确认两个平台的二维码接口当前可用（只验证生成，扫码需要真人）。
func TestLoginQRStartLive(t *testing.T) {
	if testing.Short() {
		t.Skip("联网测试")
	}
	for _, q := range []sites.QRLogin{&sites.Bilibili{}, &sites.Douyu{}} {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		code, err := q.QRStart(ctx)
		cancel()
		if err != nil || code.Key == "" || !strings.HasPrefix(code.Content, "https://") {
			t.Errorf("%s 生成二维码失败: %+v %v", q.ID(), code, err)
			continue
		}
		ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
		res, err := q.QRPoll(ctx, code.Key)
		cancel()
		if err != nil || res.State != sites.QRWaiting {
			t.Errorf("%s 刚生成的二维码应为未扫码状态: %+v %v", q.ID(), res, err)
		}
		t.Logf("%s ✅ 二维码内容 %s", q.ID(), code.Content)
	}
}
