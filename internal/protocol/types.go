// Package protocol defines the wire types shared between the hub server and
// the CLI client.
package protocol

const (
	// AgentStateFormat is the opaque OpenCode session export format understood
	// by zync's client adapter. The hub stores these bytes without parsing them.
	AgentStateFormat = "opencode-session-json-v1"
	// MaxAgentStateBytes bounds client and hub memory/disk use per bundle.
	MaxAgentStateBytes int64 = 64 << 20
	ExtrasFormat             = "zync-age-extras-v1"
	MaxExtrasBytes     int64 = 16 << 20

	// ReplicaHeader identifies the calling replica on every API request.
	ReplicaHeader = "X-Zync-Replica"
	// OpencodeURLHeader advertises the calling replica's opencode server (if
	// it runs one), stored by the hub so other clients can attach to it.
	OpencodeURLHeader = "X-Zync-Opencode-Url"
	// WorkspacesDirHeader advertises where the calling replica materializes
	// workspace clones.
	WorkspacesDirHeader = "X-Zync-Workspaces-Dir"
	// ReplicaKindHeader declares whether the calling replica is a human
	// machine ("local", leases never expire) or an unattended runtime
	// ("remote", leases carry a TTL and must be heartbeat-renewed).
	ReplicaKindHeader = "X-Zync-Replica-Kind"

	ReplicaKindLocal  = "local"
	ReplicaKindRemote = "remote"
	// PushOptionPrefix is the git push option carrying the lease push token,
	// validated by the hub's pre-receive hook.
	PushOptionPrefix = "zync-token="
)

type LeaseInfo struct {
	Workspace string `json:"workspace"`
	Branch    string `json:"branch"`
	State     string `json:"state"` // "held" | "released" | "expired"
	// Holder is the current holder when held, or the last holder when
	// released (empty if never held).
	Holder              string `json:"holder,omitempty"`
	HolderOpencodeURL   string `json:"holder_opencode_url,omitempty"`
	HolderWorkspacesDir string `json:"holder_workspaces_dir,omitempty"`
	Generation          int64  `json:"generation"`
	SnapshotCommit      string `json:"snapshot_commit,omitempty"`
	BaseCommit          string `json:"base_commit,omitempty"`
	ExpiresAt           int64  `json:"expires_at,omitempty"`
	UpdatedAt           string `json:"updated_at"`
}

type AgentStateBundle struct {
	Digest           string `json:"digest"`
	Size             int64  `json:"size"`
	Format           string `json:"format"`
	SessionID        string `json:"session_id"`
	SourceGeneration int64  `json:"source_generation"`
}

type ExtrasBundle struct {
	Digest           string `json:"digest"`
	Size             int64  `json:"size"`
	Format           string `json:"format"`
	SourceGeneration int64  `json:"source_generation"`
}

type ReplicaInfo struct {
	Name          string `json:"name"`
	Kind          string `json:"kind,omitempty"`
	OpencodeURL   string `json:"opencode_url,omitempty"`
	WorkspacesDir string `json:"workspaces_dir,omitempty"`
	LastSeenAt    string `json:"last_seen_at,omitempty"`
}

type WorkspaceInfo struct {
	Name          string      `json:"name"`
	DefaultBranch string      `json:"default_branch"`
	ArchivedAt    int64       `json:"archived_at,omitempty"`
	Leases        []LeaseInfo `json:"leases,omitempty"`
}

type CreateWorkspaceRequest struct {
	Name          string `json:"name"`
	DefaultBranch string `json:"default_branch"`
}

type TakeRequest struct {
	Branch string `json:"branch"`
	Force  bool   `json:"force"`
}

// TakeResponse is returned when a lease is granted. SnapshotCommit and
// BaseCommit are empty when the lease has never been through a flush
// (freshly created), in which case the taker has nothing to sync.
type TakeResponse struct {
	Generation        int64             `json:"generation"`
	PushToken         string            `json:"push_token"`
	SnapshotCommit    string            `json:"snapshot_commit,omitempty"`
	BaseCommit        string            `json:"base_commit,omitempty"`
	AgentState        *AgentStateBundle `json:"agent_state,omitempty"`
	Extras            *ExtrasBundle     `json:"extras,omitempty"`
	ExpiresAt         int64             `json:"expires_at,omitempty"`
	HeartbeatInterval int64             `json:"heartbeat_interval_seconds,omitempty"`
}

type HeartbeatRequest struct {
	Branch     string `json:"branch"`
	Generation int64  `json:"generation"`
	PushToken  string `json:"push_token"`
}

type HeartbeatResponse struct {
	ExpiresAt         int64 `json:"expires_at,omitempty"`
	HeartbeatInterval int64 `json:"heartbeat_interval_seconds,omitempty"`
}

type ReleaseRequest struct {
	Branch         string            `json:"branch"`
	Generation     int64             `json:"generation"`
	SnapshotCommit string            `json:"snapshot_commit"`
	BaseCommit     string            `json:"base_commit"`
	AgentState     *AgentStateBundle `json:"agent_state,omitempty"`
	Extras         *ExtrasBundle     `json:"extras,omitempty"`
	// HandoffTo, when set, atomically grants the lease to the named replica
	// instead of releasing it back to the pool (release vs handoff).
	HandoffTo string `json:"handoff_to,omitempty"`
}

type ErrorResponse struct {
	Error string `json:"error"`
}
