// Package branch はブランチのライフサイクル(create / reset / delete / list)を
// 司るコア。storage と engine はインターフェースで受け、速度の仮定を持たない。
package branch

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sync"

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

	origin := m.baseline.CurrentBaseline()
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

// BaselineInfo は現在のベースラインと base の snapshot 一覧。
type BaselineInfo struct {
	Current   string
	Snapshots []string
}

// Baseline はベースライン情報を返す(API GET /baseline 用)。
func (m *Manager) Baseline(ctx context.Context) (BaselineInfo, error) {
	snaps, err := m.st.ListSnapshots(ctx)
	if err != nil {
		return BaselineInfo{}, err
	}
	info := BaselineInfo{Current: string(m.baseline.CurrentBaseline())}
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
