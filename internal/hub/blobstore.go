package hub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/notzree/zync/internal/protocol"
)

// BlobStore stores opaque, immutable, content-addressed agent-state bundles.
type BlobStore struct {
	dir string
}

func NewBlobStore(cfg Config) (*BlobStore, error) {
	dir := filepath.Join(cfg.BlobsDir(), "sha256")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create blob store: %w", err)
	}
	return &BlobStore{dir: dir}, nil
}

func (b *BlobStore) Put(ctx context.Context, digest string, size int64, src io.Reader) error {
	if !validDigest(digest) || size < 0 || size > protocol.MaxAgentStateBytes {
		return fmt.Errorf("%w: invalid agent-state blob descriptor", ErrConflict)
	}
	path := b.path(digest)
	if info, err := os.Stat(path); err == nil {
		if info.Size() != size {
			return fmt.Errorf("existing blob %s has unexpected size", digest)
		}
		// A retried upload must get a fresh grace period before it is associated
		// with a release transaction.
		now := time.Now()
		return os.Chtimes(path, now, now)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}

	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(tmp, hash), io.LimitReader(&contextReader{ctx: ctx, r: src}, protocol.MaxAgentStateBytes+1))
	if copyErr != nil {
		tmp.Close()
		return copyErr
	}
	if written != size || written > protocol.MaxAgentStateBytes || hex.EncodeToString(hash.Sum(nil)) != digest {
		tmp.Close()
		return fmt.Errorf("%w: agent-state upload digest or size mismatch", ErrConflict)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return nil
}

func (b *BlobStore) Open(digest string, size int64) (*os.File, error) {
	if !validDigest(digest) {
		return nil, fmt.Errorf("%w: invalid blob digest", ErrNotFound)
	}
	f, err := os.Open(b.path(digest))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: agent-state blob %s", ErrNotFound, digest)
	}
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if info.Size() != size {
		f.Close()
		return nil, fmt.Errorf("agent-state blob %s has unexpected size", digest)
	}
	return f, nil
}

func (b *BlobStore) Exists(digest string, size int64) bool {
	f, err := b.Open(digest, size)
	if err != nil {
		return false
	}
	f.Close()
	return true
}

func (b *BlobStore) PruneUnreferenced(ctx context.Context, retained map[string]bool, grace time.Duration) (int, error) {
	cutoff := time.Now().Add(-grace)
	removed := 0
	var errs []error
	for prefix := 0; prefix < 256; prefix++ {
		if err := ctx.Err(); err != nil {
			return removed, errors.Join(append(errs, err)...)
		}
		dir := filepath.Join(b.dir, fmt.Sprintf("%02x", prefix))
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, entry := range entries {
			digest := fmt.Sprintf("%02x", prefix) + entry.Name()
			if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() || !validDigest(digest) || retained[digest] {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if info.ModTime().After(cutoff) {
				continue
			}
			if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
				errs = append(errs, err)
			} else {
				removed++
			}
		}
	}
	return removed, errors.Join(errs...)
}

func (b *BlobStore) path(digest string) string {
	return filepath.Join(b.dir, digest[:2], digest[2:])
}

func validDigest(digest string) bool {
	return len(digest) == sha256.Size*2 && strings.ToLower(digest) == digest && validObjectID(digest)
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
