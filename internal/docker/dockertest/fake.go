// Package dockertest is an in-process Docker API for tests.
package dockertest

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"

	"github.com/motwok/npm-switchyard/internal/docker"
)

// Fake is an in-process Docker API for tests: containers, networks and an event stream.
type Fake struct {
	Server *httptest.Server

	mu         sync.Mutex
	Containers map[string]docker.Container // by ID
	Networks   map[string]docker.Network   // by ID
	VersionAPI string
	EventsFail bool // answer the event stream with 500
	events     chan docker.Event
}

func NewFake() *Fake {
	f := &Fake{Containers: map[string]docker.Container{}, Networks: map[string]docker.Network{}, VersionAPI: "1.47",
		events: make(chan docker.Event, 16)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /"+docker.APIVersion+"/version", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(docker.Version{APIVersion: f.VersionAPI, Os: "linux"})
	})
	mux.HandleFunc("GET /"+docker.APIVersion+"/containers/json", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var list []map[string]any
		for _, c := range f.Containers {
			if !c.State.Running {
				continue
			}
			nets := map[string]any{}
			for n, ep := range c.NetworkSettings.Networks {
				// The real list omits Aliases and DNSNames.
				nets[n] = map[string]any{"NetworkID": ep.NetworkID, "IPAddress": ep.IPAddress}
			}
			list = append(list, map[string]any{"Id": c.ID, "Names": []string{"/" + strings.TrimPrefix(c.Name, "/")},
				"State": "running", "Labels": c.Config.Labels, "NetworkSettings": map[string]any{"Networks": nets}})
		}
		_ = json.NewEncoder(w).Encode(list)
	})
	mux.HandleFunc("GET /"+docker.APIVersion+"/containers/{id}/json", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		id := r.PathValue("id")
		for _, c := range f.Containers {
			if c.ID == id || strings.HasPrefix(c.ID, id) || strings.TrimPrefix(c.Name, "/") == id {
				_ = json.NewEncoder(w).Encode(c)
				return
			}
		}
		http.Error(w, `{"message":"no such container"}`, http.StatusNotFound)
	})
	mux.HandleFunc("GET /"+docker.APIVersion+"/networks/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if n, ok := f.Networks[r.PathValue("id")]; ok {
			_ = json.NewEncoder(w).Encode(n)
			return
		}
		http.Error(w, `{"message":"no such network"}`, http.StatusNotFound)
	})
	mux.HandleFunc("GET /"+docker.APIVersion+"/events", func(w http.ResponseWriter, r *http.Request) {
		if f.EventsFail {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		enc := json.NewEncoder(w)
		for {
			select {
			case <-r.Context().Done():
				return
			case e, ok := <-f.events:
				if !ok {
					return
				}
				_ = enc.Encode(e)
				w.(http.Flusher).Flush()
			}
		}
	})
	f.Server = httptest.NewServer(mux)
	return f
}

// Client returns a Client talking to the fake.
func (f *Fake) Client() *docker.Client {
	addr := f.Server.Listener.Addr().String()
	return docker.NewWithDialer(func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	})
}

// Set adds or replaces a container.
func (f *Fake) Set(c docker.Container) {
	f.mu.Lock()
	f.Containers[c.ID] = c
	f.mu.Unlock()
}

// Remove deletes a container.
func (f *Fake) Remove(id string) {
	f.mu.Lock()
	delete(f.Containers, id)
	f.mu.Unlock()
}

// Emit sends an event to open event streams.
func (f *Fake) Emit(e docker.Event) { f.events <- e }

// CloseEvents ends open event streams.
func (f *Fake) CloseEvents() { close(f.events) }

func (f *Fake) Close() { f.Server.Close() }

// Ctr builds a running container in the given networks (name -> endpoint).
func Ctr(id, name string, labels map[string]string, nets map[string]docker.Endpoint) docker.Container {
	c := docker.Container{ID: id, Name: "/" + name}
	c.Config.Labels = labels
	c.State.Running = true
	c.NetworkSettings.Networks = nets
	return c
}
