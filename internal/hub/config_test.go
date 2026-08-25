package hub

import (
	"path/filepath"
	"testing"
	"time"
)

func TestNewConfigGitGCDefaults(t *testing.T) {
	clearHubEnv(t)
	dataDir := t.TempDir()
	t.Setenv("ZYNC_TOKEN", "test-token")
	t.Setenv("ZYNC_DATA_DIR", dataDir)

	cfg, err := NewConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GitGCInterval != 24*time.Hour {
		t.Fatalf("GitGCInterval = %s, want 24h", cfg.GitGCInterval)
	}
	if cfg.GitGCPruneAge != 14*24*time.Hour {
		t.Fatalf("GitGCPruneAge = %s, want 336h", cfg.GitGCPruneAge)
	}
	if cfg.LeaseTTL != 2*time.Minute {
		t.Fatalf("LeaseTTL = %s, want 2m", cfg.LeaseTTL)
	}
	if cfg.ReposDir() != filepath.Join(dataDir, "repos") {
		t.Fatalf("ReposDir = %q", cfg.ReposDir())
	}
}

func TestNewConfigGitGCOverrides(t *testing.T) {
	clearHubEnv(t)
	t.Setenv("ZYNC_TOKEN", "test-token")
	t.Setenv("ZYNC_DATA_DIR", t.TempDir())
	t.Setenv("ZYNC_GIT_GC_INTERVAL", "0s")
	t.Setenv("ZYNC_GIT_GC_PRUNE_AGE", "720h")
	t.Setenv("ZYNC_LEASE_TTL", "0s")

	cfg, err := NewConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GitGCInterval != 0 {
		t.Fatalf("GitGCInterval = %s, want disabled", cfg.GitGCInterval)
	}
	if cfg.GitGCPruneAge != 30*24*time.Hour {
		t.Fatalf("GitGCPruneAge = %s, want 720h", cfg.GitGCPruneAge)
	}
	if cfg.LeaseTTL != 0 {
		t.Fatalf("LeaseTTL = %s, want disabled", cfg.LeaseTTL)
	}
}

func TestNewConfigRejectsUnsafeGitGCValues(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{name: "invalid interval", key: "ZYNC_GIT_GC_INTERVAL", value: "daily"},
		{name: "negative interval", key: "ZYNC_GIT_GC_INTERVAL", value: "-1h"},
		{name: "invalid prune age", key: "ZYNC_GIT_GC_PRUNE_AGE", value: "later"},
		{name: "short prune age", key: "ZYNC_GIT_GC_PRUNE_AGE", value: "23h59m"},
		{name: "invalid lease TTL", key: "ZYNC_LEASE_TTL", value: "soon"},
		{name: "short lease TTL", key: "ZYNC_LEASE_TTL", value: "500ms"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearHubEnv(t)
			t.Setenv("ZYNC_TOKEN", "test-token")
			t.Setenv("ZYNC_DATA_DIR", t.TempDir())
			t.Setenv(tt.key, tt.value)
			if _, err := NewConfig(); err == nil {
				t.Fatal("NewConfig succeeded, want an error")
			}
		})
	}
}

func clearHubEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"ZYNC_PORT",
		"ZYNC_DATA_DIR",
		"ZYNC_GIT_BIN",
		"ZYNC_GIT_GC_INTERVAL",
		"ZYNC_GIT_GC_PRUNE_AGE",
		"ZYNC_LEASE_TTL",
		"ZYNC_TOKEN",
	} {
		t.Setenv(key, "")
	}
}
