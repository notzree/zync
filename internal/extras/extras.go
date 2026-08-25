// Package extras implements the client-side encrypted side channel for
// allowlisted files that Git intentionally ignores.
package extras

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"filippo.io/age"

	"github.com/notzree/zync/internal/protocol"
)

const (
	ManifestName      = ".zync-extras.json"
	maxManifestBytes  = 64 << 10
	maxPaths          = 128
	maxRecipients     = 32
	maxPlaintextBytes = 10 << 20
)

type Manifest struct {
	Version    int      `json:"version"`
	Paths      []string `json:"paths"`
	Recipients []string `json:"recipients"`
}

type payload struct {
	Version int     `json:"version"`
	Entries []entry `json:"entries"`
}

type entry struct {
	Path    string      `json:"path"`
	Mode    os.FileMode `json:"mode,omitempty"`
	Deleted bool        `json:"deleted,omitempty"`
	Content []byte      `json:"content,omitempty"`
}

// Export returns nil when the repository has no extras manifest.
func Export(ctx context.Context, root string) (*protocol.ExtrasBundle, []byte, error) {
	manifest, err := loadManifest(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	recipients := make([]age.Recipient, 0, len(manifest.Recipients))
	for _, encoded := range manifest.Recipients {
		recipient, err := age.ParseX25519Recipient(encoded)
		if err != nil {
			return nil, nil, fmt.Errorf("parse age recipient: %w", err)
		}
		recipients = append(recipients, recipient)
	}

	p := payload{Version: 1, Entries: make([]entry, 0, len(manifest.Paths))}
	total := 0
	for _, name := range manifest.Paths {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		rel, err := validatePath(name)
		if err != nil {
			return nil, nil, err
		}
		if err := checkParents(root, rel, false); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, nil, err
		}
		full := filepath.Join(root, filepath.FromSlash(rel))
		info, err := os.Lstat(full)
		if errors.Is(err, os.ErrNotExist) {
			p.Entries = append(p.Entries, entry{Path: rel, Deleted: true})
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, nil, fmt.Errorf("extra %q must be a regular file", rel)
		}
		data, err := os.ReadFile(full)
		if err != nil {
			return nil, nil, err
		}
		total += len(data)
		if total > maxPlaintextBytes {
			return nil, nil, fmt.Errorf("extras exceed the %d-byte plaintext limit", maxPlaintextBytes)
		}
		p.Entries = append(p.Entries, entry{Path: rel, Mode: info.Mode().Perm(), Content: data})
	}
	plain, err := json.Marshal(p)
	if err != nil {
		return nil, nil, err
	}
	var encrypted bytes.Buffer
	w, err := age.Encrypt(&encrypted, recipients...)
	if err != nil {
		return nil, nil, err
	}
	if _, err := w.Write(plain); err != nil {
		return nil, nil, err
	}
	if err := w.Close(); err != nil {
		return nil, nil, err
	}
	if int64(encrypted.Len()) > protocol.MaxExtrasBytes {
		return nil, nil, fmt.Errorf("encrypted extras exceed the %d-byte bundle limit", protocol.MaxExtrasBytes)
	}
	ciphertext := encrypted.Bytes()
	sum := sha256.Sum256(ciphertext)
	bundle := &protocol.ExtrasBundle{
		Digest: hex.EncodeToString(sum[:]), Size: int64(len(ciphertext)), Format: protocol.ExtrasFormat,
	}
	return bundle, ciphertext, nil
}

func Import(ctx context.Context, root string, bundle protocol.ExtrasBundle, ciphertext []byte) error {
	if bundle.Format != protocol.ExtrasFormat || bundle.Size != int64(len(ciphertext)) || bundle.Size > protocol.MaxExtrasBytes {
		return errors.New("invalid encrypted extras metadata")
	}
	sum := sha256.Sum256(ciphertext)
	if hex.EncodeToString(sum[:]) != bundle.Digest {
		return errors.New("encrypted extras digest mismatch")
	}
	identityPath := os.Getenv("ZYNC_AGE_IDENTITY_FILE")
	if identityPath == "" {
		return errors.New("encrypted extras require ZYNC_AGE_IDENTITY_FILE")
	}
	identityData, err := os.ReadFile(identityPath)
	if err != nil {
		return fmt.Errorf("read age identity: %w", err)
	}
	identities, err := age.ParseIdentities(bytes.NewReader(identityData))
	if err != nil {
		return fmt.Errorf("parse age identity: %w", err)
	}
	r, err := age.Decrypt(bytes.NewReader(ciphertext), identities...)
	if err != nil {
		return fmt.Errorf("decrypt extras: %w", err)
	}
	plain, err := io.ReadAll(io.LimitReader(&contextReader{ctx: ctx, r: r}, maxPlaintextBytes+1))
	if err != nil {
		return err
	}
	if len(plain) > maxPlaintextBytes {
		return errors.New("decrypted extras exceed the plaintext limit")
	}
	var p payload
	if err := json.Unmarshal(plain, &p); err != nil || p.Version != 1 || len(p.Entries) > maxPaths {
		return errors.New("invalid encrypted extras payload")
	}

	seen := make(map[string]bool, len(p.Entries))
	for i := range p.Entries {
		rel, err := validatePath(p.Entries[i].Path)
		if err != nil || seen[rel] {
			return errors.New("encrypted extras contain an invalid or duplicate path")
		}
		seen[rel] = true
		p.Entries[i].Path = rel
		if p.Entries[i].Deleted && len(p.Entries[i].Content) != 0 {
			return fmt.Errorf("deleted extra %q contains data", rel)
		}
		if p.Entries[i].Mode&^0o777 != 0 {
			return fmt.Errorf("extra %q has an invalid mode", rel)
		}
		if err := checkParents(root, rel, false); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(rel))); err == nil && !info.Mode().IsRegular() {
			return fmt.Errorf("extra target %q is not a regular file", rel)
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}

	sort.Slice(p.Entries, func(i, j int) bool { return p.Entries[i].Path < p.Entries[j].Path })
	temps := make(map[string]string)
	defer func() {
		for _, name := range temps {
			os.Remove(name)
		}
	}()
	for _, item := range p.Entries {
		if item.Deleted {
			continue
		}
		if err := checkParents(root, item.Path, true); err != nil {
			return err
		}
		full := filepath.Join(root, filepath.FromSlash(item.Path))
		tmp, err := os.CreateTemp(filepath.Dir(full), ".zync-extra-*")
		if err != nil {
			return err
		}
		mode := item.Mode
		if mode == 0 {
			mode = 0o600
		}
		if err := tmp.Chmod(mode); err != nil {
			tmp.Close()
			return err
		}
		if _, err := tmp.Write(item.Content); err != nil {
			tmp.Close()
			return err
		}
		if err := tmp.Close(); err != nil {
			return err
		}
		temps[item.Path] = tmp.Name()
	}
	for _, item := range p.Entries {
		full := filepath.Join(root, filepath.FromSlash(item.Path))
		if item.Deleted {
			if err := os.Remove(full); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			continue
		}
		if err := os.Rename(temps[item.Path], full); err != nil {
			return err
		}
		delete(temps, item.Path)
	}
	return nil
}

func loadManifest(root string) (Manifest, error) {
	f, err := os.Open(filepath.Join(root, ManifestName))
	if err != nil {
		return Manifest{}, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxManifestBytes+1))
	if err != nil {
		return Manifest{}, err
	}
	if len(data) > maxManifestBytes {
		return Manifest{}, errors.New("extras manifest is too large")
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("parse %s: %w", ManifestName, err)
	}
	if manifest.Version != 1 || len(manifest.Paths) == 0 || len(manifest.Paths) > maxPaths || len(manifest.Recipients) == 0 || len(manifest.Recipients) > maxRecipients {
		return Manifest{}, errors.New("extras manifest must have version 1 and bounded non-empty paths and recipients")
	}
	seen := make(map[string]bool, len(manifest.Paths))
	for _, name := range manifest.Paths {
		rel, err := validatePath(name)
		if err != nil || seen[rel] {
			return Manifest{}, errors.New("extras manifest contains an invalid or duplicate path")
		}
		seen[rel] = true
	}
	return manifest, nil
}

func validatePath(name string) (string, error) {
	if name == "" || strings.Contains(name, "\\") || strings.HasPrefix(name, "/") || path.Clean(name) != name {
		return "", fmt.Errorf("invalid extras path %q", name)
	}
	parts := strings.Split(name, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || part == ".git" {
			return "", fmt.Errorf("invalid extras path %q", name)
		}
	}
	return name, nil
}

func checkParents(root, rel string, create bool) error {
	current := root
	parts := strings.Split(rel, "/")
	for _, part := range parts[:len(parts)-1] {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) && create {
			if err := os.Mkdir(current, 0o700); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("extras parent %q is not a real directory", current)
		}
	}
	return nil
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
