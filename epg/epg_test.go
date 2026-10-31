package epg

import (
	"bytes"
	"compress/gzip"
	"encoding/xml"
	"io"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 10, 31, 12, 0, 0, 0, time.FixedZone("CST", 8*3600))

const srcA = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE tv SYSTEM "xmltv.dtd">
<tv generator-info-name="a">
  <channel id="1"><display-name lang="zh">CCTV1</display-name><icon src="x"/></channel>
  <channel id="2"><display-name>湖南卫视</display-name></channel>
  <channel id="99"><display-name>没人要的台</display-name></channel>
  <programme start="20261031100000 +0800" stop="20261031110000 +0800" channel="1"><title lang="zh">新闻联播 &amp; 天气</title><desc>今天</desc></programme>
  <programme start="20261020100000 +0800" stop="20261020110000 +0800" channel="1"><title>太早</title></programme>
  <programme start="20261110100000 +0800" stop="20261110110000 +0800" channel="1"><title>太晚</title></programme>
  <programme start="20261031120000 +0800" channel="2"><title>快乐大本营</title></programme>
  <programme start="20261031120000 +0800" channel="99"><title>不该出现</title></programme>
</tv>`

// 第二个来源也有 CCTV1：不应覆盖第一个来源；它独有的四川卫视应补上。
const srcB = `<tv>
  <channel id="cctv1"><display-name>CCTV-1 HD</display-name></channel>
  <channel id="sc"><display-name>四川卫视</display-name></channel>
  <programme start="20261031100000 +0800" channel="cctv1"><title>B 源的节目</title></programme>
  <programme start="20261031130000 +0800" channel="sc"><title>四川新闻</title></programme>
</tv>`

func gz(s string) io.Reader {
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	w.Write([]byte(s))
	w.Close()
	return &b
}

func TestNormalize(t *testing.T) {
	for _, pair := range [][2]string{{"CCTV-1高清", "CCTV1"}, {"CCTV-5＋高清", "CCTV5+"}, {"湖南卫视4K", "湖南卫视"}, {"cctv-1 HD", "CCTV1"}} {
		if Normalize(pair[0]) != Normalize(pair[1]) {
			t.Errorf("%q 与 %q 应归一化成同一个: %q / %q", pair[0], pair[1], Normalize(pair[0]), Normalize(pair[1]))
		}
	}
}

func TestMerge(t *testing.T) {
	targets := []Target{
		{ID: "CCTV-1高清", Names: []string{"CCTV-1高清"}},
		{ID: "湖南卫视4K", Names: []string{"湖南卫视4K"}},
		{ID: "四川卫视高清", Names: []string{"四川卫视高清"}},
		{ID: "某地方台", Names: []string{"某地方台"}},
	}
	res := Merge(targets, []Source{{Name: "A", Data: strings.NewReader(srcA)}, {Name: "B", Data: gz(srcB)}},
		now, 24*time.Hour, 72*time.Hour)

	if strings.Join(res.Matched, ",") != "CCTV-1高清,湖南卫视4K,四川卫视高清" || strings.Join(res.Unmatched, ",") != "某地方台" {
		t.Fatalf("匹配: matched=%v unmatched=%v", res.Matched, res.Unmatched)
	}
	if res.Sources[0].Matched != 2 || res.Sources[1].Matched != 1 || res.Sources[0].Channels != 3 {
		t.Fatalf("来源统计: %+v", res.Sources)
	}

	// 结果必须是合法 XML，并且频道 ID 换成了播放列表里的名字
	var tv struct {
		Channels []xmlChannel   `xml:"channel"`
		Progs    []xmlProgramme `xml:"programme"`
	}
	if err := xml.Unmarshal(res.XML, &tv); err != nil {
		t.Fatalf("输出不是合法 XML: %v\n%s", err, res.XML)
	}
	if len(tv.Channels) != 3 || tv.Channels[0].ID != "CCTV-1高清" || tv.Channels[0].DisplayNames[0].Text != "CCTV-1高清" {
		t.Fatalf("频道: %+v", tv.Channels)
	}
	titles := map[string]string{}
	for _, p := range tv.Progs {
		titles[p.Title[0].Text] = p.Channel
	}
	want := map[string]string{"新闻联播 & 天气": "CCTV-1高清", "快乐大本营": "湖南卫视4K", "四川新闻": "四川卫视高清"}
	if len(titles) != len(want) {
		t.Fatalf("节目: %v（时间窗外、未匹配频道、后序来源重复的节目都不应出现）", titles)
	}
	for k, v := range want {
		if titles[k] != v {
			t.Errorf("节目 %q 应属于 %q，得到 %q", k, v, titles[k])
		}
	}
	if !strings.Contains(string(res.XML), `<desc>今天</desc>`) {
		t.Errorf("节目描述丢了:\n%s", res.XML)
	}
}

func TestMergeBadSourceDoesNotBreakOthers(t *testing.T) {
	res := Merge([]Target{{ID: "湖南卫视", Names: []string{"湖南卫视"}}},
		[]Source{{Name: "坏的", Data: strings.NewReader("<html>404</html>")}, {Name: "A", Data: strings.NewReader(srcA)}},
		now, 24*time.Hour, 72*time.Hour)
	if res.Sources[0].Err == "" || len(res.Matched) != 1 {
		t.Fatalf("坏来源应只记错误、不影响其他来源: %+v matched=%v", res.Sources, res.Matched)
	}
}
