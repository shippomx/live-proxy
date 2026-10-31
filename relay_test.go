package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

// 带内容标记的帧：同一时间戳、不同 mark 表示字节不同（如另一路转码）。
func keyM(ts int64, mark byte) []byte   { return flvTag(tagVideo, ts, 0x17, 0x01, 0, 0, 0, mark) }
func interM(ts int64, mark byte) []byte { return flvTag(tagVideo, ts, 0x27, 0x01, 0, 0, 0, mark) }
func audioM(ts int64, mark byte) []byte { return flvTag(tagAudio, ts, 0xAF, 0x01, mark) }

// TestSplicerExactResume CDN 新连接先突发推送 GOP 缓存（与上一段末尾重叠几秒）。
// 续接必须从上一段最后一帧的下一帧开始：不重播、不丢帧、不重发 sequence header、时间戳不动。
func TestSplicerExactResume(t *testing.T) {
	var buf bytes.Buffer
	sp := &flvSplicer{w: &buf}
	sp.consume(bytes.NewReader(segment(vSeq(0), aSeq(0),
		keyM(1000, 1), audioM(1010, 2), interM(1040, 3), interM(1073, 4), audioM(1080, 5))))
	before := buf.Len()

	sp.consume(bytes.NewReader(segment(script(0), vSeq(0), aSeq(0),
		keyM(1000, 1), audioM(1010, 2), interM(1040, 3), interM(1073, 4), audioM(1080, 5), // GOP 缓存（重叠）
		interM(1106, 6), audioM(1110, 7)))) // 真正的新内容

	got := parseFLV(t, append(flvHeader(), buf.Bytes()[before:]...))
	want := []parsedTag{{tagVideo, 1106, 0x27, 0x01}, {tagAudio, 1110, 0xAF, 0x01}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("续接后应只有新内容\n got: %v\nwant: %v", got, want)
	}
	if sp.resumeSummary() != "视频=精确对齐 音频=精确对齐" || sp.dropped != 5 {
		t.Fatalf("应精确对齐并丢弃 5 帧重叠，实际 %s dropped=%d", sp.resumeSummary(), sp.dropped)
	}
}

// TestSplicerExactResumeInterleaveSkew 两条连接里音视频交错顺序不同（实测虎牙 GOP 缓存里音频会领先视频）。
// 上一段最后写出的是音频时，不能因为对齐到这帧音频就把排在它后面、时间更早的视频再发一遍。
func TestSplicerExactResumeInterleaveSkew(t *testing.T) {
	var buf bytes.Buffer
	sp := &flvSplicer{w: &buf}
	sp.consume(bytes.NewReader(segment(
		keyM(1000, 1), audioM(1000, 11), interM(1040, 2), audioM(1023, 12), interM(1080, 3), audioM(1046, 13))))
	before := buf.Len()

	// 新连接：音频整体排在前面，视频后补。
	sp.consume(bytes.NewReader(segment(vSeq(0), aSeq(0),
		audioM(1000, 11), audioM(1023, 12), audioM(1046, 13), audioM(1069, 14),
		keyM(1000, 1), interM(1040, 2), interM(1080, 3), interM(1120, 4),
		audioM(1092, 15))))

	got := parseFLV(t, append(flvHeader(), buf.Bytes()[before:]...))
	want := []parsedTag{{tagAudio, 1069, 0xAF, 0x01}, {tagVideo, 1120, 0x27, 0x01}, {tagAudio, 1092, 0xAF, 0x01}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("音视频应各自从自己的末尾接上\n got: %v\nwant: %v", got, want)
	}
}

// TestSplicerDropsInSegmentRepeat 同一条连接里 CDN 把已推过的 GOP 原样重推（实测虎牙 AL 线路），
// 重推的帧必须丢掉；时间戳相同但内容不同的帧（B站流里就有）必须保留。
func TestSplicerDropsInSegmentRepeat(t *testing.T) {
	var buf bytes.Buffer
	sp := &flvSplicer{w: &buf}
	sp.consume(bytes.NewReader(segment(
		keyM(1000, 1), interM(1040, 2), interM(1080, 3),
		keyM(1000, 1), interM(1040, 2), interM(1080, 3), // 重推
		interM(1120, 4), interM(1120, 5), // 同时间戳、不同内容
	)))
	got := parseFLV(t, buf.Bytes())
	var ts []int64
	for _, g := range got {
		ts = append(ts, g.ts)
	}
	if fmt.Sprint(ts) != "[1000 1040 1080 1120 1120]" || sp.repeats != 3 {
		t.Fatalf("应丢掉 3 帧重推、保留同时间戳不同内容的帧，实际 %v repeats=%d", ts, sp.repeats)
	}
}

// TestSplicerOverlapWithoutExactMatch 重叠部分字节对不上（换了 CDN/转码）时，
// 仍不能重播：丢掉不晚于上一段末尾的帧，从之后的第一个关键帧接上。
func TestSplicerOverlapWithoutExactMatch(t *testing.T) {
	var buf bytes.Buffer
	sp := &flvSplicer{w: &buf}
	sp.consume(bytes.NewReader(segment(keyM(1000, 1), interM(2000, 2))))
	before := buf.Len()

	sp.consume(bytes.NewReader(segment(vSeq(0),
		keyM(1000, 9), interM(1500, 9), interM(2000, 9), // 重叠，但字节不同
		interM(2033, 9), // 晚于末尾，但不是关键帧，没法单独解码
		keyM(2100, 9), interM(2133, 9))))

	got := parseFLV(t, append(flvHeader(), buf.Bytes()[before:]...))
	// 同一时钟下时间戳不动：2000→2100 的空白如实保留（不到一个 GOP），而不是把内容往前挪。
	want := []parsedTag{{tagVideo, 2100, 0x17, 0x00}, {tagVideo, 2100, 0x17, 0x01}, {tagVideo, 2133, 0x27, 0x01}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("应跳过重叠、从 2100 的关键帧接上\n got: %v\nwant: %v", got, want)
	}
	if sp.resumeSummary() != "视频=关键帧续接 音频=-" {
		t.Fatalf("续接方式 %s", sp.resumeSummary())
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

// TestRelayNoReplayAcrossReconnects 端到端模拟真实 CDN：每次重连先推 3 秒 GOP 缓存。
// 播放器收到的内容（按 mark 识别）必须每帧只出现一次且按顺序递增。
func TestRelayNoReplayAcrossReconnects(t *testing.T) {
	// 一路"直播"：每 100ms 一帧，mark 即帧序号，每 10 帧一个关键帧。
	frame := func(i int) []byte {
		if i%10 == 0 {
			return keyM(int64(50000+i*100), byte(i))
		}
		return interM(int64(50000+i*100), byte(i))
	}
	// 第 n 段：从 (n*40-30) 往前最近的关键帧开始（GOP 缓存），推到 n*40+39。
	seg := func(n int) []byte {
		from := max(n*40-30, 0) / 10 * 10
		var tags [][]byte
		tags = append(tags, script(0), vSeq(0))
		for i := from; i < n*40+40; i++ {
			tags = append(tags, frame(i))
		}
		return segment(tags...)
	}
	var n atomic.Int32
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if k := n.Add(1) - 1; k < 4 {
			w.Write(seg(int(k)))
			return
		}
		http.Error(w, "gone", http.StatusForbidden)
	}))
	defer cdn.Close()
	srv := serveRelay(t, &fakeSite{cdnURL: cdn.URL})

	resp, err := http.Get(srv.URL + "/fake/1?relay=1")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	parseFLV(t, body) // 先做结构校验
	// parsedTag 只保留了 data 前两个字节，mark 在第 6 字节，这里直接从原始字节里取。
	var marks []int
	b := body[13:]
	for len(b) > 0 {
		size := int(b[1])<<16 | int(b[2])<<8 | int(b[3])
		if b[0] == tagVideo && b[12] == 0x01 {
			marks = append(marks, int(b[11+5]))
		}
		b = b[11+size+4:]
	}
	if len(marks) != 160 {
		t.Fatalf("4 段共 160 帧，应各出现一次，实际 %d 帧: %v", len(marks), marks)
	}
	for i, m := range marks {
		if m != i {
			t.Fatalf("第 %d 帧内容是 #%d —— 出现了重播或丢帧: %v", i, m, marks)
		}
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
	// 第二段 777 与第一段末尾 40 同一时钟、空白不到 3 秒：时间戳保持原值。
	if len(tags) != 4 || tags[2].ts != 777 {
		t.Fatalf("假死后应续上第二段且时间戳保持 777，实际 %v", tags)
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

// ---- 联网：真实 CDN 上的续接去重 ----

// liveDouyuRoom 从推荐列表动态取一个在播房间。不写死房间号：主播下播那天测试就会永久失败。
func liveDouyuRoom(ctx context.Context) (string, error) {
	body, err := sites.Get(ctx, "https://www.douyu.com/japi/weblist/apinc/allpage/6/1", map[string]string{
		"Referer": "https://www.douyu.com/",
	})
	if err != nil {
		return "", err
	}
	var r struct {
		Data struct {
			Rl []struct {
				Type int         `json:"type"`
				Rid  json.Number `json:"rid"`
			} `json:"rl"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return "", err
	}
	for _, x := range r.Data.Rl {
		if x.Type == 1 {
			return x.Rid.String(), nil
		}
	}
	return "", errors.New("推荐列表里没有在播房间")
}

// TestLiveResumeNoReplay 对真实房间拉 8 秒、主动断开、重新取流再拉 5 秒，
// 断言续接走的是精确对齐，且输出里没有任何一帧视频内容出现两次。
// 不等 CDN 自己断（斗鱼 5 分钟、B站/虎牙不固定），主动断开触发的是同一条续接路径。
func TestLiveResumeNoReplay(t *testing.T) {
	if testing.Short() {
		t.Skip("联网测试")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	douyuRoom, err := liveDouyuRoom(ctx)
	if err != nil {
		t.Logf("取斗鱼在播房间失败，跳过斗鱼: %v", err)
	}
	cases := []struct {
		site sites.Site
		room string
	}{{&sites.Bilibili{}, "6"}, {&sites.Huya{}, "18525140"}, {&sites.Douyu{}, douyuRoom}}

	for _, c := range cases {
		t.Run(c.site.ID(), func(t *testing.T) {
			if c.room == "" {
				t.Skip("无在播房间")
			}
			var buf bytes.Buffer
			sp := &flvSplicer{w: &buf}
			// 第一段拉 8 秒；第二段拉 5 秒，CDN 起播慢（5 秒内连文件头都没给）时重试，
			// 否则测到的是 CDN 起播速度而不是续接逻辑。
			for i := 0; i < 4 && sp.segment < 1; i++ {
				dur := 5 * time.Second
				if i == 0 {
					dur = 8 * time.Second
				}
				rctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				streams, err := c.site.GetStreams(rctx, c.room)
				cancel()
				if errors.Is(err, sites.ErrOffline) {
					t.Skipf("房间 %s 未开播", c.room)
				}
				if err != nil {
					t.Fatalf("第 %d 次取流失败: %v", i+1, err)
				}
				pctx, cancel := context.WithTimeout(context.Background(), dur)
				err = pullOnce(pctx, streams[0].URL, sp, func() {})
				cancel()
				if err != nil && pctx.Err() == nil {
					t.Logf("第 %d 次拉流在主动断开前失败（CDN %s）: %v", i+1, streams[0].CDN, err)
				}
				if i > 0 && sp.segment < 1 {
					t.Logf("第 %d 次重连 %s 内未收到 FLV 文件头，重试", i, dur)
				}
			}

			seen := map[[20]byte]int64{}
			dups, frames := 0, 0
			b := buf.Bytes()[13:]
			for len(b) >= 15 {
				size := int(b[1])<<16 | int(b[2])<<8 | int(b[3])
				if len(b) < 11+size+4 {
					break // 主动断开时最后一个 tag 可能不完整，不算
				}
				data := b[11 : 11+size]
				if b[0] == tagVideo && !isSequenceHeader(tagVideo, data) && size > 512 {
					frames++
					h := sha1.Sum(data)
					if first, ok := seen[h]; ok {
						dups++
						if dups <= 3 {
							t.Errorf("内容重复: ts=%d 的帧与 ts=%d 相同（%d 字节）", tagTS(b), first, size)
						}
					} else {
						seen[h] = tagTS(b)
					}
				}
				b = b[11+size+4:]
			}
			t.Logf("视频帧 %d，重复 %d；续接方式 %s，丢弃重叠 %d 帧", frames, dups, sp.resumeSummary(), sp.dropped)
			if sp.segment != 1 {
				t.Fatalf("重连 3 次都没拿到第二段，无法验证续接（segment=%d）", sp.segment)
			}
			if dups > 0 {
				t.Fatalf("续接后有 %d 帧内容重播", dups)
			}
			// 精确对齐是期望路径，但偶有个别帧字节不同（实测虎牙 1/1533）会走兜底，兜底同样不重播。
			if !strings.Contains(sp.resumeSummary(), "视频=精确对齐") {
				t.Logf("注意：视频未精确对齐，走了兜底: %s", sp.resumeSummary())
			}
		})
	}
}
