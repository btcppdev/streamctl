package runpodclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const apiBase = "https://api.runpod.io/v2"

type Client struct {
	Token string
	HTTP  *http.Client
}

type Pod struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	State     string    `json:"status"`
	Runtime   Runtime   `json:"runtime"`
	CostPerHr float64   `json:"cost"`
	GPU       GPUConfig `json:"gpu"`
	ImageName string    `json:"image"`
	CreatedAt string    `json:"createdAt"`
	SSH       struct {
		Direct *SSHEndpoint `json:"direct"`
	} `json:"ssh"`
}

type SSHEndpoint struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
}

type GPUConfig struct {
	ID    string `json:"id"`
	Count int    `json:"count"`
}

type PersistentMount struct {
	Size int    `json:"size"`
	Path string `json:"path"`
}

type Mounts struct {
	Persistent *PersistentMount `json:"persistent,omitempty"`
}

type Runtime struct {
	Ports []RuntimePort `json:"ports"`
}

type RuntimePort struct {
	IP          string `json:"ip"`
	PrivatePort int    `json:"private"`
	PublicPort  int    `json:"public"`
	Type        string `json:"type"`
}

type CreatePodRequest struct {
	Name              string            `json:"name"`
	ImageName         string            `json:"image"`
	GPU               GPUConfig         `json:"gpu"`
	CloudType         string            `json:"cloud,omitempty"`
	ContainerDiskInGB int               `json:"disk,omitempty"`
	Mounts            Mounts            `json:"mounts"`
	Ports             []string          `json:"ports,omitempty"`
	Env               map[string]string `json:"env,omitempty"`
	DockerStartCmd    []string          `json:"cmd,omitempty"`
}

func New(token string) *Client {
	return &Client{
		Token: strings.TrimSpace(token),
		HTTP:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) ListPods(ctx context.Context) ([]Pod, error) {
	var pods []Pod
	cursor := ""
	seen := map[string]bool{}
	for {
		var page struct {
			Pods       []Pod `json:"pods"`
			Pagination struct {
				NextCursor  string `json:"nextCursor"`
				HasNextPage bool   `json:"hasNextPage"`
			} `json:"pagination"`
		}
		path := "/pods?limit=20"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		if err := c.request(ctx, http.MethodGet, path, nil, &page); err != nil {
			return nil, err
		}
		pods = append(pods, page.Pods...)
		if !page.Pagination.HasNextPage {
			return pods, nil
		}
		cursor = page.Pagination.NextCursor
		if cursor == "" || seen[cursor] {
			return nil, fmt.Errorf("RunPod returned an invalid/repeated pagination cursor")
		}
		seen[cursor] = true
	}
}

// List responses omit live direct-SSH mappings; resolve the selected pod only.
func (c *Client) GetPod(ctx context.Context, id string) (*Pod, error) {
	var pod Pod
	if err := c.request(ctx, http.MethodGet, "/pods/"+url.PathEscape(id), nil, &pod); err != nil {
		return nil, err
	}
	if pod.ID != id {
		return nil, fmt.Errorf("RunPod returned an unexpected pod ID")
	}
	return &pod, nil
}

func (c *Client) CreatePod(ctx context.Context, req CreatePodRequest) (*Pod, error) {
	// Creation is not idempotent. Decode alternate response shapes from the
	// same response, never by issuing a second potentially billable POST.
	var raw json.RawMessage
	if err := c.request(ctx, http.MethodPost, "/pods", req, &raw); err != nil {
		return nil, err
	}
	var response struct {
		Pod
		Wrapped *Pod `json:"pod"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, err
	}
	pod := response.Pod
	if response.Wrapped != nil {
		pod = *response.Wrapped
	}
	if strings.TrimSpace(pod.ID) == "" {
		return nil, fmt.Errorf("RunPod creation returned no pod ID; reconcile existing pods before retrying")
	}
	return &pod, nil
}

func (c *Client) DeletePod(ctx context.Context, id string) error {
	return c.request(ctx, http.MethodDelete, "/pods/"+url.PathEscape(id), nil, nil)
}

func (c *Client) request(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return err
		}
		reader = &buf
	}
	req, err := http.NewRequestWithContext(ctx, method, apiBase+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// List entries include container configuration, including embedded setup
	// scripts. Keep a bound while allowing a complete 20-item page.
	data, err := io.ReadAll(io.LimitReader(resp.Body, (8<<20)+1))
	if err != nil {
		return err
	}
	if len(data) > 8<<20 {
		return fmt.Errorf("RunPod response exceeded 8 MiB")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(data)))
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}

func (p Pod) Status() string {
	status := strings.ToLower(p.State)
	switch status {
	case "running", "running_secure", "running_community":
		return "active"
	case "exited", "terminated":
		return "inactive"
	case "":
		return "unknown"
	default:
		return status
	}
}

func (p Pod) SSHHost(user string) string {
	ip, port := p.SSHAddress()
	if ip == "" || port == 0 {
		return ""
	}
	if user == "" {
		user = "root"
	}
	return "ssh://" + user + "@" + ip + ":" + strconv.Itoa(port)
}

func (p Pod) SSHAddress() (string, int) {
	if direct := p.SSH.Direct; direct != nil && direct.Host != "" && direct.Port > 0 {
		return direct.Host, direct.Port
	}
	for _, port := range p.Runtime.Ports {
		if port.PrivatePort == 22 && port.PublicPort > 0 {
			return port.IP, port.PublicPort
		}
	}
	return "", 0
}
