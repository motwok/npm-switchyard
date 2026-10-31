package docker

import (
	"encoding/json"
	"testing"
)

func TestStateBothShapes(t *testing.T) {
	var list []Container
	if err := json.Unmarshal([]byte(`[{"Id":"a","State":"running"}]`), &list); err != nil || !list[0].State.Running {
		t.Fatal("list form", err)
	}
	var c Container
	if err := json.Unmarshal([]byte(`{"Id":"a","State":{"Running":true}}`), &c); err != nil || !c.State.Running {
		t.Fatal("inspect form", err)
	}
}
