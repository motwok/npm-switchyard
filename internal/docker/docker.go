// Package docker is a minimal read-only Docker API client (SC-4).
package docker

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
)

const APIVersion = "v1.44"

type Client struct {
	http *http.Client
}

func New() *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) },
	}
	return &Client{http: &http.Client{Transport: tr}}
}

func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	u := "http://docker/" + APIVersion + path
	if q != nil {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &StatusError{Code: resp.StatusCode, Msg: strings.TrimSpace(string(b))}
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

type StatusError struct {
	Code int
	Msg  string
}

func (e *StatusError) Error() string { return fmt.Sprintf("docker api %d: %s", e.Code, e.Msg) }

func IsNotFound(err error) bool {
	se, ok := err.(*StatusError)
	return ok && se.Code == http.StatusNotFound
}

type Version struct {
	APIVersion string `json:"ApiVersion"`
	Os         string `json:"Os"`
}

func (c *Client) Version(ctx context.Context) (Version, error) {
	var v Version
	err := c.get(ctx, "/version", nil, &v)
	return v, err
}

type Endpoint struct {
	NetworkID         string   `json:"NetworkID"`
	IPAddress         string   `json:"IPAddress"`
	GlobalIPv6Address string   `json:"GlobalIPv6Address"`
	Aliases           []string `json:"Aliases"`
	DNSNames          []string `json:"DNSNames"`
}

type Container struct {
	ID     string            `json:"Id"`
	Name   string            `json:"Name"`
	Names  []string          `json:"Names"`
	Labels map[string]string `json:"Labels"`
	Config struct {
		Labels   map[string]string `json:"Labels"`
		Hostname string            `json:"Hostname"`
	} `json:"Config"`
	State           State `json:"State"`
	NetworkSettings struct {
		Networks map[string]Endpoint `json:"Networks"`
	} `json:"NetworkSettings"`
}

// State is an object in inspect and a string ("running") in the container list.
type State struct {
	Running bool
}

func (s *State) UnmarshalJSON(b []byte) error {
	var str string
	if json.Unmarshal(b, &str) == nil {
		s.Running = str == "running"
		return nil
	}
	var o struct {
		Running bool `json:"Running"`
	}
	if err := json.Unmarshal(b, &o); err != nil {
		return err
	}
	s.Running = o.Running
	return nil
}

// DisplayName is the container name without leading slash.
func (c Container) DisplayName() string {
	if c.Name != "" {
		return strings.TrimPrefix(c.Name, "/")
	}
	if len(c.Names) > 0 {
		return strings.TrimPrefix(c.Names[0], "/")
	}
	return c.ID
}

func (c Container) Label(k string) string {
	if v, ok := c.Labels[k]; ok {
		return v
	}
	return c.Config.Labels[k]
}

func (c *Client) Inspect(ctx context.Context, id string) (Container, error) {
	var ct Container
	err := c.get(ctx, "/containers/"+url.PathEscape(id)+"/json", nil, &ct)
	return ct, err
}

// Running lists running containers with their network endpoints.
func (c *Client) Running(ctx context.Context) ([]Container, error) {
	var cs []Container
	err := c.get(ctx, "/containers/json", nil, &cs)
	return cs, err
}

type IPAMConfig struct {
	Subnet     string            `json:"Subnet"`
	IPRange    string            `json:"IPRange"`
	Gateway    string            `json:"Gateway"`
	AuxAddress map[string]string `json:"AuxiliaryAddresses"`
}

type Network struct {
	ID         string `json:"Id"`
	Name       string `json:"Name"`
	EnableIPv6 bool   `json:"EnableIPv6"`
	IPAM       struct {
		Config []IPAMConfig `json:"Config"`
	} `json:"IPAM"`
	Containers map[string]struct {
		Name        string `json:"Name"`
		IPv4Address string `json:"IPv4Address"`
	} `json:"Containers"`
}

func (c *Client) Network(ctx context.Context, id string) (Network, error) {
	var n Network
	err := c.get(ctx, "/networks/"+url.PathEscape(id), nil, &n)
	return n, err
}

type Event struct {
	Type   string `json:"Type"`
	Action string `json:"Action"`
	Actor  struct {
		ID         string            `json:"ID"`
		Attributes map[string]string `json:"Attributes"`
	} `json:"Actor"`
}

// Events streams container and network events until ctx ends or the stream breaks.
func (c *Client) Events(ctx context.Context, fn func(Event)) error {
	f, _ := json.Marshal(map[string][]string{"type": {"container", "network"}})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://docker/"+APIVersion+"/events?filters="+url.QueryEscape(string(f)), nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &StatusError{Code: resp.StatusCode}
	}
	dec := json.NewDecoder(bufio.NewReader(resp.Body))
	for {
		var e Event
		if err := dec.Decode(&e); err != nil {
			return err
		}
		fn(e)
	}
}

// SelfID returns the agent's own container ID or name, found via the hostname.
func SelfID() string {
	if id := selfFromMountinfo(); id != "" {
		return id
	}
	h, _ := os.Hostname()
	return h
}
