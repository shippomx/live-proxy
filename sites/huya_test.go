package sites

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestHuyaRotl64 钉住 uid 变换。期望值来自对虎牙线上 URL 中 u 参数的实测观察
// （房间 11342394，lPresenterUid=1394565201 → u=526405971），不是从实现反推的。
// 这个值算错不会有任何报错，只会让所有播放地址静默变成 403。
func TestHuyaRotl64(t *testing.T) {
	if got := rotl64(1394565201); got != 526405971 {
		t.Fatalf("rotl64 与线上观测值不符，所有虎牙播放地址将 403: got %d want 526405971", got)
	}
}

// TestHuyaAntiCodeShape 校验签名产物的字段完整性。
// 缺任何一个参数虎牙都返回 403，而 403 的响应体是空的，排障时看不出缺了谁。
func TestHuyaAntiCode(t *testing.T) {
	const anti = "wsSecret=612e7164b93c905eca7c6cbebcdc4430&wsTime=6a9e4c33&" +
		"fm=RFdxOEJjSjNoNkRKdDZUWV8kMF8kMV8kMl8kMw%3D%3D&ctype=tars_mp&fs=bgct&t=102"
	got, err := huyaBuildAntiCode("1250011-1250011-5368756364640256-2789253858-10057-A-0-1", 1394565201, anti)
	if err != nil {
		t.Fatalf("签名失败: %v", err)
	}
	for _, k := range []string{"wsSecret=", "wsTime=6a9e4c33", "seqid=", "ctype=tars_mp", "ver=1", "fs=bgct", "fm=", "t=102", "u=526405971"} {
		if !strings.Contains(got, k) {
			t.Errorf("签名缺少必需参数 %q，虎牙将返回 403（响应体为空，难以定位）: %s", k, got)
		}
	}
	// 必须重算，不能透传服务端给网页版的 wsSecret —— 透传的表现就是 403。
	if strings.Contains(got, "612e7164b93c905eca7c6cbebcdc4430") {
		t.Error("wsSecret 未重算，直接透传了服务端下发值，播放会 403")
	}
}

// TestHuyaLive 端到端：真实房间号 → 可播地址 → 确认拉到 FLV 魔数。
// 只验到"拿到 URL"不算验过，报价能出不代表能交付。
func TestHuyaLive(t *testing.T) {
	if testing.Short() {
		t.Skip("联网测试")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	streams, err := (&Huya{}).GetStreams(ctx, "11342394")
	if errors.Is(err, ErrOffline) {
		t.Skip("主播未开播，跳过端到端验证")
	}
	if err != nil {
		t.Fatalf("取流失败: %v", err)
	}
	t.Logf("拿到 %d 条线路", len(streams))

	var lastErr error
	for _, s := range streams {
		req, _ := http.NewRequestWithContext(ctx, "GET", s.URL, nil)
		req.Header.Set("Range", "bytes=0-2047")
		req.Header.Set("User-Agent", UserAgent)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		buf := make([]byte, 3)
		n, _ := resp.Body.Read(buf)
		resp.Body.Close()
		if n >= 3 && string(buf[:3]) == "FLV" {
			t.Logf("✅ [%s] 拉到有效 FLV 流", s.CDN)
			return
		}
		lastErr = errors.New("非 FLV 响应，HTTP " + resp.Status)
	}
	t.Fatalf("所有线路都拉不到 FLV 流，最后错误: %v", lastErr)
}

// TestHuyaInvalidRoom 房间号不存在时虎牙返回 data:[]（数组而非对象），status 仍是 200。
// 不识别这个形状就会掉进 json 解析失败，把用户输错房间号误报成"平台接口已变更"，
// 让人去查一个根本不存在的服务端故障。
func TestHuyaInvalidRoom(t *testing.T) {
	if testing.Short() {
		t.Skip("联网测试")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := (&Huya{}).GetStreams(ctx, "99999999999")
	if !errors.Is(err, ErrRoomInvalid) {
		t.Fatalf("不存在的房间应归类为 ErrRoomInvalid(404)，实际: %v", err)
	}
}
