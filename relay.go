package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"live-proxy/sites"
)

// 中转模式存在的理由：各平台 CDN 不允许一条 FLV 长连接一直推下去 ——
// 实测斗鱼在 300 秒整（与签名里 expire=300 吻合）正常关闭连接，B站约 8 分钟
// 被 RST。302 模式下播放器直连 CDN，断开后 ExoPlayer 要么把"正常关闭"当成
// 直播结束停在最后一帧，要么重连期间缓冲耗尽，表现就是"播十分钟左右卡住"。
// 中转模式由本服务拉流，上游断开时重新取流（拿新签名）并把两段 FLV 接成一条，
// 播放器那条连接从头到尾不断。

// 上游超过这么久一个字节都不给就当它死了。直播流正常每秒几十个 tag，
// 10 秒没数据不可能是"网络慢"，再等下去只会把播放器缓冲耗光。
// 是变量只为让测试不必真等 10 秒。
var relayIdleTimeout = 10 * time.Second

const (
	// 重新取流的总预算。超过就放弃并结束响应，让播放器走它自己的重试 ——
	// 继续挂着一条不出数据的连接，播放器既不报错也不重连，比断开更糟。
	relayReconnectBudget = 20 * time.Second
	// 给播放器的单次写超时。播放器停止读取（暂停/切台但没关连接）时，
	// 不设超时会让这条中转一直占着上游连接。
	relayWriteTimeout = 30 * time.Second
	// 续接时新段第一个关键帧相对上一段最后一帧的时间间隔，约一帧。
	spliceGapMs = 40
	// 单个 tag 的上限。正常视频帧几百 KB，超过它说明字节流已经错位，
	// 按错位的长度去读只会把后面全部读成垃圾。
	maxTagSize = 16 << 20
)

// relayClient 拉流专用客户端。不能复用 sites 里的 client：那个有 15 秒总超时，
// 用来拉直播流会在第 15 秒被准时掐断。这里只限制建连和等响应头的时间，
// body 的"卡死"由 idle 看门狗负责。
var relayClient = &http.Client{
	Transport: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   8 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
	},
}

// relay 把 FLV 流中转给播放器，上游断开时自动重新取流并续接。
func relay(ctx context.Context, w http.ResponseWriter, log *slog.Logger, site sites.Site, roomID string, first sites.Stream) {
	rc := http.NewResponseController(w)
	out := &deadlineWriter{w: w, rc: rc}
	sp := &flvSplicer{w: out}

	target := first
	responded := false
	barren := 0 // 连续没写出任何数据的段数
	for seg := 0; ; seg++ {
		start, before := time.Now(), sp.written
		err := pullOnce(ctx, target.URL, sp, func() {
			// 只在第一段成功拿到上游响应后才写响应头：第一段就失败时
			// 还能给播放器返回 502，而不是一个 200 加空 body。
			if !responded {
				w.Header().Set("Content-Type", "video/x-flv")
				w.Header().Set("Cache-Control", "no-store")
				w.WriteHeader(http.StatusOK)
				responded = true
			}
		})
		if ctx.Err() != nil {
			log.Info("播放器断开，结束中转", "segments", seg+1, "bytes", sp.written)
			return
		}
		if !responded {
			log.Warn("中转首次拉流失败", "cdn", target.CDN, "err", err)
			writeErr(w, http.StatusBadGateway, "上游拉流失败: "+err.Error())
			return
		}
		// 取流成功但 CDN 一连就断时，不加限制会变成每秒数次的取流紧循环。
		if sp.written == before {
			if barren++; barren >= 3 {
				log.Warn("上游连续多次无数据，结束中转", "err", err, "segments", seg+1)
				return
			}
		} else {
			barren = 0
		}
		log.Info("上游断开，重新取流", "segment", seg, "cdn", target.CDN, "dur", time.Since(start).Round(time.Second), "err", err)

		target, err = reresolve(ctx, site, roomID, target.CDN)
		if err != nil {
			log.Warn("重新取流失败，结束中转", "err", err, "segments", seg+1)
			return
		}
	}
}

// reresolve 在预算内反复取流。下播直接放弃 —— 对已下播的房间重试只是空转。
// 优先沿用原线路：换 CDN 可能换了转码参数，续接后画面参数突变的风险更大。
func reresolve(ctx context.Context, site sites.Site, roomID, preferCDN string) (sites.Stream, error) {
	deadline := time.Now().Add(relayReconnectBudget)
	backoff := 500 * time.Millisecond
	for {
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		streams, err := site.GetStreams(rctx, roomID)
		cancel()
		if err == nil {
			for _, s := range streams {
				if s.Format == sites.FormatFLV && strings.EqualFold(s.CDN, preferCDN) {
					return s, nil
				}
			}
			for _, s := range streams {
				if s.Format == sites.FormatFLV {
					return s, nil
				}
			}
			return sites.Stream{}, errors.New("没有可中转的 FLV 线路")
		}
		if errors.Is(err, sites.ErrOffline) || errors.Is(err, sites.ErrRoomInvalid) {
			return sites.Stream{}, err
		}
		if ctx.Err() != nil || time.Now().Add(backoff).After(deadline) {
			return sites.Stream{}, fmt.Errorf("重试预算耗尽: %w", err)
		}
		select {
		case <-ctx.Done():
			return sites.Stream{}, ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
	}
}

// pullOnce 拉一段上游流交给 splicer，直到上游结束、出错或卡死。
// onConnected 在拿到 200 响应后、写出第一个字节前调用。
func pullOnce(ctx context.Context, url string, sp *flvSplicer, onConnected func()) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", sites.UserAgent)
	resp, err := relayClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	onConnected()

	// 看门狗：每读到数据就续期，超时则取消请求让 Read 返回。
	idle := time.AfterFunc(relayIdleTimeout, cancel)
	defer idle.Stop()
	body := &idleReader{r: resp.Body, timer: idle}

	err = sp.consume(body)
	if errors.Is(err, context.Canceled) && ctx.Err() != nil && !idle.Stop() {
		return fmt.Errorf("上游 %s 无数据", relayIdleTimeout)
	}
	return err
}

type idleReader struct {
	r     io.Reader
	timer *time.Timer
}

func (r *idleReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 {
		r.timer.Reset(relayIdleTimeout)
	}
	return n, err
}

// deadlineWriter 每次写之前续期写超时并在写后立即 flush。
// 服务器级 WriteTimeout 是给 302/JSON 这类短响应定的，对一条要持续几小时的
// 流来说等于定时掐断，所以这里按次续期而不是整体取消。
type deadlineWriter struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

func (d *deadlineWriter) Write(p []byte) (int, error) {
	_ = d.rc.SetWriteDeadline(time.Now().Add(relayWriteTimeout))
	n, err := d.w.Write(p)
	if err == nil {
		err = d.rc.Flush()
	}
	return n, err
}

// flvSplicer 把多段上游 FLV 拼成一条连续的 FLV 输出。
//
// 第一段原样透传。之后每一段：
//   - 丢掉 FLV 文件头：同一条响应里出现第二个文件头，播放器会当成数据错乱。
//   - 丢掉 script tag（onMetaData）：内容只是元信息，重复出现没有收益。
//   - 暂存音视频 sequence header，等到关键帧再一起写出：编解码参数要在帧之前到达，
//     而它们自带的时间戳（通常是 0）还没法换算。
//   - 丢掉第一个视频关键帧之前的所有音视频：从非关键帧开始解码会花屏。
//   - 时间戳整体平移，让新段第一个关键帧紧接上一段最后一帧。CDN 给的时间戳
//     各段互不相干，不平移的话播放器会看到时间倒退或跳几小时，直接卡死。
type flvSplicer struct {
	w          io.Writer
	headerSent bool
	written    int64

	segment int
	lastTS  int64 // 已写出音视频 tag 的最大时间戳
	offset  int64 // 当前段的时间戳平移量
	rebased bool  // 当前段是否已对齐到关键帧
	pending [][]byte
}

const (
	tagAudio  = 8
	tagVideo  = 9
	tagScript = 18
)

func (s *flvSplicer) consume(r io.Reader) error {
	var hdr [9]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err
	}
	if string(hdr[:3]) != "FLV" {
		return fmt.Errorf("非 FLV 响应，前 3 字节 %q", hdr[:3])
	}
	// 文件头长度由 DataOffset 字段给出，规范值是 9，但不能假设。
	dataOffset := binary.BigEndian.Uint32(hdr[5:9])
	if dataOffset < 9 || dataOffset > 1024 {
		return fmt.Errorf("FLV 文件头长度异常: %d", dataOffset)
	}
	if _, err := io.CopyN(io.Discard, r, int64(dataOffset-9)+4); err != nil { // +4: PreviousTagSize0
		return err
	}

	if !s.headerSent {
		var out [13]byte
		copy(out[:], hdr[:])
		binary.BigEndian.PutUint32(out[5:9], 9)
		if err := s.write(out[:]); err != nil {
			return err
		}
		s.headerSent = true
	} else {
		s.segment++
		s.rebased = false
		s.pending = nil
	}

	var th [11]byte
	for {
		if _, err := io.ReadFull(r, th[:]); err != nil {
			return err
		}
		typ := th[0] & 0x1f
		size := int(th[1])<<16 | int(th[2])<<8 | int(th[3])
		if typ != tagAudio && typ != tagVideo && typ != tagScript || size > maxTagSize {
			return fmt.Errorf("FLV tag 错位: type=%d size=%d", typ, size)
		}
		tag := make([]byte, 11+size+4)
		copy(tag, th[:])
		if _, err := io.ReadFull(r, tag[11:]); err != nil {
			return err
		}
		if err := s.handleTag(typ, tag); err != nil {
			return err
		}
	}
}

func (s *flvSplicer) handleTag(typ byte, tag []byte) error {
	data := tag[11 : len(tag)-4]

	if s.segment == 0 {
		if typ != tagScript {
			s.trackTS(tagTS(tag))
		}
		return s.write(tag)
	}

	switch {
	case typ == tagScript:
		return nil
	case !s.rebased && isSequenceHeader(typ, data):
		s.pending = append(s.pending, tag)
		return nil
	case !s.rebased && !(typ == tagVideo && isKeyframe(data)):
		return nil
	case !s.rebased:
		s.offset = s.lastTS + spliceGapMs - tagTS(tag)
		s.rebased = true
		for _, p := range s.pending {
			setTagTS(p, s.lastTS+spliceGapMs)
			if err := s.write(p); err != nil {
				return err
			}
		}
		s.pending = nil
	}

	// 音频和视频交错时，紧跟关键帧的音频时间戳可能略早于关键帧，平移后会落到负数。
	ts := tagTS(tag) + s.offset
	if ts < 0 {
		ts = 0
	}
	setTagTS(tag, ts)
	s.trackTS(ts)
	return s.write(tag)
}

func (s *flvSplicer) trackTS(ts int64) {
	if ts > s.lastTS {
		s.lastTS = ts
	}
}

func (s *flvSplicer) write(p []byte) error {
	n, err := s.w.Write(p)
	s.written += int64(n)
	return err
}

// tagTS 读 tag 时间戳：低 24 位在前，第 4 字节是高 8 位扩展。
func tagTS(tag []byte) int64 {
	return int64(uint32(tag[7])<<24 | uint32(tag[4])<<16 | uint32(tag[5])<<8 | uint32(tag[6]))
}

func setTagTS(tag []byte, ts int64) {
	u := uint32(ts)
	tag[4], tag[5], tag[6], tag[7] = byte(u>>16), byte(u>>8), byte(u), byte(u>>24)
}

// isSequenceHeader 识别音视频编解码参数 tag（AVC/HEVC 的 SPS/PPS、AAC 的 AudioSpecificConfig）。
// 视频同时兼容传统 FLV（codec id 7/12）和 Enhanced RTMP（首字节最高位为 1）。
func isSequenceHeader(typ byte, data []byte) bool {
	if len(data) < 2 {
		return false
	}
	switch typ {
	case tagAudio:
		return data[0]>>4 == 10 && data[1] == 0 // AAC sequence header
	case tagVideo:
		if data[0]&0x80 != 0 {
			return data[0]&0x0f == 0 // PacketTypeSequenceStart
		}
		codec := data[0] & 0x0f
		return (codec == 7 || codec == 12) && data[1] == 0
	}
	return false
}

func isKeyframe(data []byte) bool {
	return len(data) >= 2 && (data[0]>>4)&0x07 == 1 && !isSequenceHeader(tagVideo, data)
}
