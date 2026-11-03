package sites

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

var _ Browser = (*Huya)(nil)

// 虎牙一级分区是写死的四个（与网页导航一致），二级分区按 bussType 拉取。
var huyaTopCategories = []Category{
	{ID: "1", Name: "网游"},
	{ID: "2", Name: "单机"},
	{ID: "8", Name: "娱乐"},
	{ID: "3", Name: "手游"},
}

func (h *Huya) Categories(ctx context.Context) ([]Category, error) {
	out := make([]Category, 0, len(huyaTopCategories))
	for _, top := range huyaTopCategories {
		var r struct {
			Data []struct {
				// gid 有时是数字、有时是 {"value":"1,2"}；后者取第一个。
				Gid          json.RawMessage `json:"gid"`
				GameFullName string          `json:"gameFullName"`
			} `json:"data"`
		}
		api := "https://live.cdn.huya.com/liveconfig/game/bussLive?bussType=" + top.ID
		if err := getJSON(ctx, api, nil, &r); err != nil {
			return nil, err
		}
		c := Category{ID: top.ID, Name: top.Name}
		for _, it := range r.Data {
			gid := huyaGid(it.Gid)
			if gid == "" {
				continue
			}
			c.Children = append(c.Children, SubCategory{
				ID:       gid,
				ParentID: top.ID,
				Name:     it.GameFullName,
				Pic:      "https://huyaimg.msstatic.com/cdnimage/game/" + gid + "-MS.jpg",
			})
		}
		out = append(out, c)
	}
	return out, nil
}

func huyaGid(raw json.RawMessage) string {
	var obj struct {
		Value string `json:"value"`
	}
	if len(raw) > 0 && raw[0] == '{' {
		if json.Unmarshal(raw, &obj) == nil {
			return strings.Split(obj.Value, ",")[0]
		}
		return ""
	}
	var s jsonStr
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return string(s)
}

// huyaListResp cache.php 的 LiveList 接口：推荐和分区列表共用，分区只是多一个 gameId。
type huyaListResp struct {
	Status int `json:"status"`
	Data   struct {
		Page      int `json:"page"`
		TotalPage int `json:"totalPage"`
		Datas     []struct {
			ProfileRoom  jsonStr `json:"profileRoom"`
			Introduction string  `json:"introduction"`
			RoomName     string  `json:"roomName"`
			Screenshot   string  `json:"screenshot"`
			Nick         string  `json:"nick"`
			Avatar180    string  `json:"avatar180"`
			TotalCount   jsonNum `json:"totalCount"`
		} `json:"datas"`
	} `json:"data"`
}

func (h *Huya) liveList(ctx context.Context, gameID string, page int) (RoomPage, error) {
	q := url.Values{
		"m":      {"LiveList"},
		"do":     {"getLiveListByPage"},
		"tagAll": {"0"},
		"page":   {strconv.Itoa(pageOrFirst(page))},
	}
	if gameID != "" {
		q.Set("gameId", gameID)
	}
	var r huyaListResp
	if err := getJSON(ctx, "https://www.huya.com/cache.php?"+q.Encode(), nil, &r); err != nil {
		return RoomPage{}, err
	}
	if r.Status != 200 {
		return RoomPage{}, fmt.Errorf("%w: huya LiveList status=%d", ErrUpstream, r.Status)
	}
	out := RoomPage{HasMore: r.Data.Page < r.Data.TotalPage}
	for _, it := range r.Data.Datas {
		out.Items = append(out.Items, Room{
			RoomID:   string(it.ProfileRoom),
			Title:    firstNonEmpty(it.Introduction, it.RoomName),
			Cover:    huyaCover(it.Screenshot),
			UserName: it.Nick,
			Avatar:   it.Avatar180,
			Online:   int64(it.TotalCount),
			Live:     true,
		})
	}
	return out, nil
}

func (h *Huya) CategoryRooms(ctx context.Context, sub SubCategory, page int) (RoomPage, error) {
	return h.liveList(ctx, sub.ID, page)
}

func (h *Huya) RecommendRooms(ctx context.Context, page int) (RoomPage, error) {
	return h.liveList(ctx, "", page)
}

func (h *Huya) SearchRooms(ctx context.Context, keyword string, page int) (RoomPage, error) {
	const rows = 20
	page = pageOrFirst(page)
	q := url.Values{
		"m": {"Search"}, "do": {"getSearchContent"}, "q": {keyword},
		"uid": {"0"}, "v": {"4"}, "typ": {"-5"}, "livestate": {"0"},
		"rows": {strconv.Itoa(rows)}, "start": {strconv.Itoa((page - 1) * rows)},
	}
	// response 里 "1" 是主播、"3" 是直播间；只取直播间，主播结果没有可直接播放的房间号。
	// 其他键的值有时是数组而不是对象，所以先按 RawMessage 收下，只解析 "3"。
	var r struct {
		Response map[string]json.RawMessage `json:"response"`
	}
	if err := getJSON(ctx, "https://search.cdn.huya.com/?"+q.Encode(), nil, &r); err != nil {
		return RoomPage{}, err
	}
	var rooms struct {
		NumFound int `json:"numFound"`
		Docs     []struct {
			RoomID         jsonStr `json:"room_id"`
			GameIntro      string  `json:"game_introduction"`
			GameRoomName   string  `json:"game_roomName"`
			GameScreenshot string  `json:"game_screenshot"`
			GameNick       string  `json:"game_nick"`
			GameTotalCount jsonNum `json:"game_total_count"`
			GameAvatarURL  string  `json:"game_avatarUrl180"`
		} `json:"docs"`
	}
	if raw := r.Response["3"]; len(raw) > 0 && raw[0] == '{' {
		if err := json.Unmarshal(raw, &rooms); err != nil {
			return RoomPage{}, fmt.Errorf("%w: huya search: %v", ErrParse, err)
		}
	}
	out := RoomPage{HasMore: rooms.NumFound > page*rows}
	for _, it := range rooms.Docs {
		out.Items = append(out.Items, Room{
			RoomID:   string(it.RoomID),
			Title:    firstNonEmpty(it.GameIntro, it.GameRoomName),
			Cover:    huyaCover(it.GameScreenshot),
			UserName: it.GameNick,
			Avatar:   it.GameAvatarURL,
			Online:   int64(it.GameTotalCount),
			Live:     true,
		})
	}
	return out, nil
}

func (h *Huya) SearchAnchors(ctx context.Context, keyword string, page int) (RoomPage, error) {
	const rows = 20
	page = pageOrFirst(page)
	q := url.Values{
		"m": {"Search"}, "do": {"getSearchContent"}, "q": {keyword},
		"uid": {"0"}, "v": {"1"}, "typ": {"-5"}, "livestate": {"0"},
		"rows": {strconv.Itoa(rows)}, "start": {strconv.Itoa((page - 1) * rows)},
	}
	var r struct {
		Response map[string]json.RawMessage `json:"response"`
	}
	if err := getJSON(ctx, "https://search.cdn.huya.com/?"+q.Encode(), nil, &r); err != nil {
		return RoomPage{}, err
	}
	var users struct {
		NumFound int `json:"numFound"`
		Docs     []struct {
			RoomID     jsonStr `json:"room_id"`
			GameNick   string  `json:"game_nick"`
			GameAvatar string  `json:"game_avatarUrl180"`
			GameLiveOn bool    `json:"gameLiveOn"`
			Activity   jsonNum `json:"game_activityCount"` // 订阅数
		} `json:"docs"`
	}
	if raw := r.Response["1"]; len(raw) > 0 && raw[0] == '{' {
		if err := json.Unmarshal(raw, &users); err != nil {
			return RoomPage{}, fmt.Errorf("%w: huya search user: %v", ErrParse, err)
		}
	}
	out := RoomPage{HasMore: users.NumFound > page*rows}
	for _, it := range users.Docs {
		if it.RoomID == "" || it.RoomID == "0" {
			continue // 没开过直播间的用户，没有可播放的房间号
		}
		out.Items = append(out.Items, Room{
			RoomID: string(it.RoomID), UserName: it.GameNick, Avatar: it.GameAvatar,
			Online: int64(it.Activity), Live: it.GameLiveOn,
		})
	}
	return out, nil
}

// RoomInfo 复用取流用的 profileRoom 接口：同一个请求里就有主播名、标题和开播状态。
func (h *Huya) RoomInfo(ctx context.Context, roomID string) (Room, error) {
	api := "https://mp.huya.com/cache.php?m=Live&do=profileRoom&showSecret=1&roomid=" + url.QueryEscape(roomID)
	var r huyaResp
	if err := getJSON(ctx, api, map[string]string{"Referer": "https://www.huya.com/"}, &r); err != nil {
		return Room{}, err
	}
	// 与 GetStreams 相同：房间不存在时 data 是空数组而不是报错。
	if r.Status != 200 || len(r.Data) == 0 || r.Data[0] == '[' {
		return Room{}, ErrRoomInvalid
	}
	var d struct {
		LiveStatus  string `json:"liveStatus"`
		ProfileInfo struct {
			Nick        string  `json:"nick"`
			Avatar180   string  `json:"avatar180"`
			ProfileRoom jsonStr `json:"profileRoom"`
		} `json:"profileInfo"`
		LiveData struct {
			Introduction string  `json:"introduction"`
			RoomName     string  `json:"roomName"`
			Screenshot   string  `json:"screenshot"`
			TotalCount   jsonNum `json:"totalCount"`
		} `json:"liveData"`
	}
	if err := json.Unmarshal(r.Data, &d); err != nil {
		return Room{}, fmt.Errorf("%w: huya profileRoom: %v", ErrParse, err)
	}
	id := string(d.ProfileInfo.ProfileRoom)
	if id == "" {
		id = roomID
	}
	return Room{
		RoomID:   id,
		Title:    firstNonEmpty(d.LiveData.Introduction, d.LiveData.RoomName),
		Cover:    huyaCover(d.LiveData.Screenshot),
		UserName: d.ProfileInfo.Nick,
		Avatar:   d.ProfileInfo.Avatar180,
		Online:   int64(d.LiveData.TotalCount),
		Live:     d.LiveStatus == "ON",
	}, nil
}

// huyaCover 截图地址不带参数时是原图（几百 KB），列表页要加缩放参数。
func huyaCover(s string) string {
	if s == "" || strings.Contains(s, "?") {
		return s
	}
	return s + "?x-oss-process=style/w338_h190&"
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}
