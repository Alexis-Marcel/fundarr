package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"sort"
	"time"
)

// ---- changes feed
//
// The feed holds the latest state of every activity as apps see it, each
// at a sequence number that only grows: an app keeping its own copy asks
// for what changed after the last number it saw. A removed activity stays
// as a tombstone (empty data) for TombstoneKeep, then is forgotten; a
// cursor older than that must start again from scratch.

// TombstoneKeep is how long a removal stays in the feed.
const TombstoneKeep = 180 * 24 * time.Hour

// FeedItem is one activity as apps see it, encoded.
type FeedItem struct {
	ID   string
	Data []byte
}

// Change is a feed row: Data is empty when the activity was removed.
type Change struct {
	Seq  int64
	ID   string
	Data []byte
}

// ApplyFeed makes the feed match items, the whole current set, in one
// transaction: what is new or different gets a new sequence number, what
// is gone becomes a tombstone. Returns how many rows changed.
func (s *Store) ApplyFeed(ctx context.Context, items []FeedItem) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	live := map[string]string{}
	rows, err := tx.QueryContext(ctx, `SELECT id, hash FROM feed WHERE data != ''`)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var id, h string
		if err := rows.Scan(&id, &h); err != nil {
			rows.Close()
			return 0, err
		}
		live[id] = h
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	now := ms(s.Clock())
	put := func(id, hash, data string) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM feed WHERE id = ?`, id); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO feed (id, hash, data, at) VALUES (?, ?, ?, ?)`, id, hash, data, now)
		return err
	}
	type up struct{ id, hash, data string }
	var ups []up
	seen := map[string]bool{}
	for _, it := range items {
		seen[it.ID] = true
		sum := sha256.Sum256(it.Data)
		h := hex.EncodeToString(sum[:16])
		if live[it.ID] != h {
			ups = append(ups, up{it.ID, h, string(it.Data)})
		}
	}
	var gone []string
	for id := range live {
		if !seen[id] {
			gone = append(gone, id)
		}
	}
	sort.Strings(gone)
	n := 0
	// Removals first, then upserts in the order given.
	for _, id := range gone {
		if err := put(id, "", ""); err != nil {
			return 0, err
		}
		n++
	}
	for _, u := range ups {
		if err := put(u.id, u.hash, u.data); err != nil {
			return 0, err
		}
		n++
	}
	// Forget old tombstones; cursors from before them are too old.
	old := now - TombstoneKeep.Milliseconds()
	var floor sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT MAX(seq) FROM feed WHERE data = '' AND at < ?`, old).Scan(&floor); err != nil {
		return 0, err
	}
	if floor.Valid {
		if _, err := tx.ExecContext(ctx, `DELETE FROM feed WHERE data = '' AND at < ?`, old); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES ('feed.floor', ?)
ON CONFLICT(key) DO UPDATE SET value=excluded.value`, floor.Int64); err != nil {
			return 0, err
		}
	}
	return n, tx.Commit()
}

// FeedState is what cursors are checked against.
type FeedState struct {
	// Epoch changes when the feed starts over (a new database): cursors
	// of another epoch are meaningless.
	Epoch string
	// Floor: tombstones up to it were forgotten. Head: the last number.
	Floor, Head int64
}

func (s *Store) FeedState(ctx context.Context) (FeedState, error) {
	var st FeedState
	b := make([]byte, 4)
	rand.Read(b)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES ('feed.epoch', ?) ON CONFLICT DO NOTHING`, hex.EncodeToString(b)); err != nil {
		return st, err
	}
	err := s.db.QueryRowContext(ctx, `SELECT
(SELECT value FROM settings WHERE key = 'feed.epoch'),
COALESCE((SELECT CAST(value AS INTEGER) FROM settings WHERE key = 'feed.floor'), 0),
COALESCE((SELECT seq FROM sqlite_sequence WHERE name = 'feed'), 0)`).Scan(&st.Epoch, &st.Floor, &st.Head)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return st, err
}

// Changes returns up to limit rows after seq, in order, and whether more
// follow. From 0 (a first copy) tombstones are left out.
func (s *Store) Changes(ctx context.Context, after int64, limit int) ([]Change, bool, error) {
	q := `SELECT seq, id, data FROM feed WHERE seq > ?`
	if after == 0 {
		q += ` AND data != ''`
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY seq LIMIT ?`, after, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []Change
	for rows.Next() {
		var c Change
		var data string
		if err := rows.Scan(&c.Seq, &c.ID, &data); err != nil {
			return nil, false, err
		}
		c.Data = []byte(data)
		out = append(out, c)
	}
	if len(out) > limit {
		return out[:limit], true, rows.Err()
	}
	return out, false, rows.Err()
}
