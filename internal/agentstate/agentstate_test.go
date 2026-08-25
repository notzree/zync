package agentstate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/notzree/zync/internal/protocol"
)

func TestExportImportRoundTrip(t *testing.T) {
	dir := t.TempDir()
	gitDir := filepath.Join(dir, ".git")
	if err := os.Mkdir(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	imported := filepath.Join(dir, "imported.json")
	bin := filepath.Join(dir, "opencode-test")
	script := `#!/bin/sh
set -eu
case "$1" in
  export) printf '{"info":{"id":"%s"},"messages":[]}\n' "$2" ;;
  import) cp "$2" "$ZYNC_TEST_IMPORTED" ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZYNC_OPENCODE_BIN", bin)
	t.Setenv("ZYNC_TEST_IMPORTED", imported)

	bundle, data, err := Export(context.Background(), dir, "ses_roundtrip")
	if err != nil {
		t.Fatal(err)
	}
	if bundle.SessionID != "ses_roundtrip" || bundle.Size != int64(len(data)) || bundle.Digest == "" {
		t.Fatalf("unexpected bundle: %+v", bundle)
	}
	if err := Import(context.Background(), dir, gitDir, bundle, data); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(imported)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Fatal("import did not receive exported bytes")
	}
}

func TestImportRejectsTamperedBundle(t *testing.T) {
	data := []byte(`{"info":{"id":"ses_tamper"},"messages":[]}`)
	sum := sha256.Sum256(data)
	bundle := protocol.AgentStateBundle{
		Digest:    hex.EncodeToString(sum[:]),
		Size:      int64(len(data)),
		Format:    protocol.AgentStateFormat,
		SessionID: "ses_tamper",
	}
	data[len(data)-2] ^= 1
	if err := Import(context.Background(), t.TempDir(), t.TempDir(), bundle, data); err == nil {
		t.Fatal("Import accepted tampered data")
	}
}
