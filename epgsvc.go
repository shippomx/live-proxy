package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"live-proxy/epg"
	"live-proxy/sites"
	"live-proxy/store"
)

// 节目单：页面里维护来源列表，本服务定时拉取合并，只留播放列表里有的频道和近几天的节目，
// 压缩后在 /epg.xml.gz 提供，并写进 /playlist.m3u 的 x-tvg-url。
// 电视从局域网拉几十 KB，而不是自己去外网下几 MB 的原始节目单。

const (
	epgRefreshEvery = 6 * time.Hour
	epgKeepBefore   = 24 * time.Hour // 回看用
	epgKeepAfter    = 72 * time.Hour
	epgMaxSource    = 128 << 20 // 单个来源上限，防止错误的地址把内存打满
	epgDebounce     = 3 * time.Second
)

// epgClient 拉节目单专用：节目单动辄几 MB 到几十 MB，sites 里 15 秒总超时的客户端拉不完。
var epgClient = &http.Client{Timeout: 3 * time.Minute}

var epgSvc struct {
	mu       sync.Mutex
	gz       []byte
	result   epg.Result // XML 字段不保留，只留统计
	builtAt  time.Time
	building bool
	lastErr  string
	timer    *time.Timer
	started  bool // 只有正式启动后，频道变化才自动重新生成（测试里不应在后台去外网拉节目单）
}

func epgFile(name string) string {
	if loginDir == "" {
		return ""
	}
	return filepath.Join(loginDir, name)
}

// loadSavedEPG 启动时先用上次生成的结果：拉取要几十秒，这期间电视请求节目单不应拿到空内容。
func loadSavedEPG() {
	p := epgFile("epg.xml.gz")
	if p == "" {
		return
	}
	gz, err := os.ReadFile(p)
	if err != nil {
		return
	}
	var meta struct {
		Result  epg.Result `json:"result"`
		BuiltAt time.Time  `json:"built_at"`
	}
	if b, err := os.ReadFile(epgFile("epg.json")); err == nil {
		json.Unmarshal(b, &meta)
	}
	epgSvc.mu.Lock()
	epgSvc.gz, epgSvc.result, epgSvc.builtAt = gz, meta.Result, meta.BuiltAt
	epgSvc.mu.Unlock()
}

func writeAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// startEPG 启动后台刷新：先生成一次，之后定时刷新。
func startEPG(ctx context.Context) {
	epgSvc.mu.Lock()
	epgSvc.started = true
	epgSvc.mu.Unlock()
	loadSavedEPG()
	go func() {
		buildEPG(ctx)
		t := time.NewTicker(epgRefreshEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				buildEPG(ctx)
			}
		}
	}()
}

// triggerEPG 频道或来源变了之后重新生成。去抖：一次导入会连续触发多次，只生成一次。
func triggerEPG() {
	epgSvc.mu.Lock()
	defer epgSvc.mu.Unlock()
	if !epgSvc.started {
		return
	}
	if epgSvc.timer != nil {
		epgSvc.timer.Stop()
	}
	epgSvc.timer = time.AfterFunc(epgDebounce, func() { buildEPG(context.Background()) })
}

// epgTargets 播放列表里需要节目单的频道。直播间没有节目单，跳过。
// 频道带 tvg-id 时沿用它作为节目单里的 ID，否则用频道名。
func epgTargets(chs []store.Channel) []epg.Target {
	var out []epg.Target
	seen := map[string]bool{}
	for _, c := range chs {
		if c.Platform != "" {
			continue
		}
		id := attrValue(c.Attrs, "tvg-id")
		if id == "" {
			id = c.Name
		}
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		names := []string{c.Name}
		if n := attrValue(c.Attrs, "tvg-name"); n != "" && n != c.Name {
			names = append(names, n)
		}
		out = append(out, epg.Target{ID: id, Names: names})
	}
	return out
}

func attrValue(attrs, key string) string {
	for _, m := range attrValRe.FindAllStringSubmatch(attrs, -1) {
		if strings.EqualFold(m[1], key) {
			return m[2]
		}
	}
	return ""
}

func fetchEPGSource(ctx context.Context, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", sites.UserAgent)
	resp, err := epgClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, epgMaxSource+1))
	if err != nil {
		return nil, err
	}
	if len(b) > epgMaxSource {
		return nil, fmt.Errorf("超过 %d MB 上限", epgMaxSource>>20)
	}
	return b, nil
}

func buildEPG(ctx context.Context) {
	epgSvc.mu.Lock()
	if epgSvc.building {
		epgSvc.mu.Unlock()
		triggerEPG() // 正在生成时又有变化：生成完再来一次
		return
	}
	epgSvc.building = true
	epgSvc.mu.Unlock()
	defer func() {
		epgSvc.mu.Lock()
		epgSvc.building = false
		epgSvc.mu.Unlock()
	}()

	start := time.Now()
	fail := func(err error) {
		slog.Warn("节目单生成失败", "err", err)
		epgSvc.mu.Lock()
		epgSvc.lastErr = err.Error()
		epgSvc.mu.Unlock()
	}
	srcs, err := db.EPGSources(ctx)
	if err != nil {
		fail(err)
		return
	}
	chs, err := db.List(ctx)
	if err != nil {
		fail(err)
		return
	}
	var inputs []epg.Source
	var fetchErrs []epg.SourceStat
	for _, s := range srcs {
		if !s.Enabled {
			continue
		}
		b, err := fetchEPGSource(ctx, s.URL)
		if err != nil {
			fetchErrs = append(fetchErrs, epg.SourceStat{Name: s.URL, Err: "下载失败: " + err.Error()})
			continue
		}
		inputs = append(inputs, epg.Source{Name: s.URL, Data: bytes.NewReader(b)})
	}
	res := epg.Merge(epgTargets(chs), inputs, time.Now(), epgKeepBefore, epgKeepAfter)
	res.Sources = append(res.Sources, fetchErrs...)
	gz := epg.Gzip(res.XML)
	res.XML = nil

	now := time.Now()
	epgSvc.mu.Lock()
	epgSvc.gz, epgSvc.result, epgSvc.builtAt, epgSvc.lastErr = gz, res, now, ""
	epgSvc.mu.Unlock()
	if p := epgFile("epg.xml.gz"); p != "" {
		meta, _ := json.Marshal(map[string]any{"result": res, "built_at": now})
		if err := writeAtomic(p, gz); err != nil {
			slog.Warn("保存节目单失败", "err", err)
		} else {
			writeAtomic(epgFile("epg.json"), meta)
		}
	}
	slog.Info("节目单已生成", "matched", len(res.Matched), "unmatched", len(res.Unmatched),
		"programmes", res.Programmes, "gz_kb", len(gz)/1024, "took", time.Since(start).Round(time.Millisecond))
}

// matchedEPGIDs 当前节目单覆盖的频道 ID，播放列表据此补 tvg-id。
func matchedEPGIDs() map[string]bool {
	epgSvc.mu.Lock()
	defer epgSvc.mu.Unlock()
	m := make(map[string]bool, len(epgSvc.result.Matched))
	for _, id := range epgSvc.result.Matched {
		m[id] = true
	}
	return m
}

func hasEPG() bool {
	epgSvc.mu.Lock()
	defer epgSvc.mu.Unlock()
	return len(epgSvc.gz) > 0
}

func registerEPGRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /epg.xml.gz", func(w http.ResponseWriter, r *http.Request) {
		epgSvc.mu.Lock()
		gz, at := epgSvc.gz, epgSvc.builtAt
		epgSvc.mu.Unlock()
		if len(gz) == 0 {
			writeErr(w, http.StatusServiceUnavailable, "节目单还没生成，稍后再试")
			return
		}
		w.Header().Set("Content-Type", "application/gzip")
		http.ServeContent(w, r, "epg.xml.gz", at, bytes.NewReader(gz))
	})
	mux.HandleFunc("GET /epg.xml", func(w http.ResponseWriter, r *http.Request) {
		epgSvc.mu.Lock()
		gz := epgSvc.gz
		epgSvc.mu.Unlock()
		if len(gz) == 0 {
			writeErr(w, http.StatusServiceUnavailable, "节目单还没生成，稍后再试")
			return
		}
		zr, err := gzip.NewReader(bytes.NewReader(gz))
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		io.Copy(w, zr)
	})
	mux.HandleFunc("GET /api/epg", handleEPGStatus)
	mux.HandleFunc("POST /api/epg/sources", handleAddEPGSource)
	mux.HandleFunc("PATCH /api/epg/sources/{id}", handleToggleEPGSource)
	mux.HandleFunc("DELETE /api/epg/sources/{id}", handleDeleteEPGSource)
	mux.HandleFunc("POST /api/epg/refresh", func(w http.ResponseWriter, r *http.Request) {
		go buildEPG(context.Background())
		writeJSON(w, http.StatusAccepted, map[string]bool{"started": true})
	})
}

func handleEPGStatus(w http.ResponseWriter, r *http.Request) {
	srcs, err := db.EPGSources(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	epgSvc.mu.Lock()
	res, at, building, lastErr, size := epgSvc.result, epgSvc.builtAt, epgSvc.building, epgSvc.lastErr, len(epgSvc.gz)
	epgSvc.mu.Unlock()
	stats := map[string]epg.SourceStat{}
	for _, s := range res.Sources {
		stats[s.Name] = s
	}
	type row struct {
		store.EPGSource
		Stat *epg.SourceStat `json:"stat,omitempty"`
	}
	rows := []row{}
	for _, s := range srcs {
		rw := row{EPGSource: s}
		if st, ok := stats[s.URL]; ok {
			rw.Stat = &st
		}
		rows = append(rows, rw)
	}
	var builtAt int64
	if !at.IsZero() {
		builtAt = at.Unix()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"url": "http://" + r.Host + "/epg.xml.gz", "sources": rows, "built_at": builtAt, "building": building,
		"error": lastErr, "size": size, "programmes": res.Programmes,
		"matched": nonNil(res.Matched), "unmatched": nonNil(res.Unmatched),
	})
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func handleAddEPGSource(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体不是合法 JSON"})
		return
	}
	u, err := url.Parse(strings.TrimSpace(body.URL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请填写 http(s) 开头的节目单地址"})
		return
	}
	added, err := db.AddEPGSource(r.Context(), u.String())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if added {
		triggerEPG()
	}
	writeJSON(w, http.StatusOK, map[string]bool{"added": added})
}

func epgSourceID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "无效的来源 id"})
		return 0, false
	}
	return id, true
}

func handleToggleEPGSource(w http.ResponseWriter, r *http.Request) {
	id, ok := epgSourceID(w, r)
	if !ok {
		return
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&body); err != nil || body.Enabled == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "需要 {\"enabled\": true|false}"})
		return
	}
	changed, err := db.SetEPGSourceEnabled(r.Context(), id, *body.Enabled)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if changed {
		triggerEPG()
	}
	writeJSON(w, http.StatusOK, map[string]bool{"changed": changed})
}

func handleDeleteEPGSource(w http.ResponseWriter, r *http.Request) {
	id, ok := epgSourceID(w, r)
	if !ok {
		return
	}
	removed, err := db.DeleteEPGSource(r.Context(), id)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if removed {
		triggerEPG()
	}
	writeJSON(w, http.StatusOK, map[string]bool{"removed": removed})
}
