package metrics

import (
	"strings"
	"testing"
)

func TestWrite(t *testing.T) {
	r := New()
	r.SetPeerUp("hostb", true)
	tg := Target{Container: "nc-app-1", Service: "app", Stack: "nc"}
	r.StreamResult("hostb", "ok", tg)
	r.StreamStarted("hostb", tg)
	r.AddBytes("hostb", "in", tg, 10)
	r.StreamResult("hostb", "refused", Target{})
	r.SetAddressesFree(7)
	var b strings.Builder
	if err := r.Write(&b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"# TYPE switchyard_peer_up gauge",
		`switchyard_peer_up{peer="hostb"} 1`,
		`switchyard_streams_total{peer="hostb",result="ok",container="nc-app-1",service="app",stack="nc"} 1`,
		`switchyard_streams_total{peer="hostb",result="refused"} 1`,
		`switchyard_streams_active{peer="hostb",container="nc-app-1",service="app",stack="nc"} 1`,
		`switchyard_bytes_total{peer="hostb",direction="in",container="nc-app-1",service="app",stack="nc"} 10`,
		"switchyard_addresses_free 7",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	r.StreamEnded("hostb", tg)
	b.Reset()
	_ = r.Write(&b)
	if strings.Contains(b.String(), "switchyard_streams_active{") {
		t.Fatal("ended stream still active")
	}
}
