package sites

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

var _ Browser = (*Bilibili)(nil)

const biliReferer = "https://live.bilibili.com/"

// biliBrowseState 浏览接口需要的匿名凭据：WBI 签名密钥、buvid。
// 都是取一次就能用很久的东西，每个请求都去拿会让一次翻页变成四个请求。
// 放在包级而非 Bilibili 结构体里：取流不需要它们。
var biliBrowseState struct {
	mu     sync.Mutex
	imgKey string
	subKey string
	keysAt time.Time
	buvid3 string
	buvid4 string
}

// WBI 密钥每天轮换；一小时刷新一次足够，过期的密钥会让签名接口返回 -352。
const biliWbiKeyTTL = time.Hour

var biliMixinKeyEncTab = [...]int{
	46, 47, 18, 2, 53, 8, 23, 32, 15, 50, 10, 31, 58, 3, 45, 35, 27, 43, 5, 49,
	33, 9, 42, 19, 29, 28, 14, 39, 12, 38, 41, 13, 37, 48, 7, 16, 24, 55, 40,
	61, 26, 17, 0, 1, 60, 51, 30, 4, 22, 25, 54, 21, 56, 59, 6, 63, 57, 62, 11,
	36, 20, 34, 44, 52,
}

func biliMixinKey(orig string) string {
	var b strings.Builder
	for _, i := range biliMixinKeyEncTab {
		if i < len(orig) {
			b.WriteByte(orig[i])
		}
	}
	s := b.String()
	if len(s) > 32 {
		s = s[:32]
	}
	return s
}

// biliWbiSign 按 WBI 规则给参数签名：加 wts、按 key 排序、值里去掉 !'()*、
// 拼上 mixin key 取 md5 作为 w_rid。返回可直接拼在 URL 上的查询串。
func biliWbiSign(params url.Values, imgKey, subKey string, now time.Time) string {
	p := url.Values{}
	for k, vs := range params {
		for _, v := range vs {
			p.Add(k, strings.Map(func(r rune) rune {
				if strings.ContainsRune("!'()*", r) {
					return -1
				}
				return r
			}, v))
		}
	}
	p.Set("wts", strconv.FormatInt(now.Unix(), 10))
	query := p.Encode() // Encode 按 key 排序，正是签名要求的顺序
	sum := md5.Sum([]byte(query + biliMixinKey(imgKey+subKey)))
	return query + "&w_rid=" + hex.EncodeToString(sum[:])
}

// biliHeader 浏览请求的公共头。搜索接口不带 buvid3 会直接返回 -412。
func (b *Bilibili) biliHeader(ctx context.Context) map[string]string {
	st := &biliBrowseState
	st.mu.Lock()
	need := st.buvid3 == ""
	st.mu.Unlock()
	if need {
		var r struct {
			Data struct {
				B3 string `json:"b_3"`
				B4 string `json:"b_4"`
			} `json:"data"`
		}
		// 拿不到 buvid 不中断：多数列表接口没有它也能用，只有搜索会失败，失败时再报。
		if err := getJSON(ctx, "https://api.bilibili.com/x/frontend/finger/spi", nil, &r); err == nil {
			st.mu.Lock()
			st.buvid3, st.buvid4 = r.Data.B3, r.Data.B4
			st.mu.Unlock()
		}
	}
	st.mu.Lock()
	cookie := "buvid3=" + st.buvid3 + ";buvid4=" + st.buvid4 + ";"
	st.mu.Unlock()
	if c := b.cookie(); c != "" {
		if strings.Contains(c, "buvid3") {
			cookie = c
		} else {
			cookie = c + ";" + cookie
		}
	}
	return map[string]string{"Referer": biliReferer, "Cookie": cookie}
}

func (b *Bilibili) wbiKeys(ctx context.Context) (string, string, error) {
	st := &biliBrowseState
	st.mu.Lock()
	if st.imgKey != "" && time.Since(st.keysAt) < biliWbiKeyTTL {
		img, sub := st.imgKey, st.subKey
		st.mu.Unlock()
		return img, sub, nil
	}
	st.mu.Unlock()

	// nav 未登录时 code=-101，但 wbi_img 照样返回，所以不看 code。
	var r struct {
		Data struct {
			WbiImg struct {
				ImgURL string `json:"img_url"`
				SubURL string `json:"sub_url"`
			} `json:"wbi_img"`
		} `json:"data"`
	}
	if err := getJSON(ctx, "https://api.bilibili.com/x/web-interface/nav", b.biliHeader(ctx), &r); err != nil {
		return "", "", err
	}
	img, sub := biliKeyFromURL(r.Data.WbiImg.ImgURL), biliKeyFromURL(r.Data.WbiImg.SubURL)
	if img == "" || sub == "" {
		return "", "", fmt.Errorf("%w: bilibili nav 缺少 wbi_img", ErrParse)
	}
	st.mu.Lock()
	st.imgKey, st.subKey, st.keysAt = img, sub, time.Now()
	st.mu.Unlock()
	return img, sub, nil
}

func biliKeyFromURL(u string) string {
	base := u[strings.LastIndex(u, "/")+1:]
	return strings.SplitN(base, ".", 2)[0]
}

// biliSigned 对签名接口发请求。code 非 0 时按上游错误返回，并带上 message，
// 例如 -352（风控/签名失效）能在日志里直接看出来。
func (b *Bilibili) biliSigned(ctx context.Context, base string, params url.Values, out any) error {
	img, sub, err := b.wbiKeys(ctx)
	if err != nil {
		return err
	}
	api := base + "?" + biliWbiSign(params, img, sub, time.Now())
	return getJSON(ctx, api, b.biliHeader(ctx), out)
}

type biliEnvelope struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e biliEnvelope) err(endpoint string) error {
	if e.Code == 0 {
		return nil
	}
	if e.Code == -352 {
		// 签名密钥可能刚轮换，清掉缓存让下次请求重新取。
		biliBrowseState.mu.Lock()
		biliBrowseState.imgKey = ""
		biliBrowseState.mu.Unlock()
	}
	return &UpstreamError{Endpoint: endpoint, Err: fmt.Errorf("code=%d %s", e.Code, e.Message)}
}

func (b *Bilibili) Categories(ctx context.Context) ([]Category, error) {
	const api = "https://api.live.bilibili.com/room/v1/Area/getList?need_entrance=1&parent_id=0"
	var r struct {
		biliEnvelope
		Data []struct {
			ID   jsonStr `json:"id"`
			Name string  `json:"name"`
			List []struct {
				ID       jsonStr `json:"id"`
				ParentID jsonStr `json:"parent_id"`
				Name     string  `json:"name"`
				Pic      string  `json:"pic"`
			} `json:"list"`
		} `json:"data"`
	}
	if err := getJSON(ctx, api, b.biliHeader(ctx), &r); err != nil {
		return nil, err
	}
	if err := r.err(api); err != nil {
		return nil, err
	}
	out := make([]Category, 0, len(r.Data))
	for _, top := range r.Data {
		c := Category{ID: string(top.ID), Name: top.Name}
		for _, s := range top.List {
			pic := s.Pic
			if pic != "" {
				pic += "@100w.png"
			}
			c.Children = append(c.Children, SubCategory{
				ID: string(s.ID), ParentID: string(s.ParentID), Name: s.Name, Pic: pic,
			})
		}
		out = append(out, c)
	}
	return out, nil
}

type biliListResp struct {
	biliEnvelope
	Data struct {
		HasMore int `json:"has_more"`
		List    []struct {
			RoomID jsonStr `json:"roomid"`
			Title  string  `json:"title"`
			Cover  string  `json:"cover"`
			Uname  string  `json:"uname"`
			Face   string  `json:"face"`
			Online jsonNum `json:"online"`
		} `json:"list"`
	} `json:"data"`
}

func (r biliListResp) rooms() []Room {
	out := make([]Room, 0, len(r.Data.List))
	for _, it := range r.Data.List {
		out = append(out, Room{
			RoomID:   string(it.RoomID),
			Title:    it.Title,
			Cover:    biliImg(it.Cover, "@400w.jpg"),
			UserName: it.Uname,
			Avatar:   biliImg(it.Face, "@100w.jpg"),
			Online:   int64(it.Online),
			Live:     true,
		})
	}
	return out
}

// CategoryRooms 用老版 room/v3/area/getRoomList 而不是网页现在用的 second/getList：
// 后者在 WBI 签名之外还有一层风控，匿名请求实测恒返回 -352（2026-10-31），
// v3 接口不需要签名，按分区过滤正确，还带总数可以判断翻页。
func (b *Bilibili) CategoryRooms(ctx context.Context, sub SubCategory, page int) (RoomPage, error) {
	const pageSize = 30
	page = pageOrFirst(page)
	q := url.Values{
		"platform":       {"web"},
		"parent_area_id": {sub.ParentID},
		"area_id":        {sub.ID},
		"sort_type":      {"online"},
		"page":           {strconv.Itoa(page)},
		"page_size":      {strconv.Itoa(pageSize)},
	}
	api := "https://api.live.bilibili.com/room/v3/area/getRoomList?" + q.Encode()
	var r struct {
		biliEnvelope
		Data struct {
			Count int `json:"count"`
			List  []struct {
				RoomID    jsonStr `json:"roomid"`
				Title     string  `json:"title"`
				Cover     string  `json:"cover"`
				UserCover string  `json:"user_cover"`
				Uname     string  `json:"uname"`
				Face      string  `json:"face"`
				Online    jsonNum `json:"online"`
			} `json:"list"`
		} `json:"data"`
	}
	if err := getJSON(ctx, api, b.biliHeader(ctx), &r); err != nil {
		return RoomPage{}, err
	}
	if err := r.err(api); err != nil {
		return RoomPage{}, err
	}
	out := RoomPage{HasMore: page*pageSize < r.Data.Count}
	for _, it := range r.Data.List {
		out.Items = append(out.Items, Room{
			RoomID:   string(it.RoomID),
			Title:    it.Title,
			Cover:    biliImg(firstNonEmpty(it.UserCover, it.Cover), "@400w.jpg"),
			UserName: it.Uname,
			Avatar:   biliImg(it.Face, "@100w.jpg"),
			Online:   int64(it.Online),
			Live:     true,
		})
	}
	return out, nil
}

func (b *Bilibili) RecommendRooms(ctx context.Context, page int) (RoomPage, error) {
	const base = "https://api.live.bilibili.com/xlive/web-interface/v1/second/getListByArea"
	params := url.Values{
		"platform":  {"web"},
		"sort":      {"online"},
		"page_size": {"30"},
		"page":      {strconv.Itoa(pageOrFirst(page))},
	}
	var r biliListResp
	if err := b.biliSigned(ctx, base, params, &r); err != nil {
		return RoomPage{}, err
	}
	if err := r.err(base); err != nil {
		return RoomPage{}, err
	}
	rooms := r.rooms()
	// 这个接口没有 has_more，翻到空页为止。
	return RoomPage{HasMore: len(rooms) > 0, Items: rooms}, nil
}

var biliEmTagRe = regexp.MustCompile(`<[^>]*em[^>]*>`)

func (b *Bilibili) SearchRooms(ctx context.Context, keyword string, page int) (RoomPage, error) {
	q := url.Values{
		"context": {""}, "search_type": {"live"}, "cover_type": {"user_cover"},
		"order": {""}, "keyword": {keyword}, "category_id": {""},
		"__refresh__": {""}, "_extra": {""}, "highlight": {"0"}, "single_column": {"0"},
		"page": {strconv.Itoa(pageOrFirst(page))},
	}
	api := "https://api.bilibili.com/x/web-interface/search/type?" + q.Encode()
	var r struct {
		biliEnvelope
		Data struct {
			Result struct {
				LiveRoom []struct {
					RoomID     jsonStr `json:"roomid"`
					Title      string  `json:"title"`
					Cover      string  `json:"cover"`
					UserCover  string  `json:"user_cover"`
					Uname      string  `json:"uname"`
					Uface      string  `json:"uface"`
					Online     jsonNum `json:"online"`
					LiveStatus int     `json:"live_status"`
				} `json:"live_room"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := getJSON(ctx, api, b.biliHeader(ctx), &r); err != nil {
		return RoomPage{}, err
	}
	if err := r.err(api); err != nil {
		return RoomPage{}, err
	}
	var out RoomPage
	for _, it := range r.Data.Result.LiveRoom {
		out.Items = append(out.Items, Room{
			RoomID:   string(it.RoomID),
			Title:    biliEmTagRe.ReplaceAllString(it.Title, ""),
			Cover:    biliImg(firstNonEmpty(it.UserCover, it.Cover), "@400w.jpg"),
			UserName: biliEmTagRe.ReplaceAllString(it.Uname, ""),
			Avatar:   biliImg(it.Uface, "@100w.jpg"),
			Online:   int64(it.Online),
			Live:     it.LiveStatus == 1,
		})
	}
	// 每页最多 40 条，不足一页说明到底了。
	out.HasMore = len(out.Items) >= 40
	return out, nil
}

func (b *Bilibili) SearchAnchors(ctx context.Context, keyword string, page int) (RoomPage, error) {
	page = pageOrFirst(page)
	q := url.Values{
		"context": {""}, "search_type": {"live_user"}, "cover_type": {"user_cover"},
		"order": {""}, "keyword": {keyword}, "category_id": {""},
		"__refresh__": {""}, "_extra": {""}, "highlight": {"0"}, "single_column": {"0"},
		"page": {strconv.Itoa(page)},
	}
	api := "https://api.bilibili.com/x/web-interface/search/type?" + q.Encode()
	var r struct {
		biliEnvelope
		Data struct {
			NumPages int `json:"numPages"`
			Result   []struct {
				RoomID     jsonStr `json:"roomid"`
				Uname      string  `json:"uname"`
				Uface      string  `json:"uface"`
				IsLive     bool    `json:"is_live"`
				Attentions jsonNum `json:"attentions"` // 粉丝数
			} `json:"result"`
		} `json:"data"`
	}
	if err := getJSON(ctx, api, b.biliHeader(ctx), &r); err != nil {
		return RoomPage{}, err
	}
	if err := r.err(api); err != nil {
		return RoomPage{}, err
	}
	out := RoomPage{HasMore: page < r.Data.NumPages}
	for _, it := range r.Data.Result {
		if it.RoomID == "" || it.RoomID == "0" {
			continue
		}
		out.Items = append(out.Items, Room{
			RoomID:   string(it.RoomID),
			UserName: biliEmTagRe.ReplaceAllString(it.Uname, ""),
			Avatar:   biliImg(it.Uface, "@100w.jpg"),
			Online:   int64(it.Attentions),
			Live:     it.IsLive,
		})
	}
	return out, nil
}

func (b *Bilibili) RoomInfo(ctx context.Context, roomID string) (Room, error) {
	const base = "https://api.live.bilibili.com/xlive/web-room/v1/index/getInfoByRoom"
	var r struct {
		biliEnvelope
		Data *struct {
			RoomInfo struct {
				RoomID     jsonStr `json:"room_id"`
				Title      string  `json:"title"`
				Cover      string  `json:"cover"`
				Online     jsonNum `json:"online"`
				LiveStatus int     `json:"live_status"`
			} `json:"room_info"`
			AnchorInfo struct {
				BaseInfo struct {
					Uname string `json:"uname"`
					Face  string `json:"face"`
				} `json:"base_info"`
			} `json:"anchor_info"`
		} `json:"data"`
	}
	if err := b.biliSigned(ctx, base, url.Values{"room_id": {roomID}}, &r); err != nil {
		return Room{}, err
	}
	// 房间不存在时 code=19002000 或 data 为空。
	if r.Code != 0 || r.Data == nil {
		if r.Code == 19002000 || r.Code == 1 || r.Data == nil {
			return Room{}, ErrRoomInvalid
		}
		return Room{}, r.err(base)
	}
	ri := r.Data.RoomInfo
	return Room{
		RoomID:   string(ri.RoomID),
		Title:    ri.Title,
		Cover:    biliImg(ri.Cover, "@400w.jpg"),
		UserName: r.Data.AnchorInfo.BaseInfo.Uname,
		Avatar:   biliImg(r.Data.AnchorInfo.BaseInfo.Face, "@100w.jpg"),
		Online:   int64(ri.Online),
		Live:     ri.LiveStatus == 1,
	}, nil
}

// biliImg 补协议头并加缩放后缀。搜索结果的图片是 //i0.hdslb.com/... 这种协议相对地址，
// 浏览页如果走 https 以外的方式打开会解析错。
func biliImg(u, suffix string) string {
	if u == "" {
		return ""
	}
	if strings.HasPrefix(u, "//") {
		u = "https:" + u
	}
	return u + suffix
}
