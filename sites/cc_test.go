package sites

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestCCLive 端到端：ccid → channel → 各档 m3u8 真能拉。
func TestCCLive(t *testing.T) {
	if testing.Short() {
		t.Skip("联网测试")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	streams, err := (&NeteaseCC{}).GetStreams(ctx, "5200")
	if errors.Is(err, ErrOffline) {
		t.Skip("房间 5200 未开播，跳过")
	}
	if err != nil {
		t.Fatalf("取流失败: %v", err)
	}
	t.Logf("拿到 %d 条线路", len(streams))

	// 第一条必须是能选清晰度的构造地址，不能是那个由服务端决定档位的兜底跳转 ——
	// 兜底排到最前会让"选原画"静默变成"服务端给什么是什么"。
	if streams[0].CDN == "redirect" {
		t.Error("兜底的服务端选档地址不应排在首位，否则清晰度选择静默失效")
	}
	for _, s := range streams {
		if s.Format != FormatHLS {
			t.Errorf("CC 只提供 HLS，%s 档被标成了 %s", s.Quality, s.Format)
		}
	}

	req, _ := http.NewRequestWithContext(ctx, "GET", streams[0].URL, nil)
	req.Header.Set("User-Agent", UserAgent)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("拉流失败: %v", err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 7)
	n, _ := resp.Body.Read(buf)
	if n >= 7 && string(buf[:7]) == "#EXTM3U" {
		t.Logf("✅ [%s] %s 拉到有效 m3u8", streams[0].CDN, streams[0].Quality)
		return
	}
	t.Fatalf("非 m3u8 响应: HTTP %s, 前 %d 字节 %q", resp.Status, n, buf[:n])
}

// TestCCQualityDistinct 各档必须指向不同的 streamname。
// CC 用 tc1/tc2/tc4 后缀区分清晰度；如果拼错导致各档 URL 相同，
// 界面上仍会正常显示三个档位、每档都能播，只是画质其实一样 —— 无从发现。
func TestCCQualityDistinct(t *testing.T) {
	if testing.Short() {
		t.Skip("联网测试")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	streams, err := (&NeteaseCC{}).GetStreams(ctx, "5200")
	if err != nil {
		t.Skipf("取流失败，跳过: %v", err)
	}
	seen := map[string]string{}
	for _, s := range streams {
		if s.CDN == "redirect" {
			continue
		}
		base := s.URL
		if i := strings.Index(base, "?"); i > 0 {
			base = base[:i]
		}
		if prev, dup := seen[base]; dup {
			t.Errorf("档位 %q 与 %q 指向同一个流地址 %s，清晰度选择是假的", s.Quality, prev, base)
		}
		seen[base] = s.Quality
	}
	t.Logf("✅ %d 个档位各自指向不同的流", len(seen))
}
