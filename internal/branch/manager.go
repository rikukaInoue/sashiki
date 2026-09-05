// Package branch はブランチのライフサイクル(create / reset / delete / list)を
// 司るコア。storage と engine はインターフェースで受け、速度の仮定を持たない。
package branch

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"time"

	"github.com/rikukaInoue/twig/internal/engine"
	"github.com/rikukaInoue/twig/internal/hooks"
	"github.com/rikukaInoue/twig/internal/state"
	"github.com/rikukaInoue/twig/internal/storage"
)

// エラー種別(API 層で HTTP ステータスに写像する)。
var (
	ErrInvalidName  = errors.New("invalid branch name")
	ErrExists       = errors.New("branch already exists")
	ErrNotFound     = state.ErrNotFound
	ErrLimitReached = errors.New("branch limit reached")
	ErrNoFreePort   = errors.New("no free port in range")
)

// BaselineProvider は現在のベースライン snapshot を返す。
type BaselineProvider interface {
	CurrentBaseline() storage.SnapshotRef
}

// Config は Manager の設定。
type Config struct {
	NamePattern string
	MaxBranches int
	PortLow     int
	PortHigh    int
	EngineType  string
	StateDir    string // hook 用の作業ディレクトリの親(/var/lib/twig/branches)

	// LazyCreate: プロキシに未知のブランチ名で接続が来たとき自動作成する。
	// バックエンドの TypicalCreate が LazyMaxWait を超える場合は無効(仕様 15-3)。
	LazyCreate  bool
	LazyMaxWait time.Duration

	// リーパー(reaper.go)
	IdleStopAfter   time.Duration // 0 = アイドル停止しない
	DeleteAfterIdle time.Duration // 0 = 自動削除しない

	// ActiveConns はブランチの現在の接続数(プロキシが提供)。nil なら常に 0 扱い。
	// last_conn_at は接続開始時刻しか進まないため、長寿命接続を張ったまま
	// 使用中のブランチをリーパーが停止・削除しないための判定に使う。
	ActiveConns func(name string) int

	// メモリガード: create 時に空きメモリを確認する。AvailableMem が nil なら無効。
	// OOM killer は新しいブランチではなく既存の無関係な mysqld を殺す(PoC 実測)ため、
	// 事後の監視ではなく事前の拒否で守る。
	AvailableMem    func() (int64, error)
	BufferPoolBytes int64
}

// Manager はブランチライフサイクルの実装。
type Manager struct {
	cfg      Config
	st       storage.Storage
	baseline BaselineProvider
	eng      engine.Engine
	hooks    *hooks.Runner
	db       *state.DB
	nameRe   *regexp.Regexp

	// ブランチ名ごとの直列化(同名の同時 create/delete を防ぐ)
	mu    sync.Mutex
	locks map[string]*sync.Mutex

	// TouchConn のスロットリング
	touchMu   sync.Mutex
	lastTouch map[string]time.Time
}

// New は Manager を作る。
func New(cfg Config, st storage.Storage, bp BaselineProvider, eng engine.Engine, hr *hooks.Runner, db *state.DB) (*Manager, error) {
	re, err := regexp.Compile(cfg.NamePattern)
	if err != nil {
		return nil, fmt.Errorf("name pattern: %w", err)
	}
	return &Manager{
		cfg: cfg, st: st, baseline: bp, eng: eng, hooks: hr, db: db,
		nameRe: re, locks: map[string]*sync.Mutex{},
		lastTouch: map[string]time.Time{},
	}, nil
}

func (m *Manager) lock(name string) func() {
	m.mu.Lock()
	l, ok := m.locks[name]
	if !ok {
		l = &sync.Mutex{}
		m.locks[name] = l
	}
	m.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// Info は API に返すブランチ情報。
type Info struct {
	state.Branch
	UsedBytes  int64
	HookStatus map[string]string
}

func (m *Manager) instance(b state.Branch, vol storage.Volume) engine.Instance {
	return engine.Instance{Branch: b.Name, DataDir: vol.Path + "/data", Port: b.Port}
}

func (m *Manager) hookEnv(b state.Branch, vol storage.Volume) hooks.Env {
	return hooks.Env{
		Branch:         b.Name,
		Port:           b.Port,
		Socket:         "/tmp/mysql-" + b.Name + ".sock",
		DataDir:        vol.Path + "/data",
		EngineType:     m.cfg.EngineType,
		AdminUser:      "root",
		OriginSnapshot: b.OriginSnapshot,
		StateDir:       m.cfg.StateDir + "/" + b.Name,
	}
}

func (m *Manager) allocPort(requested int) (int, error) {
	used, err := m.db.UsedPorts()
	if err != nil {
		return 0, err
	}
	if requested != 0 {
		if requested < m.cfg.PortLow || requested > m.cfg.PortHigh {
			return 0, fmt.Errorf("port %d out of range [%d, %d]", requested, m.cfg.PortLow, m.cfg.PortHigh)
		}
		if used[requested] {
			return 0, fmt.Errorf("port %d already in use", requested)
		}
		return requested, nil
	}
	for p := m.cfg.PortLow; p <= m.cfg.PortHigh; p++ {
		if !used[p] {
			return p, nil
		}
	}
	return 0, ErrNoFreePort
}

// Create はブランチを作成する(仕様 14-1 / 14-2)。
//
// hook がある場合の順序: clone → 起動 → ready → on-create → 正常終了 →
// @init snapshot → 再起動。hook の結果(マイグレーション適用済み)を
// reset の戻り先にしつつ、@init を必ずクリーンな状態でのみ取得するため。
// hook がない場合: clone → @init snapshot → 起動(最速経路)。
func (m *Manager) Create(ctx context.Context, name string, port int) (Info, error) {
	if !m.nameRe.MatchString(name) {
		return Info{}, ErrInvalidName
	}
	unlock := m.lock(name)
	defer unlock()

	if _, err := m.db.GetBranch(name); err == nil {
		return Info{}, ErrExists
	}
	all, err := m.db.ListBranches()
	if err != nil {
		return Info{}, err
	}
	if m.cfg.MaxBranches > 0 && len(all) >= m.cfg.MaxBranches {
		return Info{}, ErrLimitReached
	}
	p, err := m.allocPort(port)
	if err != nil {
		return Info{}, err
	}
	if err := m.checkMemory(); err != nil {
		return Info{}, err
	}

	origin := m.currentBaseline()
	if err := m.db.CreateBranch(name, p, string(origin)); err != nil {
		return Info{}, err
	}
	fail := func(cause error) (Info, error) {
		// error 状態で残す(ログ確認のため自動削除しない)。仕様 14-2。
		_ = m.db.SetState(name, state.StateError, cause.Error())
		return Info{}, cause
	}

	vol, err := m.st.Clone(ctx, origin, name)
	if err != nil {
		return fail(fmt.Errorf("clone: %w", err))
	}
	b, _ := m.db.GetBranch(name)
	ins := m.instance(b, vol)

	if _, hasHook := m.hookExists(hooks.OnCreate); hasHook {
		if err := m.eng.Start(ctx, ins); err != nil {
			return fail(fmt.Errorf("engine start: %w", err))
		}
		if err := m.eng.WaitReady(ctx, ins); err != nil {
			return fail(err)
		}
		if err := m.runHook(ctx, hooks.OnCreate, b, vol); err != nil {
			return fail(err)
		}
		// @init は必ず正常終了状態でのみ取得する(クラッシュリカバリ防止)。
		if err := m.eng.Stop(ctx, ins); err != nil {
			return fail(fmt.Errorf("engine stop before @init: %w", err))
		}
	}
	if _, err := m.st.SnapshotInit(ctx, vol); err != nil {
		return fail(fmt.Errorf("snapshot @init: %w", err))
	}
	if err := m.eng.Start(ctx, ins); err != nil {
		return fail(fmt.Errorf("engine start: %w", err))
	}
	if err := m.eng.WaitReady(ctx, ins); err != nil {
		return fail(err)
	}
	if err := m.db.SetState(name, state.StateRunning, ""); err != nil {
		return Info{}, err
	}
	return m.info(ctx, name)
}

// Reset はブランチを @init に巻き戻す(FastRollback バックエンド)。
// 遅いバックエンドでは「新クローン+付け替え」になるが v0.1 では未実装
// (docs/DECISIONS.md 参照)。
func (m *Manager) Reset(ctx context.Context, name string) (Info, error) {
	unlock := m.lock(name)
	defer unlock()

	b, err := m.db.GetBranch(name)
	if err != nil {
		return Info{}, err
	}
	if !m.st.Capabilities().FastRollback {
		return Info{}, errors.New("reset is not supported on this storage backend yet (planned: recreate+repoint)")
	}
	if err := m.db.SetState(name, state.StateResetting, ""); err != nil {
		return Info{}, err
	}
	vol, err := m.resolveVolume(ctx, b)
	if err != nil {
		return Info{}, err
	}
	ins := m.instance(b, vol)

	_ = m.eng.Stop(ctx, ins) // 停止済みでもエラーにしない
	initSnap := storage.SnapshotRef(vol.Dataset + "@init")
	if err := m.st.Rollback(ctx, vol, initSnap); err != nil {
		_ = m.db.SetState(name, state.StateError, err.Error())
		return Info{}, fmt.Errorf("rollback: %w", err)
	}
	if err := m.eng.Start(ctx, ins); err != nil {
		_ = m.db.SetState(name, state.StateError, err.Error())
		return Info{}, err
	}
	if err := m.eng.WaitReady(ctx, ins); err != nil {
		_ = m.db.SetState(name, state.StateError, err.Error())
		return Info{}, err
	}
	// on-reset の失敗は記録して続行(仕様 14-2)。
	_ = m.runHook(ctx, hooks.OnReset, b, vol)
	if err := m.db.SetState(name, state.StateRunning, ""); err != nil {
		return Info{}, err
	}
	return m.info(ctx, name)
}

// Delete はブランチを削除する。
func (m *Manager) Delete(ctx context.Context, name string) error {
	unlock := m.lock(name)
	defer unlock()

	b, err := m.db.GetBranch(name)
	if err != nil {
		return err
	}
	if err := m.db.SetState(name, state.StateDeleting, ""); err != nil {
		return err
	}
	vol, err := m.resolveVolume(ctx, b)
	if err != nil {
		return err
	}
	ins := m.instance(b, vol)
	_ = m.eng.Stop(ctx, ins) // 動いていなくてもよい
	// on-delete の失敗は記録して続行(仕様 14-2)。
	_ = m.runHook(ctx, hooks.OnDelete, b, vol)

	job, err := m.st.DeleteAsync(ctx, vol)
	if err != nil {
		_ = m.db.SetState(name, state.StateError, err.Error())
		return fmt.Errorf("destroy: %w", err)
	}
	status, err := m.st.Poll(ctx, job)
	if err != nil {
		return err
	}
	if status == storage.JobCompleted {
		return m.db.DeleteBranch(name)
	}
	// 非同期バックエンドでは deleting のまま残し、回収ループが Poll する(v1.0)。
	return nil
}

// SetActiveConns は接続数の参照先を設定する(プロキシは Manager に依存する
// ため、twigd がプロキシ起動後に配線する)。
func (m *Manager) SetActiveConns(fn func(name string) int) {
	m.cfg.ActiveConns = fn
}

func (m *Manager) activeConns(name string) int {
	if m.cfg.ActiveConns == nil {
		return 0
	}
	return m.cfg.ActiveConns(name)
}

// RouteBranch はプロキシ用: ブランチのポートを返す。
// 未知の名前は lazy create(有効時)、sleeping は wake する。
// 接続を保持したまま待たせる前提なので、作成完了までブロックする。
func (m *Manager) RouteBranch(ctx context.Context, name string) (int, error) {
	b, err := m.db.GetBranch(name)
	if errors.Is(err, ErrNotFound) {
		if !m.lazyEnabled() {
			return 0, err
		}
		info, cerr := m.Create(ctx, name, 0)
		if errors.Is(cerr, ErrExists) {
			// 同時接続が先に作成した場合: 出来上がりを引く
			b, err = m.db.GetBranch(name)
			if err != nil {
				return 0, err
			}
			return m.routeExisting(ctx, b)
		}
		if cerr != nil {
			return 0, cerr
		}
		return info.Port, nil
	}
	if err != nil {
		return 0, err
	}
	return m.routeExisting(ctx, b)
}

func (m *Manager) routeExisting(ctx context.Context, b state.Branch) (int, error) {
	switch b.State {
	case state.StateRunning:
		return b.Port, nil
	case state.StateSleeping:
		if _, err := m.Wake(ctx, b.Name); err != nil {
			return 0, err
		}
		return b.Port, nil
	case state.StateCreating, state.StateResetting:
		// 別の接続が作成/リセット中: 完了を待って通す(CI の接続プールが
		// 同時に張ってくるケース)。TCP は保持されたままなので待てる。
		return m.waitRunning(ctx, b.Name)
	default:
		return 0, fmt.Errorf("branch %s is %s", b.Name, b.State)
	}
}

// waitRunning はブランチが running になるまで待ってポートを返す。
func (m *Manager) waitRunning(ctx context.Context, name string) (int, error) {
	maxWait := m.cfg.LazyMaxWait
	if maxWait == 0 {
		maxWait = 20 * time.Second
	}
	deadline := time.Now().Add(maxWait)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		b, err := m.db.GetBranch(name)
		if err != nil {
			return 0, err
		}
		switch b.State {
		case state.StateRunning:
			return b.Port, nil
		case state.StateCreating, state.StateResetting:
			time.Sleep(200 * time.Millisecond)
		default:
			return 0, fmt.Errorf("branch %s is %s", name, b.State)
		}
	}
	return 0, fmt.Errorf("timeout waiting for branch %s to become running", name)
}

// checkMemory は空きメモリ < buffer pool + 300MB なら作成を拒否する。
func (m *Manager) checkMemory() error {
	if m.cfg.AvailableMem == nil || m.cfg.BufferPoolBytes <= 0 {
		return nil
	}
	avail, err := m.cfg.AvailableMem()
	if err != nil {
		return nil // 判定不能なら通す(ガードは best-effort)
	}
	need := m.cfg.BufferPoolBytes + 300*1024*1024
	if avail < need {
		return fmt.Errorf("%w: available memory %dMB < required %dMB",
			ErrLimitReached, avail/1024/1024, need/1024/1024)
	}
	return nil
}

func (m *Manager) lazyEnabled() bool {
	if !m.cfg.LazyCreate {
		return false
	}
	maxWait := m.cfg.LazyMaxWait
	if maxWait == 0 {
		maxWait = 20 * time.Second
	}
	return m.st.Capabilities().TypicalCreate <= maxWait
}

// Wake は sleeping(または停止している)ブランチの mysqld を起動する。
// error / deleting / creating のブランチは対象外(状態を上書きしない)。
func (m *Manager) Wake(ctx context.Context, name string) (Info, error) {
	unlock := m.lock(name)
	defer unlock()

	b, err := m.db.GetBranch(name)
	if err != nil {
		return Info{}, err
	}
	if b.State != state.StateSleeping && b.State != state.StateRunning {
		return Info{}, fmt.Errorf("branch %s is %s (cannot wake)", name, b.State)
	}
	vol, err := m.resolveVolume(ctx, b)
	if err != nil {
		return Info{}, err
	}
	ins := m.instance(b, vol)
	if running, _ := m.eng.IsRunning(ctx, ins); !running {
		if err := m.eng.Start(ctx, ins); err != nil {
			return Info{}, err
		}
		if err := m.eng.WaitReady(ctx, ins); err != nil {
			return Info{}, err
		}
	}
	if err := m.db.SetState(name, state.StateRunning, ""); err != nil {
		return Info{}, err
	}
	return m.info(ctx, name)
}

// TouchConn は最終接続時刻を記録する。SQLite への書き込みを抑えるため
// ブランチごとに 1 分に 1 回まで。
func (m *Manager) TouchConn(name string) {
	m.touchMu.Lock()
	last, ok := m.lastTouch[name]
	now := time.Now()
	if ok && now.Sub(last) < time.Minute {
		m.touchMu.Unlock()
		return
	}
	m.lastTouch[name] = now
	m.touchMu.Unlock()
	_ = m.db.TouchLastConn(name)
}

// BaselineInfo は現在のベースラインと base の snapshot 一覧。
type BaselineInfo struct {
	Current   string
	Snapshots []string
}

// currentBaseline は DB の切り替え記録を優先し、無ければバックエンド既定を使う。
func (m *Manager) currentBaseline() storage.SnapshotRef {
	if snap, ok := m.db.CurrentBaselineOverride(); ok {
		return storage.SnapshotRef(snap)
	}
	return m.baseline.CurrentBaseline()
}

// Baseline はベースライン情報を返す(API GET /baseline 用)。
func (m *Manager) Baseline(ctx context.Context) (BaselineInfo, error) {
	snaps, err := m.st.ListSnapshots(ctx)
	if err != nil {
		return BaselineInfo{}, err
	}
	info := BaselineInfo{Current: string(m.currentBaseline())}
	for _, s := range snaps {
		info.Snapshots = append(info.Snapshots, string(s))
	}
	return info, nil
}

// Get は 1 件の詳細。
func (m *Manager) Get(ctx context.Context, name string) (Info, error) {
	return m.info(ctx, name)
}

// List は全ブランチの詳細。
func (m *Manager) List(ctx context.Context) ([]Info, error) {
	bs, err := m.db.ListBranches()
	if err != nil {
		return nil, err
	}
	out := make([]Info, 0, len(bs))
	for _, b := range bs {
		info, err := m.info(ctx, b.Name)
		if err != nil {
			return nil, err
		}
		out = append(out, info)
	}
	return out, nil
}

func (m *Manager) info(ctx context.Context, name string) (Info, error) {
	b, err := m.db.GetBranch(name)
	if err != nil {
		return Info{}, err
	}
	info := Info{Branch: b}
	if vol, err := m.resolveVolume(ctx, b); err == nil {
		if used, err := m.st.UsedBytes(ctx, vol); err == nil {
			info.UsedBytes = used
		}
	}
	if hs, err := m.db.LastHookStatus(name); err == nil && len(hs) > 0 {
		info.HookStatus = hs
	}
	return info, nil
}

// resolveVolume は既存ブランチの Volume を storage の規約から再構成する。
// v0.1 では Clone と同じ名前規約(zfs: branch_parent/<name>)を使う。
type volumeResolver interface {
	ResolveVolume(ctx context.Context, name string) (storage.Volume, error)
}

func (m *Manager) resolveVolume(ctx context.Context, b state.Branch) (storage.Volume, error) {
	if r, ok := m.st.(volumeResolver); ok {
		return r.ResolveVolume(ctx, b.Name)
	}
	return storage.Volume{Name: b.Name}, nil
}

func (m *Manager) hookExists(event hooks.Event) (string, bool) {
	if m.hooks == nil {
		return "", false
	}
	return m.hooks.Find(event)
}

func (m *Manager) runHook(ctx context.Context, event hooks.Event, b state.Branch, vol storage.Volume) error {
	if m.hooks == nil {
		return nil
	}
	if _, ok := m.hooks.Find(event); !ok {
		return nil
	}
	id, _ := m.db.RecordHookStart(b.Name, string(event), "")
	res, err := m.hooks.Run(ctx, event, m.hookEnv(b, vol))
	_ = m.db.RecordHookFinish(id, res.ExitCode)
	if err != nil {
		return fmt.Errorf("hook %s failed: %w", event, err)
	}
	return nil
}
