// Package client is the HTTP client for the zync hub API.
package client

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/notzree/zync/internal/protocol"
)

// ErrConflict maps 409 responses (lease held elsewhere, stale generation).
var ErrConflict = errors.New("conflict")

// ErrNotFound maps 404 responses.
var ErrNotFound = errors.New("not found")

type Client struct {
	hubURL        string
	token         string
	replica       string
	kind          string
	opencodeURL   string
	workspacesDir string
	http          *http.Client
}

func New(hubURL, token, replica, kind, opencodeURL, workspacesDir string) *Client {
	return &Client{
		hubURL:        strings.TrimRight(hubURL, "/"),
		token:         token,
		replica:       replica,
		kind:          kind,
		opencodeURL:   opencodeURL,
		workspacesDir: workspacesDir,
		http:          &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) GitURL(workspace string) string {
	return c.hubURL + "/git/" + workspace + ".git"
}

func (c *Client) do(method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, c.hubURL+path, reader)
	if err != nil {
		return err
	}
	c.setHeaders(req)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("hub unreachable: %w", err)
	}
	defer resp.Body.Close()

	if err := responseError(resp); err != nil {
		return err
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (c *Client) CreateWorkspace(name, defaultBranch string) (protocol.WorkspaceInfo, error) {
	var out protocol.WorkspaceInfo
	err := c.do("POST", "/api/workspaces", protocol.CreateWorkspaceRequest{Name: name, DefaultBranch: defaultBranch}, &out)
	return out, err
}

func (c *Client) ListWorkspaces() ([]protocol.WorkspaceInfo, error) {
	var out []protocol.WorkspaceInfo
	err := c.do("GET", "/api/workspaces", nil, &out)
	return out, err
}

func (c *Client) ListAllWorkspaces() ([]protocol.WorkspaceInfo, error) {
	var out []protocol.WorkspaceInfo
	err := c.do("GET", "/api/workspaces?include_archived=1", nil, &out)
	return out, err
}

func (c *Client) GetWorkspace(name string) (protocol.WorkspaceInfo, error) {
	var out protocol.WorkspaceInfo
	err := c.do("GET", "/api/workspaces/"+name, nil, &out)
	return out, err
}

func (c *Client) ArchiveWorkspace(name string) error {
	return c.do("POST", "/api/workspaces/"+url.PathEscape(name)+"/archive", nil, nil)
}

func (c *Client) RestoreWorkspace(name string) error {
	return c.do("POST", "/api/workspaces/"+url.PathEscape(name)+"/restore", nil, nil)
}

func (c *Client) ListReplicas() ([]protocol.ReplicaInfo, error) {
	var out []protocol.ReplicaInfo
	err := c.do("GET", "/api/replicas", nil, &out)
	return out, err
}

func (c *Client) ListLeases() ([]protocol.LeaseInfo, error) {
	var out []protocol.LeaseInfo
	err := c.do("GET", "/api/leases", nil, &out)
	return out, err
}

func (c *Client) Take(workspace, branch string, force bool) (protocol.TakeResponse, error) {
	var out protocol.TakeResponse
	err := c.do("POST", "/api/workspaces/"+workspace+"/take", protocol.TakeRequest{Branch: branch, Force: force}, &out)
	return out, err
}

func (c *Client) Release(workspace string, req protocol.ReleaseRequest) error {
	return c.do("POST", "/api/workspaces/"+workspace+"/release", req, nil)
}

func (c *Client) Heartbeat(workspace string, req protocol.HeartbeatRequest) (protocol.HeartbeatResponse, error) {
	var out protocol.HeartbeatResponse
	err := c.do("POST", "/api/workspaces/"+workspace+"/heartbeat", req, &out)
	return out, err
}

func (c *Client) UploadAgentState(workspace, branch string, generation int64, bundle protocol.AgentStateBundle, data []byte) error {
	path := "/api/workspaces/" + url.PathEscape(workspace) + "/agent-state/" + bundle.Digest +
		"?branch=" + url.QueryEscape(branch) + "&generation=" + fmt.Sprint(generation)
	req, err := http.NewRequest(http.MethodPut, c.hubURL+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	c.setHeaders(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("hub unreachable: %w", err)
	}
	defer resp.Body.Close()
	return responseError(resp)
}

func (c *Client) DownloadAgentState(workspace, branch string, bundle protocol.AgentStateBundle) ([]byte, error) {
	if bundle.Size < 0 || bundle.Size > protocol.MaxAgentStateBytes {
		return nil, errors.New("invalid agent-state bundle size")
	}
	path := "/api/workspaces/" + url.PathEscape(workspace) + "/agent-state/" + bundle.Digest +
		"?branch=" + url.QueryEscape(branch)
	req, err := http.NewRequest(http.MethodGet, c.hubURL+path, nil)
	if err != nil {
		return nil, err
	}
	c.setHeaders(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("hub unreachable: %w", err)
	}
	defer resp.Body.Close()
	if err := responseError(resp); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, bundle.Size+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != bundle.Size {
		return nil, errors.New("agent-state download size mismatch")
	}
	return data, nil
}

func (c *Client) UploadExtras(workspace, branch string, generation int64, bundle protocol.ExtrasBundle, data []byte) error {
	path := "/api/workspaces/" + url.PathEscape(workspace) + "/extras/" + bundle.Digest +
		"?branch=" + url.QueryEscape(branch) + "&generation=" + fmt.Sprint(generation)
	req, err := http.NewRequest(http.MethodPut, c.hubURL+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	c.setHeaders(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("hub unreachable: %w", err)
	}
	defer resp.Body.Close()
	return responseError(resp)
}

func (c *Client) DownloadExtras(workspace, branch string, bundle protocol.ExtrasBundle) ([]byte, error) {
	if bundle.Size < 0 || bundle.Size > protocol.MaxExtrasBytes {
		return nil, errors.New("invalid encrypted extras bundle size")
	}
	path := "/api/workspaces/" + url.PathEscape(workspace) + "/extras/" + bundle.Digest +
		"?branch=" + url.QueryEscape(branch)
	req, err := http.NewRequest(http.MethodGet, c.hubURL+path, nil)
	if err != nil {
		return nil, err
	}
	c.setHeaders(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("hub unreachable: %w", err)
	}
	defer resp.Body.Close()
	if err := responseError(resp); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, bundle.Size+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != bundle.Size {
		return nil, errors.New("encrypted extras download size mismatch")
	}
	return data, nil
}

func (c *Client) setHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set(protocol.ReplicaHeader, c.replica)
	if c.kind != "" {
		req.Header.Set(protocol.ReplicaKindHeader, c.kind)
	}
	if c.opencodeURL != "" {
		req.Header.Set(protocol.OpencodeURLHeader, c.opencodeURL)
	}
	if c.workspacesDir != "" {
		req.Header.Set(protocol.WorkspacesDirHeader, c.workspacesDir)
	}
}

func responseError(resp *http.Response) error {
	if resp.StatusCode < 400 {
		return nil
	}
	var apiErr protocol.ErrorResponse
	msg := resp.Status
	if json.NewDecoder(resp.Body).Decode(&apiErr) == nil && apiErr.Error != "" {
		msg = apiErr.Error
	}
	switch resp.StatusCode {
	case http.StatusConflict:
		return fmt.Errorf("%w: %s", ErrConflict, msg)
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrNotFound, msg)
	default:
		return errors.New(msg)
	}
}

func (c *Client) Replica() string { return c.replica }
