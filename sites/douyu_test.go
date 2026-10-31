package sites

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// douyuLiveRoom 从斗鱼推荐列表里取一个真正在播的房间号。
// 写死房间号的测试会在那个主播下播的当天开始永久失败，
// 然后被当成"这个测试一直红，忽略它"—— 守卫就此失效。
func douyuLiveRoom(ctx context.Context) (string, error) {
	body, err := Get(ctx, "https://www.douyu.com/japi/weblist/apinc/allpage/6/1", map[string]string{
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
		if x.Type == 1 { // type=1 才是直播中的房间
			return x.Rid.String(), nil
		}
	}
	return "", errors.New("推荐列表里没有在播房间")
}

// TestDouyuSignShape 校验签名产物的形状。
// 斗鱼每天换一份混淆 JS，我们无法固定期望的 sign 值，但可以固定它的形状：
// 缺 sign= 或缺 did= 后续 POST 只会回一个语义不明的错误码。
func TestDouyuSignShape(t *testing.T) {
	if testing.Short() {
		t.Skip("联网测试")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	room, err := douyuLiveRoom(ctx)
	if err != nil {
		t.Skipf("拿不到在播房间: %v", err)
	}
	d := &Douyu{}
	body, err := Get(ctx, "https://www.douyu.com/betard/"+room, map[string]string{"Referer": "https://www.douyu.com/" + room})
	if err != nil {
		t.Skipf("betard 失败: %v", err)
	}
	var b struct {
		Room struct {
			RoomID json.Number `json:"room_id"`
		} `json:"room"`
	}
	if err := json.Unmarshal(body, &b); err != nil {
		t.Fatalf("betard 解析失败: %v", err)
	}
	realRid := b.Room.RoomID.String()

	js, err := d.script(ctx, realRid, "https://www.douyu.com/"+room)
	if err != nil {
		t.Fatalf("取签名脚本失败: %v", err)
	}
	if len(js) < 1000 {
		t.Fatalf("签名脚本只有 %d 字节，太短，多半不是完整脚本", len(js))
	}

	args, err := douyuSign(js, realRid, douyuDid, time.Now().Unix())
	if err != nil {
		t.Fatalf("签名失败（斗鱼今天的 JS 可能换了形状）: %v", err)
	}
	for _, k := range []string{"sign=", "did=" + douyuDid, "tt="} {
		if !strings.Contains(args, k) {
			t.Errorf("签名串缺少 %q，后续取流会拿到语义不明的错误码: %s", k, args)
		}
	}
	t.Logf("✅ 签名成功: %s", args)
}

// TestDouyuSignRejectsGarbage 变异防线：喂一段不含 ub98484234 的 JS，
// 必须明确报错，而不是返回一个空串让调用方拿着它去请求。
func TestDouyuSignRejectsGarbage(t *testing.T) {
	_, err := douyuSign(`var x = 1;`, "123", douyuDid, time.Now().Unix())
	if err == nil {
		t.Fatal("脚本里没有 ub98484234 时必须报错，不能返回空签名继续跑")
	}
	if !errors.Is(err, ErrParse) {
		t.Errorf("应归类为 ErrParse，实际: %v", err)
	}
}

// TestDouyuJSTimeout 死循环脚本必须被打断。
// 没有这道防线，斗鱼哪天下发一段不终止的代码就会永久占住一个 goroutine，
// 而进程从外面看完全正常 —— 表现为"服务越来越慢"，查不到原因。
func TestDouyuJSTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("耗时测试")
	}
	start := time.Now()
	_, err := douyuSign(`function ub98484234(){ while(true){} }`, "123", douyuDid, time.Now().Unix())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("死循环脚本必须被中断并报错")
	}
	if elapsed > douyuJSTimeout+5*time.Second {
		t.Errorf("中断耗时 %v，超时机制没有生效", elapsed)
	}
	t.Logf("✅ %v 后被中断: %v", elapsed.Round(time.Second), err)
}

// TestDouyuLive 端到端：真实在播房间 → 可播 FLV。
func TestDouyuLive(t *testing.T) {
	if testing.Short() {
		t.Skip("联网测试")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	room, err := douyuLiveRoom(ctx)
	if err != nil {
		t.Skipf("拿不到在播房间: %v", err)
	}
	streams, err := (&Douyu{}).GetStreams(ctx, room)
	if errors.Is(err, ErrOffline) {
		t.Skipf("房间 %s 刚好下播了", room)
	}
	if err != nil {
		t.Fatalf("房间 %s 取流失败: %v", room, err)
	}
	t.Logf("房间 %s 拿到 %d 条线路，首选 [%s] %s", room, len(streams), streams[0].CDN, streams[0].Quality)

	req, _ := http.NewRequestWithContext(ctx, "GET", streams[0].URL, nil)
	req.Header.Set("Range", "bytes=0-1023")
	req.Header.Set("User-Agent", UserAgent)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("拉流失败: %v", err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 3)
	n, _ := resp.Body.Read(buf)
	if n >= 3 && string(buf[:3]) == "FLV" {
		t.Logf("✅ 拉到有效 FLV 流")
		return
	}
	t.Fatalf("非 FLV 响应: HTTP %s, 前 %d 字节 %q", resp.Status, n, buf[:n])
}

// TestDouyuUpgradeRates rate=0 是原画（码率最高），必须排在升档候选的第一位。
// 早先把 0 当成"自动"跳过，导致登录后也拿不到原画。
func TestDouyuUpgradeRates(t *testing.T) {
	p := douyuPlayData{Rate: 2, Multirates: []douyuRate{
		{Rate: 0, Name: "原画1080P60", Bit: 8312}, {Rate: 4, Name: "蓝光4M", Bit: 4000},
		{Rate: 3, Name: "超清", Bit: 2000}, {Rate: 2, Name: "高清", Bit: 900},
	}}
	got := fmt.Sprint(p.upgradeRates())
	if got != "[0 4 3]" {
		t.Fatalf("升档顺序应为 [0 4 3]（原画优先、不含当前档），实际 %s", got)
	}
	p.Rate = 0
	if r := p.upgradeRates(); len(r) != 0 {
		t.Fatalf("已是原画时不应再升档，实际 %v", r)
	}
}

// TestDouyuCookieFallback 校验接口确认不了时以 Cookie 判断登录：
// 必须同时有鉴权 Cookie 和非零 uid，匿名 Cookie（uid=0、无 auth）不能被当成已登录。
func TestDouyuCookieFallback(t *testing.T) {
	cases := map[string]bool{
		"acf_auth=abc; acf_uid=123; acf_nickname=%E5%B0%8F%E9%B1%BC": true,
		"dy_auth=abc; acf_uid=123":                                   true,
		"acf_auth=abc; acf_uid=0":                                    false,
		"acf_uid=123; dy_did=x":                                      false,
		"acf_devid=x":                                                false,
	}
	for c, want := range cases {
		if got := douyuCookieLooksLoggedIn(c); got != want {
			t.Errorf("%q → %v，应为 %v", c, got, want)
		}
	}
	if n := douyuNickname(nil, "acf_auth=a; acf_uid=1; acf_nickname=%E5%B0%8F%E9%B1%BC"); n != "小鱼" {
		t.Errorf("昵称应从 acf_nickname 解码得到「小鱼」，实际 %q", n)
	}
	if n := douyuNickname([]byte(`{"info":{"nickname":"接口昵称"}}`), "acf_nickname=x"); n != "接口昵称" {
		t.Errorf("应优先用接口返回的昵称，实际 %q", n)
	}
}
