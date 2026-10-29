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

// F-6-2: the kernel's own port range, as /proc gives it, and the default
// when the agent sent none that makes sense.
func TestEphemeralRange(t *testing.T) {
	if got, ok := ParsePortRange("32768\t60999\n"); !ok || got[0] != 32768 || got[1] != 60999 {
		t.Fatalf("parse: %v %v", got, ok)
	}
	for _, bad := range []string{"", "1", "60999 32768", "0 100", "1 70000", "a b"} {
		if _, ok := ParsePortRange(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
	if lo, hi := (Capabilities{EphemeralPorts: []int{40000, 50000}}).EphemeralRange(); lo != 40000 || hi != 50000 {
		t.Errorf("sent range: %d-%d", lo, hi)
	}
	if lo, hi := (Capabilities{}).EphemeralRange(); lo != 32768 || hi != 60999 {
		t.Errorf("default: %d-%d", lo, hi)
	}
}
