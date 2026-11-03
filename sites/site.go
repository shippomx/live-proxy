package sites

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Format 播放流容器格式。播放器对两者的支持度不同，必须显式告知调用方，
// 不能让它从 URL 后缀去猜 —— 猜错的表现是播放器静默黑屏而非报错。
type Format string

const (
	FormatFLV Format = "flv"
	FormatHLS Format = "m3u8"
)

// Stream 一条可播放的线路。
type Stream struct {
	CDN     string `json:"cdn"`     // 上游 CDN 标识（AL/TX/HS…），用于排障与线路偏好
	Quality string `json:"quality"` // 清晰度名称，如"原画"
	Format  Format `json:"format"`
	URL     string `json:"url"`
}

// Site 一个直播平台的取流实现。
type Site interface {
	ID() string
	Name() string
	// GetStreams 返回该房间当前所有可播线路，按平台自身的优先级排序。
	// 房间未开播必须返回 ErrOffline，不得返回空列表冒充成功 —— 调用方
	// 靠错误类型区分"没开播"和"取流失败"，前者不该触发告警和重试。
	GetStreams(ctx context.Context, roomID string) ([]Stream, error)
}

// 错误分层：调用方需要据此判断请求断在哪一层，压平成一个 500 会让排障失去方向。
var (
	ErrOffline     = errors.New("主播未开播")
	ErrRoomInvalid = errors.New("房间号无效或不存在")
	ErrUpstream    = errors.New("上游接口请求失败")
	ErrParse       = errors.New("上游响应解析失败（对方接口结构可能已变更）")
	ErrNeedAuth    = errors.New("该内容需要登录凭据")
)

// UpstreamError 包住底层错误并保留上游身份，便于日志里一眼看出是谁挂了。
type UpstreamError struct {
	Endpoint string
	Err      error
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("%v: %s: %v", ErrUpstream, e.Endpoint, e.Err)
}
func (e *UpstreamError) Unwrap() error { return ErrUpstream }

const UserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

// client 全局唯一的出站客户端。就地 new 一个 http.Client 会同时丢掉三样东西
// 且都不报错：超时（零值=永不超时，一个不响应的上游能把 handler 挂死）、
// 连接池复用（高频调用下本地端口被 TIME_WAIT 耗尽）、统一的 UA。
var client = &http.Client{
	Timeout: 15 * time.Second,
	Transport: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   8 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
	},
	// 取流接口的 302 往往是有意义的（CDN 调度），但我们只要 body，
	// 跟随重定向即可；播放地址本身的重定向交给播放器处理。
}

// Get 发起一次带上下文的 GET，返回 body。header 可为 nil。
func Get(ctx context.Context, url string, header map[string]string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, &UpstreamError{Endpoint: url, Err: err}
	}
	req.Header.Set("User-Agent", UserAgent)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, &UpstreamError{Endpoint: url, Err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &UpstreamError{Endpoint: url, Err: fmt.Errorf("HTTP %d", resp.StatusCode)}
	}
	// 限制读取上限，防止上游异常返回超大响应把内存打满。
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, &UpstreamError{Endpoint: url, Err: err}
	}
	return body, nil
}

// PostForm 发起一次表单 POST。与 Get 共用同一个客户端，因此超时、
// 连接池、UA 的约束完全一致 —— 不要在别处 new 一个客户端来发 POST。
func PostForm(ctx context.Context, url string, form string, header map[string]string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(form))
	if err != nil {
		return nil, &UpstreamError{Endpoint: url, Err: err}
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, &UpstreamError{Endpoint: url, Err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &UpstreamError{Endpoint: url, Err: fmt.Errorf("HTTP %d", resp.StatusCode)}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, &UpstreamError{Endpoint: url, Err: err}
	}
	return body, nil
}
