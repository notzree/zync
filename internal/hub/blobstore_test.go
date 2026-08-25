package hub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"
	"time"
)

func TestBlobStoreRoundTripAndPrune(t *testing.T) {
	store, err := NewBlobStore(Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"info":{"id":"ses_blob"},"messages":[]}`)
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	if err := store.Put(context.Background(), digest, int64(len(data)), bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	f, err := store.Open(digest, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(store.path(digest), old, old); err != nil {
		t.Fatal(err)
	}
	removed, err := store.PruneUnreferenced(context.Background(), nil, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 || store.Exists(digest, int64(len(data))) {
		t.Fatal("unreferenced blob was not pruned")
	}
}

func TestBlobStoreRejectsDigestMismatch(t *testing.T) {
	store, err := NewBlobStore(Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	digest := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := store.Put(context.Background(), digest, 3, bytes.NewReader([]byte("bad"))); err == nil {
		t.Fatal("Put accepted a digest mismatch")
	}
}
