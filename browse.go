package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"live-proxy/playlist"
	"live-proxy/sites"
	"live-proxy/store"
)

// 浏览页：/browse。按平台浏览分区、推荐、搜索直播间，收藏的房间存进 SQLite，
// 与导入的 m3u 频道一起从 /playlist.m3u 输出给电视。
//
// 与 /login 一样不做鉴权：只在局域网内提供，局域网里的设备都能收藏和导入。

//go:embed browse.html
var browsePage []byte

// db 收藏与导入频道的存储；-data 为空时是内存库，重启即丢。
var db *store.Store

// 浏览页上平台的显示顺序。
var browseOrder = []string{"huya", "bilibili", "douyu"}

func browsers() []sites.Browser {
	var out []sites.Browser
	for _, id := range browseOrder {
		if b, ok := registry[id].(sites.Browser); ok {
			out = append(out, b)
		}
	}
	return out
}

func browserByID(id string) (sites.Browser, bool) {
	b, ok := registry[id].(sites.Browser)
	return b, ok
}

// 分区几乎不变，而虎牙拉一次要四个请求；缓存半天，翻页切分区时不重复拉。
const categoryTTL = 12 * time.Hour

var categoryCache struct {
	mu sync.Mutex
	m  map[string]cachedCategories
}

type cachedCategories struct {
	at   time.Time
	cats []sites.Category
}

func categories(ctx context.Context, b sites.Browser) ([]sites.Category, error) {
	categoryCache.mu.Lock()
	c, ok := categoryCache.m[b.ID()]
	categoryCache.mu.Unlock()
	if ok && time.Since(c.at) < categoryTTL {
		return c.cats, nil
	}
	cats, err := b.Categories(ctx)
	if err != nil {
		return nil, err
	}
	categoryCache.mu.Lock()
	if categoryCache.m == nil {
		categoryCache.m = map[string]cachedCategories{}
	}
	categoryCache.m[b.ID()] = cachedCategories{at: time.Now(), cats: cats}
	categoryCache.mu.Unlock()
	return cats, nil
}

func registerBrowseRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /browse", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(browsePage)
	})
	mux.HandleFunc("GET /api/sites", handleSites)
	mux.HandleFunc("GET /api/sites/{p}/categories", withBrowser(handleCategories))
	mux.HandleFunc("GET /api/sites/{p}/rooms", withBrowser(handleCategoryRooms))
	mux.HandleFunc("GET /api/sites/{p}/recommend", withBrowser(handleRecommend))
	mux.HandleFunc("GET /api/sites/{p}/search", withBrowser(handleSearch))
	mux.HandleFunc("GET /api/sites/{p}/room/{rid}", withBrowser(handleRoomInfo))

	mux.HandleFunc("GET /api/favorites", handleFavoriteKeys)
	mux.HandleFunc("PUT /api/favorites/{p}/{rid}", withBrowser(handleAddFavorite))
	mux.HandleFunc("DELETE /api/favorites/{p}/{rid}", handleRemoveFavorite)

	mux.HandleFunc("GET /api/channels", handleChannels)
	mux.HandleFunc("DELETE /api/channels/{id}", handleDeleteChannel)
	mux.HandleFunc("POST /api/import", handleImport)

	mux.HandleFunc("GET /playlist.m3u", handlePlaylist)
}

// writeAPIErr 状态码沿用 classify：浏览页据此区分"房间号写错"(404)和"平台接口挂了"(502)。
func writeAPIErr(w http.ResponseWriter, r *http.Request, err error) {
	code, msg := classify(err)
	if code >= 500 {
		loggerFrom(r.Context()).Warn("浏览接口失败", "err", err)
	}
	writeJSON(w, code, map[string]string{"error": msg})
}

func withBrowser(h func(http.ResponseWriter, *http.Request, sites.Browser)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, ok := browserByID(r.PathValue("p"))
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "该平台不支持浏览: " + r.PathValue("p")})
			return
		}
		h(w, r, b)
	}
}

func pageParam(r *http.Request) int {
	p, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if p < 1 {
		return 1
	}
	return p
}

func handleSites(w http.ResponseWriter, r *http.Request) {
	type site struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	out := []site{}
	for _, b := range browsers() {
		out = append(out, site{b.ID(), b.Name()})
	}
	writeJSON(w, http.StatusOK, out)
}

func handleCategories(w http.ResponseWriter, r *http.Request, b sites.Browser) {
	cats, err := categories(r.Context(), b)
	if err != nil {
		writeAPIErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, cats)
}

// handleCategoryRooms ?id=二级分区&parent=一级分区。B站的列表接口两个都要。
func handleCategoryRooms(w http.ResponseWriter, r *http.Request, b sites.Browser) {
	q := r.URL.Query()
	sub := sites.SubCategory{ID: q.Get("id"), ParentID: q.Get("parent")}
	if sub.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "缺少分区 id"})
		return
	}
	page, err := b.CategoryRooms(r.Context(), sub, pageParam(r))
	writePage(w, r, page, err)
}

func handleRecommend(w http.ResponseWriter, r *http.Request, b sites.Browser) {
	page, err := b.RecommendRooms(r.Context(), pageParam(r))
	writePage(w, r, page, err)
}

func handleSearch(w http.ResponseWriter, r *http.Request, b sites.Browser) {
	kw := strings.TrimSpace(r.URL.Query().Get("kw"))
	if kw == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "缺少关键词"})
		return
	}
	page, err := b.SearchRooms(r.Context(), kw, pageParam(r))
	writePage(w, r, page, err)
}

func writePage(w http.ResponseWriter, r *http.Request, page sites.RoomPage, err error) {
	if err != nil {
		writeAPIErr(w, r, err)
		return
	}
	if page.Items == nil {
		page.Items = []sites.Room{} // 前端按数组处理，null 会让页面报错
	}
	writeJSON(w, http.StatusOK, page)
}

func handleRoomInfo(w http.ResponseWriter, r *http.Request, b sites.Browser) {
	room, err := b.RoomInfo(r.Context(), r.PathValue("rid"))
	if err != nil {
		writeAPIErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, room)
}

func handleFavoriteKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := db.RoomKeys(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, keys)
}

// handleAddFavorite 收藏。请求体可带 {"name","logo"}（从列表收藏时页面上已有，省一次请求）；
// 不带时按房间号查一次房间信息——"按房间号添加"走这条路，顺便把短号换成规范房间号，
// 否则同一个房间从列表收藏一次、按短号再加一次会变成两条。
func handleAddFavorite(w http.ResponseWriter, r *http.Request, b sites.Browser) {
	var body struct {
		Name string `json:"name"`
		Logo string `json:"logo"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体不是合法 JSON"})
			return
		}
	}
	roomID := r.PathValue("rid")
	if body.Name == "" {
		info, err := b.RoomInfo(r.Context(), roomID)
		if err != nil {
			writeAPIErr(w, r, err)
			return
		}
		roomID, body.Name, body.Logo = info.RoomID, info.UserName, info.Avatar
	}
	added, err := db.AddFavorite(r.Context(), playlist.Channel{
		Name: body.Name, Group: b.Name(), Logo: body.Logo, Platform: b.ID(), RoomID: roomID,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	loggerFrom(r.Context()).Info("收藏", "platform", b.ID(), "room", roomID, "name", body.Name, "added", added)
	writeJSON(w, http.StatusOK, map[string]any{"added": added, "room_id": roomID, "name": body.Name})
}

func handleRemoveFavorite(w http.ResponseWriter, r *http.Request) {
	removed, err := db.RemoveFavorite(r.Context(), r.PathValue("p"), r.PathValue("rid"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	loggerFrom(r.Context()).Info("取消收藏", "platform", r.PathValue("p"), "room", r.PathValue("rid"), "removed", removed)
	writeJSON(w, http.StatusOK, map[string]bool{"removed": removed})
}

func handleChannels(w http.ResponseWriter, r *http.Request) {
	list, err := db.List(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if list == nil {
		list = []store.Channel{}
	}
	writeJSON(w, http.StatusOK, list)
}

func handleDeleteChannel(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "无效的频道 id"})
		return
	}
	removed, err := db.Delete(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"removed": removed})
}

// 导入文件上限。344 个频道的 scdx.m3u 约 80KB，5MB 足够容纳上万个频道。
const maxImportSize = 5 << 20

// handleImport 接受两种请求体：multipart 表单的 file 字段（页面上选文件），
// 或直接把 m3u 文本作为请求体（curl --data-binary @x.m3u）。
func handleImport(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxImportSize)
	var src io.Reader = r.Body
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt == "multipart/form-data" {
		f, _, err := r.FormFile("file")
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "读取上传文件失败: " + err.Error()})
			return
		}
		defer f.Close()
		src = f
	}
	chs, skipped, err := playlist.Parse(src, func(id string) bool { _, ok := registry[id]; return ok })
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if len(chs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "文件里没有可识别的频道", "skipped": skipped})
		return
	}
	res, err := db.Import(r.Context(), chs)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	res.Skipped = skipped
	loggerFrom(r.Context()).Info("导入 m3u", "added", res.Added, "existed", res.Existed, "skipped", res.Skipped)
	writeJSON(w, http.StatusOK, res)
}

// handlePlaylist 合并输出收藏与导入的频道。本服务的直播间用请求里的 Host 拼地址：
// 电视用什么地址拉到这份列表，就用同一个地址去播，换了 IP 或用主机名访问都不用改库。
func handlePlaylist(w http.ResponseWriter, r *http.Request) {
	list, err := db.List(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	chs := make([]playlist.Channel, len(list))
	for i, c := range list {
		chs[i] = c.Channel
	}
	w.Header().Set("Content-Type", "audio/x-mpegurl; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	playlist.Write(w, chs, "http://"+r.Host)
}
