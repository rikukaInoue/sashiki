// Package state は sashikid の状態を SQLite に保持する(仕様 14-3)。
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
	// sashikid はシングルプロセスで、SQLite への同時書き込みを避ける。
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
CREATE TABLE IF NOT EXISTS operations (
  id          TEXT PRIMARY KEY,
  type        TEXT NOT NULL,          -- create|reset|recreate|delete|wake|baseline-refresh
  target      TEXT NOT NULL,          -- branch 名 or baseline tag
  state       TEXT NOT NULL,          -- running|completed|failed
  started_at  TEXT NOT NULL,
  finished_at TEXT,
  error_json  TEXT
);
CREATE INDEX IF NOT EXISTS idx_operations_target ON operations(target);
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

// UpdateOrigin は branch の origin_snapshot を更新する(recreate 時)。
func (d *DB) UpdateOrigin(name, origin string) error {
	_, err := d.sql.Exec(`UPDATE branches SET origin_snapshot = ? WHERE name = ?`, origin, name)
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

// Token は tokens テーブルの 1 行。
type Token struct {
	Name       string
	CreatedAt  time.Time
	LastUsedAt *time.Time
}

// CreateToken はトークンのハッシュを保存する。同名は上書きしない。
func (d *DB) CreateToken(name, hash string) error {
	_, err := d.sql.Exec(
		`INSERT INTO tokens (name, hash, created_at) VALUES (?, ?, ?)`,
		name, hash, time.Now().UTC().Format(timeFmt))
	return err
}

// RevokeToken は削除する。
func (d *DB) RevokeToken(name string) error {
	res, err := d.sql.Exec(`DELETE FROM tokens WHERE name = ?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListTokens は一覧(ハッシュは返さない)。
func (d *DB) ListTokens() ([]Token, error) {
	rows, err := d.sql.Query(`SELECT name, created_at, last_used_at FROM tokens ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Token
	for rows.Next() {
		var t Token
		var created string
		var lastUsed sql.NullString
		if err := rows.Scan(&t.Name, &created, &lastUsed); err != nil {
			return nil, err
		}
		if ts, err := time.Parse(timeFmt, created); err == nil {
			t.CreatedAt = ts
		}
		if lastUsed.Valid {
			if ts, err := time.Parse(timeFmt, lastUsed.String); err == nil {
				t.LastUsedAt = &ts
			}
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// CheckTokenHash はハッシュが登録済みなら true を返し、last_used_at を更新する。
func (d *DB) CheckTokenHash(hash string) (bool, error) {
	res, err := d.sql.Exec(`UPDATE tokens SET last_used_at = ? WHERE hash = ?`,
		time.Now().UTC().Format(timeFmt), hash)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SetCurrentBaseline は snapshot を登録して current に切り替える。
func (d *DB) SetCurrentBaseline(snapshot string) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`UPDATE baselines SET is_current = 0`); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`INSERT INTO baselines (snapshot, created_at, is_current) VALUES (?, ?, 1)
		 ON CONFLICT(snapshot) DO UPDATE SET is_current = 1`,
		snapshot, time.Now().UTC().Format(timeFmt)); err != nil {
		return err
	}
	return tx.Commit()
}

// CurrentBaselineOverride は DB に記録された current baseline を返す(無ければ false)。
func (d *DB) CurrentBaselineOverride() (string, bool) {
	var snap string
	err := d.sql.QueryRow(`SELECT snapshot FROM baselines WHERE is_current = 1`).Scan(&snap)
	if err != nil {
		return "", false
	}
	return snap, true
}

// Operation は operations テーブルの 1 行。
type Operation struct {
	ID         string
	Type       string
	Target     string
	State      string // running|completed|failed
	StartedAt  time.Time
	FinishedAt *time.Time
	Error      string
}

// Operation の state 値。
const (
	OpRunning   = "running"
	OpCompleted = "completed"
	OpFailed    = "failed"
)

// CreateOperation は running 状態の operation を挿入する。
func (d *DB) CreateOperation(id, typ, target string) error {
	_, err := d.sql.Exec(
		`INSERT INTO operations (id, type, target, state, started_at) VALUES (?, ?, ?, ?, ?)`,
		id, typ, target, OpRunning, time.Now().UTC().Format(timeFmt))
	return err
}

// FinishOperation は operation を completed/failed にする。errMsg が空なら completed。
func (d *DB) FinishOperation(id, errMsg string) error {
	st := OpCompleted
	var e any
	if errMsg != "" {
		st = OpFailed
		e = errMsg
	}
	_, err := d.sql.Exec(
		`UPDATE operations SET state = ?, finished_at = ?, error_json = ? WHERE id = ?`,
		st, time.Now().UTC().Format(timeFmt), e, id)
	return err
}

// GetOperation は 1 件取得。
func (d *DB) GetOperation(id string) (Operation, error) {
	row := d.sql.QueryRow(
		`SELECT id, type, target, state, started_at, finished_at, COALESCE(error_json,'')
		 FROM operations WHERE id = ?`, id)
	return scanOperation(row)
}

// ListOperations は新しい順に最大 limit 件返す。
func (d *DB) ListOperations(limit int) ([]Operation, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := d.sql.Query(
		`SELECT id, type, target, state, started_at, finished_at, COALESCE(error_json,'')
		 FROM operations ORDER BY started_at DESC, rowid DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Operation
	for rows.Next() {
		op, err := scanOperation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	return out, rows.Err()
}

func scanOperation(row scannable) (Operation, error) {
	var o Operation
	var started string
	var finished sql.NullString
	err := row.Scan(&o.ID, &o.Type, &o.Target, &o.State, &started, &finished, &o.Error)
	if errors.Is(err, sql.ErrNoRows) {
		return o, ErrNotFound
	}
	if err != nil {
		return o, err
	}
	if t, err := time.Parse(timeFmt, started); err == nil {
		o.StartedAt = t
	}
	if finished.Valid {
		if t, err := time.Parse(timeFmt, finished.String); err == nil {
			o.FinishedAt = &t
		}
	}
	return o, nil
}
