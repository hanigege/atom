package main

import (
	"fmt"
	"net"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var ifaceNameRe = regexp.MustCompile(`^[A-Za-z0-9_.:@-]{1,15}$`)

func nftList(names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = `"` + n + `"`
	}
	return strings.Join(q, ", ")
}

// renderNft is deploy-nft.md 3.1: every IPv4 TCP and UDP connection from the LAN that is not for this
// machine goes to sing-box. The main router only sends this machine what is to be proxied (the FakeIP
// range and the networks to proxy), so sing-box decides all of it: a FakeIP by its binding or Final, a
// real address by the IP, port and network rules, else Final. reply (single-NIC only, 3.4) marks these
// connections and sends their answers through the main router, so that it sees both directions.
func renderNft(lan []string, reply bool) (string, error) {
	if err := checkIfaceNames(lan); err != nil {
		return "", err
	}
	l := nftList(lan)
	ct, out := "", ""
	if reply {
		ct = "ct mark set " + replyMark + " "
		out = fmt.Sprintf(`

	# Answers to the connections above go back through the main router (policy routing, table %[2]s).
	chain output_reply {
		type route hook output priority mangle; policy accept;
		ct direction reply ct mark %[1]s meta mark set %[1]s
	}`, replyMark, replyTable)
	}
	return fmt.Sprintf(`#!/usr/sbin/nft -f
# atom: send LAN traffic and local FakeIP traffic to sing-box. Written by atom-installer.
table inet atom
delete table inet atom
table inet atom {
	set output_fakeip4 {
		type ipv4_addr
		flags interval
	}

	# TCP: redirect to sing-box redirect-in (7892).
	chain prerouting_tcp {
		type nat hook prerouting priority dstnat; policy accept;
		iifname != { %[1]s } return
		fib daddr type { local, broadcast, multicast } return
		meta nfproto ipv4 meta l4proto tcp %[3]sredirect to :7892
	}

	# UDP: TProxy to sing-box tproxy-in (7893); the mark sends it to table 233 (local). The
	# replies of a flow opened from elsewhere (the WAN) are forwarded as they are.
	chain prerouting_udp {
		type filter hook prerouting priority mangle; policy accept;
		iifname "lo" meta mark %[2]s ip daddr @output_fakeip4 meta l4proto udp ct direction original counter tproxy ip to :7893 accept
		iifname != { %[1]s } return
		fib daddr type { local, broadcast, multicast } return
		ct direction reply return
		meta nfproto ipv4 meta l4proto udp %[3]stproxy ip to :7893 meta mark set %[2]s accept
	}

	chain output_tcp {
		type nat hook output priority -100; policy accept;
		ip daddr @output_fakeip4 meta l4proto tcp ct direction original counter redirect to :7892
	}

	chain output_udp {
		type route hook output priority mangle - 1; policy accept;
		ip daddr @output_fakeip4 meta l4proto udp ct direction original counter meta mark set %[2]s
	}%[4]s
}
`, l, fwmark, ct, out), nil
}

// renderGatewayNft is deploy-nft.md 3.2: what is neither TCP nor UDP (ping) is forwarded and NATed out of the WAN.
func renderGatewayNft(lan []string, wan string) (string, error) {
	if err := checkIfaceNames(append(append([]string{}, lan...), wan)); err != nil {
		return "", err
	}
	return fmt.Sprintf(`#!/usr/sbin/nft -f
# atom, dual-NIC gateway only: what is neither TCP nor UDP from the LAN is forwarded and NATed out of the WAN. Written by atom-installer.
table ip atom_nat
delete table ip atom_nat
table ip atom_nat {
	chain postrouting {
		type nat hook postrouting priority srcnat; policy accept;
		iifname { %[1]s } oifname "%[2]s" masquerade
	}
}
`, nftList(lan), wan), nil
}

// Keep the dynamic set through DNS restarts and sing-box reloads. Only a
// full sing-box start, with no listener left from the old instance, resets it.
func renderWANNft(wan string, panelPort int) (string, error) {
	if err := checkIfaceNames([]string{wan}); err != nil {
		return "", err
	}
	if panelPort < 1 || panelPort > 65535 {
		return "", fmt.Errorf("invalid panel port %d", panelPort)
	}
	return fmt.Sprintf(`add table inet atom_wan
add set inet atom_wan tcp_proxy_ports { type inet_service; }
add set inet atom_wan udp_proxy_ports { type inet_service; }
add chain inet atom_wan input { type filter hook input priority filter; policy accept; }
flush chain inet atom_wan input
add rule inet atom_wan input iifname %[1]q tcp dport { 53, %[2]d } drop
add rule inet atom_wan input iifname %[1]q udp dport 53 drop
add rule inet atom_wan input iifname %[1]q tcp dport @tcp_proxy_ports drop
add rule inet atom_wan input iifname %[1]q udp dport @udp_proxy_ports drop
`, wan, panelPort), nil
}

func checkIfaceNames(names []string) error {
	if len(names) == 0 {
		return fmt.Errorf("no interface given")
	}
	for _, n := range names {
		if !ifaceNameRe.MatchString(n) {
			return fmt.Errorf("invalid interface name %q", n)
		}
	}
	return nil
}

// renderPolicyRoute is deploy-nft.md 3.3; ip rule is added only when it is not there yet. The client routes
// of a dual-NIC gateway follow: answers to clients behind the LAN-side router go back through the LAN. reply
// (single-NIC, 3.4) is the route of the marked answers through the main router.
func renderPolicyRoute(routes []ClientRoute, reply *ClientRoute) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, `#!/bin/sh
# atom: policy routing for the TProxy mark. Written by atom-installer.
set -e
ip rule show | grep -q 'fwmark %[1]s lookup %[2]s' || ip rule add fwmark %[1]s lookup %[2]s priority %[3]s
ip route replace local 0.0.0.0/0 dev lo table %[2]s
`, fwmark, routeTable, routePriority)
	for _, r := range routes {
		if err := r.check(); err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "ip route replace %s via %s dev %s\n", r.Net, r.Via, r.Dev)
	}
	if reply != nil {
		if err := reply.check(); err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "ip rule show | grep -q 'from all fwmark %[1]s lookup %[2]s' || ip rule add fwmark %[1]s lookup %[2]s priority %[3]s\n", replyMark, replyTable, replyPriority)
		fmt.Fprintf(&b, "ip route replace default via %s dev %s table %s\n", reply.Via, reply.Dev, replyTable)
	}
	return b.String(), nil
}

// check makes sure a recorded route is safe to put into a shell script.
func (r ClientRoute) check() error {
	ip, n, err := net.ParseCIDR(r.Net)
	if err != nil || ip.To4() == nil || n.String() != r.Net {
		return fmt.Errorf("invalid client network %q", r.Net)
	}
	if v := net.ParseIP(r.Via); v == nil || v.To4() == nil {
		return fmt.Errorf("invalid LAN gateway %q", r.Via)
	}
	return checkIfaceNames([]string{r.Dev})
}

// renderDNSUnit is deploy-nft.md 4: load the rules and the routing before atomdns starts. replyDev (3.4) is
// the LAN interface that must not take ICMP redirects from the main router.
func renderDNSUnit(p Paths, nftBin, sysctlBin string, gateway bool, replyDev string) string {
	var pre strings.Builder
	fmt.Fprintf(&pre, "ExecStartPre=%s sync-nft --root %q --load-atom\n", p.Bin("atom-installer"), p.Root)
	if gateway {
		fmt.Fprintf(&pre, "ExecStartPre=%s -f %s\n", nftBin, p.Etc("atom-gateway.nft"))
		fmt.Fprintf(&pre, "ExecStartPre=%s -w net.ipv4.ip_forward=1\n", sysctlBin)
	}
	if replyDev != "" {
		fmt.Fprintf(&pre, "ExecStartPre=%s -w net/ipv4/conf/all/accept_redirects=0 net/ipv4/conf/%s/accept_redirects=0\n", sysctlBin, replyDev)
	}
	fmt.Fprintf(&pre, "ExecStartPre=%s\n", p.Lib("policy-route.sh"))
	return fmt.Sprintf(`[Unit]
Description=atom DNS (AtomDNS fork: rules, FakeIP and the control plane)
After=network-online.target atom-sing-box.service atom-mihomo.service
Wants=network-online.target

[Service]
Environment="ATOM_MAINTENANCE_SOCKET=%[4]s"
%[1]sExecStart=%[2]s start -d %[3]s -c config_custom.yaml
Restart=on-failure
RestartSec=2
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
`, pre.String(), p.Bin("atomdns"), p.Data(), p.Run(maintenanceSocket))
}

// renderSingBoxUnit is design 6: started and reloaded by atomdns (systemctl reload-or-restart).
func renderSingBoxUnit(p Paths) string {
	return renderProxyUnit(p, "sing-box", "config.json", "run -D", "-c")
}

func renderMihomoUnit(p Paths) string {
	return renderProxyUnit(p, "mihomo", "config.yaml", "-d", "-f")
}

func renderProxyUnit(p Paths, backend, config, workArg, configArg string) string {
	dir := filepath.Join(p.Data(), backend)
	return fmt.Sprintf(`[Unit]
Description=atom %[5]s (FakeIP outbound fork), configured by atom-dns
After=network-online.target
ConditionPathExists=%[2]s/%[6]s

[Service]
ExecStartPre=%[3]s sync-nft --root %[4]q --backend %[5]s --start
ExecStart=%[1]s %[7]s %[2]s %[8]s %[2]s/%[6]s
ExecReload=%[3]s sync-nft --root %[4]q --backend %[5]s
ExecReload=/bin/kill -HUP $MAINPID
Restart=on-failure
RestartSec=1
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
`, p.Bin(backend), dir, p.Bin("atom-installer"), p.Root, backend, config, workArg, configArg)
}

var panelPortRe = regexp.MustCompile(`(?m)^(api:[ \t]*\r?\n[ \t]+http:[ \t]*")[^"]*(")`)

// setPanelPort rewrites api.http of config_custom.yaml to 0.0.0.0:<port>.
func setPanelPort(yaml string, port int) (string, error) {
	if !panelPortRe.MatchString(yaml) {
		return "", fmt.Errorf("config_custom.yaml has no api.http line")
	}
	return panelPortRe.ReplaceAllString(yaml, "${1}0.0.0.0:"+strconv.Itoa(port)+"${2}"), nil
}

// renderDNSInit is renderDNSUnit for OpenRC: supervise-daemon restarts atomdns when it exits (Restart=on-failure),
// and what the unit does in ExecStartPre happens in start_pre.
func renderDNSInit(p Paths, nftBin, sysctlBin string, gateway bool, replyDev string) string {
	var pre strings.Builder
	fmt.Fprintf(&pre, "\t%s sync-nft --root %q --load-atom || return 1\n", p.Bin("atom-installer"), p.Root)
	if gateway {
		fmt.Fprintf(&pre, "\t%s -f %s || return 1\n", nftBin, p.Etc("atom-gateway.nft"))
		fmt.Fprintf(&pre, "\t%s -w net.ipv4.ip_forward=1 || return 1\n", sysctlBin)
	}
	if replyDev != "" {
		fmt.Fprintf(&pre, "\t%s -w net/ipv4/conf/all/accept_redirects=0 net/ipv4/conf/%s/accept_redirects=0 || return 1\n", sysctlBin, replyDev)
	}
	fmt.Fprintf(&pre, "\t%s || return 1\n", p.Lib("policy-route.sh"))
	return fmt.Sprintf(`#!/sbin/openrc-run
# atom DNS (AtomDNS fork: rules, FakeIP and the control plane). Written by atom-installer.
description="atom DNS"
supervisor=supervise-daemon
command=%[2]s
command_args="start -d %[3]s -c config_custom.yaml"
export ATOM_MAINTENANCE_SOCKET=%[5]q
respawn_delay=2
respawn_max=0
rc_ulimit="-n 1048576"
output_log=%[4]s/atom-dns.log
error_log=%[4]s/atom-dns.log

depend() {
	need net
	after atom-sing-box atom-mihomo
	want crond
}

start_pre() {
	checkpath -d -m 0755 %[4]s
%[1]s}
`, pre.String(), p.Bin("atomdns"), p.Data(), openRCLogDir, p.Run(maintenanceSocket))
}

// renderSingBoxInit is renderSingBoxUnit for OpenRC: reload sends SIGHUP; without config.json it does not start.
func renderSingBoxInit(p Paths) string {
	return renderProxyInit(p, "sing-box", "config.json", "run -D", "-c")
}

func renderMihomoInit(p Paths) string {
	return renderProxyInit(p, "mihomo", "config.yaml", "-d", "-f")
}

func renderProxyInit(p Paths, backend, config, workArg, configArg string) string {
	dir := filepath.Join(p.Data(), backend)
	return fmt.Sprintf(`#!/sbin/openrc-run
# atom %[6]s (FakeIP outbound fork), configured by atom-dns. Written by atom-installer.
description="atom %[6]s"
supervisor=supervise-daemon
command=%[1]s
command_args="%[8]s %[2]s %[9]s %[2]s/%[7]s"
respawn_delay=1
respawn_max=0
rc_ulimit="-n 1048576"
output_log=%[3]s/atom-%[6]s.log
error_log=%[3]s/atom-%[6]s.log
extra_started_commands="reload"

depend() {
	need net
}

start_pre() {
	[ -f %[2]s/%[7]s ] || { eerror "no %[2]s/%[7]s"; return 1; }
	%[4]s sync-nft --root %[5]q --backend %[6]s --start || return 1
	checkpath -d -m 0755 %[3]s
}

reload() {
	%[4]s sync-nft --root %[5]q --backend %[6]s || return 1
	ebegin "Reloading $RC_SVCNAME"
	supervise-daemon "$RC_SVCNAME" --signal HUP
	eend $?
}
`, p.Bin(backend), dir, openRCLogDir, p.Bin("atom-installer"), p.Root, backend, config, workArg, configArg)
}

// renderReloadScript is the reload command of atom_rules.yaml on OpenRC: reload sing-box when it runs, else
// start it (what systemctl reload-or-restart does).
func renderReloadScript(service ...string) string {
	name := "atom-sing-box"
	if len(service) > 0 {
		name = service[0]
	}
	return fmt.Sprintf(`#!/bin/sh
# atom: reload the proxy when it runs, else start it. Written by atom-installer.
if rc-service %[1]s status >/dev/null 2>&1; then
	exec rc-service %[1]s reload
fi
exec rc-service %[1]s start
`, name)
}

var reloadCmdRe = regexp.MustCompile(`(?m)^([ 	]+reload:[ 	]*)\[[^\]\n]*atom-sing-box\.service\]`)

// setReloadCommand rewrites the reload and stop commands of the two kernels in atom_rules.yaml. cmd is the
// reload script of sing-box (OpenRC); when it is the installer itself (the container version, which has no
// service manager), every command becomes `atom-installer svc <action> <service>`.
func setReloadCommand(yaml, cmd string) (string, error) {
	if !reloadCmdRe.MatchString(yaml) {
		return "", fmt.Errorf("atom_rules.yaml has no sing-box reload command")
	}
	if filepath.Base(cmd) == "atom-installer" {
		for _, backend := range []string{"sing-box", "mihomo"} {
			for _, a := range []struct{ yamlAction, action string }{{"reload-or-restart", "reload"}, {"stop", "stop"}} {
				yaml = strings.ReplaceAll(yaml, "[systemctl, "+a.yamlAction+", atom-"+backend+".service]",
					"["+cmd+", svc, "+a.action+", atom-"+backend+"]")
			}
		}
		return yaml, nil
	}
	yaml = reloadCmdRe.ReplaceAllString(yaml, "${1}["+strings.ReplaceAll(cmd, "$", "$$")+"]")
	yaml = strings.ReplaceAll(yaml, "[systemctl, reload-or-restart, atom-mihomo.service]", "["+filepath.Join(filepath.Dir(cmd), "reload-mihomo.sh")+"]")
	for _, backend := range []string{"sing-box", "mihomo"} {
		yaml = strings.ReplaceAll(yaml, "[systemctl, stop, atom-"+backend+".service]", "[rc-service, --ifstarted, atom-"+backend+", stop]")
	}
	return yaml, nil
}
