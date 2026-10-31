package store

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"live-proxy/playlist"
)

func openTemp(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "live.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func room(p, id, name string) playlist.Channel {
	return playlist.Channel{Platform: p, RoomID: id, Name: name, Group: p}
}

func TestFavoriteToggleAndPersist(t *testing.T) {
	ctx := context.Background()
	s, path := openTemp(t)

	if ok, err := s.AddFavorite(ctx, room("huya", "1", "甲")); err != nil || !ok {
		t.Fatalf("首次收藏应成功: %v %v", ok, err)
	}
	if ok, err := s.AddFavorite(ctx, room("huya", "1", "改了名")); err != nil || ok {
		t.Fatalf("重复收藏应返回 false 且不报错: %v %v", ok, err)
	}
	if _, err := s.AddFavorite(ctx, room("douyu", "1", "乙")); err != nil {
		t.Fatal(err) // 不同平台同房间号是不同房间
	}

	// 重启后仍在，且重复收藏没有改掉原名
	s.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	list, err := s2.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Name != "甲" || list[0].Source != SourceFavorite {
		t.Fatalf("重开后列表不对: %+v", list)
	}
	keys, _ := s2.RoomKeys(ctx)
	if !slices.Equal(keys, []string{"huya/1", "douyu/1"}) {
		t.Fatalf("RoomKeys = %v", keys)
	}

	if ok, err := s2.RemoveFavorite(ctx, "huya", "1"); err != nil || !ok {
		t.Fatalf("取消收藏: %v %v", ok, err)
	}
	if ok, _ := s2.RemoveFavorite(ctx, "huya", "1"); ok {
		t.Fatal("重复取消应返回 false")
	}
	if list, _ := s2.List(ctx); len(list) != 1 || list[0].Platform != "douyu" {
		t.Fatalf("取消后剩余: %+v", list)
	}
}

func TestImportDoesNotOverwrite(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)

	// 先收藏一个房间
	if _, err := s.AddFavorite(ctx, room("huya", "11342394", "我收藏时的名字")); err != nil {
		t.Fatal(err)
	}
	file := []playlist.Channel{
		{Name: "CCTV-1", Group: "央视", URL: "http://192.168.5.3:4022/rtp/239.94.0.31:5140", Attrs: `catchup-source="x"`},
		{Name: "导入文件里的名字", Group: "网络直播", Platform: "huya", RoomID: "11342394", Query: "relay=1"},
		{Name: "B站", Group: "网络直播", Platform: "bilibili", RoomID: "10"},
		{Name: "同一文件内重复", Group: "央视", URL: "http://192.168.5.3:4022/rtp/239.94.0.31:5140"},
	}
	r, err := s.Import(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	if r.Added != 2 || r.Existed != 2 {
		t.Fatalf("第一次导入: %+v（应新增 CCTV-1 和 B站，已收藏的虎牙与文件内重复的各算已存在）", r)
	}

	// 同一文件再导入一次：全部已存在
	r, err = s.Import(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	if r.Added != 0 || r.Existed != 4 {
		t.Fatalf("第二次导入应全部跳过: %+v", r)
	}

	list, _ := s.List(ctx)
	if len(list) != 3 {
		t.Fatalf("库里应有 3 个频道: %+v", list)
	}
	if list[0].Name != "我收藏时的名字" || list[0].Query != "" || list[0].Source != SourceFavorite {
		t.Fatalf("已收藏的房间被导入改动了: %+v", list[0])
	}
	if list[1].Attrs != `catchup-source="x"` || list[1].Source != SourceImport {
		t.Fatalf("导入的属性没保存: %+v", list[1])
	}

	// 导入的直播间也算"已收藏"，取消收藏能把它删掉
	if ok, _ := s.RemoveFavorite(ctx, "bilibili", "10"); !ok {
		t.Fatal("取消收藏应能删掉导入进来的同一房间")
	}
	if ok, _ := s.Delete(ctx, list[1].ID); !ok {
		t.Fatal("按 ID 删除外部频道失败")
	}
	if list, _ := s.List(ctx); len(list) != 1 {
		t.Fatalf("删除后剩余: %+v", list)
	}
}

func TestImportRejectsInvalidAtomically(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	_, err := s.Import(ctx, []playlist.Channel{
		{Name: "好的", URL: "http://a/1.m3u8"},
		{Name: "既无地址也无房间"},
	})
	if err == nil {
		t.Fatal("非法条目应报错")
	}
	if list, _ := s.List(ctx); len(list) != 0 {
		t.Fatalf("出错时整个导入应回滚，库里却有: %+v", list)
	}
}

func TestDatabaseRejectsMixedRow(t *testing.T) {
	// 绕过应用层直接写，确认 CHECK 约束真的在库里。
	s, _ := openTemp(t)
	_, err := s.db.Exec(`INSERT INTO channels (name, platform, room_id, url, source, created_at)
		VALUES ('x', 'huya', '1', 'http://a', 'import', 0)`)
	if err == nil {
		t.Fatal("同时有房间和 URL 的行应被 CHECK 约束拒绝")
	}
}
