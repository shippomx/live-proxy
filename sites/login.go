package sites

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sort"
	"strings"
)

// 部分平台匿名取流会被静默降级（B站原画→超清、斗鱼原画→高清），要高画质必须带登录 Cookie。
// QRLogin 是扫码登录的平台实现：只负责和平台交互；二维码展示、Cookie 持久化由调用方负责，
// 这样同一套页面能挂任意多个平台，平台代码里也不必关心文件和 HTTP 路由。
type QRLogin interface {
	Site
	// QRStart 申请一个二维码。Content 是要编码进二维码的内容，Key 用于后续轮询。
	QRStart(ctx context.Context) (QRCode, error)
	// QRPoll 查询扫码状态。State 为 QRDone 时 Cookie 是可直接用于取流的完整 Cookie。
	QRPoll(ctx context.Context, key string) (QRResult, error)
	// Account 用给定 Cookie 查询账号，Cookie 失效时 LoggedIn=false。
	Account(ctx context.Context, cookie string) (Account, error)
	// SetCookie 替换取流时使用的 Cookie，传空串即退出登录。
	SetCookie(cookie string)
}

type QRCode struct {
	Key     string
	Content string
	// Image 平台自己给的二维码图片（如微信开放平台），非空时直接展示它而不是由 Content 生成：
	// 自己重新编码可能和平台图片里的内容有出入，扫出来对不上时无从排查。
	Image     []byte
	ImageType string
}

// QRMethod 一种扫码方式。
type QRMethod struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// QRMethodLogin 平台支持多种扫码方式时实现（如斗鱼可用斗鱼 App 或微信扫码）。
// 不论用哪种方式，QRPoll 都能凭 Key 认出对应的流程，最终得到同一份平台 Cookie。
type QRMethodLogin interface {
	QRMethods() []QRMethod // 第一个是默认方式，等同于 QRStart
	QRStartMethod(ctx context.Context, method string) (QRCode, error)
}

type QRState string

const (
	QRWaiting QRState = "waiting" // 未扫码
	QRScanned QRState = "scanned" // 已扫码，等待手机上确认
	QRExpired QRState = "expired" // 二维码失效，需要重新生成
	QRCancel  QRState = "cancel"  // 用户在手机上取消了
	QRDone    QRState = "done"    // 登录成功
)

type QRResult struct {
	State  QRState
	Cookie string
}

type Account struct {
	LoggedIn bool
	Name     string
	Note     string // 如"大会员"，可为空
}

// cookieString 把 Cookie 列表拼成请求头格式，同名取最后一个，按名字排序让结果稳定可比。
func cookieString(cs []*http.Cookie) string {
	m := map[string]string{}
	for _, c := range cs {
		if c.Value != "" {
			m[c.Name] = c.Value
		}
	}
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, n+"="+m[n])
	}
	return strings.Join(parts, "; ")
}

// jarClient 返回一个带 Cookie 罐、复用全局 Transport 的客户端。登录回调往往要经过
// 多次跳转才把 Cookie 种齐，只读最后一个响应的 Set-Cookie 会漏掉中间跳转种下的。
func jarClient() (*http.Client, *cookiejar.Jar) {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, Timeout: client.Timeout, Transport: client.Transport}, jar
}

func jarCookies(jar *cookiejar.Jar, urls ...string) []*http.Cookie {
	var out []*http.Cookie
	for _, s := range urls {
		u, err := url.Parse(s)
		if err == nil {
			out = append(out, jar.Cookies(u)...)
		}
	}
	return out
}
