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
	// 根路径默认进浏览页。{$} 只匹配精确的 "/"，/{平台}/{房间号} 仍落到取流的兜底路由。
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/browse", http.StatusFound)
	})
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
	mux.HandleFunc("PUT /api/channels/order", handleReorderChannels)
	mux.HandleFunc("PATCH /api/channels/{id}", handleMoveChannel)
	mux.HandleFunc("GET /api/groups", handleGroups)
	mux.HandleFunc("POST /api/groups", handleAddGroup)
	mux.HandleFunc("PATCH /api/groups", handleRenameGroup)
	mux.HandleFunc("DELETE /api/groups", handleDeleteGroup)
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
	// type=user 按主播搜索；返回结构相同，Online 是粉丝/订阅数而不是在线人数。
	search := b.SearchRooms
	if r.URL.Query().Get("type") == "user" {
		search = b.SearchAnchors
	}
	page, err := search(r.Context(), kw, pageParam(r))
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
		Name: body.Name, Group: "网络直播", Logo: body.Logo, Platform: b.ID(), RoomID: roomID,
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

func handleReorderChannels(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var body struct {
		IDs []int64 `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.IDs == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "频道顺序不是合法 JSON"})
		return
	}
	if err := db.ReorderChannels(r.Context(), body.IDs); errors.Is(err, store.ErrOrderConflict) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "频道列表已变化，请刷新后重试"})
		return
	} else if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	triggerEPG()
	writeJSON(w, http.StatusOK, map[string]bool{"updated": true})
}

func readGroup(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体不是合法 JSON"})
		return false
	}
	return true
}

func validGroup(s string) bool { return len([]rune(s)) <= 100 }

func handleGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := db.Groups(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, groups)
}

func handleAddGroup(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if !readGroup(w, r, &body) {
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" || !validGroup(body.Name) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "分组名必须为 1–100 个字符"})
		return
	}
	added, err := db.AddGroup(r.Context(), body.Name)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if !added {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "分组已存在"})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]bool{"added": true})
}

func handleMoveChannel(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "无效的频道 id"})
		return
	}
	var body struct {
		Group string `json:"group"`
	}
	if !readGroup(w, r, &body) {
		return
	}
	body.Group = strings.TrimSpace(body.Group)
	if !validGroup(body.Group) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "分组名不能超过 100 个字符"})
		return
	}
	ok, err := db.SetChannelGroup(r.Context(), id, body.Group)
	if errors.Is(err, store.ErrGroupNotFound) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "目标分组已不存在，请刷新后重试"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "频道不存在"})
		return
	}
	triggerEPG()
	writeJSON(w, http.StatusOK, map[string]bool{"updated": true})
}

func handleRenameGroup(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Old string `json:"old"`
		New string `json:"new"`
	}
	if !readGroup(w, r, &body) {
		return
	}
	body.Old, body.New = strings.TrimSpace(body.Old), strings.TrimSpace(body.New)
	if body.Old == "" || body.New == "" || !validGroup(body.Old) || !validGroup(body.New) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "新旧分组名必须为 1–100 个字符"})
		return
	}
	n, err := db.RenameGroup(r.Context(), body.Old, body.New)
	if errors.Is(err, store.ErrGroupExists) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if n == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "分组不存在"})
		return
	}
	triggerEPG()
	writeJSON(w, http.StatusOK, map[string]int64{"updated": n})
}

func handleDeleteGroup(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if !readGroup(w, r, &body) {
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" || !validGroup(body.Name) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "无效的分组名"})
		return
	}
	n, err := db.DeleteGroup(r.Context(), body.Name)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if n == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "分组不存在"})
		return
	}
	triggerEPG()
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
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
	if removed {
		triggerEPG()
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
	if res.Added > 0 {
		triggerEPG() // 新频道可能有节目单
	}
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
	// 对上节目单的频道补 tvg-id：有的播放器只按 tvg-id 匹配节目单，不看频道名。
	matched := matchedEPGIDs()
	chs := make([]playlist.Channel, len(list))
	for i, c := range list {
		chs[i] = c.Channel
		if c.Platform == "" && matched[c.Name] && attrValue(c.Attrs, "tvg-id") == "" {
			chs[i].Attrs = strings.TrimSpace(c.Attrs + ` tvg-id="` + strings.ReplaceAll(c.Name, `"`, "'") + `"`)
		}
	}
	epgURL := ""
	if hasEPG() {
		epgURL = "http://" + r.Host + "/epg.xml.gz"
	}
	w.Header().Set("Content-Type", "audio/x-mpegurl; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	playlist.WriteEPG(w, chs, "http://"+r.Host, epgURL)
}
