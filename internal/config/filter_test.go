package config

import "testing"

func TestNetworksAllows(t *testing.T) {
	all := Networks{}
	if !all.Allows("anything") {
		t.Fatal("no lists must allow every network")
	}
	n := Networks{Include: []string{"apps", "nextcloud_*"}, Exclude: []string{"nextcloud_backend", "*_internal"}}
	for name, want := range map[string]bool{
		"apps":              true,
		"nextcloud_default": true,
		"nextcloud_backend": false, // exclude wins
		"apps_internal":     false,
		"other":             false, // not included
	} {
		if got := n.Allows(name); got != want {
			t.Errorf("%s: %v, want %v", name, got, want)
		}
	}
	if !(Networks{Exclude: []string{"secret"}}).Allows("apps") {
		t.Fatal("exclude only must allow the rest")
	}
}

func TestNetworksConfig(t *testing.T) {
	c, err := Parse([]byte(head + "local_networks:\n  include: [metrics]\npartners:\n  - name: b\n    public_key: " +
		peer + "\n    networks:\n      include: [\"web_*\"]\n      exclude: [web_db]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Partners[0].Networks.Allows("web_front") || c.Partners[0].Networks.Allows("web_db") ||
		!c.LocalNetworks.Allows("metrics") || c.LocalNetworks.Allows("web_front") {
		t.Fatal("lists not applied")
	}
	for _, bad := range []string{
		head + "local_networks:\n  include: [\"[\"]\n",
		head + "partners:\n  - name: b\n    public_key: " + peer + "\n    networks:\n      exclude: [\"\"]\n",
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
