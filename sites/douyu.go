package sites

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dop251/goja"
)

type Douyu struct {
	mu    sync.Mutex
	cache map[string]douyuScript // key: 真实 room_id

	// 登录 Cookie，可选。匿名请求原画会被静默降级成高清，要原画必须登录。
	// 运行中会被扫码登录替换，读写经过 SetCookie/loginCookie。
	ckMu   sync.RWMutex
	cookie string

	// 微信扫码登录进行中的会话，key 为 QRCode.Key。
	wxMu sync.Mutex
	wx   map[string]*douyuWxSession
}

func (d *Douyu) SetCookie(c string) {
	d.ckMu.Lock()
	d.cookie = c
	d.ckMu.Unlock()
}

func (d *Douyu) loginCookie() string {
	d.ckMu.RLock()
	defer d.ckMu.RUnlock()
	return d.cookie
}

type douyuScript struct {
	js      string
	fetched time.Time
}

func (d *Douyu) ID() string   { return "douyu" }
func (d *Douyu) Name() string { return "斗鱼直播" }

// 斗鱼签名用的设备号是源码里的固定常量，不是随机值 —— 换成随机值签名不通过。
const douyuDid = "10000000000000000000000000001501"

// 斗鱼每天重新生成一份混淆过的签名 JS（里面连日期都编进了常量），
// 所以脚本可以按时间缓存，但**签名本身绝不能缓存** —— 它含时间戳，有时效。
const douyuScriptTTL = 30 * time.Minute

// goja 执行的是斗鱼每天下发的混淆代码。必须给硬超时：
// 没有超时的话，对方哪天下发一段死循环，这个 goroutine 会永久占住不释放，
// 而进程从外面看完全正常。
const douyuJSTimeout = 10 * time.Second

type douyuRate struct {
	Name string `json:"name"`
	Rate int    `json:"rate"`
	Bit  int    `json:"bit"`
}

type douyuPlayData struct {
	RtmpURL    string      `json:"rtmp_url"`
	RtmpLive   string      `json:"rtmp_live"`
	RtmpCdn    string      `json:"rtmp_cdn"`
	Rate       int         `json:"rate"`
	Multirates []douyuRate `json:"multirates"`
}

// url 拼出完整播放地址。rtmp_live 里的 & 有时被转义成 &amp;，
// 不反转义会让 URL 从第二个查询参数起全部作废，表现为 403 而不是解析报错。
func (p douyuPlayData) url() string {
	if p.RtmpURL == "" || p.RtmpLive == "" {
		return ""
	}
	return p.RtmpURL + "/" + html.UnescapeString(p.RtmpLive)
}

func (p douyuPlayData) qualityName() string {
	for _, r := range p.Multirates {
		if r.Rate == p.Rate {
			return r.Name
		}
	}
	return "rate" + strconv.Itoa(p.Rate)
}

// upgradeRates 返回比当前档位码率更高的档位，按码率从高到低排序。
//
// rate=0 是"原画"（实测名称如"原画1080P60"，码率在列表里最高），不是"自动"——
// 早先按"自动"跳过了它，于是登录后也永远拿不到原画。
func (p douyuPlayData) upgradeRates() []int {
	cur := -1
	for _, r := range p.Multirates {
		if r.Rate == p.Rate {
			cur = r.Bit
		}
	}
	rs := make([]douyuRate, 0, len(p.Multirates))
	for _, r := range p.Multirates {
		if r.Bit > cur {
			rs = append(rs, r)
		}
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].Bit > rs[j].Bit })
	out := make([]int, len(rs))
	for i, r := range rs {
		out[i] = r.Rate
	}
	return out
}

type douyuPlayResp struct {
	Error int           `json:"error"`
	Msg   string        `json:"msg"`
	Data  douyuPlayData `json:"data"`
}

func (d *Douyu) GetStreams(ctx context.Context, roomID string) ([]Stream, error) {
	referer := "https://www.douyu.com/" + roomID

	// 第一步：短号 → 真实 room_id。签名必须用真实 room_id，
	// 而后续 POST 的路径用的是入参（可能是短号）。两者在短号房间下不同，
	// "顺手统一"成一个会让签名通过但取流失败，且错误信息毫无指向。
	body, err := Get(ctx, "https://www.douyu.com/betard/"+roomID, map[string]string{"Referer": referer})
	if err != nil {
		return nil, err
	}
	var betard struct {
		Room struct {
			RoomID     json.Number `json:"room_id"`
			ShowStatus int         `json:"show_status"` // 1=开播
			VideoLoop  int         `json:"videoLoop"`   // 1=轮播录像
		} `json:"room"`
	}
	if err := json.Unmarshal(body, &betard); err != nil {
		return nil, fmt.Errorf("%w: douyu betard: %v", ErrParse, err)
	}
	realRid := betard.Room.RoomID.String()
	if realRid == "" || realRid == "0" {
		return nil, ErrRoomInvalid
	}
	// 轮播放的是录像。当直播播出去会让人以为主播在线，所以和未开播同等处理。
	if betard.Room.ShowStatus != 1 || betard.Room.VideoLoop == 1 {
		return nil, ErrOffline
	}

	js, err := d.script(ctx, realRid, referer)
	if err != nil {
		return nil, err
	}
	args, err := douyuSign(js, realRid, douyuDid, time.Now().Unix())
	if err != nil {
		return nil, err
	}

	// 第二步：拿默认档 + 清晰度列表。rate=-1 表示"给我默认档"。
	playAPI := "https://www.douyu.com/lapi/live/getH5Play/" + roomID
	header := map[string]string{"Referer": referer}
	if c := d.loginCookie(); c != "" {
		header["Cookie"] = c
	}
	body, err = PostForm(ctx, playAPI, args+"&cdn=&rate=-1&ver=Douyu_223061205&iar=1&ive=1&hevc=0&fa=0", header)
	if err != nil {
		return nil, err
	}
	var meta douyuPlayResp
	if err := json.Unmarshal(body, &meta); err != nil {
		return nil, fmt.Errorf("%w: douyu getH5Play: %v", ErrParse, err)
	}
	if meta.Error != 0 {
		return nil, fmt.Errorf("%w: douyu error=%d msg=%s", ErrUpstream, meta.Error, meta.Msg)
	}

	var out []Stream
	if u := meta.Data.url(); u != "" {
		out = append(out, Stream{
			CDN:     meta.Data.RtmpCdn,
			Quality: meta.Data.qualityName(),
			Format:  FormatFLV,
			URL:     u,
		})
	}

	// 第三步：往高档升。只补一条最高可得的线路，不遍历"档位×CDN"的全组合 ——
	// 盒子上实际只会播一条，把请求数放大 N 倍只会更容易触发风控。
	// 未登录时请求原画会被静默降级成高清（返回的 rate 与请求的不同），
	// 所以按码率从高到低试，最多试两档：匿名时落到蓝光/超清，登录后拿到原画。
	for i, want := range meta.Data.upgradeRates() {
		if i >= 2 {
			break
		}
		body, err = PostForm(ctx, playAPI, fmt.Sprintf("%s&cdn=&rate=%d", args, want), header)
		if err != nil {
			break
		}
		var hi douyuPlayResp
		if json.Unmarshal(body, &hi) != nil || hi.Error != 0 || hi.Data.Rate != want {
			continue
		}
		if u := hi.Data.url(); u != "" {
			// 最高档排在最前。
			out = append([]Stream{{
				CDN:     hi.Data.RtmpCdn,
				Quality: hi.Data.qualityName(),
				Format:  FormatFLV,
				URL:     u,
			}}, out...)
			break
		}
	}
	// 升档失败不致命：默认档已经在手，返回它比整个请求失败好。
	// 这不是静默降级 —— 返回结果里的 Quality 字段如实写着实际档位，
	// 调用方看得见自己拿到的不是最高清晰度。

	if len(out) == 0 {
		return nil, fmt.Errorf("%w: douyu: 签名通过但没有可用流地址", ErrParse)
	}
	return out, nil
}

// script 取当天的签名 JS，按 TTL 缓存。
func (d *Douyu) script(ctx context.Context, realRid, referer string) (string, error) {
	d.mu.Lock()
	if s, ok := d.cache[realRid]; ok && time.Since(s.fetched) < douyuScriptTTL {
		d.mu.Unlock()
		return s.js, nil
	}
	d.mu.Unlock()

	body, err := Get(ctx, "https://www.douyu.com/swf_api/homeH5Enc?rids="+realRid, map[string]string{"Referer": referer})
	if err != nil {
		return "", err
	}
	var enc struct {
		Error int               `json:"error"`
		Data  map[string]string `json:"data"`
	}
	if err := json.Unmarshal(body, &enc); err != nil {
		return "", fmt.Errorf("%w: douyu homeH5Enc: %v", ErrParse, err)
	}
	js, ok := enc.Data["room"+realRid]
	if !ok || js == "" {
		return "", fmt.Errorf("%w: douyu: homeH5Enc 未返回 room%s 的签名脚本", ErrParse, realRid)
	}

	d.mu.Lock()
	if d.cache == nil {
		d.cache = map[string]douyuScript{}
	}
	d.cache[realRid] = douyuScript{js: js, fetched: time.Now()}
	d.mu.Unlock()
	return js, nil
}

// douyuSign 在 JS 引擎里执行斗鱼下发的 ub98484234，产出签名串。
//
// 为什么必须跑 JS：这段脚本是一层自解密 VM（常量表 → 位运算 → TEA 解密 →
// eval 出真正的签名函数），而且斗鱼**每天重新生成**它 —— TEA 常量、轮数表、
// 运算次序都会变。今天把它翻译成 Go，明天就签不出来，
// 且失败形式是"本地算得出签名但服务端不认"，没有任何报错指向根因。
//
// 但它不需要浏览器：整段脚本里没有 window/document/navigator，
// 外部依赖只有 CryptoJS.MD5 一个函数，打个五行的桩就够，不必打包 crypto-js。
func douyuSign(js, rid, did string, tt int64) (string, error) {
	vm := goja.New()

	timer := time.AfterFunc(douyuJSTimeout, func() {
		vm.Interrupt("douyu 签名脚本执行超时")
	})
	defer timer.Stop()

	if err := vm.Set("__goMd5", func(call goja.FunctionCall) goja.Value {
		return vm.ToValue(md5hex(call.Argument(0).String()))
	}); err != nil {
		return "", fmt.Errorf("%w: douyu: 注入 md5 失败: %v", ErrParse, err)
	}
	// 脚本只用到 CryptoJS.MD5(x).toString()，桩到这个形状即可。
	const cryptoStub = `var CryptoJS = { MD5: function (s) { return { toString: function () { return __goMd5(s); } }; } };`
	if _, err := vm.RunString(cryptoStub); err != nil {
		return "", fmt.Errorf("%w: douyu: 注入 CryptoJS 桩失败: %v", ErrParse, err)
	}
	if _, err := vm.RunString(js); err != nil {
		return "", fmt.Errorf("%w: douyu: 执行签名脚本失败: %v", ErrParse, err)
	}

	fn, err := vm.RunString("ub98484234")
	if err != nil {
		return "", fmt.Errorf("%w: douyu: 脚本里没有 ub98484234: %v", ErrParse, err)
	}
	callable, ok := goja.AssertFunction(fn)
	if !ok {
		return "", fmt.Errorf("%w: douyu: ub98484234 不是函数", ErrParse)
	}
	res, err := callable(goja.Undefined(), vm.ToValue(rid), vm.ToValue(did), vm.ToValue(tt))
	if err != nil {
		return "", fmt.Errorf("%w: douyu: 调用 ub98484234 失败: %v", ErrParse, err)
	}
	args := res.String()
	// 签名串必须含 sign=。缺了它后面的 POST 只会回一个语义不明的错误码，
	// 排障时看不出是签名没算出来还是参数拼错了。
	if !strings.Contains(args, "sign=") {
		return "", fmt.Errorf("%w: douyu: 签名结果不含 sign 字段: %q", ErrParse, args)
	}
	return args, nil
}

var _ = context.Background
