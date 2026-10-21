// Package registry is the controller's SQLite store: adopted nodes, presets,
// operators. One file, copied for backup.
package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Node is an adopted device: a speaker node (role "node") or an audio
// source (role "source": a source board, or Civitone Source on a computer).
type Node struct {
	DeviceID  string
	Role      string // node | source
	Name      string
	Zone      int
	Token     string // SHA-256 hex of the device token issued at adoption
	SnapID    string // snapclient id (MAC) once seen
	Volume    int
	StreamID  string   // sources: the snapserver stream they feed
	Port      int      // sources: snapserver TCP source port
	BufferMs  int      // sources: jitter buffer target; 0 = default
	GainDB    int      // sources: makeup gain before the limiter
	Paging    bool     // sources: its push-to-talk pages every speaker
	Speakers  Speakers // nodes: trim and mute for each of its two speakers
	AdoptedAt time.Time
	LastSeen  time.Time
}

// Speaker is one output of a speaker node. A is the left channel, B the
// right; each drives its own speaker on the pole.
type Speaker struct {
	TrimDB float64 `json:"trim_db"` // 0 to -24, below the zone volume
	Muted  bool    `json:"muted"`
}

type Speakers struct {
	A Speaker `json:"a"`
	B Speaker `json:"b"`
}

type Registry struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS nodes (
  device_id  TEXT PRIMARY KEY,
  name       TEXT NOT NULL DEFAULT '',
  zone       INTEGER NOT NULL DEFAULT 0,
  token      TEXT NOT NULL DEFAULT '',
  snap_id    TEXT NOT NULL DEFAULT '',
  volume     INTEGER NOT NULL DEFAULT 60,
  adopted_at INTEGER,
  last_seen  INTEGER
);
CREATE TABLE IF NOT EXISTS presets (
  name  TEXT PRIMARY KEY,
  body  TEXT NOT NULL            -- JSON: {zone: stream, ...}, profile
);
CREATE TABLE IF NOT EXISTS operators (
  name  TEXT PRIMARY KEY,
  role  TEXT NOT NULL,           -- owner | operator | mute_only
  pin   TEXT NOT NULL            -- hashed
);
CREATE TABLE IF NOT EXISTS adopt_codes (
  code        TEXT PRIMARY KEY,  -- normalized, no dash
  created_at  INTEGER NOT NULL,
  expires_at  INTEGER NOT NULL,
  auto        INTEGER NOT NULL DEFAULT 0,  -- approve nodes without asking
  uses        INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS settings (
  key    TEXT PRIMARY KEY,
  value  TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS events (
  at     INTEGER NOT NULL,
  who    TEXT NOT NULL,
  level  TEXT NOT NULL,          -- info | warn | bad | ops
  text   TEXT NOT NULL
);
`

func Open(path string) (*Registry, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	// Columns added after the first release; ALTER fails harmlessly when
	// they already exist.
	for _, col := range []string{
		`ALTER TABLE nodes ADD COLUMN role TEXT NOT NULL DEFAULT 'node'`,
		`ALTER TABLE nodes ADD COLUMN stream_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE nodes ADD COLUMN port INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE nodes ADD COLUMN buffer_ms INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE nodes ADD COLUMN gain_db INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE nodes ADD COLUMN paging INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE nodes ADD COLUMN speakers TEXT NOT NULL DEFAULT ''`, // JSON Speakers; '' = both at 0 dB
	} {
		if _, err := db.Exec(col); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			db.Close()
			return nil, err
		}
	}
	return &Registry{db: db}, nil
}

func (r *Registry) Close() error { return r.db.Close() }

const nodeCols = `device_id,role,name,zone,token,snap_id,volume,stream_id,port,buffer_ms,gain_db,paging,speakers,COALESCE(adopted_at,0),COALESCE(last_seen,0)`

func scanNode(row interface{ Scan(...any) error }) (Node, error) {
	var n Node
	var adopted, seen int64
	var speakers string
	err := row.Scan(&n.DeviceID, &n.Role, &n.Name, &n.Zone, &n.Token, &n.SnapID, &n.Volume, &n.StreamID, &n.Port, &n.BufferMs, &n.GainDB, &n.Paging, &speakers, &adopted, &seen)
	if speakers != "" {
		json.Unmarshal([]byte(speakers), &n.Speakers)
	}
	if adopted > 0 {
		n.AdoptedAt = time.Unix(adopted, 0)
	}
	if seen > 0 {
		n.LastSeen = time.Unix(seen, 0)
	}
	return n, err
}

// Node returns one adopted device, or ok=false if it was never adopted here.
func (r *Registry) Node(ctx context.Context, deviceID string) (n Node, ok bool, err error) {
	n, err = scanNode(r.db.QueryRowContext(ctx, `SELECT `+nodeCols+` FROM nodes WHERE device_id=?`, deviceID))
	if err == sql.ErrNoRows {
		return Node{}, false, nil
	}
	return n, err == nil, err
}

// ByRole lists adopted devices of one role, oldest adoption first.
func (r *Registry) ByRole(ctx context.Context, role string) ([]Node, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+nodeCols+` FROM nodes WHERE role=? ORDER BY adopted_at, device_id`, role)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Node{}
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// Adopt records a device with the hash of its new device token, replacing
// any earlier adoption of the same device (a source keeps its stream).
func (r *Registry) Adopt(ctx context.Context, deviceID, role, name, tokenHash string) error {
	_, err := r.db.ExecContext(ctx, `
INSERT INTO nodes (device_id,role,name,token,adopted_at) VALUES (?,?,?,?,?)
ON CONFLICT(device_id) DO UPDATE SET role=excluded.role, name=excluded.name, token=excluded.token, adopted_at=excluded.adopted_at`,
		deviceID, role, name, tokenHash, time.Now().Unix())
	return err
}

// SetStream records which snapserver stream and port a source feeds.
func (r *Registry) SetStream(ctx context.Context, deviceID, streamID string, port int) error {
	_, err := r.db.ExecContext(ctx, `UPDATE nodes SET stream_id=?, port=? WHERE device_id=?`, streamID, port, deviceID)
	return err
}

// SetBuffer records a source's jitter buffer target.
func (r *Registry) SetBuffer(ctx context.Context, deviceID string, ms int) error {
	_, err := r.db.ExecContext(ctx, `UPDATE nodes SET buffer_ms=? WHERE device_id=?`, ms, deviceID)
	return err
}

// SetGain records a source's makeup gain.
func (r *Registry) SetGain(ctx context.Context, deviceID string, db int) error {
	_, err := r.db.ExecContext(ctx, `UPDATE nodes SET gain_db=? WHERE device_id=?`, db, deviceID)
	return err
}

// SetPaging marks a source as the paging mic, or not.
func (r *Registry) SetPaging(ctx context.Context, deviceID string, on bool) error {
	_, err := r.db.ExecContext(ctx, `UPDATE nodes SET paging=? WHERE device_id=?`, on, deviceID)
	return err
}

// SetSpeakers records a node's speaker trims and mutes.
func (r *Registry) SetSpeakers(ctx context.Context, deviceID string, s Speakers) error {
	b, _ := json.Marshal(s)
	_, err := r.db.ExecContext(ctx, `UPDATE nodes SET speakers=? WHERE device_id=?`, string(b), deviceID)
	return err
}

// Setting reads a controller setting, or def when unset.
func (r *Registry) Setting(ctx context.Context, key, def string) string {
	var v string
	if err := r.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, key).Scan(&v); err != nil {
		return def
	}
	return v
}

// SetSetting stores a controller setting.
func (r *Registry) SetSetting(ctx context.Context, key, value string) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO settings (key,value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// FreePort returns the lowest source port at or above base not in use.
func (r *Registry) FreePort(ctx context.Context, base int) (int, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT port FROM nodes WHERE port >= ? ORDER BY port`, base)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	p := base
	for rows.Next() {
		var used int
		if err := rows.Scan(&used); err != nil {
			return 0, err
		}
		if used == p {
			p++
		} else if used > p {
			break
		}
	}
	return p, rows.Err()
}

// Forget deletes a node; its token stops working.
func (r *Registry) Forget(ctx context.Context, deviceID string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM nodes WHERE device_id=?`, deviceID)
	return err
}

// Rename sets a node's display name.
func (r *Registry) Rename(ctx context.Context, deviceID, name string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE nodes SET name=? WHERE device_id=?`, name, deviceID)
	return err
}

// AdoptCode is a code an operator hands to nodes during setup.
type AdoptCode struct {
	Code      string    `json:"code"` // normalized, no dash
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Auto      bool      `json:"auto_approve"`
	Uses      int       `json:"uses"`
}

func (r *Registry) CreateCode(ctx context.Context, c AdoptCode) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO adopt_codes (code,created_at,expires_at,auto) VALUES (?,?,?,?)`,
		c.Code, c.CreatedAt.Unix(), c.ExpiresAt.Unix(), c.Auto)
	return err
}

// ActiveCodes lists unexpired codes, newest first, and deletes expired ones.
func (r *Registry) ActiveCodes(ctx context.Context) ([]AdoptCode, error) {
	now := time.Now().Unix()
	if _, err := r.db.ExecContext(ctx, `DELETE FROM adopt_codes WHERE expires_at <= ?`, now); err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx, `SELECT code,created_at,expires_at,auto,uses FROM adopt_codes ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AdoptCode{}
	for rows.Next() {
		var c AdoptCode
		var created, expires int64
		if err := rows.Scan(&c.Code, &created, &expires, &c.Auto, &c.Uses); err != nil {
			return nil, err
		}
		c.CreatedAt, c.ExpiresAt = time.Unix(created, 0), time.Unix(expires, 0)
		out = append(out, c)
	}
	return out, rows.Err()
}

// UseCode counts one use of an unexpired code and reports whether it exists
// and auto-approves.
func (r *Registry) UseCode(ctx context.Context, code string) (ok, auto bool, err error) {
	res, err := r.db.ExecContext(ctx, `UPDATE adopt_codes SET uses = uses + 1 WHERE code = ? AND expires_at > ?`, code, time.Now().Unix())
	if err != nil {
		return false, false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, false, nil
	}
	err = r.db.QueryRowContext(ctx, `SELECT auto FROM adopt_codes WHERE code = ?`, code).Scan(&auto)
	return err == nil, auto, err
}

func (r *Registry) RevokeCode(ctx context.Context, code string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM adopt_codes WHERE code = ?`, code)
	return err
}

func (r *Registry) Log(ctx context.Context, who, level, text string) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO events (at,who,level,text) VALUES (?,?,?,?)`, time.Now().Unix(), who, level, text)
	return err
}

// Event is one row of the activity log.
type Event struct {
	At    time.Time `json:"at"`
	Who   string    `json:"who"`
	Level string    `json:"level"`
	Text  string    `json:"text"`
}

// Events returns the newest events first, at most limit.
func (r *Registry) Events(ctx context.Context, limit int) ([]Event, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT at,who,level,text FROM events ORDER BY at DESC, rowid DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		var at int64
		if err := rows.Scan(&at, &e.Who, &e.Level, &e.Text); err != nil {
			return nil, err
		}
		e.At = time.Unix(at, 0)
		out = append(out, e)
	}
	return out, rows.Err()
}
