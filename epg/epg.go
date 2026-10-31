// Package epg 合并 XMLTV 节目单：只保留播放列表里有的频道和近几天的节目，
// 并把频道 ID 换成播放列表里用的名字，电视上的播放器才能按名字对上。
//
// 不依赖网络和存储，输入是已经下载好的节目单内容。
package epg

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
	"time"
)

// Normalize 频道名归一化。节目单和播放列表对同一频道的叫法常差在修饰词上
// （"CCTV-1高清" / "CCTV1" / "CCTV-1 HD"），去掉这些再比较。
func Normalize(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	s = strings.NewReplacer("＋", "+", " ", "", "-", "", "_", "", "·", "").Replace(s)
	for _, w := range []string{"超高清", "高清", "超清", "标清", "HD", "4K", "8K", "频道", "台"} {
		s = strings.ReplaceAll(s, w, "")
	}
	return s
}

// Target 播放列表里的一个频道：ID 写进节目单的 channel id，Names 用来与节目单匹配。
type Target struct {
	ID    string
	Names []string
}

type Source struct {
	Name string // 只用于统计显示
	Data io.Reader
}

// SourceStat 每个来源的匹配情况，页面上展示用。
type SourceStat struct {
	Name       string `json:"name"`
	Channels   int    `json:"channels"`   // 来源里的频道总数
	Matched    int    `json:"matched"`    // 本次由它提供节目的播放列表频道数
	Programmes int    `json:"programmes"` // 写进结果的节目条数
	Err        string `json:"error,omitempty"`
}

type Result struct {
	XML        []byte       `json:"-"`
	Matched    []string     `json:"matched"`   // 有节目单的播放列表频道 ID
	Unmatched  []string     `json:"unmatched"` // 没对上的
	Programmes int          `json:"programmes"`
	Sources    []SourceStat `json:"sources"`
}

type xmlText struct {
	Lang string `xml:"lang,attr,omitempty"`
	Text string `xml:",chardata"`
}

type xmlChannel struct {
	XMLName      xml.Name  `xml:"channel"`
	ID           string    `xml:"id,attr"`
	DisplayNames []xmlText `xml:"display-name"`
}

type xmlProgramme struct {
	XMLName  xml.Name  `xml:"programme"`
	Start    string    `xml:"start,attr"`
	Stop     string    `xml:"stop,attr,omitempty"`
	Channel  string    `xml:"channel,attr"`
	Title    []xmlText `xml:"title"`
	SubTitle []xmlText `xml:"sub-title,omitempty"`
	Desc     []xmlText `xml:"desc,omitempty"`
	Category []xmlText `xml:"category,omitempty"`
}

// Merge 按顺序处理来源：一个播放列表频道由第一个能匹配上它的来源提供节目，
// 后面的来源不再覆盖（排在前面的来源优先）。只保留 [now-before, now+after] 内开始的节目。
func Merge(targets []Target, sources []Source, now time.Time, before, after time.Duration) Result {
	byNorm := map[string][]int{} // 归一化名 → targets 下标
	for i, t := range targets {
		for _, n := range t.Names {
			k := Normalize(n)
			if k != "" {
				byNorm[k] = append(byNorm[k], i)
			}
		}
	}
	owner := make([]int, len(targets)) // 提供节目的来源下标 +1；0 表示还没有
	from := now.Add(-before).Format("20060102150405")
	to := now.Add(after).Format("20060102150405")

	var res Result
	var progBuf bytes.Buffer
	enc := xml.NewEncoder(&progBuf)
	for si, src := range sources {
		stat := SourceStat{Name: src.Name}
		// 第一遍不可能先知道 programme 属于谁：XMLTV 规定 channel 在前、programme 在后，
		// 所以边读边建映射即可，一遍流式处理，不把几十 MB 的源整个读进内存。
		mapping := map[string][]int{} // 来源里的 channel id → 本来源负责的 targets
		err := decode(src.Data, func(d *xml.Decoder, se xml.StartElement) error {
			switch se.Name.Local {
			case "channel":
				var ch xmlChannel
				if err := d.DecodeElement(&ch, &se); err != nil {
					return err
				}
				stat.Channels++
				seen := map[int]bool{}
				for _, dn := range ch.DisplayNames {
					for _, ti := range byNorm[Normalize(dn.Text)] {
						if owner[ti] == 0 && !seen[ti] {
							owner[ti] = si + 1
							seen[ti] = true
							mapping[ch.ID] = append(mapping[ch.ID], ti)
							stat.Matched++
						}
					}
				}
			case "programme":
				var p xmlProgramme
				if err := d.DecodeElement(&p, &se); err != nil {
					return err
				}
				tis := mapping[p.Channel]
				if len(tis) == 0 || len(p.Start) < 14 || p.Start[:14] < from || p.Start[:14] > to {
					return nil
				}
				for _, ti := range tis {
					p.Channel = targets[ti].ID
					if err := enc.Encode(p); err != nil {
						return err
					}
					progBuf.WriteByte('\n')
					stat.Programmes++
				}
			default:
				return d.Skip()
			}
			return nil
		})
		if err != nil {
			stat.Err = err.Error()
		}
		res.Programmes += stat.Programmes
		res.Sources = append(res.Sources, stat)
	}
	enc.Flush()

	var out bytes.Buffer
	out.WriteString(xml.Header)
	out.WriteString(`<tv generator-info-name="live-proxy">` + "\n")
	for i, t := range targets {
		if owner[i] == 0 {
			res.Unmatched = append(res.Unmatched, t.ID)
			continue
		}
		res.Matched = append(res.Matched, t.ID)
		xml.NewEncoder(&out).Encode(xmlChannel{ID: t.ID, DisplayNames: []xmlText{{Text: t.ID}}})
		out.WriteByte('\n')
	}
	out.Write(progBuf.Bytes())
	out.WriteString("</tv>\n")
	res.XML = out.Bytes()
	return res
}

// decode 遍历 <tv> 下的直接子元素。gzip 压缩的源按魔数自动解压。
func decode(r io.Reader, fn func(*xml.Decoder, xml.StartElement) error) error {
	br := bufio.NewReader(r)
	if magic, _ := br.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(br)
		if err != nil {
			return fmt.Errorf("解压失败: %w", err)
		}
		defer gz.Close()
		r = gz
	} else {
		r = br
	}
	d := xml.NewDecoder(r)
	d.Strict = false // 不少节目单有不规范的实体或编码声明，宽松解析能救回大部分内容
	d.CharsetReader = func(label string, in io.Reader) (io.Reader, error) { return in, nil }
	depth := 0
	for {
		tok, err := d.Token()
		if err == io.EOF {
			if depth == 0 {
				return fmt.Errorf("不是 XMLTV 节目单（没有 <tv> 元素）")
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("解析失败: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if depth == 0 {
				if t.Name.Local != "tv" {
					return fmt.Errorf("不是 XMLTV 节目单（根元素是 <%s>）", t.Name.Local)
				}
				depth = 1
				continue
			}
			if err := fn(d, t); err != nil {
				return err
			}
		case xml.EndElement:
			if t.Name.Local == "tv" {
				return nil
			}
		}
	}
}

// Gzip 压缩结果，电视上的播放器都支持 .xml.gz。
func Gzip(b []byte) []byte {
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	zw.Write(b)
	zw.Close()
	return buf.Bytes()
}
