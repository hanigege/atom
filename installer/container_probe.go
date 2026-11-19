package main

// The environment step of the wizard in the container (docs/container.md 6.2): instead of the system and port 53
// checks, what the network rules need is tried: NET_ADMIN, a temporary nft table with the redirect, tproxy and fib
// statements, an ip rule, and the network interfaces.

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// probeCheck is one line of the environment step.
type probeCheck struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
}

type containerProbe struct {
	Checks   []probeCheck `json:"checks"`
	Problems []string     `json:"problems"` // the reasons of the checks that failed
	NICs     int          `json:"nics"`     // network interfaces other than lo
}

// probeEnv is what the probe touches; the tests replace it.
type probeEnv struct {
	capEff func() (string, error)                                   // the CapEff line of /proc/self/status
	run    func(stdin, name string, args ...string) (string, error) // the output includes stderr
	nics   func() (total, withIPv4 int)
}

func realProbeEnv() probeEnv {
	return probeEnv{
		capEff: func() (string, error) {
			data, err := os.ReadFile("/proc/self/status")
			if err != nil {
				return "", err
			}
			for _, l := range strings.Split(string(data), "\n") {
				if v, ok := strings.CutPrefix(l, "CapEff:"); ok {
					return strings.TrimSpace(v), nil
				}
			}
			return "", fmt.Errorf("no CapEff in /proc/self/status")
		},
		run: func(stdin, name string, args ...string) (string, error) {
			ctx := time.After(15 * time.Second)
			cmd := exec.Command(name, args...)
			if stdin != "" {
				cmd.Stdin = strings.NewReader(stdin)
			}
			type res struct {
				out []byte
				err error
			}
			ch := make(chan res, 1)
			go func() { out, err := cmd.CombinedOutput(); ch <- res{out, err} }()
			select {
			case r := <-ch:
				return strings.TrimSpace(string(r.out)), r.err
			case <-ctx:
				if cmd.Process != nil {
					cmd.Process.Kill()
				}
				return "", fmt.Errorf("没有在 15 秒内完成")
			}
		},
		nics: func() (int, int) {
			all, _ := net.Interfaces()
			total := 0
			for _, i := range all {
				if i.Flags&net.FlagLoopback == 0 {
					total++
				}
			}
			return total, len(listInterfaces())
		},
	}
}

const capNetAdmin = 12

// hasCap reads a CapEff value (hex) for the capability bit.
func hasCap(capEff string, bit uint) bool {
	v, err := strconv.ParseUint(strings.TrimSpace(capEff), 16, 64)
	return err == nil && v&(1<<bit) != 0
}

// the probe table: its name is not atom's, so nothing of the installation is touched
const probeTable = "atom_probe"

// probeIface is an interface name nothing has: the probe's rules begin with `iifname`, so no packet can match them
// while they are loaded (the wizard of a reconfigure runs beside live traffic).
const probeIface = "atom-probe0"

func probeNft(chain, rule string) string {
	return fmt.Sprintf("table inet %[1]s\ndelete table inet %[1]s\ntable inet %[1]s {\n\tchain p {\n\t\t%[2]s\n\t\tiifname %[4]q %[3]s\n\t}\n}\n", probeTable, chain, rule, probeIface)
}

// the three statements the atom table uses, each in a chain of the kind it is used in
var probeStatements = []struct{ what, module, script string }{
	{"redirect", "nft_redir", probeNft("type nat hook prerouting priority dstnat; policy accept;", "meta nfproto ipv4 meta l4proto tcp redirect to :7892")},
	{"tproxy", "nft_tproxy", probeNft("type filter hook prerouting priority mangle; policy accept;", "meta nfproto ipv4 meta l4proto udp tproxy ip to :7893 meta mark set "+fwmark+" accept")},
	{"fib", "nft_fib", probeNft("type filter hook prerouting priority mangle; policy accept;", "fib daddr type { local, broadcast, multicast } return")},
}

// firstLine is the first line of an error output, without the "Error: " the tools put in front.
func firstLine(out string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	line = strings.TrimPrefix(line, "Error: ")
	if len(line) > 120 {
		line = line[:120]
	}
	return line
}

func (e probeEnv) probeNftTable() (reasons []string) {
	nft := lookPathOr("nft", "/usr/sbin/nft")
	if _, err := os.Stat(nft); err != nil {
		return []string{"没有 nft 命令"}
	}
	for _, st := range probeStatements {
		out, err := e.run(st.script, nft, "-f", "-")
		if err == nil {
			if out, err := e.run("", nft, "delete", "table", "inet", probeTable); err != nil {
				reasons = append(reasons, "不能删除探测用的 nft 表："+firstLine(out))
			}
			continue
		}
		switch {
		case strings.Contains(out, "not permitted"):
			reasons = append(reasons, "缺少 NET_ADMIN")
		case strings.Contains(out, "not supported") || strings.Contains(out, "No such file") || strings.Contains(out, "Operation not supported"):
			reasons = append(reasons, "内核缺少 "+st.module)
		default:
			reasons = append(reasons, fmt.Sprintf("nft %s 加载失败：%s", st.what, firstLine(out)))
		}
	}
	return reasons
}

func (e probeEnv) probeIPRule() string {
	ip := lookPathOr("ip", "/usr/sbin/ip")
	if _, err := os.Stat(ip); err != nil {
		return "没有 ip 命令"
	}
	args := []string{"fwmark", "0x2ff", "lookup", "2990", "priority", "9990"}
	if out, err := e.run("", ip, append([]string{"rule", "add"}, args...)...); err != nil {
		if strings.Contains(out, "not permitted") {
			return "缺少 NET_ADMIN"
		}
		return "不能添加 ip rule：" + firstLine(out)
	}
	if out, err := e.run("", ip, append([]string{"rule", "del"}, args...)...); err != nil {
		return "不能删除 ip rule：" + firstLine(out)
	}
	return ""
}

func (e probeEnv) probe() containerProbe {
	var p containerProbe
	add := func(id, label string, reasons ...string) {
		var rs []string
		for _, r := range reasons {
			if r != "" && !slices.Contains(rs, r) {
				rs = append(rs, r)
			}
		}
		c := probeCheck{ID: id, Label: label, OK: len(rs) == 0, Reason: strings.Join(rs, "；")}
		p.Checks = append(p.Checks, c)
		for _, r := range rs {
			if !slices.Contains(p.Problems, r) {
				p.Problems = append(p.Problems, r)
			}
		}
	}
	capEff, err := e.capEff()
	switch {
	case err != nil:
		add("cap", "NET_ADMIN", "读不到进程的权限："+err.Error())
	case !hasCap(capEff, capNetAdmin):
		add("cap", "NET_ADMIN", "缺少 NET_ADMIN")
	default:
		add("cap", "NET_ADMIN")
		add("nft", "nft 规则（redirect、tproxy）", e.probeNftTable()...)
		add("rule", "策略路由（ip rule）", e.probeIPRule())
	}
	total, withIP := e.nics()
	p.NICs = total
	switch {
	case total == 0:
		add("nic", "网卡", "没有网卡")
	case withIP == 0:
		add("nic", "网卡", "网卡没有 IPv4 地址")
	default:
		add("nic", "网卡")
	}
	if p.Checks == nil {
		p.Checks = []probeCheck{}
	}
	if p.Problems == nil {
		p.Problems = []string{}
	}
	return p
}

// probeCache keeps the last answer for a few seconds: the page asks for the state often.
type probeCache struct {
	mu   sync.Mutex
	env  probeEnv
	at   time.Time
	last containerProbe
}

const probeTTL = 5 * time.Second

func (c *probeCache) get(fresh bool) containerProbe {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.at.IsZero() || fresh || time.Since(c.at) > probeTTL {
		c.last = c.env.probe()
		c.at = time.Now()
	}
	return c.last
}
