// Package hooks は on-create / on-reset / on-delete フックの実行(仕様 14-2)。
// 自社固有処理(マイグレーション適用など)をコアの外に追い出すための仕組み。
package hooks

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// Event はフックの種類。
type Event string

// フックイベント。
const (
	OnCreate Event = "on-create"
	OnReset  Event = "on-reset"
	OnDelete Event = "on-delete"
)

// Env はフックに環境変数で渡す情報。
type Env struct {
	Branch         string
	Port           int
	Socket         string
	DataDir        string
	EngineType     string
	AdminUser      string
	OriginSnapshot string
	StateDir       string
}

// Result はフック実行結果。
type Result struct {
	Ran      bool // フックが存在して実行された
	ExitCode int
	LogPath  string
}

// Runner はフックを見つけて実行する。
type Runner struct {
	Dir     string        // /etc/twig/hooks
	LogDir  string        // /var/log/twig/hooks
	Timeout time.Duration // 既定 10 分
	// now はテストで固定するための時計。
	now func() time.Time
}

// NewRunner は Runner を作る。
func NewRunner(dir, logDir string, timeout time.Duration) *Runner {
	if timeout == 0 {
		timeout = 10 * time.Minute
	}
	return &Runner{Dir: dir, LogDir: logDir, Timeout: timeout, now: time.Now}
}

// Find はイベント名に一致する実行可能ファイルを探す(拡張子は見ない)。
func (r *Runner) Find(event Event) (string, bool) {
	return r.find(event)
}

// find はイベント名に一致する実行可能ファイルを探す(拡張子は見ない)。
func (r *Runner) find(event Event) (string, bool) {
	entries, err := os.ReadDir(r.Dir)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		base := name
		if ext := filepath.Ext(name); ext != "" {
			base = name[:len(name)-len(ext)]
		}
		if base != string(event) && name != string(event) {
			continue
		}
		p := filepath.Join(r.Dir, name)
		if info, err := os.Stat(p); err == nil && info.Mode()&0o111 != 0 {
			return p, true
		}
	}
	return "", false
}

// Run はフックを実行する。フックが無ければ Ran=false で成功扱い。
// stdout/stderr は LogDir に保存する。
func (r *Runner) Run(ctx context.Context, event Event, env Env) (Result, error) {
	path, ok := r.find(event)
	if !ok {
		return Result{Ran: false, ExitCode: 0}, nil
	}

	logPath := filepath.Join(r.LogDir,
		fmt.Sprintf("%s-%s-%s.log", env.Branch, event, r.now().UTC().Format("20060102T150405Z")))
	if err := os.MkdirAll(r.LogDir, 0o755); err != nil {
		return Result{}, err
	}
	logFile, err := os.Create(logPath)
	if err != nil {
		return Result{}, err
	}
	defer logFile.Close()

	cctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, path)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Env = append(os.Environ(),
		"TWIG_EVENT="+string(event),
		"TWIG_BRANCH="+env.Branch,
		fmt.Sprintf("TWIG_PORT=%d", env.Port),
		"TWIG_SOCKET="+env.Socket,
		"TWIG_DATADIR="+env.DataDir,
		"TWIG_ENGINE="+env.EngineType,
		"TWIG_ADMIN_USER="+env.AdminUser,
		"TWIG_ORIGIN_SNAPSHOT="+env.OriginSnapshot,
		"TWIG_STATE_DIR="+env.StateDir,
	)

	runErr := cmd.Run()
	res := Result{Ran: true, LogPath: logPath}
	if runErr != nil {
		if ee, ok := runErr.(*exec.ExitError); ok {
			res.ExitCode = ee.ExitCode()
			return res, fmt.Errorf("hook %s exited %d (log: %s)", event, res.ExitCode, logPath)
		}
		res.ExitCode = -1
		return res, fmt.Errorf("hook %s: %w (log: %s)", event, runErr, logPath)
	}
	return res, nil
}
