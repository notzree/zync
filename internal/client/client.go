// Package client is the HTTP client for the zync hub API.
package client

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/notzree/zync/internal/protocol"
)

// ErrConflict maps 409 responses (lease held elsewhere, stale generation).
var ErrConflict = errors.New("conflict")

// ErrNotFound maps 404 responses.
var ErrNotFound = errors.New("not found")

type Client struct {
	hubURL  string
	token   string
	replica string
	http    *http.Client
}

func New(hubURL, token, replica string) *Client {
	return &Client{
		hubURL:  strings.TrimRight(hubURL, "/"),
		token:   token,
		replica: replica,
		http:    &http.Client{Timeout: 30 * time.Second},
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
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set(protocol.ReplicaHeader, c.replica)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("hub unreachable: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
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

func (c *Client) GetWorkspace(name string) (protocol.WorkspaceInfo, error) {
	var out protocol.WorkspaceInfo
	err := c.do("GET", "/api/workspaces/"+name, nil, &out)
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

func (c *Client) Replica() string { return c.replica }
