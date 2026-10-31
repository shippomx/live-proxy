package sites

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// 斗鱼网页扫码登录，接口取自 passport.douyu.com 登录页的 app-login-all.js：
// POST /scan/generateCode → 轮询 GET /japi/scan/auth → 成功后再访问一次返回的 data.url，
// 斗鱼在那次访问（含跳转）里种下登录 Cookie。
//
// 两个接口都是登录页的同域 AJAX，缺 X-Requested-With 或 Referer 时只回
// "系统异常，请重试"，不会提示缺了什么。

const douyuPassport = "https://passport.douyu.com"

var douyuLoginHeader = map[string]string{
	"Referer":          douyuPassport + "/index/login?type=login&client_id=1",
	"X-Requested-With": "XMLHttpRequest",
}

func (d *Douyu) QRStart(ctx context.Context) (QRCode, error) {
	body, err := PostForm(ctx, douyuPassport+"/scan/generateCode", "client_id=1&isMultiAccount=0", douyuLoginHeader)
	if err != nil {
		return QRCode{}, err
	}
	var r struct {
		Error int             `json:"error"`
		Data  json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return QRCode{}, fmt.Errorf("%w: douyu generateCode: %v", ErrParse, err)
	}
	var data struct {
		Code string `json:"code"`
		URL  string `json:"url"`
	}
	// 出错时 data 是一句提示文本而不是对象。
	if r.Error != 0 || json.Unmarshal(r.Data, &data) != nil || data.Code == "" {
		return QRCode{}, fmt.Errorf("%w: douyu generateCode error=%d %s", ErrUpstream, r.Error, r.Data)
	}
	return QRCode{Key: data.Code, Content: data.URL}, nil
}

func (d *Douyu) QRPoll(ctx context.Context, key string) (QRResult, error) {
	if strings.HasPrefix(key, douyuWxKeyPref) {
		return d.wxPoll(ctx, key)
	}
	u := fmt.Sprintf("%s/japi/scan/auth?time=%d&code=%s", douyuPassport, time.Now().UnixMilli(), url.QueryEscape(key))
	body, err := Get(ctx, u, douyuLoginHeader)
	if err != nil {
		return QRResult{}, err
	}
	var r struct {
		Error int             `json:"error"`
		Msg   string          `json:"msg"`
		Data  json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return QRResult{}, fmt.Errorf("%w: douyu scan/auth: %v", ErrParse, err)
	}
	// 状态码取自登录页：0 成功，1 已扫码待确认，-1 失败/过期；实测 -2 为"客户端还未扫码"。
	switch r.Error {
	case -2:
		return QRResult{State: QRWaiting}, nil
	case 1:
		return QRResult{State: QRScanned}, nil
	case -1:
		return QRResult{State: QRExpired}, nil
	case 0:
	default:
		return QRResult{}, fmt.Errorf("%w: douyu scan/auth error=%d %s", ErrUpstream, r.Error, r.Msg)
	}

	var data struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(r.Data, &data); err != nil || data.URL == "" {
		return QRResult{}, fmt.Errorf("%w: douyu 扫码成功但没有回调地址: %s", ErrParse, r.Data)
	}
	cookie, err := douyuFinishLogin(ctx, data.URL)
	if err != nil {
		return QRResult{}, err
	}
	return QRResult{State: QRDone, Cookie: cookie}, nil
}

// douyuFinishLogin 访问扫码成功后的回调地址，收集沿途种下的 Cookie。
// 登录页以 JSONP 方式请求它，所以带上同样的 callback 参数，避免服务端按普通页面处理。
func douyuFinishLogin(ctx context.Context, callback string) (string, error) {
	if strings.HasPrefix(callback, "//") {
		callback = "https:" + callback
	}
	sep := "?"
	if strings.Contains(callback, "?") {
		sep = "&"
	}
	u := callback + sep + "callback=appClient_json_callback&_=" + strconv.FormatInt(time.Now().UnixMilli(), 10)

	hc, jar := jarClient()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Referer", douyuLoginHeader["Referer"])
	resp, err := hc.Do(req)
	if err != nil {
		return "", &UpstreamError{Endpoint: callback, Err: err}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if !strings.Contains(string(body), `"error":0`) {
		return "", fmt.Errorf("%w: douyu 登录回调失败: %.200s", ErrUpstream, body)
	}

	cookies := jarCookies(jar, "https://www.douyu.com/", douyuPassport+"/")
	if !hasCookie(cookies, "acf_auth") && !hasCookie(cookies, "dy_auth") {
		names := make([]string, 0, len(cookies))
		for _, c := range cookies {
			names = append(names, c.Name)
		}
		return "", fmt.Errorf("%w: douyu 登录回调没有种下鉴权 Cookie，实际拿到: %v", ErrParse, names)
	}
	return cookieString(cookies), nil
}

func (d *Douyu) Account(ctx context.Context, cookie string) (Account, error) {
	if cookie == "" {
		return Account{}, nil
	}
	body, err := Get(ctx, "https://www.douyu.com/member/cp/getUserInfo", map[string]string{
		"Cookie": cookie, "Referer": "https://www.douyu.com/", "X-Requested-With": "XMLHttpRequest",
	})
	if err != nil {
		return Account{}, err
	}
	var r struct {
		Error int             `json:"error"`
		Data  json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return Account{}, fmt.Errorf("%w: douyu getUserInfo: %v", ErrParse, err)
	}
	// -99 = 请先登录，即 Cookie 已失效。
	if r.Error != 0 {
		return Account{}, nil
	}
	// 登录态下的返回结构未经固定样本确认，昵称按常见字段名宽松提取，取不到不影响"已登录"判断。
	var data map[string]any
	_ = json.Unmarshal(r.Data, &data)
	name := "已登录"
	for _, k := range []string{"nickname", "nn", "username"} {
		if v, ok := data[k].(string); ok && v != "" {
			name = v
			break
		}
	}
	return Account{LoggedIn: true, Name: name}, nil
}
