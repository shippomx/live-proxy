package sites

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// 斗鱼的微信扫码登录走微信开放平台的标准网页扫码（snsapi_login）：
//
//  1. 访问斗鱼的 /member/oauth/signin/weixin，302 到 open.weixin.qq.com/connect/qrconnect，
//     页面里有二维码 uuid；斗鱼同时种下 acf_devid，回调时校验 state 要用到。
//  2. 长轮询 lp.open.weixin.qq.com/connect/l/qrconnect，约 15 秒返回一次
//     window.wx_errcode=...（含义取自微信扫码页的 JS，见 douyuWxState）。
//  3. 405 时带回 wx_code，用第 1 步的同一个 Cookie 罐访问
//     斗鱼回调 /member/oauth/signin/weixin?code=&state=，斗鱼在此种下登录 Cookie。
//
// 所以整个流程必须在服务端保持会话（Cookie 罐 + state + uuid），按二维码 key 存放。

const (
	douyuWxEntry    = "https://www.douyu.com/member/oauth/signin/weixin"
	douyuWxKeyPref  = "wx:"
	douyuWxTTL      = 5 * time.Minute // 与微信二维码的有效期一致
	douyuWxLongPoll = 30 * time.Second
)

type douyuWxSession struct {
	hc      *http.Client
	jar     *cookiejar.Jar
	uuid    string
	state   string
	last    int // 上一次的 wx_errcode，长轮询要带上，否则"已扫码"会被反复立即返回
	created time.Time
}

// wxPollClient 微信长轮询专用：服务端要挂起约 15~25 秒才回响应头，
// 全局客户端的 ResponseHeaderTimeout(10s) 会在它返回前掐断连接。
var wxPollClient = &http.Client{
	Timeout: douyuWxLongPoll + 5*time.Second,
	Transport: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		TLSHandshakeTimeout:   8 * time.Second,
		ResponseHeaderTimeout: douyuWxLongPoll,
	},
}

var (
	reWxUUID  = regexp.MustCompile(`connect/qrcode/([A-Za-z0-9_-]+)`)
	reWxCode  = regexp.MustCompile(`wx_errcode=(\d+)`)
	reWxToken = regexp.MustCompile(`wx_code='([^']*)'`)
)

// 微信排第一，页面上作为主按钮：多数人手机上必有微信，不一定装了斗鱼 App。
func (d *Douyu) QRMethods() []QRMethod {
	return []QRMethod{{ID: "weixin", Name: "微信扫码"}, {ID: "app", Name: "斗鱼 App 扫码"}}
}

func (d *Douyu) QRStartMethod(ctx context.Context, method string) (QRCode, error) {
	switch method {
	case "", "app":
		return d.QRStart(ctx)
	case "weixin":
		return d.wxStart(ctx)
	}
	return QRCode{}, fmt.Errorf("斗鱼不支持扫码方式 %q", method)
}

func (d *Douyu) wxStart(ctx context.Context) (QRCode, error) {
	hc, jar := jarClient()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		douyuWxEntry+"?biz_type=1&ref_url="+url.QueryEscape("https://www.douyu.com/"), nil)
	if err != nil {
		return QRCode{}, err
	}
	req.Header.Set("User-Agent", UserAgent)
	resp, err := hc.Do(req)
	if err != nil {
		return QRCode{}, &UpstreamError{Endpoint: douyuWxEntry, Err: err}
	}
	page, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	resp.Body.Close()

	// state 取自跳转后的地址：它和斗鱼种下的 acf_devid 绑定，回调时必须原样带回。
	final := resp.Request.URL
	state := final.Query().Get("state")
	m := reWxUUID.FindSubmatch(page)
	if final.Host != "open.weixin.qq.com" || state == "" || m == nil {
		return QRCode{}, fmt.Errorf("%w: 斗鱼微信登录入口没有跳到微信扫码页（实际 %s）", ErrParse, final.Host)
	}
	uuid := string(m[1])

	img, err := wxFetch(ctx, hc, "https://open.weixin.qq.com/connect/qrcode/"+uuid)
	if err != nil {
		return QRCode{}, err
	}

	s := &douyuWxSession{hc: hc, jar: jar, uuid: uuid, state: state, created: time.Now()}
	key := douyuWxKeyPref + uuid
	d.wxMu.Lock()
	if d.wx == nil {
		d.wx = map[string]*douyuWxSession{}
	}
	for k, v := range d.wx { // 顺手清理过期会话：没人扫的二维码不会再被轮询到
		if time.Since(v.created) > douyuWxTTL {
			delete(d.wx, k)
		}
	}
	d.wx[key] = s
	d.wxMu.Unlock()

	return QRCode{
		Key:       key,
		Content:   "https://open.weixin.qq.com/connect/confirm?uuid=" + uuid,
		Image:     img,
		ImageType: "image/jpeg",
	}, nil
}

func wxFetch(ctx context.Context, hc *http.Client, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Referer", "https://open.weixin.qq.com/")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, &UpstreamError{Endpoint: u, Err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &UpstreamError{Endpoint: u, Err: fmt.Errorf("HTTP %d", resp.StatusCode)}
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

func (d *Douyu) wxSession(key string) *douyuWxSession {
	d.wxMu.Lock()
	defer d.wxMu.Unlock()
	s := d.wx[key]
	if s != nil && time.Since(s.created) > douyuWxTTL {
		delete(d.wx, key)
		return nil
	}
	return s
}

func (d *Douyu) wxDrop(key string) {
	d.wxMu.Lock()
	delete(d.wx, key)
	d.wxMu.Unlock()
}

// parseWxPoll 解析长轮询返回的 "window.wx_errcode=405;window.wx_code='xxx';"。
func parseWxPoll(body []byte) (code int, wxCode string, err error) {
	m := reWxCode.FindSubmatch(body)
	if m == nil {
		return 0, "", fmt.Errorf("%w: 微信扫码轮询响应无法识别: %.120s", ErrParse, body)
	}
	code, _ = strconv.Atoi(string(m[1]))
	if t := reWxToken.FindSubmatch(body); t != nil {
		wxCode = string(t[1])
	}
	return code, wxCode, nil
}

func (d *Douyu) wxPoll(ctx context.Context, key string) (QRResult, error) {
	s := d.wxSession(key)
	if s == nil {
		return QRResult{State: QRExpired}, nil
	}
	u := fmt.Sprintf("https://lp.open.weixin.qq.com/connect/l/qrconnect?uuid=%s&_=%d", s.uuid, time.Now().UnixMilli())
	d.wxMu.Lock() // 同一个二维码可能被两个打开的页面同时轮询
	last := s.last
	d.wxMu.Unlock()
	if last != 0 {
		u += "&last=" + strconv.Itoa(last)
	}
	// 长轮询单独计时：它本身就要挂 15~30 秒，若和后面的斗鱼回调共用一个超时，
	// 确认登录那一刻剩给回调（含多次跳转）的时间可能只有几秒，回调被掐断就登录失败。
	pctx, cancel := context.WithTimeout(ctx, douyuWxLongPoll+5*time.Second)
	defer cancel()
	body, err := wxFetch(pctx, wxPollClient, u)
	if err != nil {
		return QRResult{}, err
	}
	code, wxCode, err := parseWxPoll(body)
	if err != nil {
		return QRResult{}, err
	}
	d.wxMu.Lock()
	s.last = code
	d.wxMu.Unlock()

	// 状态码含义取自微信扫码页 JS 的 switch：
	// 408 等待扫码、404 已扫码待确认、403 用户取消、402 二维码过期、500 服务异常、405 确认登录。
	switch code {
	case 408:
		return QRResult{State: QRWaiting}, nil
	case 404:
		return QRResult{State: QRScanned}, nil
	case 403:
		d.wxDrop(key)
		return QRResult{State: QRCancel}, nil
	case 402, 500:
		d.wxDrop(key)
		return QRResult{State: QRExpired}, nil
	case 405:
	default:
		return QRResult{}, fmt.Errorf("%w: 微信扫码未知状态 %d", ErrUpstream, code)
	}
	d.wxDrop(key)
	if wxCode == "" {
		return QRResult{}, fmt.Errorf("%w: 微信确认登录但没有返回 wx_code", ErrParse)
	}
	fctx, fcancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer fcancel()
	cookie, err := d.wxFinish(fctx, s, wxCode)
	if err != nil {
		return QRResult{}, err
	}
	return QRResult{State: QRDone, Cookie: cookie}, nil
}

// ErrDouyuWxNotBound 微信登录回调没拿到斗鱼登录态：通常是该微信没有绑定斗鱼账号，
// 斗鱼把它带去了注册/绑定手机的页面，而那一步必须在斗鱼 App 或网页里由本人完成。
var ErrDouyuWxNotBound = errors.New("微信未关联斗鱼账号（或斗鱼要求先绑定手机号），请先在斗鱼 App 里用微信登录一次并完成绑定，或改用斗鱼 App 扫码")

func (d *Douyu) wxFinish(ctx context.Context, s *douyuWxSession, wxCode string) (string, error) {
	u := douyuWxEntry + "?code=" + url.QueryEscape(wxCode) + "&state=" + url.QueryEscape(s.state)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Referer", "https://open.weixin.qq.com/")
	resp, err := s.hc.Do(req)
	if err != nil {
		return "", &UpstreamError{Endpoint: douyuWxEntry, Err: err}
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 2<<20))
	resp.Body.Close()

	cookies := jarCookies(s.jar, "https://www.douyu.com/", douyuPassport+"/")
	if hasCookie(cookies, "acf_auth") || hasCookie(cookies, "dy_auth") {
		return cookieString(cookies), nil
	}
	final := resp.Request.URL.String()
	if strings.Contains(final, "bind") || strings.Contains(final, "register") || strings.Contains(final, "phone") {
		return "", fmt.Errorf("%w（停在 %s）", ErrDouyuWxNotBound, resp.Request.URL.Path)
	}
	names := make([]string, 0, len(cookies))
	for _, c := range cookies {
		names = append(names, c.Name)
	}
	return "", fmt.Errorf("%w: 斗鱼微信登录回调没有种下鉴权 Cookie（停在 %s，拿到 %v）", ErrUpstream, resp.Request.URL.Path, names)
}
