package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"live-proxy/playlist"
)

// fakeUpstream 模拟三类源：udpxy 式的 TS、FLV、带相对与跨主机地址的 HLS。
func fakeUpstream(t *testing.T, other *httptest.Server) *httptest.Server {
	t.Helper()
	ts := bytes.Repeat(append([]byte{0x47}, make([]byte, 187)...), 20)
	mux := http.NewServeMux()
	mux.HandleFunc("/rtp/239.94.0.31:5140", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != "MyUA/1.0" {
			http.Error(w, "need ua", http.StatusForbidden) // 验证频道属性里的 UA 被带上
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(ts)
	})
	mux.HandleFunc("/live.flv", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("FLV\x01\x05rest")) })
	mux.HandleFunc("/hls/index.m3u8", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/real/master.m3u8", http.StatusFound) // 相对路径要以跳转后的地址为基准
	})
	mux.HandleFunc("/real/master.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\nmedia.m3u8\n")
	})
	mux.HandleFunc("/real/media.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"key.bin\"\n#EXTINF:2,\nseg1.ts\n#EXTINF:2,\n%s/cdn/seg2.ts\n", other.URL)
	})
	mux.HandleFunc("/real/seg1.ts", func(w http.ResponseWriter, r *http.Request) { w.Write(ts[:188]) })
	return httptest.NewServer(mux)
}

func TestWatchProxy(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte{0x47, 1, 2}) }))
	defer other.Close()
	up := fakeUpstream(t, other)
	defer up.Close()
	srv := newBrowseServer(t)

	ctx := context.Background()
	_, err := db.Import(ctx, []playlist.Channel{
		{Name: "CCTV-1", URL: up.URL + "/rtp/239.94.0.31:5140", Attrs: `http-user-agent="MyUA/1.0"`},
		{Name: "FLV", URL: up.URL + "/live.flv"},
		{Name: "HLS", URL: up.URL + "/hls/index.m3u8"},
		{Name: "RTSP", URL: "rtsp://1.2.3.4/x"},
		{Name: "虎牙", Platform: "huya", RoomID: "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	list, _ := db.List(ctx)
	id := map[string]int64{}
	for _, c := range list {
		id[c.Name] = c.ID
	}

	probe := func(name string) (int, map[string]string) {
		code, body := do(t, "GET", fmt.Sprintf("%s/api/channels/%d/probe", srv.URL, id[name]), "", nil)
		var m map[string]string
		json.Unmarshal([]byte(body), &m)
		return code, m
	}
	for name, want := range map[string]string{"CCTV-1": kindMPEGTS, "FLV": kindFLV, "HLS": kindHLS, "虎牙": kindFLV} {
		if code, m := probe(name); code != 200 || m["kind"] != want {
			t.Errorf("probe %s = %d %v, want kind %s", name, code, m, want)
		}
	}
	if code, m := probe("RTSP"); code != 422 || !strings.Contains(m["error"], "rtsp") {
		t.Errorf("rtsp 应提示浏览器播不了: %d %v", code, m)
	}

	// TS 直接透传，且带上了频道指定的 UA
	code, body := do(t, "GET", fmt.Sprintf("%s/api/channels/%d/stream", srv.URL, id["CCTV-1"]), "", nil)
	if code != 200 || len(body) != 188*20 || body[0] != 0x47 {
		t.Fatalf("TS 透传: %d len=%d", code, len(body))
	}

	// HLS：主列表 → 子列表 → 分片，地址全部改写成走代理
	code, master := do(t, "GET", fmt.Sprintf("%s/api/channels/%d/stream", srv.URL, id["HLS"]), "", nil)
	if code != 200 || !strings.Contains(master, url.QueryEscape(up.URL+"/real/media.m3u8")) {
		t.Fatalf("主列表改写（相对路径应以跳转后的地址为基准）: %d\n%s", code, master)
	}
	mediaURL := srv.URL + strings.TrimSpace(strings.Split(master, "\n")[2])
	_, media := do(t, "GET", mediaURL, "", nil)
	for _, want := range []string{
		`URI="/api/channels/`, url.QueryEscape(up.URL + "/real/key.bin"),
		url.QueryEscape(up.URL + "/real/seg1.ts"), url.QueryEscape(other.URL + "/cdn/seg2.ts"),
	} {
		if !strings.Contains(media, want) {
			t.Fatalf("子列表缺少 %q:\n%s", want, media)
		}
	}
	// 跨主机的分片：出现在播放列表里，所以允许
	for _, line := range strings.Split(media, "\n") {
		if strings.HasPrefix(line, "/api/") {
			if code, _ := do(t, "GET", srv.URL+line, "", nil); code != 200 {
				t.Errorf("分片代理失败 %d: %s", code, line)
			}
		}
	}

	// 没在播放列表里出现过的主机：拒绝，防止变成任意转发
	code, _ = do(t, "GET", fmt.Sprintf("%s/api/channels/%d/stream?u=%s", srv.URL, id["HLS"], url.QueryEscape("http://169.254.169.254/latest")), "", nil)
	if code != 403 {
		t.Fatalf("任意地址应被拒绝，得到 %d", code)
	}
	// 别的频道登记过的主机，不能借这个频道的 ID 访问
	code, _ = do(t, "GET", fmt.Sprintf("%s/api/channels/%d/stream?u=%s", srv.URL, id["FLV"], url.QueryEscape(other.URL+"/cdn/seg2.ts")), "", nil)
	if code != 403 {
		t.Fatalf("跨频道借用主机应被拒绝，得到 %d", code)
	}

	// 静态播放库
	if code, body := do(t, "GET", srv.URL+"/static/mpegts.js", "", nil); code != 200 || len(body) < 100000 {
		t.Fatalf("/static/mpegts.js: %d %d", code, len(body))
	}
}
