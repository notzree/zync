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
	"strconv"

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
		var out []protocol.WorkspaceInfo
		var err error
		if r.URL.Query().Get("include_archived") == "1" {
			out, err = svc.ListAllWorkspaces(r.Context())
		} else {
			out, err = svc.ListWorkspaces(r.Context())
		}
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	})

	mux.HandleFunc("POST /api/workspaces/{ws}/archive", func(w http.ResponseWriter, r *http.Request) {
		if err := svc.ArchiveWorkspace(r.Context(), r.PathValue("ws")); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"archived": true})
	})

	mux.HandleFunc("POST /api/workspaces/{ws}/restore", func(w http.ResponseWriter, r *http.Request) {
		if err := svc.RestoreWorkspace(r.Context(), r.PathValue("ws")); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"restored": true})
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

	mux.HandleFunc("GET /api/replicas", func(w http.ResponseWriter, r *http.Request) {
		out, err := svc.ListReplicas(r.Context())
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

	mux.HandleFunc("POST /api/workspaces/{ws}/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		var req protocol.HeartbeatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, fmt.Errorf("%w: invalid json body", ErrConflict))
			return
		}
		resp, err := svc.Heartbeat(r.Context(), r.PathValue("ws"), replica(r), req)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, resp)
	})

	mux.HandleFunc("PUT /api/workspaces/{ws}/agent-state/{digest}", func(w http.ResponseWriter, r *http.Request) {
		generation, err := strconv.ParseInt(r.URL.Query().Get("generation"), 10, 64)
		if err != nil || r.ContentLength < 0 {
			writeErr(w, fmt.Errorf("%w: generation and Content-Length are required", ErrConflict))
			return
		}
		if err := svc.UploadAgentState(r.Context(), r.PathValue("ws"), r.URL.Query().Get("branch"), replica(r), generation, r.PathValue("digest"), r.ContentLength, r.Body); err != nil {
			writeErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("GET /api/workspaces/{ws}/agent-state/{digest}", func(w http.ResponseWriter, r *http.Request) {
		f, size, err := svc.OpenAgentState(r.Context(), r.PathValue("ws"), r.URL.Query().Get("branch"), r.PathValue("digest"))
		if err != nil {
			writeErr(w, err)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		if _, err := io.Copy(w, f); err != nil {
			slog.Warn("agent-state download interrupted", "digest", r.PathValue("digest"), "error", err)
		}
	})

	mux.HandleFunc("PUT /api/workspaces/{ws}/extras/{digest}", func(w http.ResponseWriter, r *http.Request) {
		generation, err := strconv.ParseInt(r.URL.Query().Get("generation"), 10, 64)
		if err != nil || r.ContentLength < 0 {
			writeErr(w, fmt.Errorf("%w: generation and Content-Length are required", ErrConflict))
			return
		}
		if err := svc.UploadExtras(r.Context(), r.PathValue("ws"), r.URL.Query().Get("branch"), replica(r), generation, r.PathValue("digest"), r.ContentLength, r.Body); err != nil {
			writeErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("GET /api/workspaces/{ws}/extras/{digest}", func(w http.ResponseWriter, r *http.Request) {
		f, size, err := svc.OpenExtras(r.Context(), r.PathValue("ws"), r.URL.Query().Get("branch"), r.PathValue("digest"))
		if err != nil {
			writeErr(w, err)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		if _, err := io.Copy(w, f); err != nil {
			slog.Warn("encrypted extras download interrupted", "digest", r.PathValue("digest"), "error", err)
		}
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
			err := svc.EnsureReplica(r.Context(), name,
				r.Header.Get(protocol.OpencodeURLHeader),
				r.Header.Get(protocol.WorkspacesDirHeader),
				r.Header.Get(protocol.ReplicaKindHeader))
			if err != nil {
				http.Error(w, "replica registration failed", http.StatusBadRequest)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
