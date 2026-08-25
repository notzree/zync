// Package cliconf handles client-side configuration: the global replica
// identity, per-repo state, and the workspace->path registry used by the TUI.
package cliconf

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type Global struct {
	HubURL  string `json:"hub_url"`
	Token   string `json:"token"`
	Replica string `json:"replica"`
	// Kind is "local" (human machine, leases never expire) or "remote"
	// (unattended runtime, leases carry a TTL and are heartbeat-renewed).
	Kind string `json:"kind,omitempty"`
	// OpencodeURL is this replica's own opencode server, advertised to the
	// hub so other machines can attach to it. Empty for replicas that don't
	// run one (typical laptops).
	OpencodeURL string `json:"opencode_url,omitempty"`
	// WorkspacesDir is where this replica materializes workspace clones,
	// advertised alongside OpencodeURL.
	WorkspacesDir string `json:"workspaces_dir,omitempty"`
}

func ConfigDir() string {
	if v := os.Getenv("ZYNC_CONFIG_DIR"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "zync")
}

// LoadGlobal reads the global config, with ZYNC_HUB_URL / ZYNC_TOKEN /
// ZYNC_REPLICA env vars taking precedence (useful for agents and CI).
func LoadGlobal() (Global, error) {
	var g Global
	data, err := os.ReadFile(filepath.Join(ConfigDir(), "config.json"))
	if err == nil {
		if err := json.Unmarshal(data, &g); err != nil {
			return g, fmt.Errorf("parse config.json: %w", err)
		}
	}
	if v := os.Getenv("ZYNC_HUB_URL"); v != "" {
		g.HubURL = v
	}
	if v := os.Getenv("ZYNC_TOKEN"); v != "" {
		g.Token = v
	}
	if v := os.Getenv("ZYNC_REPLICA"); v != "" {
		g.Replica = v
	}
	if v := os.Getenv("ZYNC_OPENCODE_URL"); v != "" {
		g.OpencodeURL = v
	}
	if v := os.Getenv("ZYNC_WORKSPACES_DIR"); v != "" {
		g.WorkspacesDir = v
	}
	if v := os.Getenv("ZYNC_REPLICA_KIND"); v != "" {
		g.Kind = v
	}
	if g.Kind == "" {
		g.Kind = "local"
	}
	if g.Kind != "local" && g.Kind != "remote" {
		return g, fmt.Errorf("replica kind must be \"local\" or \"remote\", got %q", g.Kind)
	}
	if g.HubURL == "" || g.Token == "" || g.Replica == "" {
		return g, errors.New("zync is not configured: run `zync setup --hub <url> --token <token> --name <replica>` (or set ZYNC_HUB_URL, ZYNC_TOKEN, ZYNC_REPLICA)")
	}
	return g, nil
}

func SaveGlobal(g Global) error {
	dir := ConfigDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(g, "", "  ")
	return os.WriteFile(filepath.Join(dir, "config.json"), data, 0o600)
}

// WorktreeState records what the single working tree is expected to contain:
// branch B with the content of snapshot Commit (tree Tree). Any deviation
// while not holding the lease is divergence.
type WorktreeState struct {
	Branch    string `json:"branch"`
	Tree      string `json:"tree"`
	IndexTree string `json:"index_tree,omitempty"`
	Commit    string `json:"commit"`
}

type BranchState struct {
	Holding          bool   `json:"holding"`
	Generation       int64  `json:"generation"`
	PushToken        string `json:"push_token,omitempty"`
	SnapshotCommit   string `json:"snapshot_commit,omitempty"`
	BaseCommit       string `json:"base_commit,omitempty"`
	AgentStateDigest string `json:"agent_state_digest,omitempty"`
	AgentSessionID   string `json:"agent_session_id,omitempty"`
	ExtrasDigest     string `json:"extras_digest,omitempty"`
	ExpiresAt        int64  `json:"expires_at,omitempty"`
}

type RepoState struct {
	Workspace string                  `json:"workspace"`
	Worktree  *WorktreeState          `json:"worktree,omitempty"`
	Branches  map[string]*BranchState `json:"branches"`
}

func repoStatePath(gitDir string) string { return filepath.Join(gitDir, "zync-state.json") }

func LoadRepoState(gitDir string) (*RepoState, error) {
	data, err := os.ReadFile(repoStatePath(gitDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s RepoState
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse zync-state.json: %w", err)
	}
	if s.Branches == nil {
		s.Branches = map[string]*BranchState{}
	}
	return &s, nil
}

func SaveRepoState(gitDir string, s *RepoState) error {
	data, _ := json.MarshalIndent(s, "", "  ")
	return os.WriteFile(repoStatePath(gitDir), data, 0o600)
}

// Registry maps workspace names to local checkout paths so the TUI can act
// on any enrolled repo from anywhere.
type Registry map[string]string

func registryPath() string { return filepath.Join(ConfigDir(), "repos.json") }

func LoadRegistry() (Registry, error) {
	data, err := os.ReadFile(registryPath())
	if errors.Is(err, os.ErrNotExist) {
		return Registry{}, nil
	}
	if err != nil {
		return nil, err
	}
	var reg Registry
	if err := json.Unmarshal(data, &reg); err != nil {
		return nil, err
	}
	return reg, nil
}

func SaveRegistryEntry(workspace, path string) error {
	reg, err := LoadRegistry()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(ConfigDir(), 0o700); err != nil {
		return err
	}
	reg[workspace] = path
	data, _ := json.MarshalIndent(reg, "", "  ")
	return os.WriteFile(registryPath(), data, 0o600)
}
