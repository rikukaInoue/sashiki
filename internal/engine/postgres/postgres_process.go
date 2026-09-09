package postgres

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/rikukaInoue/sashiki/internal/engine"
)

// process モード(#227): systemd を使わずに postgres を直接起動する。
// macOS ネイティブ(Homebrew の postgresql)や、systemd の無いコンテナ向け。
//
// 起動/停止は pg_ctl に任せる。postgres 本体を自前 spawn するより、
// pidfile(postmaster.pid)の管理と graceful shutdown(-m fast)を
// pg_ctl が正しくやってくれるぶん安全なため。
//
// 重要: pg_ctl start は postgres をデーモン化するが、-l を渡さないと
// サーバは親から継いだ stdout/stderr を握ったまま生き続ける。出力をパイプで
// 捕まえる実行方法(CombinedOutput)だとパイプが閉じず永久にブロックするので、
// 必ず -l でログをファイルへ逃がし、かつパイプを使わずに起動する(#223 で
// baseline import が CI を 13 分ハングさせた原因と同じ)。

// pidPath は postgres が書く pidfile。datadir 直下に固定で置かれる。
func (e *Engine) pidPath(ins engine.Instance) string {
	return filepath.Join(ins.DataDir, "postmaster.pid")
}

// logPath はブランチごとのサーバログ。datadir の中に置くと snapshot に
// 入ってしまうので外に出す。
func (e *Engine) logPath(ins engine.Instance) string {
	dir := e.cfg.LogDir
	if dir == "" {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "postgres-"+ins.Branch+".log")
}

func (e *Engine) pgCtl() string { return filepath.Join(e.cfg.BinDir, "pg_ctl") }

// startOptions は postgres に渡すオプション列(pg_ctl -o の中身)。
func (e *Engine) startOptions(ins engine.Instance) string {
	opts := []string{
		"-p " + strconv.Itoa(ins.Port),
		"-c listen_addresses=" + e.cfg.ListenAddresses,
		// unix socket は /tmp に置く。datadir 配下だと snapshot に入り、
		// クローン先で古い socket ファイルが残る。
		"-c unix_socket_directories=" + os.TempDir(),
	}
	if e.cfg.SharedBuffers != "" {
		opts = append(opts, "-c shared_buffers="+e.cfg.SharedBuffers)
	}
	return strings.Join(opts, " ")
}

// startProcess は pg_ctl start でブランチの postgres を起動する。
func (e *Engine) startProcess(ctx context.Context, ins engine.Instance) error {
	if err := os.MkdirAll(filepath.Dir(e.logPath(ins)), 0o755); err != nil {
		return fmt.Errorf("ensure log dir: %w", err)
	}
	args := []string{"start", "-D", ins.DataDir, "-o", e.startOptions(ins),
		"-l", e.logPath(ins), "-w", "-t", strconv.Itoa(int(e.cfg.ReadyTimeout.Seconds()))}
	if err := e.runProcess(ctx, args...); err != nil {
		return fmt.Errorf("pg_ctl start (詳細は %s): %w", e.logPath(ins), err)
	}
	return nil
}

// stopProcess は fast shutdown で正常終了させる。snapshot の一貫性はこれに依存
// するので、immediate ではなく必ず fast を使う。
func (e *Engine) stopProcess(ctx context.Context, ins engine.Instance) error {
	if !e.isRunningProcess(ins) {
		return nil
	}
	return e.runProcess(ctx, "stop", "-D", ins.DataDir, "-m", "fast", "-w",
		"-t", strconv.Itoa(int(e.cfg.ReadyTimeout.Seconds())))
}

// killProcess は immediate shutdown。dirty state を捨てる rollback 直前用で、
// snapshot 前には使わない。
func (e *Engine) killProcess(ctx context.Context, ins engine.Instance) error {
	if !e.isRunningProcess(ins) {
		return nil
	}
	// immediate でも postgres は pidfile を片付けて終わる。取りこぼした場合に
	// 備えて、消えるまで少しだけ待つ(datadir を掴んだままの rollback を避ける)。
	_ = e.runProcess(ctx, "stop", "-D", ins.DataDir, "-m", "immediate", "-w", "-t", "10")
	if !e.waitGone(ins, 10*time.Second) {
		if pid, ok := e.readPid(ins); ok {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			e.waitGone(ins, 5*time.Second)
		}
	}
	return nil
}

// isRunningProcess は pidfile とプロセスの存在で判定する。
func (e *Engine) isRunningProcess(ins engine.Instance) bool {
	pid, ok := e.readPid(ins)
	if !ok {
		return false
	}
	// シグナル 0 は存在確認だけ行う。
	return syscall.Kill(pid, 0) == nil
}

// readPid は postmaster.pid の 1 行目(PID)を読む。
func (e *Engine) readPid(ins engine.Instance) (int, bool) {
	b, err := os.ReadFile(e.pidPath(ins))
	if err != nil {
		return 0, false
	}
	lines := strings.SplitN(strings.TrimSpace(string(b)), "\n", 2)
	pid, err := strconv.Atoi(strings.TrimSpace(lines[0]))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

func (e *Engine) waitGone(ins engine.Instance, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !e.isRunningProcess(ins) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return !e.isRunningProcess(ins)
}

// runProcess は pg_ctl を実行する。出力をパイプで捕まえないのが要点(冒頭の
// コメント参照)。root で動いている場合は postgres が root 起動を拒むため
// run_user へ降格する。
func (e *Engine) runProcess(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, e.pgCtl(), args...)
	cmd.Stdout = os.Stderr // pg_ctl 自身の進捗はデーモンのログに混ぜず stderr へ
	cmd.Stderr = os.Stderr
	if os.Geteuid() == 0 {
		runUser := e.cfg.RunUser
		if runUser == "" {
			runUser = "postgres"
		}
		uid, gid, err := lookupUser(runUser)
		if err != nil {
			return err
		}
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Credential: &syscall.Credential{Uid: uid, Gid: gid},
		}
		// pg_ctl は $HOME を触ることがあるので、降格先が書ける場所を指す。
		cmd.Env = append(os.Environ(), "HOME="+os.TempDir())
	}
	return cmd.Run()
}

func lookupUser(name string) (uint32, uint32, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return 0, 0, fmt.Errorf("run_user %q が見つかりません: %w", name, err)
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	return uint32(uid), uint32(gid), nil
}
