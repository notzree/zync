// Package agentstate adapts OpenCode's native session export/import format to
// zync's opaque bundle protocol.
package agentstate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/notzree/zync/internal/protocol"
)

func Export(ctx context.Context, dir, sessionID string) (protocol.AgentStateBundle, []byte, error) {
	if sessionID == "" {
		return protocol.AgentStateBundle{}, nil, errors.New("agent session ID is required")
	}
	var stdout boundedBuffer
	stdout.max = protocol.MaxAgentStateBytes
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, opencodeBin(), "export", sessionID, "--pure")
	cmd.Dir = dir
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Env = append(os.Environ(), "OPENCODE_DISABLE_AUTOUPDATE=true")
	if err := cmd.Run(); err != nil {
		return protocol.AgentStateBundle{}, nil, fmt.Errorf("opencode export %s: %w: %s", sessionID, err, strings.TrimSpace(stderr.String()))
	}
	if stdout.exceeded {
		return protocol.AgentStateBundle{}, nil, fmt.Errorf("opencode session exceeds the %d-byte bundle limit", protocol.MaxAgentStateBytes)
	}
	data := stdout.Bytes()
	var envelope struct {
		Info struct {
			ID string `json:"id"`
		} `json:"info"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return protocol.AgentStateBundle{}, nil, fmt.Errorf("parse opencode export: %w", err)
	}
	if envelope.Info.ID == "" || envelope.Info.ID != sessionID {
		return protocol.AgentStateBundle{}, nil, fmt.Errorf("opencode export returned session %q, want %q", envelope.Info.ID, sessionID)
	}
	sum := sha256.Sum256(data)
	bundle := protocol.AgentStateBundle{
		Digest:    hex.EncodeToString(sum[:]),
		Size:      int64(len(data)),
		Format:    protocol.AgentStateFormat,
		SessionID: sessionID,
	}
	return bundle, data, nil
}

func Import(ctx context.Context, dir, gitDir string, bundle protocol.AgentStateBundle, data []byte) error {
	if bundle.Format != protocol.AgentStateFormat || bundle.Size != int64(len(data)) {
		return errors.New("invalid agent-state bundle metadata")
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != bundle.Digest {
		return errors.New("agent-state bundle digest mismatch")
	}
	var envelope struct {
		Info struct {
			ID string `json:"id"`
		} `json:"info"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return fmt.Errorf("parse agent-state bundle: %w", err)
	}
	if envelope.Info.ID != bundle.SessionID {
		return fmt.Errorf("agent-state bundle contains session %q, want %q", envelope.Info.ID, bundle.SessionID)
	}

	tmp, err := os.CreateTemp(gitDir, "zync-agent-state-*.json")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	cmd := exec.CommandContext(ctx, opencodeBin(), "import", name, "--pure")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "OPENCODE_DISABLE_AUTOUPDATE=true")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("opencode import: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func opencodeBin() string {
	if bin := os.Getenv("ZYNC_OPENCODE_BIN"); bin != "" {
		return bin
	}
	return "opencode"
}

type boundedBuffer struct {
	bytes.Buffer
	max      int64
	exceeded bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	original := len(p)
	remaining := b.max - int64(b.Len())
	if int64(len(p)) > remaining {
		b.exceeded = true
		if remaining <= 0 {
			return original, nil
		}
		p = p[:remaining]
	}
	_, err := b.Buffer.Write(p)
	return original, err
}
