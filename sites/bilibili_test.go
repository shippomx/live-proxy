package sites

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestBilibiliLive 端到端。房间 6 是 B 站官方直播间，长期在播。
func TestBilibiliLive(t *testing.T) {
	if testing.Short() {
		t.Skip("联网测试")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	streams, err := (&Bilibili{}).GetStreams(ctx, "6")
	if errors.Is(err, ErrOffline) {
		t.Skip("房间 6 未开播，跳过")
	}
	if err != nil {
		t.Fatalf("取流失败: %v", err)
	}
	if len(streams) == 0 {
		t.Fatal("返回空列表 —— 无流必须报 ErrOffline，不能用空列表冒充成功")
	}
	// flv 必须排在最前：盒子播放器对 fmp4 的兼容性差，顺序错了表现是黑屏而非报错。
	if streams[0].Format != FormatFLV {
		t.Errorf("首选线路应为 flv，实际 %s", streams[0].Format)
	}
	t.Logf("拿到 %d 条线路，首选 [%s] %s %s", len(streams), streams[0].CDN, streams[0].Quality, streams[0].Format)

	req, _ := http.NewRequestWithContext(ctx, "GET", streams[0].URL, nil)
	req.Header.Set("Range", "bytes=0-1023")
	req.Header.Set("User-Agent", UserAgent)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("拉流失败: %v", err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 8)
	n, _ := resp.Body.Read(buf)
	if n >= 3 && string(buf[:3]) == "FLV" {
		t.Logf("✅ 拉到有效 FLV 流")
		return
	}
	t.Fatalf("非 FLV 响应: HTTP %s, 前 %d 字节 %q", resp.Status, n, buf[:n])
}

// TestBilibiliOffline 未开播/轮播房间必须报 ErrOffline 而不是返回空列表。
// 轮播(live_status=2)放的是录播，当直播播出去会让人以为主播在线。
func TestBilibiliOffline(t *testing.T) {
	if testing.Short() {
		t.Skip("联网测试")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := (&Bilibili{}).GetStreams(ctx, "22637261")
	if err == nil {
		t.Fatal("未开播房间不应返回成功")
	}
	if !errors.Is(err, ErrOffline) && !errors.Is(err, ErrRoomInvalid) {
		t.Fatalf("应归类为未开播/房间无效，实际: %v", err)
	}
}

// TestBilibiliQnDowngrade 服务端把请求的原画静默降级成超清时，
// 必须在 Quality 里如实标注 —— 否则界面写着"原画"播的却是超清，无从发现。
func TestBilibiliQnDowngrade(t *testing.T) {
	if testing.Short() {
		t.Skip("联网测试")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	streams, err := (&Bilibili{}).GetStreams(ctx, "6")
	if err != nil {
		t.Skipf("取流失败，跳过: %v", err)
	}
	for _, s := range streams {
		if strings.Contains(s.Quality, "降级") {
			t.Logf("✅ 正确标注了降级: %s", s.Quality)
			return
		}
		if s.Quality == "原画" {
			t.Logf("✅ 拿到真原画（可能带了 Cookie）: %s", s.Quality)
			return
		}
	}
	t.Errorf("匿名请求 qn=10000 服务端必然降级，但没有任何线路标注降级: %+v", streams[0])
}
