package runpodclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const apiBase = "https://rest.runpod.io/v1"

type Client struct {
	Token string
	HTTP  *http.Client
}

type Pod struct {
	ID            string         `json:"id"`
	Name          string         `json:"name"`
	DesiredStatus string         `json:"desiredStatus"`
	Runtime       Runtime        `json:"runtime"`
	PortMappings  map[string]any `json:"portMappings"`
	PublicIP      string         `json:"publicIp"`
	CostPerHr     float64        `json:"costPerHr"`
	MachineID     string         `json:"machineId"`
	GPUTypeID     string         `json:"gpuTypeId"`
	ImageName     string         `json:"imageName"`
	CreatedAt     string         `json:"createdAt"`
}

type Runtime struct {
	Ports []RuntimePort `json:"ports"`
}

type RuntimePort struct {
	IP          string `json:"ip"`
	PrivatePort int    `json:"privatePort"`
	PublicPort  int    `json:"publicPort"`
	Type        string `json:"type"`
}

type CreatePodRequest struct {
	Name              string            `json:"name"`
	ImageName         string            `json:"imageName"`
	GPUTypeIDs        []string          `json:"gpuTypeIds"`
	GPUCount          int               `json:"gpuCount"`
	CloudType         string            `json:"cloudType,omitempty"`
	ContainerDiskInGB int               `json:"containerDiskInGb,omitempty"`
	VolumeInGB        int               `json:"volumeInGb,omitempty"`
	Ports             []string          `json:"ports,omitempty"`
	Env               map[string]string `json:"env,omitempty"`
	DockerStartCmd    []string          `json:"dockerStartCmd,omitempty"`
}

func New(token string) *Client {
	return &Client{
		Token: strings.TrimSpace(token),
		HTTP:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) ListPods(ctx context.Context) ([]Pod, error) {
	var pods []Pod
	if err := c.request(ctx, http.MethodGet, "/pods", nil, &pods); err != nil {
		var resp struct {
			Pods []Pod `json:"pods"`
		}
		if err2 := c.request(ctx, http.MethodGet, "/pods", nil, &resp); err2 != nil {
			return nil, err
		}
		return resp.Pods, nil
	}
	return pods, nil
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
	return c.request(ctx, http.MethodDelete, "/pods/"+id, nil, nil)
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
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(data)))
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}

func (p Pod) Status() string {
	status := strings.ToLower(p.DesiredStatus)
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
	for _, port := range p.Runtime.Ports {
		if port.PrivatePort == 22 && port.PublicPort > 0 {
			return firstNonEmpty(port.IP, p.PublicIP), port.PublicPort
		}
	}
	for key, value := range p.PortMappings {
		if key != "22" && key != "22/tcp" {
			continue
		}
		switch v := value.(type) {
		case float64:
			return p.PublicIP, int(v)
		case string:
			port, _ := strconv.Atoi(v)
			return p.PublicIP, port
		case map[string]any:
			ip, _ := v["ip"].(string)
			publicPort := 0
			switch pv := v["publicPort"].(type) {
			case float64:
				publicPort = int(pv)
			case string:
				publicPort, _ = strconv.Atoi(pv)
			}
			return firstNonEmpty(ip, p.PublicIP), publicPort
		}
	}
	return "", 0
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
