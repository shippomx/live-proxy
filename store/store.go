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
	sort_order INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	CHECK ((platform IS NOT NULL AND room_id IS NOT NULL AND url IS NULL)
	    OR (platform IS NULL AND room_id IS NULL AND url IS NOT NULL))
);
CREATE UNIQUE INDEX IF NOT EXISTS ux_channels_room ON channels(platform, room_id) WHERE platform IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS ux_channels_url  ON channels(url) WHERE url IS NOT NULL;
CREATE TABLE IF NOT EXISTS channel_groups (
	name       TEXT PRIMARY KEY,
	created_at INTEGER NOT NULL
);
INSERT OR IGNORE INTO channel_groups (name, created_at)
	SELECT DISTINCT group_name, created_at FROM channels WHERE group_name <> '';
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
	if err := migrateChannelOrder(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("初始化频道顺序: %w", err)
	}
	if err := initEPG(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("初始化节目单来源: %w", err)
	}
	return &Store{db: db}, nil
}

func migrateChannelOrder(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(channels)`)
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		found = found || name == "sort_order"
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if !found {
		if _, err := db.Exec(`ALTER TABLE channels ADD COLUMN sort_order INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
	}
	_, err = db.Exec(`UPDATE channels SET sort_order = id WHERE sort_order = 0`)
	return err
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
	if c.Group != "" {
		if _, err := q.ExecContext(ctx, `INSERT OR IGNORE INTO channel_groups (name, created_at) VALUES (?, ?)`, c.Group, time.Now().Unix()); err != nil {
			return false, err
		}
	}
	res, err := q.ExecContext(ctx, `INSERT OR IGNORE INTO channels
		(name, group_name, logo, platform, room_id, query, url, attrs, source, sort_order, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, (SELECT COALESCE(MAX(sort_order), 0) + 1 FROM channels), ?)`,
		c.Name, c.Group, c.Logo, nullable(c.Platform), nullable(c.RoomID), c.Query,
		nullable(c.URL), c.Attrs, source, time.Now().Unix())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

var ErrOrderConflict = errors.New("频道列表已变化")

// ReorderChannels 原子保存完整频道列表顺序，避免并发增删时覆盖新列表。
func (s *Store) ReorderChannels(ctx context.Context, ids []int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM channels`).Scan(&count); err != nil {
		return err
	}
	if len(ids) != count {
		return ErrOrderConflict
	}
	seen := make(map[int64]struct{}, len(ids))
	for i, id := range ids {
		if _, ok := seen[id]; ok {
			return ErrOrderConflict
		}
		seen[id] = struct{}{}
		res, err := tx.ExecContext(ctx, `UPDATE channels SET sort_order = ? WHERE id = ?`, i+1, id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil || n != 1 {
			return ErrOrderConflict
		}
	}
	return tx.Commit()
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

// SetChannelGroup 把一个频道移到指定分组。
func (s *Store) SetChannelGroup(ctx context.Context, id int64, group string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if group != "" {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM channel_groups WHERE name = ?`, group).Scan(&n); err != nil {
			return false, err
		}
		if n == 0 {
			return false, ErrGroupNotFound
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE channels SET group_name = ? WHERE id = ?`, group, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return false, err
	}
	return true, tx.Commit()
}

var (
	ErrGroupNotFound = errors.New("分组不存在")
	ErrGroupExists   = errors.New("分组已存在")
)

func (s *Store) Groups(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM channel_groups ORDER BY created_at, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

func (s *Store) AddGroup(ctx context.Context, name string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO channel_groups (name, created_at) VALUES (?, ?)`, name, time.Now().Unix())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// RenameGroup 原子更新分组和组内频道。
func (s *Store) RenameGroup(ctx context.Context, old, new string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM channel_groups WHERE name = ?`, new).Scan(&exists); err != nil {
		return 0, err
	}
	if exists > 0 {
		return 0, ErrGroupExists
	}
	res, err := tx.ExecContext(ctx, `UPDATE channel_groups SET name = ? WHERE name = ?`, new, old)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return n, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE channels SET group_name = ? WHERE group_name = ?`, new, old); err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// DeleteGroup 删除分组，组内频道保留并移入未分组。
func (s *Store) DeleteGroup(ctx context.Context, name string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE channels SET group_name = '' WHERE group_name = ?`, name); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM channel_groups WHERE name = ?`, name)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return n, err
	}
	return n, tx.Commit()
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

// List 按用户调整后的顺序返回全部频道，播放列表也直接使用这个顺序。
func (s *Store) List(ctx context.Context) ([]Channel, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, group_name, logo,
		COALESCE(platform, ''), COALESCE(room_id, ''), query, COALESCE(url, ''), attrs, source, created_at
		FROM channels ORDER BY sort_order, id`)
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

// ErrNotFound 按 ID 找不到频道。
var ErrNotFound = errors.New("频道不存在")

// Get 按 ID 取一个频道。
func (s *Store) Get(ctx context.Context, id int64) (Channel, error) {
	var c Channel
	err := s.db.QueryRowContext(ctx, `SELECT id, name, group_name, logo,
		COALESCE(platform, ''), COALESCE(room_id, ''), query, COALESCE(url, ''), attrs, source, created_at
		FROM channels WHERE id = ?`, id).Scan(&c.ID, &c.Name, &c.Group, &c.Logo, &c.Platform, &c.RoomID,
		&c.Query, &c.URL, &c.Attrs, &c.Source, &c.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Channel{}, ErrNotFound
	}
	return c, err
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

// DefaultEPGSource 首次建库时写入的节目单来源。2026-10-31 在 R5S 上实测可直连，
// 覆盖 scdx.m3u 262 个电视频道中的 104 个，是几个可达来源里最全的。
const DefaultEPGSource = "https://epg.pw/xmltv/epg_CN.xml"

// initEPG 建节目单来源表。只在表第一次创建时写入默认来源：用户删掉它之后不应在重启时又冒出来。
func initEPG(db *sql.DB) error {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='epg_sources'`).Scan(&n); err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS epg_sources (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		url        TEXT    NOT NULL UNIQUE,
		enabled    INTEGER NOT NULL DEFAULT 1,
		created_at INTEGER NOT NULL
	)`); err != nil {
		return err
	}
	if n == 0 {
		_, err := db.Exec(`INSERT OR IGNORE INTO epg_sources (url, created_at) VALUES (?, ?)`, DefaultEPGSource, time.Now().Unix())
		return err
	}
	return nil
}

type EPGSource struct {
	ID        int64  `json:"id"`
	URL       string `json:"url"`
	Enabled   bool   `json:"enabled"`
	CreatedAt int64  `json:"created_at"`
}

// EPGSources 按加入顺序返回：合并节目单时排在前面的来源优先。
func (s *Store) EPGSources(ctx context.Context) ([]EPGSource, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, url, enabled, created_at FROM epg_sources ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EPGSource{}
	for rows.Next() {
		var e EPGSource
		if err := rows.Scan(&e.ID, &e.URL, &e.Enabled, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// AddEPGSource 已存在时返回 false，不改动。
func (s *Store) AddEPGSource(ctx context.Context, url string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO epg_sources (url, created_at) VALUES (?, ?)`, url, time.Now().Unix())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (s *Store) DeleteEPGSource(ctx context.Context, id int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM epg_sources WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *Store) SetEPGSourceEnabled(ctx context.Context, id int64, on bool) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE epg_sources SET enabled = ? WHERE id = ?`, on, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
