// Package store 用 SQLite 保存收藏的直播间和导入的频道。
//
// 选纯 Go 的 modernc.org/sqlite：镜像是 FROM scratch、交叉编译关了 CGO，
// mattn/go-sqlite3 在这里编不出来。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"

	"live-proxy/playlist"
)

// 两类频道共用一张表：
//   - 本服务的直播间：platform+room_id 非空、url 为空，按 (platform, room_id) 唯一
//   - 外部地址：url 非空，按 url 唯一
//
// 唯一约束放在数据库里而不是靠应用层先查后插：导入和收藏可能同时发生，
// 先查后插在并发下会插出重复行。
const schema = `
CREATE TABLE IF NOT EXISTS channels (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	name       TEXT    NOT NULL,
	group_name TEXT    NOT NULL DEFAULT '',
	logo       TEXT    NOT NULL DEFAULT '',
	platform   TEXT,
	room_id    TEXT,
	query      TEXT    NOT NULL DEFAULT '',
	url        TEXT,
	attrs      TEXT    NOT NULL DEFAULT '',
	source     TEXT    NOT NULL,
	created_at INTEGER NOT NULL,
	CHECK ((platform IS NOT NULL AND room_id IS NOT NULL AND url IS NULL)
	    OR (platform IS NULL AND room_id IS NULL AND url IS NOT NULL))
);
CREATE UNIQUE INDEX IF NOT EXISTS ux_channels_room ON channels(platform, room_id) WHERE platform IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS ux_channels_url  ON channels(url) WHERE url IS NOT NULL;
`

// 频道来源，只用于在管理页上区分显示，不影响输出。
const (
	SourceFavorite = "favorite"
	SourceImport   = "import"
)

type Store struct{ db *sql.DB }

// Channel 存储里的一行。
type Channel struct {
	ID int64 `json:"id"`
	playlist.Channel
	Source    string `json:"source"`
	CreatedAt int64  `json:"created_at"`
}

// Open 打开（不存在则创建）数据库。path 为 ":memory:" 时只在内存里。
func Open(path string) (*Store, error) {
	dsn := path
	if path != ":memory:" {
		// WAL：浏览页读列表时不会被导入的写事务挡住；busy_timeout 让偶发的锁冲突等一等而不是直接报错。
		// temp_store(MEMORY)：容器根文件系统只读、scratch 镜像里没有 /tmp，
		// 排序或临时索引要落盘时会失败；数据量只有几百行，放内存足够。
		dsn = "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=temp_store(MEMORY)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite 同一时刻只有一个写者；单连接让写操作在 Go 侧排队，避免 SQLITE_BUSY。
	// 内存库更必须单连接：每个新连接都是一个全新的空库。
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("初始化表结构: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func nullable(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// insertOrIgnore 已存在（命中任一唯一约束）时不做任何改动，返回 false。
// "导入不覆盖"就靠这一条：已有频道的名称、分组、台标都保持原样。
func insertOrIgnore(ctx context.Context, q interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, c playlist.Channel, source string) (bool, error) {
	if (c.Platform == "") == (c.URL == "") {
		return false, errors.New("频道必须且只能有平台房间或外部地址之一")
	}
	res, err := q.ExecContext(ctx, `INSERT OR IGNORE INTO channels
		(name, group_name, logo, platform, room_id, query, url, attrs, source, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.Name, c.Group, c.Logo, nullable(c.Platform), nullable(c.RoomID), c.Query,
		nullable(c.URL), c.Attrs, source, time.Now().Unix())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// AddFavorite 收藏一个直播间。已经在库里（无论是收藏还是导入进来的）时返回 false，不改动原记录。
func (s *Store) AddFavorite(ctx context.Context, c playlist.Channel) (bool, error) {
	if c.Platform == "" || c.RoomID == "" {
		return false, errors.New("收藏需要平台和房间号")
	}
	c.URL = ""
	return insertOrIgnore(ctx, s.db, c, SourceFavorite)
}

// RemoveFavorite 取消收藏。导入进来的同一个直播间也会被删掉：
// 对用户来说它们是同一个房间，留着会让"取消收藏"看起来没生效。
func (s *Store) RemoveFavorite(ctx context.Context, platform, roomID string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM channels WHERE platform = ? AND room_id = ?`, platform, roomID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// Delete 按 ID 删除任意频道（管理页用）。
func (s *Store) Delete(ctx context.Context, id int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM channels WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

type ImportResult struct {
	Added   int `json:"added"`
	Existed int `json:"existed"` // 库里已有（或同一文件内重复），未改动
	Skipped int `json:"skipped"` // 文件里无法识别的条目
}

// Import 在一个事务里导入：要么全部生效，要么（出错时）全部不生效，
// 不会留下导入了一半的状态让人分不清哪些进了库。
func (s *Store) Import(ctx context.Context, chs []playlist.Channel) (ImportResult, error) {
	var r ImportResult
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return r, err
	}
	defer tx.Rollback()
	for _, c := range chs {
		ok, err := insertOrIgnore(ctx, tx, c, SourceImport)
		if err != nil {
			return ImportResult{}, fmt.Errorf("导入 %q: %w", c.Name, err)
		}
		if ok {
			r.Added++
		} else {
			r.Existed++
		}
	}
	return r, tx.Commit()
}

// List 按加入顺序返回全部频道：播放器里的顺序与导入文件、收藏先后一致。
func (s *Store) List(ctx context.Context) ([]Channel, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, group_name, logo,
		COALESCE(platform, ''), COALESCE(room_id, ''), query, COALESCE(url, ''), attrs, source, created_at
		FROM channels ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Channel
	for rows.Next() {
		var c Channel
		if err := rows.Scan(&c.ID, &c.Name, &c.Group, &c.Logo, &c.Platform, &c.RoomID,
			&c.Query, &c.URL, &c.Attrs, &c.Source, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// RoomKeys 库里所有直播间的 "platform/room_id"，浏览页据此给已收藏的房间点亮星标。
func (s *Store) RoomKeys(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT platform || '/' || room_id FROM channels WHERE platform IS NOT NULL ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}
