package hub

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cgi"
	"os/exec"

	"github.com/notzree/zync/internal/protocol"
)

// NewGitHandler serves the git smart HTTP protocol by delegating to
// `git http-backend` over CGI. Fetches are allowed for any authenticated
// replica; pushes are additionally fenced by the pre-receive hook, which
// calls back into /internal/validate-push with the lease push token.
func NewGitHandler(cfg Config) (http.Handler, error) {
	gitPath, err := exec.LookPath(cfg.GitBin)
	if err != nil {
		return nil, fmt.Errorf("git binary not found: %w", err)
	}
	return &cgi.Handler{
		Path: gitPath,
		Args: []string{"http-backend"},
		Root: "/git",
		Env: []string{
			"GIT_PROJECT_ROOT=" + cfg.ReposDir(),
			"GIT_HTTP_EXPORT_ALL=1",
			fmt.Sprintf("ZYNC_INTERNAL_URL=http://127.0.0.1:%d", cfg.Port),
			"ZYNC_TOKEN=" + cfg.Token,
		},
		InheritEnv: []string{"PATH", "HOME"},
	}, nil
}

func NewMux(cfg Config, svc *LeaseService, git http.Handler) *http.ServeMux {
	mux := http.NewServeMux()

	writeErr := func(w http.ResponseWriter, err error) {
		status := http.StatusInternalServerError
		switch {
		case errors.Is(err, ErrNotFound):
			status = http.StatusNotFound
		case errors.Is(err, ErrConflict):
			status = http.StatusConflict
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(protocol.ErrorResponse{Error: err.Error()})
	}
	writeJSON := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(v)
	}
	replica := func(r *http.Request) string { return r.Header.Get(protocol.ReplicaHeader) }

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("POST /api/workspaces", func(w http.ResponseWriter, r *http.Request) {
		var req protocol.CreateWorkspaceRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, fmt.Errorf("%w: invalid json body", ErrConflict))
			return
		}
		info, err := svc.CreateWorkspace(r.Context(), req.Name, req.DefaultBranch)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, info)
	})

	mux.HandleFunc("GET /api/workspaces", func(w http.ResponseWriter, r *http.Request) {
		out, err := svc.ListWorkspaces(r.Context())
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	})

	mux.HandleFunc("GET /api/workspaces/{ws}", func(w http.ResponseWriter, r *http.Request) {
		info, err := svc.GetWorkspace(r.Context(), r.PathValue("ws"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, info)
	})

	mux.HandleFunc("GET /api/leases", func(w http.ResponseWriter, r *http.Request) {
		out, err := svc.ListLeases(r.Context())
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	})

	mux.HandleFunc("POST /api/workspaces/{ws}/take", func(w http.ResponseWriter, r *http.Request) {
		var req protocol.TakeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, fmt.Errorf("%w: invalid json body", ErrConflict))
			return
		}
		resp, err := svc.Take(r.Context(), r.PathValue("ws"), req.Branch, replica(r), req.Force)
		if err != nil {
			writeErr(w, err)
			return
		}
		slog.Info("lease taken", "workspace", r.PathValue("ws"), "branch", req.Branch, "replica", replica(r), "generation", resp.Generation, "force", req.Force)
		writeJSON(w, http.StatusOK, resp)
	})

	mux.HandleFunc("POST /api/workspaces/{ws}/release", func(w http.ResponseWriter, r *http.Request) {
		var req protocol.ReleaseRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, fmt.Errorf("%w: invalid json body", ErrConflict))
			return
		}
		if err := svc.Release(r.Context(), r.PathValue("ws"), req, replica(r)); err != nil {
			writeErr(w, err)
			return
		}
		slog.Info("lease released", "workspace", r.PathValue("ws"), "branch", req.Branch, "replica", replica(r), "generation", req.Generation)
		writeJSON(w, http.StatusOK, map[string]bool{"released": true})
	})

	mux.HandleFunc("POST /internal/validate-push", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			writeErr(w, err)
			return
		}
		q := r.URL.Query()
		if err := svc.ValidatePush(r.Context(), q.Get("workspace"), q.Get("token"), string(body)); err != nil {
			slog.Warn("push rejected", "workspace", q.Get("workspace"), "err", err)
			writeErr(w, err)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	mux.Handle("/git/", git)
	return mux
}

// WithAuth wraps the mux with bearer token auth and replica registration.
func WithAuth(cfg Config, svc *LeaseService, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		auth := r.Header.Get("Authorization")
		expect := "Bearer " + cfg.Token
		if subtle.ConstantTimeCompare([]byte(auth), []byte(expect)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="zync"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if name := r.Header.Get(protocol.ReplicaHeader); name != "" {
			if err := svc.EnsureReplica(r.Context(), name); err != nil {
				http.Error(w, "replica registration failed", http.StatusInternalServerError)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
