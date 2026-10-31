package sites

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

var _ Browser = (*Douyu)(nil)

func (d *Douyu) Categories(ctx context.Context) ([]Category, error) {
	var r struct {
		Data struct {
			Cate1Info []struct {
				Cate1ID   jsonStr `json:"cate1Id"`
				Cate1Name string  `json:"cate1Name"`
			} `json:"cate1Info"`
			Cate2Info []struct {
				Cate1ID   jsonStr `json:"cate1Id"`
				Cate2ID   jsonStr `json:"cate2Id"`
				Cate2Name string  `json:"cate2Name"`
				Icon      string  `json:"icon"`
			} `json:"cate2Info"`
		} `json:"data"`
	}
	if err := getJSON(ctx, "https://m.douyu.com/api/cate/list", nil, &r); err != nil {
		return nil, err
	}
	byParent := map[string][]SubCategory{}
	for _, s := range r.Data.Cate2Info {
		pid := string(s.Cate1ID)
		byParent[pid] = append(byParent[pid], SubCategory{
			ID: string(s.Cate2ID), ParentID: pid, Name: s.Cate2Name, Pic: s.Icon,
		})
	}
	out := make([]Category, 0, len(r.Data.Cate1Info))
	for _, c := range r.Data.Cate1Info {
		id := string(c.Cate1ID)
		out = append(out, Category{ID: id, Name: c.Cate1Name, Children: byParent[id]})
	}
	// 接口返回的一级分区顺序不稳定，按 ID 排序让页面上的位置固定。
	sort.SliceStable(out, func(i, j int) bool {
		a, _ := strconv.Atoi(out[i].ID)
		b, _ := strconv.Atoi(out[j].ID)
		return a < b
	})
	return out, nil
}

// douyuListResp 分区列表和推荐列表的结构相同。rl 里混有广告/专题位，只有 type=1 是直播间。
type douyuListResp struct {
	Data struct {
		Pgcnt int `json:"pgcnt"`
		Rl    []struct {
			Type int     `json:"type"`
			Rid  jsonStr `json:"rid"`
			Rn   string  `json:"rn"`
			Nn   string  `json:"nn"`
			Av   string  `json:"av"`
			Rs16 string  `json:"rs16"`
			Ol   jsonNum `json:"ol"`
		} `json:"rl"`
	} `json:"data"`
}

func (r douyuListResp) rooms() []Room {
	var out []Room
	for _, it := range r.Data.Rl {
		if it.Type != 1 {
			continue
		}
		out = append(out, Room{
			RoomID:   string(it.Rid),
			Title:    it.Rn,
			Cover:    it.Rs16,
			UserName: it.Nn,
			Avatar:   douyuAvatar(it.Av),
			Online:   int64(it.Ol),
			Live:     true,
		})
	}
	return out
}

func (d *Douyu) CategoryRooms(ctx context.Context, sub SubCategory, page int) (RoomPage, error) {
	page = pageOrFirst(page)
	api := fmt.Sprintf("https://www.douyu.com/gapi/rkc/directory/mixList/2_%s/%d", url.PathEscape(sub.ID), page)
	var r douyuListResp
	if err := getJSON(ctx, api, nil, &r); err != nil {
		return RoomPage{}, err
	}
	return RoomPage{HasMore: page < r.Data.Pgcnt, Items: r.rooms()}, nil
}

func (d *Douyu) RecommendRooms(ctx context.Context, page int) (RoomPage, error) {
	page = pageOrFirst(page)
	api := fmt.Sprintf("https://www.douyu.com/japi/weblist/apinc/allpage/6/%d", page)
	var r douyuListResp
	if err := getJSON(ctx, api, nil, &r); err != nil {
		return RoomPage{}, err
	}
	rooms := r.rooms()
	// 推荐接口的 pgcnt 实测恒为 0（2026-10-31），按它判断会让推荐永远只有一页，改为翻到空页为止。
	return RoomPage{HasMore: len(rooms) > 0, Items: rooms}, nil
}

func (d *Douyu) SearchRooms(ctx context.Context, keyword string, page int) (RoomPage, error) {
	q := url.Values{"kw": {keyword}, "page": {strconv.Itoa(pageOrFirst(page))}, "pageSize": {"20"}}
	api := "https://www.douyu.com/japi/search/api/searchShow?" + q.Encode()
	var r struct {
		Error int    `json:"error"`
		Msg   string `json:"msg"`
		Data  struct {
			RelateShow []struct {
				Rid      jsonStr `json:"rid"`
				RoomName string  `json:"roomName"`
				RoomSrc  string  `json:"roomSrc"`
				NickName string  `json:"nickName"`
				Avatar   string  `json:"avatar"`
				Hot      string  `json:"hot"`
				IsLive   int     `json:"isLive"`
			} `json:"relateShow"`
		} `json:"data"`
	}
	// 搜索接口不带 did Cookie 会返回 error!=0。
	header := map[string]string{
		"Referer": "https://www.douyu.com/search/",
		"Cookie":  "dy_did=" + douyuDid + ";acf_did=" + douyuDid,
	}
	if err := getJSON(ctx, api, header, &r); err != nil {
		return RoomPage{}, err
	}
	if r.Error != 0 {
		return RoomPage{}, &UpstreamError{Endpoint: api, Err: fmt.Errorf("error=%d %s", r.Error, r.Msg)}
	}
	var out RoomPage
	for _, it := range r.Data.RelateShow {
		out.Items = append(out.Items, Room{
			RoomID:   string(it.Rid),
			Title:    it.RoomName,
			Cover:    it.RoomSrc,
			UserName: it.NickName,
			Avatar:   it.Avatar,
			Online:   douyuHotNum(it.Hot),
			Live:     it.IsLive == 1,
		})
	}
	out.HasMore = len(out.Items) > 0
	return out, nil
}

func (d *Douyu) RoomInfo(ctx context.Context, roomID string) (Room, error) {
	body, err := Get(ctx, "https://www.douyu.com/betard/"+url.PathEscape(roomID),
		map[string]string{"Referer": "https://www.douyu.com/" + roomID})
	if err != nil {
		return Room{}, err
	}
	// 房间不存在时 betard 返回 200 + 一张 HTML 错误页，不是 JSON。
	if b := bytes.TrimSpace(body); len(b) > 0 && b[0] == '<' {
		return Room{}, ErrRoomInvalid
	}
	var r struct {
		Room struct {
			RoomID      jsonStr `json:"room_id"`
			RoomName    string  `json:"room_name"`
			OwnerName   string  `json:"owner_name"`
			OwnerAvatar string  `json:"owner_avatar"`
			RoomPic     string  `json:"room_pic"`
			ShowStatus  jsonNum `json:"show_status"`
			VideoLoop   jsonNum `json:"videoLoop"`
			RoomBizAll  struct {
				Hot jsonNum `json:"hot"`
			} `json:"room_biz_all"`
		} `json:"room"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return Room{}, fmt.Errorf("%w: douyu betard: %v", ErrParse, err)
	}
	if r.Room.RoomID == "" || r.Room.RoomID == "0" {
		return Room{}, ErrRoomInvalid
	}
	return Room{
		RoomID:   string(r.Room.RoomID),
		Title:    r.Room.RoomName,
		Cover:    r.Room.RoomPic,
		UserName: r.Room.OwnerName,
		Avatar:   r.Room.OwnerAvatar,
		Online:   int64(r.Room.RoomBizAll.Hot),
		// 轮播录像不算直播，与 GetStreams 的判断一致。
		Live: r.Room.ShowStatus == 1 && r.Room.VideoLoop != 1,
	}, nil
}

// douyuHotNum 搜索结果的热度是"12.3万"这样的文本。
func douyuHotNum(s string) int64 {
	s = strings.TrimSpace(s)
	mul := 1.0
	if rest, ok := strings.CutSuffix(s, "万"); ok {
		mul, s = 10000, rest
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return int64(f * mul)
}

// douyuAvatar 列表里的头像只给了路径片段。
func douyuAvatar(av string) string {
	if av == "" || strings.HasPrefix(av, "http") {
		return av
	}
	return "https://apic.douyucdn.cn/upload/" + av + "_middle.jpg"
}
