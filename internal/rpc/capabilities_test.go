package rpc

import "testing"

func TestCapabilitiesHelpers(t *testing.T) {
	c := Capabilities{PHPVersions: []string{"8.4", "8.3"}, Services: []ServiceCapability{{Name: "nginx", Restartable: true}}}
	if !c.HasPHP("8.3") || c.HasPHP("8.5") {
		t.Fatal("HasPHP reads the list")
	}
	if !(Capabilities{}).HasPHP("8.5") {
		t.Fatal("no list known refuses nothing, as the agent")
	}
	if s, ok := c.Service("nginx"); !ok || !s.Restartable {
		t.Fatal("Service finds nginx")
	}
	if _, ok := c.Service("sshd"); ok {
		t.Fatal("Service invents nothing")
	}
	r := TableRule{Match: "suffix", Pattern: "_users"}
	if !r.Protects("WP_USERS") || r.Protects("wp_users_x") {
		t.Fatal("suffix rule")
	}
	if !(TableRule{Match: "contains", Pattern: "_wc_order"}).Protects("wp_wc_orders_meta") {
		t.Fatal("contains rule")
	}
	if (TableRule{Match: "regex", Pattern: "."}).Protects("x") {
		t.Fatal("an unknown match protects nothing")
	}
}
