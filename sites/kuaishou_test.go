package sites

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

// TestKuaishouLive 端到端。快手房间号是主播的用户名，会随主播下播失效，
// 所以从首页动态取一个在播的，而不是写死房间号 —— 写死的房间号会让这个
// 测试在主播下播那天开始永久失败，然后被当成"这个测试一直红，忽略它"。
func TestKuaishouLive(t *testing.T) {
	if testing.Short() {
		t.Skip("联网测试")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	rooms, err := ksLiveRooms(ctx)
	if err != nil || len(rooms) == 0 {
		t.Skipf("拿不到在播房间列表，跳过: %v", err)
	}

	k := &Kuaishou{}
	var lastErr error
	for _, room := range rooms {
		streams, err := k.GetStreams(ctx, room)
		if errors.Is(err, ErrOffline) || errors.Is(err, ErrRoomInvalid) {
			lastErr = err
			continue
		}
		if err != nil {
			lastErr = err
			continue
		}
		if streams[0].Format != FormatFLV {
			t.Errorf("快手只提供 flv，实际标成了 %s —— 格式标错会让播放器选错解码路径", streams[0].Format)
		}
		req, _ := http.NewRequestWithContext(ctx, "GET", streams[0].URL, nil)
		req.Header.Set("Range", "bytes=0-1023")
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
			t.Logf("✅ [%s] %d 条线路，首选 %s，拉到有效 FLV", room, len(streams), streams[0].Quality)
			return
		}
		lastErr = errors.New("非 FLV: HTTP " + resp.Status)
	}
	t.Fatalf("所有候选房间都没能拉到流，最后错误: %v", lastErr)
}

// TestKuaishouInvalidRoom 不存在的用户名必须报 ErrRoomInvalid，
// 而不是掉进解析失败被误报成"快手页面改版了"。
func TestKuaishouInvalidRoom(t *testing.T) {
	if testing.Short() {
		t.Skip("联网测试")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := (&Kuaishou{}).GetStreams(ctx, "zzz_not_exist_room_9x8y7z")
	if err == nil {
		t.Fatal("不存在的房间不应返回成功")
	}
	if !errors.Is(err, ErrRoomInvalid) && !errors.Is(err, ErrOffline) {
		t.Fatalf("应归类为房间无效/未开播，实际: %v", err)
	}
}
