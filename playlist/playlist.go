// Package playlist 解析与生成 m3u 播放列表。
//
// 与存储无关，只做文本 ⇄ 结构的转换，便于单独测试各种来源不规范的 m3u。
package playlist

import (
	"bufio"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
)

// Channel 播放列表里的一个频道。
//
// 指向本服务的频道（/{platform}/{room}）记 Platform+RoomID 而不是完整 URL：
// 同一个房间可能从 192.168.5.3:9000、r5s:9000 等不同地址导入，按 URL 去重会把它们当成不同频道；
// 输出时再用当前访问的地址拼回去，换了 IP 也不用重新导入。
type Channel struct {
	Name     string `json:"name"`
	Group    string `json:"group"`
	Logo     string `json:"logo"`
	Platform string `json:"platform,omitempty"` // 本服务的平台 ID；为空表示外部地址
	RoomID   string `json:"room_id,omitempty"`
	Query    string `json:"query,omitempty"` // 本服务地址上原有的查询参数（如 relay=1），原样带回
	URL      string `json:"url,omitempty"`   // 外部播放地址
	// Attrs #EXTINF 里除 group-title、tvg-logo 之外的原始属性（catchup-source 等），
	// 原样输出。不逐个解析：播放器支持的属性五花八门，丢掉任何一个都可能让回看失效。
	Attrs string `json:"attrs,omitempty"`
}

// PlayURL 输出到播放列表里的地址。base 形如 http://192.168.5.3:9000（不带结尾斜杠）。
func (c Channel) PlayURL(base string) string {
	if c.Platform == "" {
		return c.URL
	}
	u := base + "/" + c.Platform + "/" + url.PathEscape(c.RoomID)
	if c.Query != "" {
		u += "?" + c.Query
	}
	return u
}

// Parse 读取 m3u。isPlatform 判断路径第一段是否是本服务的平台 ID。
// skipped 是有 #EXTINF 但缺播放地址、或地址不是 http(s)/rtsp/rtp/udp 等可播协议的条目数。
func Parse(r io.Reader, isPlatform func(string) bool) (chs []Channel, skipped int, err error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20) // 有的 EXTINF 属性很长（回看模板），默认 64K 行长不够
	var pending *Channel
	for sc.Scan() {
		line := strings.TrimSpace(strings.TrimPrefix(sc.Text(), "\ufeff"))
		switch {
		case line == "":
			continue
		case strings.HasPrefix(line, "#EXTINF:"):
			if pending != nil {
				skipped++ // 上一条没等到地址
			}
			c := parseExtinf(line)
			pending = &c
		case strings.HasPrefix(line, "#"):
			continue // #EXTM3U、#EXTVLCOPT 等
		default:
			if pending == nil {
				// 没有 #EXTINF 的裸地址：没有名字，没法在播放器里分辨，跳过。
				skipped++
				continue
			}
			if !playable(line) {
				skipped++
				pending = nil
				continue
			}
			setTarget(pending, line, isPlatform)
			chs = append(chs, *pending)
			pending = nil
		}
	}
	if pending != nil {
		skipped++
	}
	if err := sc.Err(); err != nil {
		return nil, 0, fmt.Errorf("读取 m3u 失败: %w", err)
	}
	return chs, skipped, nil
}

func playable(u string) bool {
	for _, p := range []string{"http://", "https://", "rtsp://", "rtmp://", "rtp://", "udp://", "p3p://"} {
		if strings.HasPrefix(strings.ToLower(u), p) {
			return true
		}
	}
	return false
}

func setTarget(c *Channel, raw string, isPlatform func(string) bool) {
	c.URL = raw
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) == 2 && parts[1] != "" && isPlatform(parts[0]) {
		room, err := url.PathUnescape(parts[1])
		if err != nil {
			return
		}
		c.Platform, c.RoomID, c.Query, c.URL = parts[0], room, u.RawQuery, ""
	}
}

var attrRe = regexp.MustCompile(`([A-Za-z0-9_-]+)="([^"]*)"`)

// parseExtinf 解析 `#EXTINF:-1 k="v" ...,名称`。名称从引号外的第一个逗号开始：
// 属性值里可能有逗号（不能按第一个逗号切），频道名里也可能有逗号（不能按最后一个切）。
func parseExtinf(line string) Channel {
	body := strings.TrimPrefix(line, "#EXTINF:")
	inQuote, cut := false, -1
	for i, r := range body {
		if r == '"' {
			inQuote = !inQuote
		} else if r == ',' && !inQuote {
			cut = i
			break
		}
	}
	head, name := body, ""
	if cut >= 0 {
		head, name = body[:cut], strings.TrimSpace(body[cut+1:])
	}
	var c Channel
	var keep []string
	for _, m := range attrRe.FindAllStringSubmatch(head, -1) {
		switch strings.ToLower(m[1]) {
		case "group-title":
			c.Group = m[2]
		case "tvg-logo":
			c.Logo = m[2]
		default:
			keep = append(keep, m[0])
		}
	}
	c.Attrs = strings.Join(keep, " ")
	if name == "" {
		for _, m := range attrRe.FindAllStringSubmatch(head, -1) {
			if strings.EqualFold(m[1], "tvg-name") {
				name = m[2]
			}
		}
	}
	c.Name = name
	return c
}

// Write 输出 m3u。属性值里的双引号会破坏格式，替换成单引号。
func Write(w io.Writer, chs []Channel, base string) error {
	bw := bufio.NewWriter(w)
	bw.WriteString("#EXTM3U\n")
	for _, c := range chs {
		bw.WriteString("#EXTINF:-1")
		if c.Attrs != "" {
			bw.WriteByte(' ')
			bw.WriteString(c.Attrs)
		}
		if c.Logo != "" {
			fmt.Fprintf(bw, ` tvg-logo="%s"`, quoteSafe(c.Logo))
		}
		if c.Group != "" {
			fmt.Fprintf(bw, ` group-title="%s"`, quoteSafe(c.Group))
		}
		bw.WriteByte(',')
		bw.WriteString(strings.ReplaceAll(c.Name, "\n", " "))
		bw.WriteByte('\n')
		bw.WriteString(c.PlayURL(base))
		bw.WriteByte('\n')
	}
	return bw.Flush()
}

func quoteSafe(s string) string { return strings.ReplaceAll(s, `"`, "'") }
