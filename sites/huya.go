package sites

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Huya struct{}

func (h *Huya) ID() string   { return "huya" }
func (h *Huya) Name() string { return "虎牙直播" }

type huyaResp struct {
	Status int             `json:"status"`
	Data   json.RawMessage `json:"data"`
}

type huyaData struct {
	LiveStatus string `json:"liveStatus"` // ON / OFF / REPLAY
	Stream     *struct {
		BaseSteamInfoList []struct {
			SCdnType      string `json:"sCdnType"`
			LPresenterUid int64  `json:"lPresenterUid"`
			SStreamName   string `json:"sStreamName"`
			SFlvUrl       string `json:"sFlvUrl"`
			SFlvUrlSuffix string `json:"sFlvUrlSuffix"`
			SFlvAntiCode  string `json:"sFlvAntiCode"`
			SHlsUrl       string `json:"sHlsUrl"`
			SHlsUrlSuffix string `json:"sHlsUrlSuffix"`
			SHlsAntiCode  string `json:"sHlsAntiCode"`
		} `json:"baseSteamInfoList"`
	} `json:"stream"`
}

func (h *Huya) GetStreams(ctx context.Context, roomID string) ([]Stream, error) {
	api := "https://mp.huya.com/cache.php?m=Live&do=profileRoom&showSecret=1&roomid=" + url.QueryEscape(roomID)
	body, err := Get(ctx, api, map[string]string{
		"Origin":  "https://www.huya.com",
		"Referer": "https://www.huya.com/",
		"Accept":  "*/*",
	})
	if err != nil {
		return nil, err
	}

	var r huyaResp
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("%w: huya: %v", ErrParse, err)
	}
	if r.Status != 200 {
		return nil, fmt.Errorf("%w: huya status=%d", ErrRoomInvalid, r.Status)
	}
	// 房间号不存在时虎牙不报错，而是把 data 从对象换成空数组 []，status 仍是 200。
	// 不显式识别这个形状，它就会掉进 json 解析失败，被误报成"平台接口已变更"(502)，
	// 把一个用户输错房间号的问题伪装成服务端故障。
	if len(r.Data) == 0 || r.Data[0] == '[' {
		return nil, ErrRoomInvalid
	}

	var d huyaData
	if err := json.Unmarshal(r.Data, &d); err != nil {
		return nil, fmt.Errorf("%w: huya data: %v", ErrParse, err)
	}
	// stream 为 null 是虎牙表达"未开播"的方式，它仍然返回 status=200。
	// 必须显式区分，否则未开播会被当成解析失败去重试。
	if d.Stream == nil || len(d.Stream.BaseSteamInfoList) == 0 {
		return nil, ErrOffline
	}

	var out []Stream
	for _, b := range d.Stream.BaseSteamInfoList {
		if b.SFlvUrl == "" || b.SStreamName == "" {
			continue
		}
		anti, err := huyaBuildAntiCode(b.SStreamName, b.LPresenterUid, b.SFlvAntiCode)
		if err != nil {
			return nil, fmt.Errorf("%w: huya anticode: %v", ErrParse, err)
		}
		suffix := b.SFlvUrlSuffix
		if suffix == "" {
			suffix = "flv"
		}
		out = append(out, Stream{
			CDN:     b.SCdnType,
			Quality: "原画",
			Format:  FormatFLV,
			URL:     fmt.Sprintf("%s/%s.%s?%s&codec=264", b.SFlvUrl, b.SStreamName, suffix, anti),
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: huya: 无可用线路", ErrParse)
	}
	return out, nil
}

// rotl64 低 32 位循环左移 8 位，高位保留。虎牙用它把主播 uid 变换成 URL 里的 u 参数。
// 这个变换必须与服务端一致，错了不会报错，只会拿到 403。
func rotl64(t int64) int64 {
	low := t & 0xFFFFFFFF
	rot := ((low << 8) | (low >> 24)) & 0xFFFFFFFF
	return (t &^ 0xFFFFFFFF) | rot
}

// huyaBuildAntiCode 重算防盗链签名。
// 服务端下发的 sFlvAntiCode 里的 wsSecret 是给网页播放器用的，直接拿来播会 403；
// 真正可用的密钥前缀藏在 fm 字段（base64）里，需要用它、主播 uid 和时间戳重新算一遍 MD5。
func huyaBuildAntiCode(streamName string, presenterUid int64, antiCode string) (string, error) {
	q, err := url.ParseQuery(antiCode)
	if err != nil {
		return "", err
	}
	fmRaw := q.Get("fm")
	if fmRaw == "" {
		// 没有 fm 说明这条线路不需要重签（少见），原样返回。
		return antiCode, nil
	}
	decoded, err := url.QueryUnescape(fmRaw)
	if err != nil {
		return "", err
	}
	fmBytes, err := base64.StdEncoding.DecodeString(decoded)
	if err != nil {
		return "", fmt.Errorf("fm base64: %w", err)
	}
	secretPrefix, _, _ := strings.Cut(string(fmBytes), "_")

	ctype := q.Get("ctype")
	if ctype == "" {
		ctype = "huya_pc_exe"
	}
	platformID, _ := strconv.Atoi(q.Get("t"))
	wsTime := q.Get("wsTime")

	seqID := presenterUid + time.Now().UnixMilli()
	secretHash := md5hex(fmt.Sprintf("%d|%s|%d", seqID, ctype, platformID))

	convertUid := rotl64(presenterUid)
	// t=103 是 wap 端，它不做 uid 变换且参数名不同。分支写错不会报错，只会 403。
	isWap := platformID == 103
	calcUid := convertUid
	if isWap {
		calcUid = presenterUid
	}
	wsSecret := md5hex(fmt.Sprintf("%s_%d_%s_%s_%s", secretPrefix, calcUid, streamName, secretHash, wsTime))

	v := url.Values{}
	v.Set("wsSecret", wsSecret)
	v.Set("wsTime", wsTime)
	v.Set("seqid", strconv.FormatInt(seqID, 10))
	v.Set("ctype", ctype)
	v.Set("ver", "1")
	v.Set("fs", q.Get("fs"))
	v.Set("fm", fmRaw)
	v.Set("t", strconv.Itoa(platformID))
	if isWap {
		wsTimeInt, _ := strconv.ParseInt(wsTime, 16, 64)
		ct := float64(wsTimeInt+rand.Int63n(2)) * 1000
		v.Set("uid", strconv.FormatInt(presenterUid, 10))
		v.Set("uuid", strconv.FormatInt(int64(ct)%0xFFFFFFFF, 10))
	} else {
		v.Set("u", strconv.FormatInt(convertUid, 10))
	}
	return v.Encode(), nil
}

func md5hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}
