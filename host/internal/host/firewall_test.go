package host

import (
	"slices"
	"testing"
)

func TestParseFirewall(t *testing.T) {
	rules, err := parseFirewall([]string{"8200", "1194/udp", "10.1.0.0/16:8201/tcp"})
	if err != nil {
		t.Fatal(err)
	}
	want := []fwRule{
		{proto: "tcp", port: "8200"},
		{proto: "udp", port: "1194"},
		{proto: "tcp", port: "8201", cidr: "10.1.0.0/16"},
	}
	if !slices.Equal(rules, want) {
		t.Errorf("rules = %+v", rules)
	}
	for _, bad := range []string{"", "/tcp", "8200/sctp", "10.0.0.0/8:8200:extra", ":8200"} {
		if _, err := parseFirewall([]string{bad}); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestFirewallCmds(t *testing.T) {
	r := fwRule{proto: "tcp", port: "8200"}
	rs := fwRule{proto: "udp", port: "1194", cidr: "10.1.0.0/16"}
	for _, tc := range []struct {
		backend string
		r       fwRule
		add     bool
		want    []string
	}{
		{"firewalld", r, true, []string{"firewall-cmd", "--permanent", "--add-port=8200/tcp"}},
		{"firewalld", r, false, []string{"firewall-cmd", "--permanent", "--remove-port=8200/tcp"}},
		{"firewalld", rs, true, []string{"firewall-cmd", "--permanent", "--add-rich-rule=rule family=ipv4 source address=10.1.0.0/16 port port=1194 protocol=udp accept"}},
		{"ufw", r, true, []string{"ufw", "allow", "8200/tcp"}},
		{"ufw", r, false, []string{"ufw", "delete", "allow", "8200/tcp"}},
		{"ufw", rs, true, []string{"ufw", "allow", "from", "10.1.0.0/16", "to", "any", "port", "1194", "proto", "udp"}},
		{"iptables", r, true, []string{"iptables", "-A", "INPUT", "-p", "tcp", "--dport", "8200", "-j", "ACCEPT"}},
		{"iptables", r, false, []string{"iptables", "-D", "INPUT", "-p", "tcp", "--dport", "8200", "-j", "ACCEPT"}},
		{"iptables", rs, true, []string{"iptables", "-A", "INPUT", "-p", "udp", "--dport", "1194", "-s", "10.1.0.0/16", "-j", "ACCEPT"}},
	} {
		got := firewallCmds(tc.backend, tc.r, tc.add)
		if len(got) != 1 || !slices.Equal(got[0], tc.want) {
			t.Errorf("%s %+v add=%v: %v", tc.backend, tc.r, tc.add, got)
		}
	}
}
