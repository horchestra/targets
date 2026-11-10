package host

import (
	"context"
	"fmt"
	"strings"
)

// The firewall of a packages key's hosts: the placement's `firewall` is a list of rules to let through —
// "<port>", "<port>/<proto>" or "<cidr>:<port>/<proto>" (proto is tcp when none) — kept on whatever firewall the
// host runs: firewalld when it is active, else an active ufw, else bare iptables. The rules go when the key is
// removed. firewalld's and ufw's are persistent by themselves; bare iptables gets the OS's own persistence —
// iptables-persistent (Debian) or iptables-services (RHEL), installed when neither is there.

// fwRule is one parsed rule of the placement's firewall.
type fwRule struct {
	proto string
	port  string
	cidr  string // the source the rule is for; "" — anywhere
}

// parseFirewall reads the placement's rules.
func parseFirewall(list []string) ([]fwRule, error) {
	var out []fwRule
	for _, s := range list {
		r := fwRule{proto: "tcp"}
		rest := s
		if cidr, r2, ok := strings.Cut(s, ":"); ok {
			if strings.Contains(r2, ":") || len(cidr) == 0 {
				return nil, fmt.Errorf("%s: a rule is <port>[/<proto>] or <cidr>:<port>[/<proto>]", s)
			}
			r.cidr, rest = cidr, r2
		}
		port, proto, _ := strings.Cut(rest, "/")
		if len(port) == 0 {
			return nil, fmt.Errorf("%s: a rule is <port>[/<proto>] or <cidr>:<port>[/<proto>]", s)
		}
		r.port = port
		if len(proto) > 0 {
			if proto != "tcp" && proto != "udp" {
				return nil, fmt.Errorf("%s: the protocol is tcp or udp", s)
			}
			r.proto = proto
		}
		out = append(out, r)
	}
	return out, nil
}

// firewallBackend is the firewall of the host: firewalld when it runs, an active ufw else, iptables last.
func (c *conn) firewallBackend(ctx context.Context) string {
	if _, err := c.exec(ctx, []string{"systemctl", "is-active", "firewalld"}, nil, nil, nil); err == nil {
		return "firewalld"
	}
	if out, err := c.exec(ctx, []string{"ufw", "status"}, nil, nil, nil); err == nil && strings.Contains(string(out), "Status: active") {
		return "ufw"
	}
	return "iptables"
}

// firewallCmds are the commands of a rule on the backend; add is false for their removal.
func firewallCmds(backend string, r fwRule, add bool) [][]string {
	switch backend {
	case "firewalld":
		arg := "--add-port="
		if !add {
			arg = "--remove-port="
		}
		if len(r.cidr) > 0 {
			rich := fmt.Sprintf("rule family=ipv4 source address=%s port port=%s protocol=%s accept", r.cidr, r.port, r.proto)
			if add {
				return [][]string{{"firewall-cmd", "--permanent", "--add-rich-rule=" + rich}}
			}
			return [][]string{{"firewall-cmd", "--permanent", "--remove-rich-rule=" + rich}}
		}
		return [][]string{{"firewall-cmd", "--permanent", arg + r.port + "/" + r.proto}}
	case "ufw":
		cmd := []string{"ufw"}
		if !add {
			cmd = append(cmd, "delete")
		}
		if len(r.cidr) > 0 {
			return [][]string{append(cmd, "allow", "from", r.cidr, "to", "any", "port", r.port, "proto", r.proto)}
		}
		return [][]string{append(cmd, "allow", r.port+"/"+r.proto)}
	default:
		cmd := []string{"iptables"}
		if add {
			cmd = append(cmd, "-A")
		} else {
			cmd = append(cmd, "-D")
		}
		cmd = append(cmd, "INPUT", "-p", r.proto, "--dport", r.port)
		if len(r.cidr) > 0 {
			cmd = append(cmd, "-s", r.cidr)
		}
		return [][]string{append(cmd, "-j", "ACCEPT")}
	}
}

// lookup is the path of a program on the host; "" — there is none.
func (c *conn) lookup(ctx context.Context, name string) string {
	out, err := c.exec(ctx, []string{"sh", "-c", "command -v " + name}, nil, nil, nil)
	if err != nil {
		return ""
	}
	p, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return p
}

// persist keeps the iptables of the host across boots, the OS's own way: netfilter-persistent when it is there,
// else its package — iptables-persistent on debian, iptables-services on rhel (its iptables.service reads
// /etc/sysconfig/iptables on boot). Only ever called when a rule was applied, so the install is the firewall's
// own need.
func (c *conn) persist(ctx context.Context, f osFacts, events func(string)) error {
	if len(c.lookup(ctx, "netfilter-persistent")) > 0 {
		_, err := c.exec(ctx, []string{"netfilter-persistent", "save"}, nil, nil, events)
		return err
	}
	save := c.lookup(ctx, "iptables-save")
	if len(save) == 0 {
		return fmt.Errorf("no netfilter-persistent and no iptables-save: the rules would not outlive a boot")
	}
	switch f.family {
	case "debian":
		if _, err := c.exec(ctx, []string{"apt-get", "install", "-y", "iptables-persistent"}, aptEnv, nil, events); err != nil {
			return err
		}
		_, err := c.exec(ctx, []string{"netfilter-persistent", "save"}, nil, nil, events)
		return err
	case "rhel":
		if _, err := c.exec(ctx, []string{"dnf", "install", "-y", "iptables-services"}, nil, nil, events); err != nil {
			return err
		}
		out, err := c.exec(ctx, []string{save}, nil, nil, nil)
		if err != nil {
			return err
		}
		if _, err := c.writeFile(ctx, "/etc/sysconfig/iptables", out, 0o600); err != nil {
			return err
		}
		if _, err := c.exec(ctx, []string{"systemctl", "enable", "iptables.service"}, nil, nil, events); err != nil {
			return err
		}
		return nil
	}
	return fmt.Errorf("%sthe OS family %q is not known (debian, rhel)", c.h.prefix(), f.family)
}

// persistAgain is persist on the removal path: the table is saved again so a removed rule stays removed. Nothing
// is installed here — a removal on a host that never persisted is simply not saved.
func (c *conn) persistAgain(ctx context.Context, events func(string)) {
	if len(c.lookup(ctx, "netfilter-persistent")) > 0 {
		if _, err := c.exec(ctx, []string{"netfilter-persistent", "save"}, nil, nil, events); err != nil {
			events(c.h.prefix() + "netfilter-persistent save: " + err.Error())
		}
		return
	}
	save := c.lookup(ctx, "iptables-save")
	if len(save) == 0 {
		return
	}
	if _, err := c.exec(ctx, []string{"test", "-f", "/etc/sysconfig/iptables"}, nil, nil, nil); err != nil {
		return // no iptables-services: nothing was persisted
	}
	if out, err := c.exec(ctx, []string{save}, nil, nil, nil); err == nil {
		_, _ = c.writeFile(ctx, "/etc/sysconfig/iptables", out, 0o600)
	}
}

// firewallOf is the key's parsed rules; nil when the placement names none.
func (in *Installation) firewallOf(key string) ([]fwRule, error) {
	return parseFirewall(stringsOf(in.platformFor(key)["firewall"]))
}

// applyFirewall lets the key's rules through on its host. iptables: -C says whether the rule is there already,
// and the table is persisted once the rules are in.
func (c *conn) applyFirewall(ctx context.Context, f osFacts, rules []fwRule, events func(string)) error {
	if len(rules) == 0 {
		return nil
	}
	backend := c.firewallBackend(ctx)
	for _, r := range rules {
		for _, cmd := range firewallCmds(backend, r, true) {
			if backend == "iptables" {
				check := append([]string{"iptables", "-C"}, cmd[2:]...)
				if _, err := c.exec(ctx, check, nil, nil, nil); err == nil {
					continue
				}
			}
			if _, err := c.exec(ctx, cmd, nil, nil, events); err != nil {
				return fmt.Errorf("firewall %s: %w", r.port, err)
			}
		}
	}
	switch backend {
	case "firewalld":
		if _, err := c.exec(ctx, []string{"firewall-cmd", "--reload"}, nil, nil, events); err != nil {
			return err
		}
	case "iptables":
		return c.persist(ctx, f, events)
	}
	return nil
}

// removeFirewall takes the key's rules away; one that is not there is no error (the host was cleaned by hand).
func (c *conn) removeFirewall(ctx context.Context, f osFacts, rules []fwRule, events func(string)) error {
	if len(rules) == 0 {
		return nil
	}
	backend := c.firewallBackend(ctx)
	for _, r := range rules {
		for _, cmd := range firewallCmds(backend, r, false) {
			if _, err := c.exec(ctx, cmd, nil, nil, events); err != nil {
				events(c.h.prefix() + "firewall: " + err.Error())
			}
		}
	}
	switch backend {
	case "firewalld":
		_, _ = c.exec(ctx, []string{"firewall-cmd", "--reload"}, nil, nil, events)
	case "iptables":
		c.persistAgain(ctx, events)
	}
	return nil
}
