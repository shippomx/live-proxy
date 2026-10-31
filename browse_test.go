package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"live-proxy/store"
)

func newBrowseServer(t *testing.T) *httptest.Server {
	t.Helper()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db = s
	mux := http.NewServeMux()
	registerLoginRoutes(mux)
	registerBrowseRoutes(mux)
	registerWatchRoutes(mux)
	registerEPGRoutes(mux)
	mux.HandleFunc("/", handleStream)
	srv := httptest.NewServer(withTracing(sameOriginWrites(mux)))
	t.Cleanup(func() { srv.Close(); s.Close() })
	return srv
}

func do(t *testing.T, method, url, ctype string, body io.Reader) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestFavoriteFlow(t *testing.T) {
	srv := newBrowseServer(t)

	code, body := do(t, "PUT", srv.URL+"/api/favorites/huya/11342394", "application/json",
		strings.NewReader(`{"name":"扫地僧","logo":"http://img/a.png"}`))
	if code != 200 || !strings.Contains(body, `"added":true`) {
		t.Fatalf("收藏: %d %s", code, body)
	}
	// 重复收藏不报错、不重复
	code, body = do(t, "PUT", srv.URL+"/api/favorites/huya/11342394", "application/json",
		strings.NewReader(`{"name":"别的名字"}`))
	if code != 200 || !strings.Contains(body, `"added":false`) {
		t.Fatalf("重复收藏: %d %s", code, body)
	}

	_, body = do(t, "GET", srv.URL+"/api/favorites", "", nil)
	if strings.TrimSpace(body) != `["huya/11342394"]` {
		t.Fatalf("收藏列表: %s", body)
	}

	_, m3u := do(t, "GET", srv.URL+"/playlist.m3u", "", nil)
	host := strings.TrimPrefix(srv.URL, "http://")
	for _, want := range []string{"#EXTM3U", `group-title="虎牙直播",扫地僧`, "http://" + host + "/huya/11342394"} {
		if !strings.Contains(m3u, want) {
			t.Fatalf("m3u 缺少 %q:\n%s", want, m3u)
		}
	}

	code, body = do(t, "DELETE", srv.URL+"/api/favorites/huya/11342394", "", nil)
	if code != 200 || !strings.Contains(body, `"removed":true`) {
		t.Fatalf("取消收藏: %d %s", code, body)
	}
	_, m3u = do(t, "GET", srv.URL+"/playlist.m3u", "", nil)
	if strings.Contains(m3u, "11342394") {
		t.Fatalf("取消后仍在列表里:\n%s", m3u)
	}
}

const importFile = `#EXTM3U
#EXTINF:-1 tvg-logo="http://l/cctv1.png" group-title="央视" catchup-source="rtsp://x/y?playseek=${(b)yyyyMMddHHmmss}",CCTV-1
http://192.168.5.3:4022/rtp/239.94.0.31:5140
#EXTINF:-1 group-title="网络直播",SpaceX频道
http://192.168.5.3:9000/bilibili/26312855
#EXTINF:-1 group-title="坏的",没有地址
`

func TestImportFlow(t *testing.T) {
	srv := newBrowseServer(t)

	// 先收藏一个将要被导入的同一房间，确认导入不覆盖它
	do(t, "PUT", srv.URL+"/api/favorites/bilibili/26312855", "application/json", strings.NewReader(`{"name":"我起的名字"}`))

	code, body := do(t, "POST", srv.URL+"/api/import", "text/plain", strings.NewReader(importFile))
	var r store.ImportResult
	json.Unmarshal([]byte(body), &r)
	if code != 200 || r.Added != 1 || r.Existed != 1 || r.Skipped != 1 {
		t.Fatalf("第一次导入: %d %s", code, body)
	}

	// 用表单上传同一文件：全部已存在
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "scdx.m3u")
	fw.Write([]byte(importFile))
	mw.Close()
	code, body = do(t, "POST", srv.URL+"/api/import", mw.FormDataContentType(), &buf)
	json.Unmarshal([]byte(body), &r)
	if code != 200 || r.Added != 0 || r.Existed != 2 {
		t.Fatalf("重复导入应全部跳过: %d %s", code, body)
	}

	_, m3u := do(t, "GET", srv.URL+"/playlist.m3u", "", nil)
	if !strings.Contains(m3u, ",我起的名字\n") || strings.Contains(m3u, "SpaceX频道") {
		t.Fatalf("导入改动了已收藏的房间:\n%s", m3u)
	}
	if !strings.Contains(m3u, `catchup-source="rtsp://x/y?playseek=${(b)yyyyMMddHHmmss}"`) {
		t.Fatalf("回看属性丢了:\n%s", m3u)
	}

	code, body = do(t, "POST", srv.URL+"/api/import", "text/plain", strings.NewReader("not a playlist"))
	if code != 400 {
		t.Fatalf("没有频道的文件应返回 400: %d %s", code, body)
	}

	// 频道管理：按 id 删除导入的外部频道
	_, body = do(t, "GET", srv.URL+"/api/channels", "", nil)
	var list []store.Channel
	json.Unmarshal([]byte(body), &list)
	if len(list) != 2 {
		t.Fatalf("频道列表: %s", body)
	}
	var cctvID int64
	for _, c := range list {
		if c.Name == "CCTV-1" {
			cctvID = c.ID
		}
	}
	code, body = do(t, "DELETE", srv.URL+"/api/channels/"+itoa(cctvID), "", nil)
	if code != 200 || !strings.Contains(body, `"removed":true`) {
		t.Fatalf("删除频道: %d %s", code, body)
	}
}

func itoa(i int64) string { b, _ := json.Marshal(i); return string(b) }

func TestBrowseRoutesDoNotShadowStream(t *testing.T) {
	srv := newBrowseServer(t)
	// 不支持浏览的平台：浏览接口 404，且是 JSON
	code, body := do(t, "GET", srv.URL+"/api/sites/kuaishou/recommend", "", nil)
	if code != 404 || !strings.Contains(body, "不支持浏览") {
		t.Fatalf("快手浏览: %d %s", code, body)
	}
	// 平台列表只有实现了浏览的三个，且按固定顺序
	_, body = do(t, "GET", srv.URL+"/api/sites", "", nil)
	if !strings.Contains(body, `"huya"`) || strings.Contains(body, "kuaishou") ||
		strings.Index(body, "huya") > strings.Index(body, "douyu") {
		t.Fatalf("平台列表: %s", body)
	}
	// 未知路径仍由取流处理（返回的是取流的错误，而不是浏览接口的 JSON）
	code, body = do(t, "GET", srv.URL+"/nosuch/1", "", nil)
	if code != 404 || strings.HasPrefix(body, "{") {
		t.Fatalf("取流路由被浏览路由挡住了: %d %s", code, body)
	}
	code, _ = do(t, "GET", srv.URL+"/browse", "", nil)
	if code != 200 {
		t.Fatalf("/browse: %d", code)
	}
}

// 联网：按房间号添加时用 RoomInfo 的规范房间号入库（B站短号 6 → 长号）。
func TestAddFavoriteByShortID(t *testing.T) {
	if testing.Short() {
		t.Skip("联网测试")
	}
	srv := newBrowseServer(t)
	code, body := do(t, "PUT", srv.URL+"/api/favorites/bilibili/6", "", nil)
	if code != 200 {
		t.Fatalf("按房间号添加: %d %s", code, body)
	}
	var r struct {
		RoomID string `json:"room_id"`
		Name   string `json:"name"`
	}
	json.Unmarshal([]byte(body), &r)
	if r.RoomID == "6" || r.RoomID == "" || r.Name == "" {
		t.Fatalf("应换成规范长号并带上主播名: %s", body)
	}
	// 再按长号加一次：同一个房间，不应新增
	_, body = do(t, "PUT", srv.URL+"/api/favorites/bilibili/"+r.RoomID, "", nil)
	if !strings.Contains(body, `"added":false`) {
		t.Fatalf("短号与长号应是同一房间: %s", body)
	}
	t.Logf("短号 6 → %s (%s)", r.RoomID, r.Name)
}

func TestCrossSiteWritesRejected(t *testing.T) {
	srv := newBrowseServer(t)
	host := strings.TrimPrefix(srv.URL, "http://")
	post := func(origin string) int {
		req, _ := http.NewRequest("POST", srv.URL+"/api/import", strings.NewReader(importFile))
		req.Header.Set("Content-Type", "text/plain")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := post("http://evil.example"); code != 403 {
		t.Fatalf("跨站 POST 应被拒绝，得到 %d", code)
	}
	if list, _ := db.List(context.Background()); len(list) != 0 {
		t.Fatalf("被拒绝的请求不应写库: %d 条", len(list))
	}
	if code := post("http://" + host); code != 200 {
		t.Fatalf("同源 POST 应放行，得到 %d", code)
	}
	if code := post(""); code != 200 { // curl 等不带 Origin
		t.Fatalf("不带 Origin 的 POST 应放行，得到 %d", code)
	}
	// 读请求不受影响
	req, _ := http.NewRequest("GET", srv.URL+"/playlist.m3u", nil)
	req.Header.Set("Origin", "http://evil.example")
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("跨站 GET 不应被拦: %d", resp.StatusCode)
	}
}
