package main

import (
	"bufio"
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"live-proxy/sites"
	"live-proxy/store"
)

// 浏览页里的"点击观看"。
//
// 直播间直接用中转 /{平台}/{房间}?relay=1（同源 FLV，mpegts.js 播放）。
// 导入的外部频道（udpxy 组播、各家 m3u8）浏览器不能直接拉：udpxy 和多数 CDN 不给
// 跨域头，所以由本服务按频道 ID 代理。只代理库里已有的频道，且 HLS 只放行该频道
// 自己的播放列表里出现过的主机——否则这个接口就成了局域网里的任意转发器。

//go:embed static
var staticFS embed.FS

func registerWatchRoutes(mux *http.ServeMux) {
	sub, _ := fs.Sub(staticFS, "static")
	files := http.FileServerFS(sub)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 播放库版本固定在仓库里，升级时文件名不变但内容变了，缓存一天即可。
		w.Header().Set("Cache-Control", "public, max-age=86400")
		files.ServeHTTP(w, r)
	})))
	mux.HandleFunc("GET /api/channels/{id}/probe", handleProbe)
	mux.HandleFunc("GET /api/channels/{id}/stream", handleChannelStream)
}

// 流的格式，决定浏览页用哪个播放库。
const (
	kindHLS    = "hls"
	kindFLV    = "flv"
	kindMPEGTS = "mpegts"
)

func channelByPath(w http.ResponseWriter, r *http.Request) (store.Channel, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "无效的频道 id"})
		return store.Channel{}, false
	}
	c, err := db.Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "频道不存在"})
		return store.Channel{}, false
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return store.Channel{}, false
	}
	return c, true
}

var attrValRe = regexp.MustCompile(`([A-Za-z0-9_-]+)="([^"]*)"`)

// upstreamHeaders 频道属性里声明的 UA/Referer。部分源不带指定 UA 会直接 403。
func upstreamHeaders(c store.Channel) http.Header {
	h := http.Header{}
	h.Set("User-Agent", sites.UserAgent)
	for _, m := range attrValRe.FindAllStringSubmatch(c.Attrs, -1) {
		switch strings.ToLower(m[1]) {
		case "http-user-agent", "user-agent":
			h.Set("User-Agent", m[2])
		case "http-referrer", "http-referer", "referer":
			h.Set("Referer", m[2])
		}
	}
	return h
}

func fetch(ctx context.Context, c store.Channel, u string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header = upstreamHeaders(c)
	resp, err := relayClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("上游返回 HTTP %d", resp.StatusCode)
	}
	return resp, nil
}

// sniff 按内容判断格式，不信扩展名：不少源的 .m3u8 实际是 302 到 TS，udpxy 的地址也没有扩展名。
func sniff(contentType string, head []byte) string {
	ct := strings.ToLower(contentType)
	switch {
	case bytes.HasPrefix(bytes.TrimLeft(head, "\ufeff \r\n\t"), []byte("#EXTM3U")) || strings.Contains(ct, "mpegurl"):
		return kindHLS
	case bytes.HasPrefix(head, []byte("FLV")):
		return kindFLV
	case len(head) > 0 && head[0] == 0x47:
		return kindMPEGTS
	}
	return ""
}

// handleProbe 先探一次格式，浏览页据此加载对应播放库。只读开头几百字节就断开：
// udpxy 每个连接都会让上游加入一次组播，探完要立刻放掉。
func handleProbe(w http.ResponseWriter, r *http.Request) {
	c, ok := channelByPath(w, r)
	if !ok {
		return
	}
	if c.Platform != "" {
		writeJSON(w, http.StatusOK, map[string]string{"kind": kindFLV, "url": "/" + c.Platform + "/" + url.PathEscape(c.RoomID) + "?relay=1"})
		return
	}
	if !strings.HasPrefix(c.URL, "http://") && !strings.HasPrefix(c.URL, "https://") {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "浏览器只能播放 http(s) 地址，这个频道是 " + strings.SplitN(c.URL, ":", 2)[0] + "，请在电视上观看"})
		return
	}
	// 8 秒：没信号的组播只能等超时才能判定，批量检测时太长会让整轮检测拖到几分钟。
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	resp, err := fetch(ctx, c, c.URL)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "连接频道源失败: " + err.Error()})
		return
	}
	head := make([]byte, 512)
	n, _ := io.ReadAtLeast(resp.Body, head, 8)
	resp.Body.Close()
	if n == 0 {
		// udpxy 对没有信号的组播照样返回 200，只是一直不给数据；不单独说明会被误报成"格式不认识"。
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "连上了频道源，但一直没有收到数据：频道可能已停播，或这条线路没有开通它"})
		return
	}
	kind := sniff(resp.Header.Get("Content-Type"), head[:n])
	if kind == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "无法识别的流格式（Content-Type: " + resp.Header.Get("Content-Type") + "）"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"kind": kind, "url": fmt.Sprintf("/api/channels/%d/stream", c.ID)})
}

// hlsHosts 每个频道允许代理的主机：频道地址本身的主机，加上它的播放列表里出现过的主机。
// 只在内存里：重启后播放器重新拉一次主播放列表就会重新登记。
var hlsHosts = struct {
	sync.Mutex
	m map[int64]map[string]bool
}{m: map[int64]map[string]bool{}}

func allowHost(id int64, host string) {
	hlsHosts.Lock()
	defer hlsHosts.Unlock()
	if hlsHosts.m[id] == nil {
		hlsHosts.m[id] = map[string]bool{}
	}
	hlsHosts.m[id][strings.ToLower(host)] = true
}

func hostAllowed(id int64, host string) bool {
	hlsHosts.Lock()
	defer hlsHosts.Unlock()
	return hlsHosts.m[id][strings.ToLower(host)]
}

// handleChannelStream 代理频道的流。不带 ?u= 时拉频道地址本身；带 ?u= 时拉 HLS 播放列表里改写过的子地址。
func handleChannelStream(w http.ResponseWriter, r *http.Request) {
	c, ok := channelByPath(w, r)
	if !ok {
		return
	}
	log := loggerFrom(r.Context())
	if c.Platform != "" {
		http.Redirect(w, r, "/"+c.Platform+"/"+url.PathEscape(c.RoomID)+"?relay=1", http.StatusFound)
		return
	}
	target := c.URL
	if u := r.URL.Query().Get("u"); u != "" {
		pu, err := url.Parse(u)
		if err != nil || (pu.Scheme != "http" && pu.Scheme != "https") || !hostAllowed(c.ID, pu.Host) {
			writeErr(w, http.StatusForbidden, "该地址不属于这个频道的播放列表")
			return
		}
		target = u
	}
	tu, err := url.Parse(target)
	if err != nil || (tu.Scheme != "http" && tu.Scheme != "https") {
		writeErr(w, http.StatusUnprocessableEntity, "浏览器只能播放 http(s) 地址")
		return
	}
	allowHost(c.ID, tu.Host)

	resp, err := fetch(r.Context(), c, target)
	if err != nil {
		log.Warn("频道代理失败", "id", c.ID, "name", c.Name, "err", err)
		writeErr(w, http.StatusBadGateway, "连接频道源失败: "+err.Error())
		return
	}
	defer resp.Body.Close()

	br := bufio.NewReaderSize(resp.Body, 64<<10)
	head, _ := br.Peek(512)
	if sniff(resp.Header.Get("Content-Type"), head) == kindHLS {
		// 播放列表最多几百 KB；限制读取量，防止一个错误的源把内存打满。
		body, err := io.ReadAll(io.LimitReader(br, 4<<20))
		if err != nil {
			writeErr(w, http.StatusBadGateway, "读取播放列表失败: "+err.Error())
			return
		}
		// 以最终地址为基准解析相对路径：源经常 302 到另一台主机，用原地址解析会拼错。
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(rewritePlaylist(body, resp.Request.URL, c.ID))
		return
	}

	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		w.Header().Set("Content-Length", cl) // HLS 分片有长度；直播 TS 没有，按块写
	}
	w.Header().Set("Cache-Control", "no-store")
	// 直播流要持续几小时，服务器级 WriteTimeout 会把它定时掐断，按次续期（与中转相同）。
	dw := &deadlineWriter{w: w, rc: http.NewResponseController(w)}
	if _, err := io.Copy(dw, br); err != nil && r.Context().Err() == nil {
		log.Info("频道代理结束", "id", c.ID, "name", c.Name, "err", err)
	}
}

var uriAttrRe = regexp.MustCompile(`URI="([^"]*)"`)

// rewritePlaylist 把播放列表里的子地址（分片、子播放列表、密钥、初始化段）
// 都改成经本服务代理，并登记它们的主机。
func rewritePlaylist(body []byte, base *url.URL, id int64) []byte {
	proxied := func(ref string) string {
		u, err := base.Parse(strings.TrimSpace(ref))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return ref
		}
		allowHost(id, u.Host)
		return fmt.Sprintf("/api/channels/%d/stream?u=%s", id, url.QueryEscape(u.String()))
	}
	var out bytes.Buffer
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
		case strings.HasPrefix(trimmed, "#"):
			if strings.Contains(line, `URI="`) {
				line = uriAttrRe.ReplaceAllStringFunc(line, func(m string) string {
					return `URI="` + proxied(uriAttrRe.FindStringSubmatch(m)[1]) + `"`
				})
			}
		default:
			line = proxied(trimmed)
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return out.Bytes()
}
