// Package protocol defines the wire types shared between the hub server and
// the CLI client.
package protocol

const (
	// ReplicaHeader identifies the calling replica on every API request.
	ReplicaHeader = "X-Zync-Replica"
	// PushOptionPrefix is the git push option carrying the lease push token,
	// validated by the hub's pre-receive hook.
	PushOptionPrefix = "zync-token="
)

type LeaseInfo struct {
	Workspace      string `json:"workspace"`
	Branch         string `json:"branch"`
	State          string `json:"state"` // "held" | "released"
	Holder         string `json:"holder,omitempty"`
	Generation     int64  `json:"generation"`
	SnapshotCommit string `json:"snapshot_commit,omitempty"`
	BaseCommit     string `json:"base_commit,omitempty"`
	UpdatedAt      string `json:"updated_at"`
}

type WorkspaceInfo struct {
	Name          string      `json:"name"`
	DefaultBranch string      `json:"default_branch"`
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
	Generation     int64  `json:"generation"`
	PushToken      string `json:"push_token"`
	SnapshotCommit string `json:"snapshot_commit,omitempty"`
	BaseCommit     string `json:"base_commit,omitempty"`
}

type ReleaseRequest struct {
	Branch         string `json:"branch"`
	Generation     int64  `json:"generation"`
	SnapshotCommit string `json:"snapshot_commit"`
	BaseCommit     string `json:"base_commit"`
}

type ErrorResponse struct {
	Error string `json:"error"`
}
