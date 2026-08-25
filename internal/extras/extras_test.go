package extras

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
)

func TestEncryptedExtrasRoundTripAndDeletion(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	target := t.TempDir()
	writeManifest(t, source, identity.Recipient().String(), []string{".env", "config/gone.env"})
	if err := os.WriteFile(filepath.Join(source, ".env"), []byte("API_TOKEN=plaintext-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(target, "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "config", "gone.env"), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}

	bundle, ciphertext, err := Export(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	if bundle == nil {
		t.Fatal("Export returned no bundle")
	}
	if strings.Contains(string(ciphertext), "plaintext-secret") {
		t.Fatal("ciphertext contains plaintext secret")
	}
	identityFile := filepath.Join(t.TempDir(), "age-key.txt")
	if err := os.WriteFile(identityFile, []byte(identity.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZYNC_AGE_IDENTITY_FILE", identityFile)
	if err := Import(context.Background(), target, *bundle, ciphertext); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(target, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "API_TOKEN=plaintext-secret\n" {
		t.Fatalf("restored secret = %q", got)
	}
	if _, err := os.Stat(filepath.Join(target, "config", "gone.env")); !os.IsNotExist(err) {
		t.Fatal("deleted allowlisted file still exists")
	}
}

func TestEncryptedExtrasRejectWrongIdentityAndUnsafePath(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	writeManifest(t, source, identity.Recipient().String(), []string{".env"})
	if err := os.WriteFile(filepath.Join(source, ".env"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, ciphertext, err := Export(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	wrong, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	identityFile := filepath.Join(t.TempDir(), "wrong-key.txt")
	if err := os.WriteFile(identityFile, []byte(wrong.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZYNC_AGE_IDENTITY_FILE", identityFile)
	if err := Import(context.Background(), t.TempDir(), *bundle, ciphertext); err == nil {
		t.Fatal("Import accepted the wrong identity")
	}

	writeManifest(t, source, identity.Recipient().String(), []string{"../escape"})
	if _, _, err := Export(context.Background(), source); err == nil {
		t.Fatal("Export accepted path traversal")
	}
}

func writeManifest(t *testing.T, root, recipient string, paths []string) {
	t.Helper()
	data, err := json.Marshal(Manifest{Version: 1, Paths: paths, Recipients: []string{recipient}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ManifestName), data, 0o644); err != nil {
		t.Fatal(err)
	}
}
