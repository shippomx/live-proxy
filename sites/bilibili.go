package sites

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
)

type Bilibili struct {
	// Cookie 可选。匿名可取流，但服务端会把清晰度裁到超清(250)；
	// 要原画(10000)必须带登录 Cookie（SESSDATA + bili_jct + buvid3）。
	// 运行中会被扫码登录替换，取流与登录在不同 goroutine，读写一律经过 SetCookie/cookie。
	Cookie string
	mu     sync.RWMutex
}

func (b *Bilibili) SetCookie(c string) {
	b.mu.Lock()
	b.Cookie = c
	b.mu.Unlock()
}

func (b *Bilibili) cookie() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.Cookie
}

func (b *Bilibili) ID() string   { return "bilibili" }
func (b *Bilibili) Name() string { return "哔哩哔哩直播" }

type biliPlayResp struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		LiveStatus  int `json:"live_status"` // 0=未开播 1=直播中 2=轮播
		RoomID      int `json:"room_id"`
		PlayurlInfo *struct {
			Playurl struct {
				GQnDesc []struct {
					Qn   int    `json:"qn"`
					Desc string `json:"desc"`
				} `json:"g_qn_desc"`
				Stream []struct {
					ProtocolName string `json:"protocol_name"`
					Format       []struct {
						FormatName string `json:"format_name"`
						Codec      []struct {
							CodecName string `json:"codec_name"`
							CurrentQn int    `json:"current_qn"`
							AcceptQn  []int  `json:"accept_qn"`
							BaseURL   string `json:"base_url"`
							URLInfo   []struct {
								Host  string `json:"host"`
								Extra string `json:"extra"`
							} `json:"url_info"`
						} `json:"codec"`
					} `json:"format"`
				} `json:"stream"`
			} `json:"playurl"`
		} `json:"playurl_info"`
	} `json:"data"`
}

// 请求原画。服务端对匿名用户会静默降级（accept_qn 里明明有 10000，
// 返回的 current_qn 却是 250），所以拿到结果后必须回读 current_qn，
// 按实际清晰度标注 —— 否则界面上写着"原画"，播的却是超清，无从发现。
const biliWantQn = 10000

func (b *Bilibili) GetStreams(ctx context.Context, roomID string) ([]Stream, error) {
	q := url.Values{}
	q.Set("room_id", roomID)
	q.Set("protocol", "0,1") // 0=flv 1=hls
	q.Set("format", "0,1,2") // 0=flv 1=ts 2=fmp4
	q.Set("codec", "0")      // 0=avc，盒子解码器对 hevc 支持参差，只要 avc
	q.Set("platform", "web") // html5 会让服务端偏向只给 hls，web 才有 flv
	q.Set("qn", strconv.Itoa(biliWantQn))

	api := "https://api.live.bilibili.com/xlive/web-room/v2/index/getRoomPlayInfo?" + q.Encode()
	header := map[string]string{"Referer": "https://live.bilibili.com/", "Accept": "application/json"}
	if c := b.cookie(); c != "" {
		header["Cookie"] = c
	}

	body, err := Get(ctx, api, header)
	if err != nil {
		return nil, err
	}
	var r biliPlayResp
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("%w: bilibili: %v", ErrParse, err)
	}
	if r.Code != 0 {
		// 60004 = 房间不存在。其余非零码原样带出，便于对着 B 站文档查。
		if r.Code == 60004 {
			return nil, ErrRoomInvalid
		}
		return nil, fmt.Errorf("%w: bilibili code=%d msg=%s", ErrUpstream, r.Code, r.Message)
	}
	// 未开播(0)和轮播(2)都没有真实流。轮播放的是录播，播出来会让人以为在直播，
	// 所以一并按未开播处理，而不是给一个"能播但不是直播"的地址。
	if r.Data.LiveStatus != 1 || r.Data.PlayurlInfo == nil {
		return nil, ErrOffline
	}

	qnDesc := map[int]string{}
	for _, g := range r.Data.PlayurlInfo.Playurl.GQnDesc {
		qnDesc[g.Qn] = g.Desc
	}

	var out []Stream
	for _, s := range r.Data.PlayurlInfo.Playurl.Stream {
		for _, f := range s.Format {
			for _, c := range f.Codec {
				format := FormatFLV
				if f.FormatName != "flv" {
					format = FormatHLS
				}
				quality := qnDesc[c.CurrentQn]
				if quality == "" {
					quality = "qn" + strconv.Itoa(c.CurrentQn)
				}
				// 如实标注被降级的事实，不要让调用方以为拿到了原画。
				if c.CurrentQn != biliWantQn {
					quality += fmt.Sprintf("(已降级,请求%d)", biliWantQn)
				}
				for _, u := range c.URLInfo {
					out = append(out, Stream{
						CDN:     cdnNameFromHost(u.Host),
						Quality: quality,
						Format:  format,
						URL:     u.Host + c.BaseURL + u.Extra,
					})
				}
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: bilibili: 直播中但无可用线路", ErrParse)
	}

	// mcdn 是 B 站的 P2P 回源节点，直连稳定性差于普通 CDN，排到后面。
	// 用 SliceStable + 合法的严格弱序，避免原实现里那个只看 a 不看 b 的比较器。
	sort.SliceStable(out, func(i, j int) bool {
		return !strings.Contains(out[i].URL, "mcdn") && strings.Contains(out[j].URL, "mcdn")
	})
	// flv 排在前：盒子播放器对 flv 的兼容性普遍好于 fmp4。
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Format == FormatFLV && out[j].Format != FormatFLV
	})
	return out, nil
}

// cdnNameFromHost 从 CDN 主机名里提取可读标识，用于线路选择与排障。
func cdnNameFromHost(host string) string {
	h := strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://")
	if i := strings.Index(h, "."); i > 0 {
		return h[:i]
	}
	return h
}
