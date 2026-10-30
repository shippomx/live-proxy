package sites

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
)

type NeteaseCC struct{}

func (c *NeteaseCC) ID() string   { return "cc" }
func (c *NeteaseCC) Name() string { return "网易CC直播" }

// hs 线路的拉流域名。CC 在 stream_list 里只给鉴权参数不给域名，域名要自己拼。
//
// 只登记 hs：实测（2026-09-06，房间 5200）hs 的 high/medium/low 三档均 200 可播；
// ali 走 alipullhls 返回 403，fws 的域名没试出来。没验证过的线路不写进这张表 ——
// 写进去的后果是用户点了"高清"拿到一个 403，而错误显示为"上游拉流失败"，
// 查起来会往 CC 服务端故障的方向去，而不是往"我们域名拼错了"。
var ccPullHosts = map[string]string{
	"hs": "http://hspullhls.cc.netease.com/live/",
}

// ccQualityName 档位中文名。CC 返回的是英文 key。
var ccQualityName = map[string]string{
	"original": "原画",
	"high":     "高清",
	"medium":   "标清",
	"low":      "流畅",
}

type ccAnchorResp struct {
	Code string                     `json:"code"`
	Data map[string]json.RawMessage `json:"data"`
}

type ccChannelResp struct {
	Data []struct {
		Cid        int    `json:"cid"`
		Title      string `json:"title"`
		M3u8       string `json:"m3u8"`
		StreamList map[string]struct {
			Vbr        int               `json:"vbr"`
			Streamname string            `json:"streamname"`
			CdnFmt     map[string]string `json:"CDN_FMT"`
		} `json:"stream_list"`
	} `json:"data"`
}

func (c *NeteaseCC) GetStreams(ctx context.Context, roomID string) ([]Stream, error) {
	// 第一步：主播 ccid → channel_id。CC 的房间号（ccid）和频道号（cid）是两个东西，
	// 拉流只认 cid。
	api := "https://api.cc.163.com/v1/activitylives/anchor/lives?anchor_ccid=" + roomID
	body, err := Get(ctx, api, nil)
	if err != nil {
		return nil, err
	}
	var ar ccAnchorResp
	if err := json.Unmarshal(body, &ar); err != nil {
		return nil, fmt.Errorf("%w: cc anchor: %v", ErrParse, err)
	}
	raw, ok := ar.Data[roomID]
	if !ok || len(raw) == 0 {
		// 主播不在播时 data 里就没有这个 ccid 的条目，接口本身返回 code:OK。
		return nil, ErrOffline
	}
	var anchor struct {
		ChannelID int `json:"channel_id"`
	}
	if err := json.Unmarshal(raw, &anchor); err != nil {
		return nil, fmt.Errorf("%w: cc anchor entry: %v", ErrParse, err)
	}
	if anchor.ChannelID == 0 {
		return nil, ErrOffline
	}

	// 第二步：channel 详情里带各档清晰度的鉴权参数。
	chAPI := fmt.Sprintf("https://cc.163.com/live/channel/?channelids=%d", anchor.ChannelID)
	body, err = Get(ctx, chAPI, nil)
	if err != nil {
		return nil, err
	}
	var cr ccChannelResp
	if err := json.Unmarshal(body, &cr); err != nil {
		return nil, fmt.Errorf("%w: cc channel: %v", ErrParse, err)
	}
	if len(cr.Data) == 0 {
		return nil, ErrOffline
	}
	ch := cr.Data[0]

	type entry struct {
		Stream
		vbr int
	}
	var entries []entry
	for key, s := range ch.StreamList {
		if s.Streamname == "" {
			continue
		}
		name := ccQualityName[key]
		if name == "" {
			name = key
		}
		for cdn, host := range ccPullHosts {
			auth, ok := s.CdnFmt[cdn]
			if !ok {
				continue
			}
			entries = append(entries, entry{
				Stream: Stream{
					CDN:     cdn,
					Quality: name,
					Format:  FormatHLS, // CC 只给 HLS
					URL:     fmt.Sprintf("%s%s.m3u8?%s", host, s.Streamname, auth),
				},
				vbr: s.Vbr,
			})
		}
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].vbr > entries[j].vbr })

	out := make([]Stream, 0, len(entries)+1)
	for _, e := range entries {
		out = append(out, e.Stream)
	}

	// 兜底：接口直接给的 m3u8 是一个 302 跳转地址，由服务端决定落到哪一档
	// （实测落在 medium）。它不能选清晰度，所以排在最后而不是最前，
	// 但保留着 —— 当上面那张域名表因 CC 改版而全部失效时，它是唯一还能播的。
	if ch.M3u8 != "" {
		out = append(out, Stream{
			CDN:     "redirect",
			Quality: "服务端选档",
			Format:  FormatHLS,
			URL:     ch.M3u8,
		})
	}
	if len(out) == 0 {
		return nil, ErrOffline
	}
	return out, nil
}
