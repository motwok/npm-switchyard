package docker_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/motwok/npm-switchyard/internal/docker"
	"github.com/motwok/npm-switchyard/internal/docker/dockertest"
)

func TestClient(t *testing.T) {
	f := dockertest.NewFake()
	defer f.Close()
	f.Set(dockertest.Ctr("aaaaaaaaaaaa1111", "web", map[string]string{"com.docker.compose.service": "web"},
		map[string]docker.Endpoint{"apps": {NetworkID: "n1", IPAddress: "10.0.0.5", DNSNames: []string{"web"}}}))
	f.Networks["n1"] = docker.Network{ID: "n1", Name: "apps"}
	c := f.Client()
	ctx := context.Background()

	v, err := c.Version(ctx)
	if err != nil || v.APIVersion != "1.47" {
		t.Fatal(v, err)
	}
	list, err := c.Running(ctx)
	if err != nil || len(list) != 1 || !list[0].State.Running || list[0].DisplayName() != "web" {
		t.Fatalf("running: %+v %v", list, err)
	}
	if len(list[0].NetworkSettings.Networks["apps"].DNSNames) != 0 {
		t.Fatal("list should not carry DNSNames")
	}
	ins, err := c.Inspect(ctx, "web")
	if err != nil || ins.NetworkSettings.Networks["apps"].DNSNames[0] != "web" || ins.Label("com.docker.compose.service") != "web" {
		t.Fatalf("inspect: %+v %v", ins, err)
	}
	if _, err := c.Inspect(ctx, "missing"); !docker.IsNotFound(err) || err.Error() == "" {
		t.Fatalf("want not found, got %v", err)
	}
	if docker.IsNotFound(errors.New("x")) {
		t.Fatal("plain error is not 'not found'")
	}
	n, err := c.Network(ctx, "n1")
	if err != nil || n.Name != "apps" {
		t.Fatal(n, err)
	}
}

func TestEvents(t *testing.T) {
	f := dockertest.NewFake()
	defer f.Close()
	c := f.Client()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got := make(chan docker.Event, 1)
	go func() { _ = c.Events(ctx, func(e docker.Event) { got <- e }) }()
	time.Sleep(100 * time.Millisecond)
	f.Emit(docker.Event{Type: "container", Action: "die"})
	select {
	case e := <-got:
		if e.Action != "die" {
			t.Fatal(e)
		}
	case <-ctx.Done():
		t.Fatal("no event")
	}
}

func TestDisplayName(t *testing.T) {
	if (docker.Container{ID: "x"}).DisplayName() != "x" || (docker.Container{Names: []string{"/n"}}).DisplayName() != "n" {
		t.Fatal("display name")
	}
	if docker.SelfID() == "" {
		t.Fatal("self id")
	}
}

func TestListLabels(t *testing.T) {
	f := dockertest.NewFake()
	defer f.Close()
	f.Set(dockertest.Ctr("c1", "x", map[string]string{"k": "v"}, nil))
	list, err := f.Client().Running(context.Background())
	if err != nil || list[0].Label("k") != "v" || list[0].Label("none") != "" {
		t.Fatal(list, err)
	}
}
