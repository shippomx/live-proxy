package playlist

import (
	"bytes"
	"strings"
	"testing"
)

func isPlatform(s string) bool { return s == "huya" || s == "bilibili" || s == "douyu" }

// 取自 R5S 上共享给电视的 scdx.m3u：组播频道带回看模板，网络直播频道指向本服务。
const sample = "\ufeff#EXTM3U x-tvg-url=\"http://e.example/epg.xml\"\n" +
	`#EXTINF:-1 tvg-name="CCTV-1高清" tvg-logo="https://live.fanmingming.com/tv/CCTV1.png" group-title="央视" catchup-source="rtsp://182.139.234.40/PLTV/x.smil?playseek=${(b)yyyyMMddHHmmss}-${(e)yyyyMMddHHmmss}",CCTV-1高清
http://192.168.5.3:4022/rtp/239.94.0.31:5140

#EXTINF:-1 tvg-logo="https://epg-1.iill.top/logo/哔哩哔哩.png" group-title="网络直播",「B站」热门赛事
http://192.168.5.3:9000/bilibili/10
#EXTINF:-1 group-title="网络直播",带参数
#EXTVLCOPT:http-user-agent=foo
http://r5s:9000/huya/11342394?relay=1&cdn=TX
#EXTINF:-1 group-title="其他",没有地址
#EXTINF:-1 group-title="其他",逗号,在名字里
http://example.com/a,b.m3u8
http://example.com/orphan.m3u8
#EXTINF:-1 group-title="其他",不可播协议
javascript:alert(1)
#EXTINF:-1 group-title="其他",三段路径不是本服务
http://192.168.5.3:9000/huya/1/extra
`

func TestParse(t *testing.T) {
	chs, skipped, err := Parse(strings.NewReader(sample), isPlatform)
	if err != nil {
		t.Fatal(err)
	}
	if len(chs) != 5 {
		t.Fatalf("应解析出 5 个频道，得到 %d: %+v", len(chs), chs)
	}
	// 没有地址、孤立地址、不可播协议各一条
	if skipped != 3 {
		t.Errorf("skipped = %d, want 3", skipped)
	}

	cctv := chs[0]
	if cctv.Name != "CCTV-1高清" || cctv.Group != "央视" || cctv.Platform != "" ||
		cctv.URL != "http://192.168.5.3:4022/rtp/239.94.0.31:5140" ||
		!strings.Contains(cctv.Attrs, `catchup-source="rtsp://182.139.234.40/PLTV/x.smil?playseek=${(b)yyyyMMddHHmmss}-${(e)yyyyMMddHHmmss}"`) ||
		!strings.Contains(cctv.Attrs, `tvg-name="CCTV-1高清"`) || strings.Contains(cctv.Attrs, "group-title") {
		t.Errorf("组播频道解析错误: %+v", cctv)
	}

	bili := chs[1]
	if bili.Platform != "bilibili" || bili.RoomID != "10" || bili.URL != "" || bili.Logo == "" {
		t.Errorf("本服务地址应识别为平台+房间: %+v", bili)
	}

	huya := chs[2]
	if huya.Platform != "huya" || huya.RoomID != "11342394" || huya.Query != "relay=1&cdn=TX" {
		t.Errorf("应保留查询参数，且不看主机名: %+v", huya)
	}

	if chs[3].Name != "逗号,在名字里" || chs[3].URL != "http://example.com/a,b.m3u8" {
		t.Errorf("名称解析: %q", chs[3].Name)
	}
	if chs[4].Platform != "" || chs[4].URL != "http://192.168.5.3:9000/huya/1/extra" {
		t.Errorf("三段路径不应识别为本服务: %+v", chs[4])
	}
}

func TestWriteRoundTrip(t *testing.T) {
	chs, _, err := Parse(strings.NewReader(sample), isPlatform)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Write(&buf, chs, "http://10.0.0.1:9000"); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"#EXTM3U\n",
		"http://10.0.0.1:9000/bilibili/10\n", // 用当前地址重新拼
		"http://10.0.0.1:9000/huya/11342394?relay=1&cdn=TX\n", // 参数带回
		"http://192.168.5.3:4022/rtp/239.94.0.31:5140\n",      // 外部地址原样
		`catchup-source="rtsp://182.139.234.40/PLTV/x.smil?playseek=${(b)yyyyMMddHHmmss}-${(e)yyyyMMddHHmmss}"`,
		`group-title="央视",CCTV-1高清`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("输出缺少 %q\n%s", want, out)
		}
	}
	// 再解析一遍应得到相同的频道（除了本服务地址的主机变了，那本来就不存）。
	again, _, err := Parse(strings.NewReader(out), isPlatform)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != len(chs) {
		t.Fatalf("往返后频道数 %d != %d", len(again), len(chs))
	}
	for i := range chs {
		if again[i] != chs[i] {
			t.Errorf("往返不一致\n got %+v\nwant %+v", again[i], chs[i])
		}
	}
}
