package hub

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net"
	"net/http"

	"go.uber.org/fx"

	"github.com/notzree/zync/internal/db"
)

func NewDB(lc fx.Lifecycle, cfg Config) (*sql.DB, error) {
	conn, err := db.Open(cfg.DBPath())
	if err != nil {
		return nil, err
	}
	lc.Append(fx.Hook{
		OnStop: func(ctx context.Context) error { return conn.Close() },
	})
	return conn, nil
}

func NewHTTPServer(lc fx.Lifecycle, cfg Config, svc *LeaseService, mux *http.ServeMux) *http.Server {
	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.Port),
		Handler: WithAuth(cfg, svc, mux),
	}
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			ln, err := net.Listen("tcp", srv.Addr)
			if err != nil {
				return err
			}
			slog.Info("zync hub listening", "addr", srv.Addr, "data_dir", cfg.DataDir)
			go srv.Serve(ln)
			return nil
		},
		OnStop: func(ctx context.Context) error {
			return srv.Shutdown(ctx)
		},
	})
	return srv
}

// Module wires the hub server for fx.
var Module = fx.Options(
	fx.Provide(
		NewConfig,
		NewDB,
		NewGitManager,
		NewLeaseService,
		NewGitHandler,
		NewMux,
		NewHTTPServer,
	),
	fx.Invoke(func(*http.Server) {}),
)
