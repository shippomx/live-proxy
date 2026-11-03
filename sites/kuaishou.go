package sites

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

type Kuaishou struct {
	once sync.Once
	cli  *http.Client
}

func (k *Kuaishou) ID() string   { return "kuaishou" }
func (k *Kuaishou) Name() string { return "快手直播" }

// 快手把流地址塞在房间页的 window.__INITIAL_STATE__ 里，没有独立 JSON 接口。
var ksStateRe = regexp.MustCompile(`window\.__INITIAL_STATE__\s*=\s*(\{.*?\});`)

// client 快手要求同一个会话里带上服务端下发的 did cookie，所以需要独立的
// cookiejar 客户端，不能用包级的无状态 client。
//
// 注意：原 Dart 实现在这之前还会 POST 一条 log-sdk 埋点去"激活" did，
// 声称不激活拿到的 playUrls 会是空的。实测（2026-09-06，房间 kglkgl520）
// 不做那一步同样拿到了完整的 playUrls，所以这里不实现它 —— 少一个
// 无法验证是否生效的伪造埋点请求，也少一处会静默失效的依赖。
func (k *Kuaishou) client() *http.Client {
	k.once.Do(func() {
		jar, _ := cookiejar.New(nil)
		k.cli = &http.Client{
			Timeout: 20 * time.Second,
			Jar:     jar,
		}
	})
	return k.cli
}

type ksRepresentation struct {
	URL         string `json:"url"`
	Name        string `json:"name"`
	QualityType string `json:"qualityType"`
	Level       int    `json:"level"`
	Bitrate     int    `json:"bitrate"`
}

func (k *Kuaishou) GetStreams(ctx context.Context, roomID string) ([]Stream, error) {
	pageURL := "https://live.kuaishou.com/u/" + roomID

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return nil, &UpstreamError{Endpoint: pageURL, Err: err}
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")

	resp, err := k.client().Do(req)
	if err != nil {
		return nil, &UpstreamError{Endpoint: pageURL, Err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &UpstreamError{Endpoint: pageURL, Err: fmt.Errorf("HTTP %d", resp.StatusCode)}
	}
	buf := make([]byte, 0, 256<<10)
	tmp := make([]byte, 32<<10)
	for len(buf) < 4<<20 {
		n, rerr := resp.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if rerr != nil {
			break
		}
	}

	m := ksStateRe.FindSubmatch(buf)
	if m == nil {
		return nil, fmt.Errorf("%w: kuaishou: 页面里没有 __INITIAL_STATE__（可能被风控拦截或页面改版）", ErrParse)
	}
	// 页面里的 JS 字面量含 undefined，不是合法 JSON。
	raw := strings.ReplaceAll(string(m[1]), "undefined", "null")

	var state struct {
		Liveroom *struct {
			PlayList []struct {
				LiveStream *struct {
					ID       string          `json:"id"`
					PlayUrls json.RawMessage `json:"playUrls"`
				} `json:"liveStream"`
			} `json:"playList"`
		} `json:"liveroom"`
	}
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		return nil, fmt.Errorf("%w: kuaishou state: %v", ErrParse, err)
	}
	if state.Liveroom == nil || len(state.Liveroom.PlayList) == 0 {
		return nil, ErrRoomInvalid
	}
	ls := state.Liveroom.PlayList[0].LiveStream
	if ls == nil || len(ls.PlayUrls) == 0 {
		return nil, ErrOffline
	}

	reps, err := ksParsePlayUrls(ls.PlayUrls)
	if err != nil {
		return nil, err
	}
	if len(reps) == 0 {
		return nil, ErrOffline
	}

	// level 越大画质越高。
	sort.SliceStable(reps, func(i, j int) bool { return reps[i].Level > reps[j].Level })

	out := make([]Stream, 0, len(reps))
	for _, r := range reps {
		if r.URL == "" {
			continue
		}
		name := r.Name
		if name == "" {
			name = r.QualityType
		}
		out = append(out, Stream{
			CDN:     cdnNameFromHost(r.URL),
			Quality: name,
			Format:  FormatFLV, // 快手只给 flv，没有 m3u8
			URL:     r.URL,
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: kuaishou: 解析出线路但都没有 URL", ErrParse)
	}
	return out, nil
}

// ksParsePlayUrls 兼容 playUrls 的两种形状：对象 {"h264":{...}} 和数组 [{"h264":{...}}]。
// 快手在不同入口/版本下两种都出现过，只认一种的话另一种会掉进"解析失败"，
// 表现为某些房间永远打不开而另一些正常 —— 很难联想到是形状差异。
func ksParsePlayUrls(raw json.RawMessage) ([]ksRepresentation, error) {
	type codecBlock struct {
		AdaptationSet struct {
			Representation []ksRepresentation `json:"representation"`
		} `json:"adaptationSet"`
	}
	trimmed := strings.TrimLeft(string(raw), " \t\r\n")
	if strings.HasPrefix(trimmed, "[") {
		var arr []map[string]codecBlock
		if err := json.Unmarshal(raw, &arr); err != nil {
			return nil, fmt.Errorf("%w: kuaishou playUrls(数组): %v", ErrParse, err)
		}
		for _, item := range arr {
			if blk, ok := item["h264"]; ok {
				return blk.AdaptationSet.Representation, nil
			}
		}
		return nil, fmt.Errorf("%w: kuaishou playUrls 数组里没有 h264", ErrParse)
	}
	var obj map[string]codecBlock
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("%w: kuaishou playUrls(对象): %v", ErrParse, err)
	}
	blk, ok := obj["h264"]
	if !ok {
		return nil, fmt.Errorf("%w: kuaishou playUrls 里没有 h264", ErrParse)
	}
	return blk.AdaptationSet.Representation, nil
}
