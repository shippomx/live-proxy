package sites

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"testing"
	"time"
)

// 官方文档（bilibili-API-collect/docs/misc/sign/wbi.md）给出的示例：
// 输入、mixin key、最终签名串都是公开的，用来锁死排序、过滤和拼接规则。
func TestBiliWbiSignKnownVector(t *testing.T) {
	const img, sub = "7cd084941338484aae1ad9425b84077c", "4932caff0ff746eab6f01bf08b70ac45"
	if got := biliMixinKey(img + sub); got != "ea1db124af3c7062474693fa704f4ff8" {
		t.Fatalf("mixin key = %s", got)
	}
	params := url.Values{"foo": {"114"}, "bar": {"514"}, "zab": {"1919810"}}
	got := biliWbiSign(params, img, sub, time.Unix(1702204169, 0))
	want := "bar=514&foo=114&wts=1702204169&zab=1919810&w_rid=8f6f2b5b3d485fe1886cec6a0be8c5d4"
	if got != want {
		t.Fatalf("签名串\n got %s\nwant %s", got, want)
	}
}

func TestBiliWbiSignStripsReservedChars(t *testing.T) {
	a := biliWbiSign(url.Values{"k": {"a!b'c(d)e*"}}, "x", "y", time.Unix(1, 0))
	b := biliWbiSign(url.Values{"k": {"abcde"}}, "x", "y", time.Unix(1, 0))
	if a != b {
		t.Fatalf("!'()* 应在签名前去掉：\n%s\n%s", a, b)
	}
}

func TestJSONNumAndStrTolerateShapes(t *testing.T) {
	var v struct {
		A jsonNum `json:"a"`
		B jsonNum `json:"b"`
		C jsonNum `json:"c"`
		D jsonNum `json:"d"`
		S jsonStr `json:"s"`
		F jsonStr `json:"f"`
		N jsonStr `json:"n"`
	}
	in := `{"a":12,"b":"34","c":2.944323E7,"d":"abc","s":"x1","f":1.0,"n":123}`
	if err := json.Unmarshal([]byte(in), &v); err != nil {
		t.Fatal(err)
	}
	if v.A != 12 || v.B != 34 || v.C != 29443230 || v.D != 0 {
		t.Fatalf("jsonNum: %+v", v)
	}
	if v.S != "x1" || v.F != "1" || v.N != "123" {
		t.Fatalf("jsonStr: %q %q %q", v.S, v.F, v.N)
	}
}

func TestDouyuHotNum(t *testing.T) {
	for in, want := range map[string]int64{"12.3万": 123000, "8562": 8562, "": 0, "x": 0} {
		if got := douyuHotNum(in); got != want {
			t.Errorf("douyuHotNum(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestHuyaGid(t *testing.T) {
	for in, want := range map[string]string{`1.0`: "1", `2336`: "2336", `{"value":"7,8"}`: "7", `"9"`: "9"} {
		if got := huyaGid(json.RawMessage(in)); got != want {
			t.Errorf("huyaGid(%s) = %q, want %q", in, got, want)
		}
	}
}

// 联网：每个平台的分区、分区列表、推荐、搜索、查房间都要拿到真实数据，
// 并且列表里的房间号能被 RoomInfo 认出来——这是"收藏后能播"的前提。
func TestBrowseLive(t *testing.T) {
	if testing.Short() {
		t.Skip("联网测试")
	}
	browsers := []Browser{&Huya{}, &Bilibili{}, &Douyu{}}
	for _, b := range browsers {
		t.Run(b.ID(), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			cats, err := b.Categories(ctx)
			if err != nil {
				t.Fatalf("Categories: %v", err)
			}
			var sub SubCategory
			for _, c := range cats {
				// B站每个一级分区第一项是 id=0 的"全部"，列表接口不认，跳过。
				for _, s := range c.Children {
					if s.ID != "" && s.ID != "0" {
						sub = s
						break
					}
				}
				if sub.ID != "" {
					break
				}
			}
			if len(cats) == 0 || sub.ID == "" {
				t.Fatalf("没有拿到分区: %d 个一级分区", len(cats))
			}

			rooms, err := b.CategoryRooms(ctx, sub, 1)
			if err != nil {
				t.Fatalf("CategoryRooms(%s %s): %v", sub.ID, sub.Name, err)
			}
			if len(rooms.Items) == 0 {
				t.Fatalf("分区 %s %s 没有房间", sub.ID, sub.Name)
			}

			rec, err := b.RecommendRooms(ctx, 1)
			if err != nil {
				t.Fatalf("RecommendRooms: %v", err)
			}
			if len(rec.Items) == 0 || rec.Items[0].RoomID == "" || rec.Items[0].UserName == "" {
				t.Fatalf("推荐列表为空或缺字段: %+v", rec.Items)
			}

			found, err := b.SearchRooms(ctx, "英雄联盟", 1)
			if err != nil {
				t.Fatalf("SearchRooms: %v", err)
			}
			if len(found.Items) == 0 {
				t.Fatalf("搜索没有结果")
			}

			first := rec.Items[0]
			info, err := b.RoomInfo(ctx, first.RoomID)
			if err != nil {
				t.Fatalf("RoomInfo(%s): %v", first.RoomID, err)
			}
			if info.UserName == "" {
				t.Fatalf("RoomInfo 缺主播名: %+v", info)
			}
			t.Logf("%d 个一级分区；分区[%s]%d 间；推荐 %d 间；搜索 %d 间；房间 %s=%s(%s) live=%v",
				len(cats), sub.Name, len(rooms.Items), len(rec.Items), len(found.Items),
				first.RoomID, info.UserName, info.Title, info.Live)
		})
	}
}

func TestRoomInfoInvalid(t *testing.T) {
	if testing.Short() {
		t.Skip("联网测试")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, b := range []Browser{&Huya{}, &Bilibili{}, &Douyu{}} {
		if _, err := b.RoomInfo(ctx, "1999999999"); !errors.Is(err, ErrRoomInvalid) {
			t.Errorf("%s: 不存在的房间应返回 ErrRoomInvalid，得到 %v", b.ID(), err)
		}
	}
}
