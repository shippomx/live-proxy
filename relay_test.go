package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"live-proxy/sites"
)

// ---- 构造合成 FLV ----

func flvHeader() []byte {
	return []byte{'F', 'L', 'V', 1, 5, 0, 0, 0, 9, 0, 0, 0, 0}
}

func flvTag(typ byte, ts int64, data ...byte) []byte {
	tag := make([]byte, 11+len(data)+4)
	tag[0] = typ
	tag[1], tag[2], tag[3] = byte(len(data)>>16), byte(len(data)>>8), byte(len(data))
	setTagTS(tag, ts)
	copy(tag[11:], data)
	binary.BigEndian.PutUint32(tag[11+len(data):], uint32(11+len(data)))
	return tag
}

var (
	script = func(ts int64) []byte { return flvTag(tagScript, ts, 0x02, 0x00) }
	vSeq   = func(ts int64) []byte { return flvTag(tagVideo, ts, 0x17, 0x00, 0, 0, 0) }
	aSeq   = func(ts int64) []byte { return flvTag(tagAudio, ts, 0xAF, 0x00, 0x12) }
	key    = func(ts int64) []byte { return flvTag(tagVideo, ts, 0x17, 0x01, 0, 0, 0, 0xAA) }
	inter  = func(ts int64) []byte { return flvTag(tagVideo, ts, 0x27, 0x01, 0, 0, 0, 0xBB) }
	audio  = func(ts int64) []byte { return flvTag(tagAudio, ts, 0xAF, 0x01, 0xCC) }
)

func segment(tags ...[]byte) []byte {
	return append(flvHeader(), bytes.Join(tags, nil)...)
}

// ---- 解析输出 ----

type parsedTag struct {
	typ byte
	ts  int64
	b0  byte // data 首字节，用于区分关键帧/非关键帧
	b1  byte // data 第二字节，用于区分 sequence header
}

func (p parsedTag) String() string { return fmt.Sprintf("%d@%d(%02x%02x)", p.typ, p.ts, p.b0, p.b1) }

// parseFLV 按规范完整解析，任何错位都报错 —— 拼接出来的流必须能被严格解析。
func parseFLV(t *testing.T, b []byte) []parsedTag {
	t.Helper()
	if len(b) < 13 || string(b[:3]) != "FLV" {
		t.Fatalf("输出不以 FLV 文件头开头")
	}
	if n := bytes.Count(b, []byte("FLV\x01")); n != 1 {
		t.Fatalf("输出里出现 %d 个 FLV 文件头，应恰好 1 个", n)
	}
	b = b[13:]
	var out []parsedTag
	for len(b) > 0 {
		if len(b) < 15 {
			t.Fatalf("尾部残缺 %d 字节", len(b))
		}
		size := int(b[1])<<16 | int(b[2])<<8 | int(b[3])
		total := 11 + size + 4
		if len(b) < total {
			t.Fatalf("tag 残缺: 需要 %d 字节，剩 %d", total, len(b))
		}
		if prev := binary.BigEndian.Uint32(b[11+size:]); int(prev) != 11+size {
			t.Fatalf("PreviousTagSize=%d，应为 %d", prev, 11+size)
		}
		out = append(out, parsedTag{typ: b[0], ts: tagTS(b), b0: b[11], b1: b[12]})
		b = b[total:]
	}
	return out
}

// ---- splicer 单元测试 ----

// TestSplicerSecondSegment 第二段必须：无文件头、无 script、关键帧前的帧被丢弃、
// sequence header 在关键帧之前重发、时间戳紧接第一段。
func TestSplicerSecondSegment(t *testing.T) {
	var buf bytes.Buffer
	sp := &flvSplicer{w: &buf}

	seg1 := segment(script(0), vSeq(0), aSeq(0), key(0), audio(20), inter(40), inter(80))
	if err := sp.consume(bytes.NewReader(seg1)); err != io.EOF {
		t.Fatalf("第一段应以 EOF 结束，实际 %v", err)
	}
	seg2 := segment(script(0), vSeq(0), aSeq(0), inter(5000), audio(5010), key(5040), audio(5030), inter(5080))
	if err := sp.consume(bytes.NewReader(seg2)); err != io.EOF {
		t.Fatalf("第二段应以 EOF 结束，实际 %v", err)
	}

	got := parseFLV(t, buf.Bytes())
	want := []parsedTag{
		{tagScript, 0, 0x02, 0x00}, {tagVideo, 0, 0x17, 0x00}, {tagAudio, 0, 0xAF, 0x00},
		{tagVideo, 0, 0x17, 0x01}, {tagAudio, 20, 0xAF, 0x01}, {tagVideo, 40, 0x27, 0x01}, {tagVideo, 80, 0x27, 0x01},
		// 第二段：seq header 补发在关键帧前，关键帧落在 80+40=120
		{tagVideo, 120, 0x17, 0x00}, {tagAudio, 120, 0xAF, 0x00},
		{tagVideo, 120, 0x17, 0x01}, {tagAudio, 110, 0xAF, 0x01}, {tagVideo, 160, 0x27, 0x01},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("拼接结果不符\n got: %v\nwant: %v", got, want)
	}
}

// TestSplicerTimestampWrap 第一段时间戳超过 24 位时，扩展字节必须正确读写，
// 否则续接后时间戳会倒退约 4.6 小时。
func TestSplicerTimestampWrap(t *testing.T) {
	var buf bytes.Buffer
	sp := &flvSplicer{w: &buf}
	big := int64(1<<24 + 1000)
	sp.consume(bytes.NewReader(segment(key(big))))
	sp.consume(bytes.NewReader(segment(key(7))))
	got := parseFLV(t, buf.Bytes())
	if got[1].ts != big+spliceGapMs {
		t.Fatalf("续接时间戳 %d，应为 %d", got[1].ts, big+spliceGapMs)
	}
}

// TestSplicerRejectsGarbage 字节流错位必须报错让上层重连，而不是把垃圾转给播放器。
func TestSplicerRejectsGarbage(t *testing.T) {
	sp := &flvSplicer{w: io.Discard}
	if err := sp.consume(bytes.NewReader([]byte("<html>403</html>..."))); err == nil {
		t.Fatal("非 FLV 响应应报错")
	}
	bad := append(segment(key(0)), 0x07, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0)
	sp = &flvSplicer{w: io.Discard}
	if err := sp.consume(bytes.NewReader(bad)); err == nil || err == io.EOF {
		t.Fatalf("未知 tag 类型应报错位，实际 %v", err)
	}
}

// ---- 端到端：本地假 CDN + 假平台 ----

type fakeSite struct {
	cdnURL string
	calls  atomic.Int32
}

func (f *fakeSite) ID() string   { return "fake" }
func (f *fakeSite) Name() string { return "fake" }
func (f *fakeSite) GetStreams(ctx context.Context, roomID string) ([]sites.Stream, error) {
	n := f.calls.Add(1)
	return []sites.Stream{{CDN: "X", Format: sites.FormatFLV, URL: fmt.Sprintf("%s/seg%d", f.cdnURL, n)}}, nil
}

func serveRelay(t *testing.T, site sites.Site) *httptest.Server {
	t.Helper()
	registry[site.ID()] = site
	t.Cleanup(func() { delete(registry, site.ID()) })
	srv := httptest.NewServer(http.HandlerFunc(handleStream))
	t.Cleanup(srv.Close)
	return srv
}

// TestRelayAcrossUpstreamDisconnects 模拟 CDN 每段推一会儿就断（斗鱼 300 秒、B站约 8 分钟的缩影）：
// 播放器那一侧必须是一条连续、可严格解析、时间戳不倒退的 FLV。
func TestRelayAcrossUpstreamDisconnects(t *testing.T) {
	segs := map[string][]byte{
		"/seg1": segment(script(0), vSeq(0), aSeq(0), key(1000), audio(1010), inter(1040)),
		"/seg2": segment(script(0), vSeq(0), aSeq(0), key(90000), audio(90010), inter(90040)),
		"/seg3": segment(script(0), vSeq(0), aSeq(0), inter(5), key(40), inter(80)),
	}
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if b, ok := segs[r.URL.Path]; ok {
			w.Write(b)
			return
		}
		http.Error(w, "gone", http.StatusForbidden) // 之后 CDN 一连就失败
	}))
	defer cdn.Close()
	site := &fakeSite{cdnURL: cdn.URL}
	srv := serveRelay(t, site)

	resp, err := http.Get(srv.URL + "/fake/1?relay=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "video/x-flv" {
		t.Fatalf("应返回 200 video/x-flv，实际 %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	body, _ := io.ReadAll(resp.Body)

	tags := parseFLV(t, body)
	var keys int
	var lastVideo int64 = -1
	for _, tg := range tags {
		if tg.typ == tagVideo {
			if tg.ts < lastVideo {
				t.Fatalf("视频时间戳倒退: %v", tags)
			}
			lastVideo = tg.ts
			if tg.b0 == 0x17 && tg.b1 == 0x01 {
				keys++
			}
		}
	}
	if keys != 3 {
		t.Fatalf("三段都应各贡献一个关键帧，实际 %d: %v", keys, tags)
	}
	// 1 次初始取流 + 3 次重取（第 3 次之后 CDN 连续失败 3 段后放弃）
	if c := site.calls.Load(); c < 4 {
		t.Fatalf("应发生多次重新取流，实际取流 %d 次", c)
	}
}

// TestRelayFirstPullFails 第一段就拉不到时必须返回 502，而不是 200 加空 body ——
// 后者播放器会当成"流正常但没内容"一直转圈。
func TestRelayFirstPullFails(t *testing.T) {
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer cdn.Close()
	srv := serveRelay(t, &fakeSite{cdnURL: cdn.URL})

	resp, err := http.Get(srv.URL + "/fake/1?relay=1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("应返回 502，实际 %d", resp.StatusCode)
	}
}

// TestRelayIdleUpstream 上游不断开但不再给数据（假死）时，看门狗必须触发重连。
func TestRelayIdleUpstream(t *testing.T) {
	old := relayIdleTimeout
	relayIdleTimeout = 300 * time.Millisecond
	defer func() { relayIdleTimeout = old }()

	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/seg1" {
			w.Write(segment(key(0), inter(40)))
			w.(http.Flusher).Flush()
			<-r.Context().Done() // 假死：连接不断，也不再发数据
			return
		}
		if r.URL.Path == "/seg2" {
			w.Write(segment(key(777), inter(817)))
			return
		}
		http.Error(w, "gone", http.StatusForbidden)
	}))
	defer cdn.Close()
	site := &fakeSite{cdnURL: cdn.URL}
	srv := serveRelay(t, site)

	start := time.Now()
	resp, err := http.Get(srv.URL + "/fake/1?relay=1")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	tags := parseFLV(t, body)
	if len(tags) != 4 || tags[2].ts != 80 {
		t.Fatalf("假死后应续上第二段且关键帧落在 40+40=80，实际 %v", tags)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("看门狗未及时触发，耗时 %s", time.Since(start))
	}
}

// TestRedirectStillDefault 不带 relay 参数时行为必须与改动前一致（302）。
func TestRedirectStillDefault(t *testing.T) {
	srv := serveRelay(t, &fakeSite{cdnURL: "http://cdn.invalid"})
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get(srv.URL + "/fake/1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("默认应 302，实际 %d", resp.StatusCode)
	}
}
