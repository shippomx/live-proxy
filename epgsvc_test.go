package main

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"live-proxy/playlist"
)

func TestEPGEndToEnd(t *testing.T) {
	day := time.Now().Format("20060102")
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<tv><channel id="c1"><display-name>CCTV1</display-name></channel>
<programme start="%s120000 +0800" channel="c1"><title>新闻</title></programme></tv>`, day)
	}))
	defer src.Close()
	srv := newBrowseServer(t)
	ctx := context.Background()

	// 默认来源指向外网，测试里换成假的
	list, _ := db.EPGSources(ctx)
	for _, s := range list {
		db.DeleteEPGSource(ctx, s.ID)
	}
	code, body := do(t, "POST", srv.URL+"/api/epg/sources", "application/json", strings.NewReader(`{"url":"`+src.URL+`/e.xml"}`))
	if code != 200 || !strings.Contains(body, `"added":true`) {
		t.Fatalf("添加来源: %d %s", code, body)
	}
	if code, _ := do(t, "POST", srv.URL+"/api/epg/sources", "application/json", strings.NewReader(`{"url":"ftp://x"}`)); code != 400 {
		t.Fatalf("非 http 地址应拒绝，得到 %d", code)
	}
	db.Import(ctx, []playlist.Channel{
		{Name: "CCTV-1高清", Group: "央视", URL: "http://192.168.5.3:4022/rtp/239.94.0.31:5140"},
		{Name: "没有节目单的台", Group: "其他", URL: "http://x/2"},
		{Name: "虎牙", Platform: "huya", RoomID: "1"},
	})

	// 还没生成时：节目单接口 503，播放列表不带 x-tvg-url
	epgSvc.mu.Lock()
	epgSvc.gz = nil
	epgSvc.mu.Unlock()
	if code, _ := do(t, "GET", srv.URL+"/epg.xml.gz", "", nil); code != 503 {
		t.Fatalf("未生成时应 503，得到 %d", code)
	}
	_, m3u := do(t, "GET", srv.URL+"/playlist.m3u", "", nil)
	if strings.Contains(m3u, "x-tvg-url") {
		t.Fatalf("节目单还没生成，不应写 x-tvg-url:\n%s", m3u)
	}

	buildEPG(ctx)

	resp, err := http.Get(srv.URL + "/epg.xml.gz")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("/epg.xml.gz: %v %v", err, resp)
	}
	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatalf("不是 gzip: %v", err)
	}
	xmlb, _ := io.ReadAll(zr)
	resp.Body.Close()
	if !strings.Contains(string(xmlb), `channel="CCTV-1高清"`) || !strings.Contains(string(xmlb), "新闻") {
		t.Fatalf("节目单内容:\n%s", xmlb)
	}

	_, m3u = do(t, "GET", srv.URL+"/playlist.m3u", "", nil)
	host := strings.TrimPrefix(srv.URL, "http://")
	if !strings.HasPrefix(m3u, `#EXTM3U x-tvg-url="http://`+host+`/epg.xml.gz"`) {
		t.Fatalf("播放列表头部应带节目单地址:\n%s", m3u)
	}
	if !strings.Contains(m3u, `tvg-id="CCTV-1高清"`) || strings.Contains(m3u, `tvg-id="没有节目单的台"`) {
		t.Fatalf("只有对上节目单的频道才补 tvg-id:\n%s", m3u)
	}

	_, st := do(t, "GET", srv.URL+"/api/epg", "", nil)
	if !strings.Contains(st, `"matched":["CCTV-1高清"]`) || !strings.Contains(st, `"unmatched":["没有节目单的台"]`) || !strings.Contains(st, `"programmes":1`) {
		t.Fatalf("状态接口: %s", st)
	}
}
