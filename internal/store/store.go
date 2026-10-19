// Package store keeps Fundarr's state in one SQLite file: configured sources
// and destinations (secrets sealed), the normalized activity history, the
// latest balances, what was delivered where, and the run log.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Alexis-Marcel/fundarr/internal/model"
	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")

type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS sources (
	id          TEXT PRIMARY KEY,
	kind        TEXT NOT NULL,
	name        TEXT NOT NULL,
	settings    TEXT NOT NULL DEFAULT '{}',
	secrets     BLOB,
	state       BLOB,
	enabled     INTEGER NOT NULL DEFAULT 1,
	interval_m  INTEGER NOT NULL DEFAULT 360,
	created_at  INTEGER NOT NULL,
	last_run_at INTEGER,
	last_ok_at  INTEGER,
	last_error  TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS destinations (
	id         TEXT PRIMARY KEY,
	kind       TEXT NOT NULL,
	name       TEXT NOT NULL,
	settings   TEXT NOT NULL DEFAULT '{}',
	secrets    BLOB,
	enabled    INTEGER NOT NULL DEFAULT 1,
	created_at INTEGER NOT NULL,
	last_error TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS routes (
	destination_id TEXT NOT NULL REFERENCES destinations(id) ON DELETE CASCADE,
	source_id      TEXT NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
	account        TEXT NOT NULL,
	since          INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (destination_id, source_id)
);
CREATE TABLE IF NOT EXISTS activities (
	id         TEXT PRIMARY KEY,
	source_id  TEXT NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
	native_id  TEXT NOT NULL,
	time       INTEGER NOT NULL,
	type       TEXT NOT NULL,
	data       TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	UNIQUE (source_id, native_id)
);
CREATE INDEX IF NOT EXISTS activities_source_time ON activities(source_id, time);
CREATE TABLE IF NOT EXISTS balances (
	source_id TEXT PRIMARY KEY REFERENCES sources(id) ON DELETE CASCADE,
	data      TEXT NOT NULL,
	at        INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS deliveries (
	destination_id TEXT NOT NULL REFERENCES destinations(id) ON DELETE CASCADE,
	activity_id    TEXT NOT NULL REFERENCES activities(id) ON DELETE CASCADE,
	status         TEXT NOT NULL,
	external_id    TEXT NOT NULL DEFAULT '',
	message        TEXT NOT NULL DEFAULT '',
	at             INTEGER NOT NULL,
	PRIMARY KEY (destination_id, activity_id)
);
CREATE TABLE IF NOT EXISTS runs (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	kind       TEXT NOT NULL,
	subject_id TEXT NOT NULL,
	started_at INTEGER NOT NULL,
	ended_at   INTEGER,
	ok         INTEGER,
	summary    TEXT NOT NULL DEFAULT '',
	log        TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS runs_subject ON runs(subject_id, id);
CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error                   { return s.db.Close() }
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func ms(t time.Time) int64 { return t.UnixMilli() }

func fromMs(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return time.UnixMilli(v.Int64).UTC()
}

// ---- settings

// Setting returns a stored setting, "" when unset.
func (s *Store) Setting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (s *Store) PutSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?)
ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// ---- sources and destinations

// Entity is a configured source or destination.
type Entity struct {
	ID       string            `json:"id"`
	Kind     string            `json:"kind"`
	Name     string            `json:"name"`
	Settings map[string]string `json:"settings"`
	Secrets  []byte            `json:"-"` // sealed JSON map
	State    []byte            `json:"-"`
	Enabled  bool              `json:"enabled"`
	Interval time.Duration     `json:"-"`
	Created  time.Time         `json:"created"`
	LastRun  time.Time         `json:"lastRun"`
	LastOK   time.Time         `json:"lastOk"`
	LastErr  string            `json:"lastError"`
}

func (s *Store) PutSource(ctx context.Context, e *Entity) error {
	settings, _ := json.Marshal(e.Settings)
	if e.Interval == 0 {
		e.Interval = 6 * time.Hour
	}
	if e.Created.IsZero() {
		e.Created = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO sources (id, kind, name, settings, secrets, state, enabled, interval_m, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET name=excluded.name, settings=excluded.settings, secrets=excluded.secrets,
	enabled=excluded.enabled, interval_m=excluded.interval_m`,
		e.ID, e.Kind, e.Name, string(settings), e.Secrets, e.State, e.Enabled, int(e.Interval/time.Minute), ms(e.Created))
	return err
}

const sourceCols = `id, kind, name, settings, secrets, state, enabled, interval_m, created_at, last_run_at, last_ok_at, last_error`

func scanSource(row interface{ Scan(...any) error }) (*Entity, error) {
	var e Entity
	var settings string
	var interval int
	var created int64
	var lastRun, lastOK sql.NullInt64
	if err := row.Scan(&e.ID, &e.Kind, &e.Name, &settings, &e.Secrets, &e.State, &e.Enabled, &interval, &created, &lastRun, &lastOK, &e.LastErr); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	_ = json.Unmarshal([]byte(settings), &e.Settings)
	e.Interval = time.Duration(interval) * time.Minute
	e.Created = time.UnixMilli(created).UTC()
	e.LastRun, e.LastOK = fromMs(lastRun), fromMs(lastOK)
	return &e, nil
}

func (s *Store) Source(ctx context.Context, id string) (*Entity, error) {
	return scanSource(s.db.QueryRowContext(ctx, `SELECT `+sourceCols+` FROM sources WHERE id = ?`, id))
}

func (s *Store) Sources(ctx context.Context) ([]*Entity, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+sourceCols+` FROM sources ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Entity
	for rows.Next() {
		e, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) DeleteSource(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sources WHERE id = ?`, id)
	return err
}

// SourceRan records the outcome of a sync; state is only replaced on success.
func (s *Store) SourceRan(ctx context.Context, id string, at time.Time, runErr error, state []byte) error {
	if runErr != nil {
		_, err := s.db.ExecContext(ctx, `UPDATE sources SET last_run_at = ?, last_error = ? WHERE id = ?`, ms(at), runErr.Error(), id)
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE sources SET last_run_at = ?, last_ok_at = ?, last_error = '', state = ? WHERE id = ?`, ms(at), ms(at), state, id)
	return err
}

func (s *Store) PutDestination(ctx context.Context, e *Entity) error {
	settings, _ := json.Marshal(e.Settings)
	if e.Created.IsZero() {
		e.Created = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO destinations (id, kind, name, settings, secrets, enabled, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET name=excluded.name, settings=excluded.settings, secrets=excluded.secrets, enabled=excluded.enabled`,
		e.ID, e.Kind, e.Name, string(settings), e.Secrets, e.Enabled, ms(e.Created))
	return err
}

const destCols = `id, kind, name, settings, secrets, enabled, created_at, last_error`

func scanDest(row interface{ Scan(...any) error }) (*Entity, error) {
	var e Entity
	var settings string
	var created int64
	if err := row.Scan(&e.ID, &e.Kind, &e.Name, &settings, &e.Secrets, &e.Enabled, &created, &e.LastErr); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	_ = json.Unmarshal([]byte(settings), &e.Settings)
	e.Created = time.UnixMilli(created).UTC()
	return &e, nil
}

func (s *Store) Destination(ctx context.Context, id string) (*Entity, error) {
	return scanDest(s.db.QueryRowContext(ctx, `SELECT `+destCols+` FROM destinations WHERE id = ?`, id))
}

func (s *Store) Destinations(ctx context.Context) ([]*Entity, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+destCols+` FROM destinations ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Entity
	for rows.Next() {
		e, err := scanDest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) DeleteDestination(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM destinations WHERE id = ?`, id)
	return err
}

func (s *Store) DestinationError(ctx context.Context, id, msg string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE destinations SET last_error = ? WHERE id = ?`, msg, id)
	return err
}

// ---- routes: which source goes to which destination account

type Route struct {
	DestinationID string    `json:"destinationId"`
	SourceID      string    `json:"sourceId"`
	Account       string    `json:"account"`
	Since         time.Time `json:"since"` // only deliver activities from this date (zero: all)
}

func (s *Store) PutRoute(ctx context.Context, r Route) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO routes (destination_id, source_id, account, since) VALUES (?, ?, ?, ?)
ON CONFLICT(destination_id, source_id) DO UPDATE SET account=excluded.account, since=excluded.since`,
		r.DestinationID, r.SourceID, r.Account, ms0(r.Since))
	return err
}

func ms0(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func (s *Store) DeleteRoute(ctx context.Context, destID, srcID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM routes WHERE destination_id = ? AND source_id = ?`, destID, srcID)
	return err
}

func (s *Store) Routes(ctx context.Context) ([]Route, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT destination_id, source_id, account, since FROM routes ORDER BY destination_id, source_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Route
	for rows.Next() {
		var r Route
		var since int64
		if err := rows.Scan(&r.DestinationID, &r.SourceID, &r.Account, &since); err != nil {
			return nil, err
		}
		if since > 0 {
			r.Since = time.UnixMilli(since).UTC()
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- activities and balances

// AddActivities inserts new activities and ignores ones already known (same
// source and native id). Returns how many were new.
func (s *Store) AddActivities(ctx context.Context, acts []model.Activity) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO activities (id, source_id, native_id, time, type, data, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	now := ms(time.Now())
	added := 0
	for _, a := range acts {
		data, err := json.Marshal(a)
		if err != nil {
			return 0, err
		}
		res, err := stmt.ExecContext(ctx, a.ID, a.SourceID, a.NativeID, ms(a.Time), string(a.Type), string(data), now)
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			added++
		}
	}
	return added, tx.Commit()
}

// ActivityFilter selects activities; zero fields mean no filter.
type ActivityFilter struct {
	SourceID string
	Since    time.Time
	Limit    int
	Offset   int
	// Undelivered: only those without a delivered/duplicate/skipped result
	// for this destination.
	UndeliveredTo string
	// Newest lists the most recent first (oldest first otherwise).
	Newest bool
}

func (s *Store) Activities(ctx context.Context, f ActivityFilter) ([]model.Activity, error) {
	q := `SELECT a.data FROM activities a WHERE 1=1`
	var args []any
	if f.SourceID != "" {
		q += ` AND a.source_id = ?`
		args = append(args, f.SourceID)
	}
	if !f.Since.IsZero() {
		q += ` AND a.time >= ?`
		args = append(args, ms(f.Since))
	}
	if f.UndeliveredTo != "" {
		q += ` AND NOT EXISTS (SELECT 1 FROM deliveries d WHERE d.activity_id = a.id AND d.destination_id = ? AND d.status != 'failed')`
		args = append(args, f.UndeliveredTo)
	}
	if f.Newest {
		q += ` ORDER BY a.time DESC, a.id DESC`
	} else {
		q += ` ORDER BY a.time, a.id`
	}
	if f.Limit > 0 {
		q += ` LIMIT ? OFFSET ?`
		args = append(args, f.Limit, f.Offset)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Activity
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var a model.Activity
		if err := json.Unmarshal([]byte(data), &a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) CountActivities(ctx context.Context, sourceID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM activities WHERE source_id = ?`, sourceID).Scan(&n)
	return n, err
}

func (s *Store) PutBalances(ctx context.Context, sourceID string, at time.Time, b []model.Balance) error {
	data, err := json.Marshal(b)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO balances (source_id, data, at) VALUES (?, ?, ?)
ON CONFLICT(source_id) DO UPDATE SET data=excluded.data, at=excluded.at`, sourceID, string(data), ms(at))
	return err
}

func (s *Store) Balances(ctx context.Context, sourceID string) ([]model.Balance, time.Time, error) {
	var data string
	var at int64
	err := s.db.QueryRowContext(ctx, `SELECT data, at FROM balances WHERE source_id = ?`, sourceID).Scan(&data, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, time.Time{}, nil
	}
	if err != nil {
		return nil, time.Time{}, err
	}
	var b []model.Balance
	err = json.Unmarshal([]byte(data), &b)
	return b, time.UnixMilli(at).UTC(), err
}

// ---- deliveries

type Delivery struct {
	DestinationID string    `json:"destinationId"`
	ActivityID    string    `json:"activityId"`
	Status        string    `json:"status"`
	ExternalID    string    `json:"externalId,omitempty"`
	Message       string    `json:"message,omitempty"`
	At            time.Time `json:"at"`
}

func (s *Store) PutDeliveries(ctx context.Context, ds []Delivery) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, d := range ds {
		if _, err := tx.ExecContext(ctx, `INSERT INTO deliveries (destination_id, activity_id, status, external_id, message, at)
VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT(destination_id, activity_id) DO UPDATE SET status=excluded.status,
external_id=excluded.external_id, message=excluded.message, at=excluded.at`,
			d.DestinationID, d.ActivityID, d.Status, d.ExternalID, d.Message, ms(d.At)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeliveryCounts returns, for a destination, how many activities are in each status.
func (s *Store) DeliveryCounts(ctx context.Context, destID string) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM deliveries WHERE destination_id = ? GROUP BY status`, destID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n
	}
	return out, rows.Err()
}

// ResetDeliveries forgets what was delivered to a destination (to push again).
func (s *Store) ResetDeliveries(ctx context.Context, destID, sourceID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM deliveries WHERE destination_id = ? AND activity_id IN (SELECT id FROM activities WHERE source_id = ?)`, destID, sourceID)
	return err
}

// ---- run log

type Run struct {
	ID        int64     `json:"id"`
	Kind      string    `json:"kind"` // "sync" or "push"
	SubjectID string    `json:"subjectId"`
	Started   time.Time `json:"started"`
	Ended     time.Time `json:"ended"`
	OK        *bool     `json:"ok"`
	Summary   string    `json:"summary"`
	Log       string    `json:"log,omitempty"`
}

// CloseInterrupted marks runs left open by a stop or crash as failed.
func (s *Store) CloseInterrupted(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE runs SET ended_at = ?, ok = 0,
summary = CASE WHEN summary = '' THEN 'interrupted (Fundarr stopped)' ELSE summary || '; interrupted' END WHERE ended_at IS NULL`, ms(time.Now()))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) StartRun(ctx context.Context, kind, subject string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO runs (kind, subject_id, started_at) VALUES (?, ?, ?)`, kind, subject, ms(time.Now()))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) EndRun(ctx context.Context, id int64, ok bool, summary, log string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE runs SET ended_at = ?, ok = ?, summary = ?, log = ? WHERE id = ?`, ms(time.Now()), ok, summary, log, id)
	if err != nil {
		return err
	}
	// Keep the last 200 runs per subject.
	_, err = s.db.ExecContext(ctx, `DELETE FROM runs WHERE subject_id = (SELECT subject_id FROM runs WHERE id = ?)
AND id NOT IN (SELECT id FROM runs WHERE subject_id = (SELECT subject_id FROM runs WHERE id = ?) ORDER BY id DESC LIMIT 200)`, id, id)
	return err
}

func (s *Store) Runs(ctx context.Context, subject string, limit int) ([]Run, error) {
	q := `SELECT id, kind, subject_id, started_at, ended_at, ok, summary, log FROM runs`
	var args []any
	if subject != "" {
		q += ` WHERE subject_id = ?`
		args = append(args, subject)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var r Run
		var started int64
		var ended sql.NullInt64
		var ok sql.NullBool
		if err := rows.Scan(&r.ID, &r.Kind, &r.SubjectID, &started, &ended, &ok, &r.Summary, &r.Log); err != nil {
			return nil, err
		}
		r.Started = time.UnixMilli(started).UTC()
		r.Ended = fromMs(ended)
		if ok.Valid {
			v := ok.Bool
			r.OK = &v
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
