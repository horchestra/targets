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

func TestBlanketReject(t *testing.T) {
	awg := `# iptables-save of a host with a terminal REJECT
*filter
:INPUT ACCEPT [0:0]
:FORWARD ACCEPT [510252936:526098872997]
:OUTPUT ACCEPT [600763800:578558339489]
-A INPUT -i lo -j ACCEPT
-A INPUT -d 127.0.0.0/8 ! -i lo -j REJECT --reject-with icmp-port-unreachable
-A INPUT -m state --state RELATED,ESTABLISHED -j ACCEPT
-A INPUT -i awg0 -j ACCEPT
-A INPUT -p icmp -j ACCEPT
-A INPUT -j REJECT --reject-with icmp-port-unreachable
COMMIT
`
	if pos := blanketReject([]byte(awg)); pos != 6 {
		t.Errorf("pos = %d, want 6 (the scoped rejects are not terminal)", pos)
	}
	if pos := blanketReject([]byte("*filter\n:INPUT ACCEPT [0:0]\n-A INPUT -p tcp -j ACCEPT\nCOMMIT\n")); pos != 0 {
		t.Errorf("pos = %d, want 0 (no terminal rule)", pos)
	}
	if pos := blanketReject([]byte("*filter\n:INPUT DROP [0:0]\n-A INPUT -j DROP\nCOMMIT\n")); pos != 1 {
		t.Errorf("pos = %d, want 1", pos)
	}
}
