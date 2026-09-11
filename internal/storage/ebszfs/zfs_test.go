package ebszfs

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/rikukadev/sashiki/internal/storage"
)

// fakeRunner は zfs コマンドを実行せず、引数を記録して canned な出力を返す。
// キーは引数をスペース連結したもの。未登録の呼び出しは空文字を返す。
type fakeRunner struct {
	calls   [][]string
	outputs map[string]string
	errOn   string // この引数列(スペース連結)でエラーを返す
}

func (f *fakeRunner) run(_ context.Context, args ...string) (string, error) {
	f.calls = append(f.calls, args)
	key := join(args)
	if f.errOn != "" && key == f.errOn {
		return "", errors.New("boom")
	}
	return f.outputs[key], nil
}

func join(args []string) string {
	s := ""
	for i, a := range args {
		if i > 0 {
			s += " "
		}
		s += a
	}
	return s
}

func newFake(t *testing.T, outputs map[string]string) (*Backend, *fakeRunner) {
	t.Helper()
	b := New(Config{
		Pool:             "dbpool",
		BaseDataset:      "dbpool/base",
		BranchParent:     "dbpool/branches",
		BaselineSnapshot: "baseline",
	})
	f := &fakeRunner{outputs: outputs}
	b.run = f.run
	return b, f
}

func TestCloneBuildsDatasetAndReturnsMountpoint(t *testing.T) {
	b, f := newFake(t, map[string]string{
		"get -H -o value mountpoint dbpool/branches/pr-1": "/mnt/pr-1",
	})
	vol, err := b.Clone(context.Background(), "dbpool/base@baseline", "pr-1")
	if err != nil {
		t.Fatal(err)
	}
	if vol.Name != "pr-1" || vol.Dataset != "dbpool/branches/pr-1" || vol.Path != "/mnt/pr-1" {
		t.Errorf("vol = %+v", vol)
	}
	// 1 回目の呼び出しは clone <baseline> <dataset>
	if got, want := f.calls[0], []string{"clone", "dbpool/base@baseline", "dbpool/branches/pr-1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("clone args = %v, want %v", got, want)
	}
}

func TestCloneCleansUpNothingOnCloneError(t *testing.T) {
	b, _ := newFake(t, nil)
	// clone 自体が失敗したら mountpoint 取得へ進まずエラーを返す。
	b.run = func(_ context.Context, args ...string) (string, error) {
		if args[0] == "clone" {
			return "", errors.New("dataset busy")
		}
		t.Fatalf("mountpoint should not be queried after clone failure (args=%v)", args)
		return "", nil
	}
	if _, err := b.Clone(context.Background(), "dbpool/base@baseline", "pr-1"); err == nil {
		t.Fatal("Clone should fail when zfs clone fails")
	}
}

func TestListBranchVolumesFiltersParentAndSnapshots(t *testing.T) {
	out := "dbpool/branches\n" + // parent 自身 → 除外
		"dbpool/branches/pr-1\n" +
		"dbpool/branches/pr-2\n" +
		"dbpool/branches/pr-2@init\n" + // snapshot → 除外
		"dbpool/branches/pr-3/nested\n" + // ネスト → 除外
		"\n"
	b, _ := newFake(t, map[string]string{
		"list -H -o name -r dbpool/branches": out,
	})
	names, err := b.ListBranchVolumes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"pr-1", "pr-2"}; !reflect.DeepEqual(names, want) {
		t.Errorf("names = %v, want %v", names, want)
	}
}

func TestListSnapshotsParsesLines(t *testing.T) {
	b, _ := newFake(t, map[string]string{
		"list -H -t snapshot -o name -r dbpool/base": "dbpool/base@baseline\ndbpool/base@baseline-2\n",
	})
	refs, err := b.ListSnapshots(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []storage.SnapshotRef{"dbpool/base@baseline", "dbpool/base@baseline-2"}
	if !reflect.DeepEqual(refs, want) {
		t.Errorf("refs = %v, want %v", refs, want)
	}
}

func TestUsedBytesParsesInt(t *testing.T) {
	b, _ := newFake(t, map[string]string{
		"get -H -p -o value used dbpool/branches/pr-1": "1048576",
	})
	n, err := b.UsedBytes(context.Background(), storage.Volume{Dataset: "dbpool/branches/pr-1"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1048576 {
		t.Errorf("used = %d, want 1048576", n)
	}
}

func TestUsedBytesErrorsOnNonNumeric(t *testing.T) {
	b, _ := newFake(t, map[string]string{
		"get -H -p -o value used dbpool/branches/pr-1": "-", // reflink 等の "不明" 表現
	})
	if _, err := b.UsedBytes(context.Background(), storage.Volume{Dataset: "dbpool/branches/pr-1"}); err == nil {
		t.Error("non-numeric used should error")
	}
}

func TestSnapshotInitAndRollbackArgs(t *testing.T) {
	b, f := newFake(t, nil)
	vol := storage.Volume{Name: "pr-1", Dataset: "dbpool/branches/pr-1"}
	ref, err := b.SnapshotInit(context.Background(), vol)
	if err != nil {
		t.Fatal(err)
	}
	if ref != "dbpool/branches/pr-1@init" {
		t.Errorf("init ref = %q", ref)
	}
	if err := b.Rollback(context.Background(), vol, ref); err != nil {
		t.Fatal(err)
	}
	// snapshot dbpool/branches/pr-1@init → rollback -r dbpool/branches/pr-1@init
	if want := []string{"snapshot", "dbpool/branches/pr-1@init"}; !reflect.DeepEqual(f.calls[0], want) {
		t.Errorf("snapshot args = %v, want %v", f.calls[0], want)
	}
	if want := []string{"rollback", "-r", "dbpool/branches/pr-1@init"}; !reflect.DeepEqual(f.calls[1], want) {
		t.Errorf("rollback args = %v, want %v", f.calls[1], want)
	}
}

func TestDeleteAsyncDestroysRecursivelyAndCompletes(t *testing.T) {
	b, f := newFake(t, nil)
	vol := storage.Volume{Dataset: "dbpool/branches/pr-1"}
	job, err := b.DeleteAsync(context.Background(), vol)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"destroy", "-r", "dbpool/branches/pr-1"}; !reflect.DeepEqual(f.calls[0], want) {
		t.Errorf("destroy args = %v, want %v", f.calls[0], want)
	}
	// ローカル zfs は同期完了するので Poll は即 completed。
	st, err := b.Poll(context.Background(), job)
	if err != nil || st != storage.JobCompleted {
		t.Errorf("Poll = %v, %v (want completed)", st, err)
	}
}

func TestSetQuotaFormatsValueAndClears(t *testing.T) {
	b, f := newFake(t, nil)
	vol := storage.Volume{Dataset: "dbpool/branches/pr-1"}
	if err := b.SetQuota(context.Background(), vol, 2048); err != nil {
		t.Fatal(err)
	}
	if want := []string{"set", "refquota=2048", "dbpool/branches/pr-1"}; !reflect.DeepEqual(f.calls[0], want) {
		t.Errorf("set args = %v, want %v", f.calls[0], want)
	}
	if err := b.SetQuota(context.Background(), vol, 0); err != nil {
		t.Fatal(err)
	}
	if want := []string{"set", "refquota=none", "dbpool/branches/pr-1"}; !reflect.DeepEqual(f.calls[1], want) {
		t.Errorf("clear args = %v, want %v", f.calls[1], want)
	}
}

func TestPromoteBranchSnapshotsBranchDataset(t *testing.T) {
	b, f := newFake(t, nil)
	branch := storage.Volume{Dataset: "dbpool/branches/pr-1"}
	ref, err := b.PromoteBranch(context.Background(), branch, "baseline-x")
	if err != nil {
		t.Fatal(err)
	}
	if ref != "dbpool/branches/pr-1@baseline-x" {
		t.Errorf("promote ref = %q", ref)
	}
	if want := []string{"snapshot", "dbpool/branches/pr-1@baseline-x"}; !reflect.DeepEqual(f.calls[0], want) {
		t.Errorf("snapshot args = %v, want %v", f.calls[0], want)
	}
}

func TestErrorsPropagate(t *testing.T) {
	b, f := newFake(t, nil)
	f.errOn = "snapshot dbpool/base@baseline-x"
	if _, err := b.SnapshotBase(context.Background(), "baseline-x"); err == nil {
		t.Error("SnapshotBase should surface the runner error")
	}
}
