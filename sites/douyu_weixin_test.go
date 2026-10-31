package sites

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

// TestParseWxPoll 微信长轮询返回的是一段 JS 赋值语句，状态码和 wx_code 都要能取出；
// 认不出的响应必须报错，而不是当成"等待扫码"一直空转。
func TestParseWxPoll(t *testing.T) {
	cases := []struct {
		in   string
		code int
		wx   string
	}{
		{"window.wx_errcode=408;window.wx_code='';", 408, ""},
		{"window.wx_errcode=404;window.wx_code='';", 404, ""},
		{"window.wx_errcode=405;window.wx_code='061abcDEF-xyz';", 405, "061abcDEF-xyz"},
	}
	for _, c := range cases {
		code, wx, err := parseWxPoll([]byte(c.in))
		if err != nil || code != c.code || wx != c.wx {
			t.Errorf("%q → %d %q %v，应为 %d %q", c.in, code, wx, err, c.code, c.wx)
		}
	}
	if _, _, err := parseWxPoll([]byte("<html>502</html>")); err == nil {
		t.Error("无法识别的响应应报错")
	}
}

// TestDouyuWeixinQRLive 联网：拿到微信二维码图片，首次长轮询应为"等待扫码"。
// 长轮询要挂起约 15 秒，这个测试本身就慢。
func TestDouyuWeixinQRLive(t *testing.T) {
	if testing.Short() {
		t.Skip("联网测试")
	}
	d := &Douyu{}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	code, err := d.QRStartMethod(ctx, "weixin")
	if err != nil {
		t.Fatalf("生成微信二维码失败: %v", err)
	}
	if !strings.HasPrefix(code.Key, douyuWxKeyPref) || !bytes.HasPrefix(code.Image, []byte{0xFF, 0xD8}) {
		t.Fatalf("应返回 wx: 前缀的 key 和 JPEG 图片，实际 key=%q 图片前缀 %x", code.Key, code.Image[:min(4, len(code.Image))])
	}
	res, err := d.QRPoll(ctx, code.Key)
	if err != nil || res.State != QRWaiting {
		t.Fatalf("刚生成的微信二维码应为等待扫码: %+v %v", res, err)
	}
	if res, _ := d.QRPoll(ctx, "wx:not-exist"); res.State != QRExpired {
		t.Fatalf("不存在的会话应视为过期，实际 %+v", res)
	}
	t.Logf("✅ 微信二维码 %d 字节，长轮询正常", len(code.Image))
}
