package sites

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// B站网页扫码登录，流程与 dart_simple_live 一致：
// generate 拿 qrcode_key → 轮询 poll → 成功时登录 Cookie 在 poll 响应的 Set-Cookie 里。
// 另补 buvid3/buvid4：取流接口对缺 buvid 的请求同样会降级。

const biliPassport = "https://passport.bilibili.com/x/passport-login/web/qrcode"

func (b *Bilibili) QRStart(ctx context.Context) (QRCode, error) {
	body, err := Get(ctx, biliPassport+"/generate", map[string]string{"Referer": "https://www.bilibili.com/"})
	if err != nil {
		return QRCode{}, err
	}
	var r struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			URL string `json:"url"`
			Key string `json:"qrcode_key"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return QRCode{}, fmt.Errorf("%w: bilibili qrcode/generate: %v", ErrParse, err)
	}
	if r.Code != 0 || r.Data.Key == "" {
		return QRCode{}, fmt.Errorf("%w: bilibili qrcode/generate code=%d %s", ErrUpstream, r.Code, r.Message)
	}
	return QRCode{Key: r.Data.Key, Content: r.Data.URL}, nil
}

func (b *Bilibili) QRPoll(ctx context.Context, key string) (QRResult, error) {
	u := biliPassport + "/poll?qrcode_key=" + url.QueryEscape(key)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return QRResult{}, err
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Referer", "https://www.bilibili.com/")
	// 不能用 Get：登录 Cookie 只在响应头里，Get 只返回 body。
	resp, err := client.Do(req)
	if err != nil {
		return QRResult{}, &UpstreamError{Endpoint: u, Err: err}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return QRResult{}, &UpstreamError{Endpoint: u, Err: err}
	}
	var r struct {
		Code int `json:"code"`
		Data struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return QRResult{}, fmt.Errorf("%w: bilibili qrcode/poll: %v", ErrParse, err)
	}
	switch r.Data.Code {
	case 86101:
		return QRResult{State: QRWaiting}, nil
	case 86090:
		return QRResult{State: QRScanned}, nil
	case 86038:
		return QRResult{State: QRExpired}, nil
	case 0:
	default:
		return QRResult{}, fmt.Errorf("%w: bilibili qrcode/poll code=%d %s", ErrUpstream, r.Data.Code, r.Data.Message)
	}

	cookies := resp.Cookies()
	if !hasCookie(cookies, "SESSDATA") {
		return QRResult{}, fmt.Errorf("%w: bilibili 扫码成功但响应里没有 SESSDATA", ErrParse)
	}
	if !hasCookie(cookies, "buvid3") {
		b3, b4, err := biliBuvid(ctx)
		if err != nil {
			return QRResult{}, err
		}
		cookies = append(cookies, &http.Cookie{Name: "buvid3", Value: b3}, &http.Cookie{Name: "buvid4", Value: b4})
	}
	return QRResult{State: QRDone, Cookie: cookieString(cookies)}, nil
}

func biliBuvid(ctx context.Context) (string, string, error) {
	body, err := Get(ctx, "https://api.bilibili.com/x/frontend/finger/spi", nil)
	if err != nil {
		return "", "", err
	}
	var r struct {
		Code int `json:"code"`
		Data struct {
			B3 string `json:"b_3"`
			B4 string `json:"b_4"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil || r.Code != 0 || r.Data.B3 == "" {
		return "", "", fmt.Errorf("%w: bilibili finger/spi code=%d err=%v", ErrParse, r.Code, err)
	}
	return r.Data.B3, r.Data.B4, nil
}

func (b *Bilibili) Account(ctx context.Context, cookie string) (Account, error) {
	if cookie == "" {
		return Account{}, nil
	}
	body, err := Get(ctx, "https://api.bilibili.com/x/web-interface/nav", map[string]string{
		"Cookie": cookie, "Referer": "https://www.bilibili.com/",
	})
	if err != nil {
		return Account{}, err
	}
	var r struct {
		Code int `json:"code"`
		Data struct {
			IsLogin   bool   `json:"isLogin"`
			Uname     string `json:"uname"`
			VipStatus int    `json:"vipStatus"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return Account{}, fmt.Errorf("%w: bilibili nav: %v", ErrParse, err)
	}
	// -101 = 账号未登录，即 Cookie 已失效；这是结果而不是请求失败。
	if r.Code == -101 || !r.Data.IsLogin {
		return Account{}, nil
	}
	if r.Code != 0 {
		return Account{}, fmt.Errorf("%w: bilibili nav code=%d", ErrUpstream, r.Code)
	}
	a := Account{LoggedIn: true, Name: r.Data.Uname}
	if r.Data.VipStatus == 1 {
		a.Note = "大会员"
	}
	return a, nil
}

func hasCookie(cs []*http.Cookie, name string) bool {
	for _, c := range cs {
		if strings.EqualFold(c.Name, name) && c.Value != "" {
			return true
		}
	}
	return false
}
