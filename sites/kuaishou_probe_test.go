package sites

import (
	"context"
	"net/http"
	"regexp"
)

var ksRoomIDRe = regexp.MustCompile(`"author":\{"id":"([A-Za-z0-9_-]+)"`)

// ksLiveRooms 从快手直播首页抓几个当前在播的房间号，仅供测试使用。
func ksLiveRooms(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://live.kuaishou.com/", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", UserAgent)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	buf := make([]byte, 512<<10)
	n, _ := resp.Body.Read(buf)
	for read := n; read > 0 && n < len(buf); {
		read, _ = resp.Body.Read(buf[n:])
		n += read
	}
	var out []string
	seen := map[string]bool{}
	for _, m := range ksRoomIDRe.FindAllSubmatch(buf[:n], -1) {
		id := string(m[1])
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
		if len(out) >= 5 {
			break
		}
	}
	return out, nil
}
