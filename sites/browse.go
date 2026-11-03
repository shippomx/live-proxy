package sites

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Browser 浏览直播间：分区、推荐、搜索、查单个房间。与取流（Site）分开，
// 因为只有部分平台实现了浏览；调用方用类型断言判断，不支持的平台在浏览页上就不出现。
//
// 接口与字段取自 dart_simple_live（simple_live_core/lib/src/*_site.dart），
// 各平台列表接口都是公开的网页接口，匿名可用。
type Browser interface {
	ID() string
	Name() string
	Categories(ctx context.Context) ([]Category, error)
	// CategoryRooms 某个二级分区下正在直播的房间。B站的列表接口同时要父分区 ID，
	// 所以传整个 SubCategory 而不是只传 ID。
	CategoryRooms(ctx context.Context, sub SubCategory, page int) (RoomPage, error)
	RecommendRooms(ctx context.Context, page int) (RoomPage, error)
	SearchRooms(ctx context.Context, keyword string, page int) (RoomPage, error)
	// SearchAnchors 按主播名搜索。结果里只有主播名、头像、房间号和是否在播，
	// 没有标题和封面；未开播的主播也会返回，方便先收藏、开播后在电视上看。
	SearchAnchors(ctx context.Context, keyword string, page int) (RoomPage, error)
	// RoomInfo 按房间号查房间。用于"按房间号添加收藏"时拿到主播名和封面；
	// 未开播也要返回信息（Live=false），房间不存在才返回 ErrRoomInvalid。
	RoomInfo(ctx context.Context, roomID string) (Room, error)
}

type Category struct {
	ID       string        `json:"id"`
	Name     string        `json:"name"`
	Children []SubCategory `json:"children"`
}

type SubCategory struct {
	ID       string `json:"id"`
	ParentID string `json:"parent_id"`
	Name     string `json:"name"`
	Pic      string `json:"pic,omitempty"`
}

// Room 列表里的一个直播间。RoomID 必须是能直接拿去 GetStreams 的房间号
// （虎牙用 profileRoom 而非 yyid，B站用长号），否则收藏后生成的播放地址打不开。
type Room struct {
	RoomID   string `json:"room_id"`
	Title    string `json:"title"`
	Cover    string `json:"cover,omitempty"`
	UserName string `json:"user_name"`
	Avatar   string `json:"avatar,omitempty"`
	Online   int64  `json:"online"`
	Live     bool   `json:"live"`
}

type RoomPage struct {
	HasMore bool   `json:"has_more"`
	Items   []Room `json:"items"`
}

// jsonNum 兼容上游把同一个数字字段时而给成数字、时而给成字符串（虎牙的 gid
// 甚至给成 1.0 这样的浮点）。解析失败记为 0，不让一个字段的格式漂移拖垮整页列表。
type jsonNum int64

func (n *jsonNum) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*n = 0
		return nil
	}
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		*n = jsonNum(i)
		return nil
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		*n = jsonNum(int64(f))
		return nil
	}
	*n = 0
	return nil
}

// jsonStr 兼容上游把 ID 类字段时而给成字符串、时而给成数字。
type jsonStr string

func (s *jsonStr) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		*s = jsonStr(v)
		return nil
	}
	if string(b) == "null" {
		*s = ""
		return nil
	}
	// 数字原样保留；浮点形式的整数（1.0）去掉小数部分，否则拼进 URL 就成了无效 ID。
	v := string(b)
	if f, err := strconv.ParseFloat(v, 64); err == nil && f == float64(int64(f)) {
		v = strconv.FormatInt(int64(f), 10)
	}
	*s = jsonStr(v)
	return nil
}

// getJSON 取一个 JSON 接口并解到 out。解析失败归为 ErrParse：
// 上游改版时日志里能直接看出是哪个接口的结构变了。
func getJSON(ctx context.Context, url string, header map[string]string, out any) error {
	body, err := Get(ctx, url, header)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrParse, url, err)
	}
	return nil
}

func pageOrFirst(page int) int {
	if page < 1 {
		return 1
	}
	return page
}
