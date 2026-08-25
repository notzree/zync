package hub

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"sync"
	"time"

	"go.uber.org/fx"

	"github.com/notzree/zync/internal/db/dbgen"
)

// Maintenance runs bounded Git housekeeping in the hub process so the
// persistent volume does not retain superseded snapshots indefinitely.
type Maintenance struct {
	cfg  Config
	q    *dbgen.Queries
	git  *GitManager
	blob *BlobStore

	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

func NewMaintenance(lc fx.Lifecycle, cfg Config, conn *sql.DB, git *GitManager, blob *BlobStore) *Maintenance {
	m := &Maintenance{cfg: cfg, q: dbgen.New(conn), git: git, blob: blob, done: make(chan struct{})}
	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			ctx, cancel := context.WithCancel(context.Background())
			m.cancel = cancel
			go m.run(ctx)
			return nil
		},
		OnStop: func(ctx context.Context) error {
			if m.cancel != nil {
				m.cancel()
			}
			select {
			case <-m.done:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	})
	return m
}

func (m *Maintenance) run(ctx context.Context) {
	defer m.once.Do(func() { close(m.done) })
	if m.cfg.GitGCInterval == 0 {
		return
	}
	for {
		started := time.Now()
		count, err := m.sweep(ctx)
		if err != nil && ctx.Err() == nil {
			slog.Error("zync Git maintenance completed with errors", "repositories", count, "duration", time.Since(started), "error", err)
		} else if ctx.Err() == nil {
			slog.Info("zync Git maintenance completed", "repositories", count, "duration", time.Since(started))
		}

		timer := time.NewTimer(m.cfg.GitGCInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return
		case <-timer.C:
		}
	}
}

func (m *Maintenance) sweep(ctx context.Context) (count int, err error) {
	err = m.git.withGCBarrier(func() error {
		rows, err := m.q.ListAllLeases(ctx)
		if err != nil {
			return err
		}
		roots := make(map[string][]RetentionRoot)
		retainedBlobs := make(map[string]bool)
		for _, row := range rows {
			roots[row.WorkspaceName] = append(roots[row.WorkspaceName], RetentionRoot{
				LeaseID:        row.ID,
				SnapshotCommit: row.SnapshotCommit.String,
				BaseCommit:     row.BaseCommit.String,
			})
			if row.AgentStateDigest.Valid {
				retainedBlobs[row.AgentStateDigest.String] = true
			}
			if row.ExtrasDigest.Valid {
				retainedBlobs[row.ExtrasDigest.String] = true
			}
		}
		var gitErr error
		count, gitErr = m.git.compactAll(ctx, roots)
		_, blobErr := m.blob.PruneUnreferenced(ctx, retainedBlobs, m.cfg.GitGCPruneAge)
		return errors.Join(gitErr, blobErr)
	})
	return count, err
}
