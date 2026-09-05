// Package state は twigd の状態を SQLite に保持する(仕様 14-3)。
// used_bytes は保存しない(zfs get を都度引く)。
package state

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // pure Go SQLite driver
)

// Branch の state 値。
const (
	StateCreating  = "creating"
	StateRunning   = "running"
	StateSleeping  = "sleeping"
	StateResetting = "resetting"
	StateDeleting  = "deleting"
	StateError     = "error"
)

// ErrNotFound はブランチが存在しないとき。
var ErrNotFound = errors.New("branch not found")

// Branch は state.db の branches 1 行。
type Branch struct {
	Name           string
	State          string
	Port           int
	OriginSnapshot string
	CreatedAt      time.Time
	LastConnAt     *time.Time
	ErrorMessage   string
}

// HookRun は hook 実行記録。
type HookRun struct {
	ID         int64
	Branch     string
	Event      string
	StartedAt  time.Time
	FinishedAt *time.Time
	ExitCode   *int
	LogPath    string
}

// DB は state.db へのハンドル。
type DB struct {
	sql *sql.DB
}

// Open は SQLite を開き、スキーマを作成する。
func Open(path string) (*DB, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	// twigd はシングルプロセスで、SQLite への同時書き込みを避ける。
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("init schema: %w", err)
	}
	return &DB{sql: db}, nil
}

const schema = `
CREATE TABLE IF NOT EXISTS branches (
  name             TEXT PRIMARY KEY,
  state            TEXT NOT NULL,
  port             INTEGER NOT NULL UNIQUE,
  origin_snapshot  TEXT NOT NULL,
  created_at       TEXT NOT NULL,
  last_conn_at     TEXT,
  error_message    TEXT
);
CREATE TABLE IF NOT EXISTS hook_runs (
  id          INTEGER PRIMARY KEY,
  branch      TEXT NOT NULL,
  event       TEXT NOT NULL,
  started_at  TEXT NOT NULL,
  finished_at TEXT,
  exit_code   INTEGER,
  log_path    TEXT
);
CREATE TABLE IF NOT EXISTS tokens (
  name         TEXT PRIMARY KEY,
  hash         TEXT NOT NULL,
  created_at   TEXT NOT NULL,
  last_used_at TEXT
);
CREATE TABLE IF NOT EXISTS baselines (
  snapshot    TEXT PRIMARY KEY,
  created_at  TEXT NOT NULL,
  is_current  INTEGER NOT NULL DEFAULT 0
);
`

// Close は DB を閉じる。
func (d *DB) Close() error { return d.sql.Close() }

const timeFmt = time.RFC3339

// CreateBranch は新しいブランチ行を creating 状態で挿入する。
func (d *DB) CreateBranch(name string, port int, origin string) error {
	_, err := d.sql.Exec(
		`INSERT INTO branches (name, state, port, origin_snapshot, created_at) VALUES (?, ?, ?, ?, ?)`,
		name, StateCreating, port, origin, time.Now().UTC().Format(timeFmt))
	return err
}

// SetState は状態遷移を記録する。error 状態のときは message も残す。
func (d *DB) SetState(name, st, errMsg string) error {
	res, err := d.sql.Exec(`UPDATE branches SET state = ?, error_message = ? WHERE name = ?`, st, errMsg, name)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// TouchLastConn は最終接続時刻を更新する。
func (d *DB) TouchLastConn(name string) error {
	_, err := d.sql.Exec(`UPDATE branches SET last_conn_at = ? WHERE name = ?`,
		time.Now().UTC().Format(timeFmt), name)
	return err
}

// DeleteBranch は行を削除する。
func (d *DB) DeleteBranch(name string) error {
	_, err := d.sql.Exec(`DELETE FROM branches WHERE name = ?`, name)
	return err
}

// GetBranch は 1 件取得。無ければ ErrNotFound。
func (d *DB) GetBranch(name string) (Branch, error) {
	row := d.sql.QueryRow(
		`SELECT name, state, port, origin_snapshot, created_at, last_conn_at, COALESCE(error_message,'')
		 FROM branches WHERE name = ?`, name)
	return scanBranch(row)
}

// ListBranches は作成順で全件返す。
func (d *DB) ListBranches() ([]Branch, error) {
	rows, err := d.sql.Query(
		`SELECT name, state, port, origin_snapshot, created_at, last_conn_at, COALESCE(error_message,'')
		 FROM branches ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Branch
	for rows.Next() {
		b, err := scanBranch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// UsedPorts は使用中ポートの集合。
func (d *DB) UsedPorts() (map[int]bool, error) {
	rows, err := d.sql.Query(`SELECT port FROM branches`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	used := map[int]bool{}
	for rows.Next() {
		var p int
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		used[p] = true
	}
	return used, rows.Err()
}

type scannable interface{ Scan(dest ...any) error }

func scanBranch(row scannable) (Branch, error) {
	var b Branch
	var created string
	var lastConn sql.NullString
	err := row.Scan(&b.Name, &b.State, &b.Port, &b.OriginSnapshot, &created, &lastConn, &b.ErrorMessage)
	if errors.Is(err, sql.ErrNoRows) {
		return b, ErrNotFound
	}
	if err != nil {
		return b, err
	}
	if t, err := time.Parse(timeFmt, created); err == nil {
		b.CreatedAt = t
	}
	if lastConn.Valid {
		if t, err := time.Parse(timeFmt, lastConn.String); err == nil {
			b.LastConnAt = &t
		}
	}
	return b, nil
}

// RecordHookStart は hook 実行開始を記録し、行 ID を返す。
func (d *DB) RecordHookStart(branch, event, logPath string) (int64, error) {
	res, err := d.sql.Exec(
		`INSERT INTO hook_runs (branch, event, started_at, log_path) VALUES (?, ?, ?, ?)`,
		branch, event, time.Now().UTC().Format(timeFmt), logPath)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// RecordHookFinish は hook 実行終了を記録する。
func (d *DB) RecordHookFinish(id int64, exitCode int) error {
	_, err := d.sql.Exec(
		`UPDATE hook_runs SET finished_at = ?, exit_code = ? WHERE id = ?`,
		time.Now().UTC().Format(timeFmt), exitCode, id)
	return err
}

// LastHookStatus はブランチの各 event の最新 exit code を返す(API の hook_status 用)。
func (d *DB) LastHookStatus(branch string) (map[string]string, error) {
	rows, err := d.sql.Query(
		`SELECT event, exit_code FROM hook_runs
		 WHERE branch = ? AND id IN (SELECT MAX(id) FROM hook_runs WHERE branch = ? GROUP BY event)`,
		branch, branch)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var event string
		var code sql.NullInt64
		if err := rows.Scan(&event, &code); err != nil {
			return nil, err
		}
		switch {
		case !code.Valid:
			out[event] = "running"
		case code.Int64 == 0:
			out[event] = "ok"
		default:
			out[event] = fmt.Sprintf("failed(%d)", code.Int64)
		}
	}
	return out, rows.Err()
}
