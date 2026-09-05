package fsxzfs

import (
	"context"
	"testing"
	"time"

	awsfsx "github.com/aws/aws-sdk-go-v2/service/fsx"
	"github.com/aws/aws-sdk-go-v2/service/fsx/types"
	"github.com/rikukaInoue/twig/internal/storage"
)

type mockAPI struct {
	volumes       map[string]*types.Volume // by id
	createCalls   int
	describeCalls int
	// waitVolumeReady が最初の N 回 IN_PROGRESS を返すシミュレーション
	pendingUntil int
}

func newMockAPI() *mockAPI {
	return &mockAPI{volumes: map[string]*types.Volume{}}
}

func strp(s string) *string { return &s }

func (m *mockAPI) CreateVolume(ctx context.Context, in *awsfsx.CreateVolumeInput, _ ...func(*awsfsx.Options)) (*awsfsx.CreateVolumeOutput, error) {
	m.createCalls++
	id := "fsvol-mock" + *in.Name
	now := time.Now()
	v := &types.Volume{
		VolumeId:     &id,
		Name:         in.Name,
		Lifecycle:    types.VolumeLifecycleAvailable,
		CreationTime: &now,
		Tags:         in.Tags,
		OpenZFSConfiguration: &types.OpenZFSVolumeConfiguration{
			VolumePath: strp("/fsx/" + *in.Name),
		},
	}
	m.volumes[id] = v
	return &awsfsx.CreateVolumeOutput{Volume: v}, nil
}

func (m *mockAPI) DeleteVolume(ctx context.Context, in *awsfsx.DeleteVolumeInput, _ ...func(*awsfsx.Options)) (*awsfsx.DeleteVolumeOutput, error) {
	delete(m.volumes, *in.VolumeId)
	return &awsfsx.DeleteVolumeOutput{}, nil
}

func (m *mockAPI) DescribeVolumes(ctx context.Context, in *awsfsx.DescribeVolumesInput, _ ...func(*awsfsx.Options)) (*awsfsx.DescribeVolumesOutput, error) {
	m.describeCalls++
	var out []types.Volume
	if len(in.VolumeIds) > 0 {
		for _, id := range in.VolumeIds {
			if v, ok := m.volumes[id]; ok {
				vv := *v
				// pendingUntil 回目までは admin action IN_PROGRESS
				if m.describeCalls <= m.pendingUntil {
					vv.AdministrativeActions = []types.AdministrativeAction{{
						AdministrativeActionType: types.AdministrativeActionTypeVolumeInitializeWithSnapshot,
						Status:                   types.StatusInProgress,
					}}
				}
				out = append(out, vv)
			}
		}
	} else {
		for _, v := range m.volumes {
			out = append(out, *v)
		}
	}
	return &awsfsx.DescribeVolumesOutput{Volumes: out}, nil
}

func (m *mockAPI) CreateSnapshot(ctx context.Context, in *awsfsx.CreateSnapshotInput, _ ...func(*awsfsx.Options)) (*awsfsx.CreateSnapshotOutput, error) {
	id := "fsvolsnap-" + *in.Name
	arn := "arn:aws:fsx:::snapshot/" + *in.Name
	return &awsfsx.CreateSnapshotOutput{Snapshot: &types.Snapshot{
		SnapshotId: &id, Name: in.Name, ResourceARN: &arn,
		Lifecycle: types.SnapshotLifecycleAvailable,
	}}, nil
}

func (m *mockAPI) DescribeSnapshots(ctx context.Context, in *awsfsx.DescribeSnapshotsInput, _ ...func(*awsfsx.Options)) (*awsfsx.DescribeSnapshotsOutput, error) {
	if len(in.SnapshotIds) > 0 {
		name := in.SnapshotIds[0][len("fsvolsnap-"):]
		arn := "arn:aws:fsx:::snapshot/" + name
		return &awsfsx.DescribeSnapshotsOutput{Snapshots: []types.Snapshot{{
			SnapshotId: &in.SnapshotIds[0], Name: &name, ResourceARN: &arn,
			Lifecycle: types.SnapshotLifecycleAvailable,
		}}}, nil
	}
	name := "baseline"
	arn := "arn:aws:fsx:::snapshot/baseline"
	return &awsfsx.DescribeSnapshotsOutput{Snapshots: []types.Snapshot{{
		Name: &name, ResourceARN: &arn, Lifecycle: types.SnapshotLifecycleAvailable,
	}}}, nil
}

func (m *mockAPI) DescribeFileSystems(ctx context.Context, in *awsfsx.DescribeFileSystemsInput, _ ...func(*awsfsx.Options)) (*awsfsx.DescribeFileSystemsOutput, error) {
	root := "fsvol-root-discovered"
	return &awsfsx.DescribeFileSystemsOutput{FileSystems: []types.FileSystem{{
		OpenZFSConfiguration: &types.OpenZFSFileSystemConfiguration{RootVolumeId: &root},
	}}}, nil
}

func newTestBackend(m *mockAPI) *Backend {
	b := New(Config{
		FileSystemID:     "fs-1",
		BaseVolumeID:     "fsvol-base",
		ParentVolumeID:   "fsvol-root",
		BaselineSnapshot: "baseline",
		DNSName:          "fs-1.fsx.test",
		MountRoot:        "/mnt/twig",
		PollInterval:     time.Millisecond,
	}, m)
	b.mount = func(ctx context.Context, source, target string) error { return nil }
	b.umount = func(ctx context.Context, target string) error { return nil }
	return b
}

func TestCloneWaitsForAdminActions(t *testing.T) {
	m := newMockAPI()
	m.pendingUntil = 3 // 3 回目の Describe までは IN_PROGRESS
	b := newTestBackend(m)

	vol, err := b.Clone(context.Background(), "arn:snap", "pr-1")
	if err != nil {
		t.Fatal(err)
	}
	if vol.Name != "pr-1" {
		t.Errorf("name = %s", vol.Name)
	}
	if m.describeCalls < 4 {
		t.Errorf("should poll until admin actions complete: %d describes", m.describeCalls)
	}
	// 世代サフィックス付きの FSx 名でマウントパスが決まる
	if vol.Path == "/mnt/twig/pr-1" {
		t.Errorf("path should include generation suffix: %s", vol.Path)
	}
}

func TestResolveVolumePicksNewestGeneration(t *testing.T) {
	m := newMockAPI()
	b := newTestBackend(m)
	// 2 世代作る(reset の付け替え中を再現)
	v1, err := b.Clone(context.Background(), "arn:snap", "pr-1")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	v2, err := b.Clone(context.Background(), "arn:snap", "pr-1")
	if err != nil {
		t.Fatal(err)
	}
	got, err := b.ResolveVolume(context.Background(), "pr-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Dataset != v2.Dataset {
		t.Errorf("resolved %s, want newest %s (old %s)", got.Dataset, v2.Dataset, v1.Dataset)
	}
}

func TestDeleteAsyncAndPoll(t *testing.T) {
	m := newMockAPI()
	b := newTestBackend(m)
	vol, _ := b.Clone(context.Background(), "arn:snap", "pr-1")
	job, err := b.DeleteAsync(context.Background(), vol)
	if err != nil {
		t.Fatal(err)
	}
	st, err := b.Poll(context.Background(), job)
	if err != nil || st != storage.JobCompleted {
		t.Errorf("poll = %v, %v", st, err)
	}
}

func TestCapabilitiesDeclareSlowControlPlane(t *testing.T) {
	b := newTestBackend(newMockAPI())
	c := b.Capabilities()
	if c.FastRollback {
		t.Error("fsx must not claim FastRollback")
	}
	if c.TypicalCreate < 30*time.Second {
		t.Error("TypicalCreate should reflect measured ~70s")
	}
	if !c.AsyncDelete {
		t.Error("AsyncDelete should be true")
	}
}

func TestParentVolumeAutoDiscovery(t *testing.T) {
	m := newMockAPI()
	b := newTestBackend(m)
	b.cfg.ParentVolumeID = "" // 未指定 → filesystem から自動発見
	if _, err := b.Clone(context.Background(), "arn:snap", "pr-1"); err != nil {
		t.Fatal(err)
	}
	if b.cfg.ParentVolumeID != "fsvol-root-discovered" {
		t.Errorf("parent = %q, want discovered root", b.cfg.ParentVolumeID)
	}
}
