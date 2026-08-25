package hub

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/notzree/zync/internal/db"
	"github.com/notzree/zync/internal/protocol"
)

func TestLeaseExpiryHeartbeatAndTakeover(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	dataDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dataDir, "repos"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		DataDir:       dataDir,
		GitBin:        gitBin,
		GitGCPruneAge: 14 * 24 * time.Hour,
		LeaseTTL:      10 * time.Second,
	}
	conn, err := db.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	blob, err := NewBlobStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	svc := NewLeaseService(conn, NewGitManager(cfg), blob, cfg)
	now := time.Unix(1_800_000_000, 0)
	svc.now = func() time.Time { return now }
	ctx := context.Background()
	if _, err := svc.CreateWorkspace(ctx, "demo", "main"); err != nil {
		t.Fatal(err)
	}
	// TTLs apply only to remote (unattended) replicas.
	for _, name := range []string{"laptop", "server"} {
		if err := svc.EnsureReplica(ctx, name, "", "", protocol.ReplicaKindRemote); err != nil {
			t.Fatal(err)
		}
	}

	first, err := svc.Take(ctx, "demo", "main", "laptop", false)
	if err != nil {
		t.Fatal(err)
	}
	if first.Generation != 1 || first.ExpiresAt != now.Add(10*time.Second).UnixMilli() {
		t.Fatalf("unexpected initial lease: %+v", first)
	}

	now = now.Add(6 * time.Second)
	hb, err := svc.Heartbeat(ctx, "demo", "laptop", protocol.HeartbeatRequest{
		Branch: "main", Generation: first.Generation, PushToken: first.PushToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	if hb.ExpiresAt != now.Add(10*time.Second).UnixMilli() {
		t.Fatalf("heartbeat deadline = %d", hb.ExpiresAt)
	}

	now = now.Add(9 * time.Second)
	if _, err := svc.Take(ctx, "demo", "main", "server", false); !errors.Is(err, ErrConflict) {
		t.Fatalf("take before expiry error = %v, want conflict", err)
	}
	now = now.Add(2 * time.Second)
	leases, err := svc.ListLeases(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || leases[0].State != "expired" {
		t.Fatalf("expired lease listing = %+v", leases)
	}

	second, err := svc.Take(ctx, "demo", "main", "server", false)
	if err != nil {
		t.Fatal(err)
	}
	if second.Generation != first.Generation+1 || second.PushToken == first.PushToken {
		t.Fatalf("takeover did not fence old holder: first=%+v second=%+v", first, second)
	}
	now = now.Add(8 * time.Second)
	if err := svc.ValidatePush(ctx, "demo", second.PushToken, "old new refs/heads/main"); err != nil {
		t.Fatalf("current push was rejected: %v", err)
	}
	now = now.Add(3 * time.Second)
	if _, err := svc.Take(ctx, "demo", "main", "laptop", false); !errors.Is(err, ErrConflict) {
		t.Fatalf("push did not renew lease, take error = %v", err)
	}
	if _, err := svc.Heartbeat(ctx, "demo", "laptop", protocol.HeartbeatRequest{
		Branch: "main", Generation: first.Generation, PushToken: first.PushToken,
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale heartbeat error = %v, want conflict", err)
	}
	if err := svc.ValidatePush(ctx, "demo", first.PushToken, "old new refs/heads/main"); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale push error = %v, want conflict", err)
	}
	if err := svc.ArchiveWorkspace(ctx, "demo"); !errors.Is(err, ErrConflict) {
		t.Fatalf("archive with live lease error = %v, want conflict", err)
	}
	now = now.Add(8 * time.Second)
	if err := svc.ArchiveWorkspace(ctx, "demo"); err != nil {
		t.Fatal(err)
	}
	active, err := svc.ListWorkspaces(ctx)
	if err != nil || len(active) != 0 {
		t.Fatalf("active workspaces after archive = %+v, %v", active, err)
	}
	all, err := svc.ListAllWorkspaces(ctx)
	if err != nil || len(all) != 1 || all[0].ArchivedAt == 0 {
		t.Fatalf("all workspaces after archive = %+v, %v", all, err)
	}
	if _, err := svc.Take(ctx, "demo", "main", "laptop", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("take archived workspace error = %v, want not found", err)
	}
	if err := svc.RestoreWorkspace(ctx, "demo"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Take(ctx, "demo", "main", "laptop", false); err != nil {
		t.Fatalf("take restored workspace: %v", err)
	}

	// Local (human) replicas hold without expiry: even far past the TTL,
	// their lease can only be broken with force.
	human, err := svc.Take(ctx, "demo", "main", "human", true)
	if err != nil {
		t.Fatal(err)
	}
	if human.ExpiresAt != 0 {
		t.Fatalf("local replica lease must not expire: %+v", human)
	}
	now = now.Add(time.Hour)
	if _, err := svc.Take(ctx, "demo", "main", "server", false); !errors.Is(err, ErrConflict) {
		t.Fatalf("local lease was treated as expired: %v", err)
	}
}
