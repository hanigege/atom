package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// syncNft takes the FakeIP CIDR and WAN listener ports from the same generated
// config the proxy will load. Keep old WAN ports until a full proxy restart.
func syncNft(p Paths, starting, loadAtom bool, backend string) error {
	state, err := loadState(p)
	if err != nil {
		return err
	}
	// DNS startup replaces the base table. Serialize it with sing-box hooks so
	// an older CIDR read by one hook cannot overwrite a newer update by another.
	if err := os.MkdirAll(p.Run(""), 0o755); err != nil {
		return err
	}
	lock, err := os.OpenFile(p.Run("nft.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}

	if backend == "" {
		backend, _, _ = p.proxyService()
	}
	config, err := readProxyNftConfig(p, backend)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	var batch strings.Builder
	if loadAtom {
		rules, err := os.ReadFile(p.Etc("atom.nft"))
		if err != nil {
			return err
		}
		batch.Write(rules)
	}
	// sing-box can start before AtomDNS, including during upgrades from a
	// version whose live table has no output set yet.
	batch.WriteString("add table inet atom\nadd set inet atom output_fakeip4 { type ipv4_addr; flags interval; }\n")
	batch.WriteString("flush set inet atom output_fakeip4\n")
	if err := config.appendFakeIP(&batch); err != nil {
		return err
	}
	if state.Topology == "gateway" {
		rules, err := os.ReadFile(p.Etc("atom-wan.nft"))
		if err != nil {
			return err
		}
		batch.Write(rules)
		if starting {
			batch.WriteString("flush set inet atom_wan tcp_proxy_ports\nflush set inet atom_wan udp_proxy_ports\n")
		}
		config.appendProxyPorts(&batch)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, state.NftBin, "-f", "-")
	cmd.Stdin = strings.NewReader(batch.String())
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("更新 nftables：%w（%s）", err, strings.TrimSpace(string(output)))
	}
	return nil
}

type proxyDNS struct {
	Type  string `json:"type"`
	Range string `json:"inet4_range"`
}

type proxyInbound struct {
	Type string `json:"type"`
	Port uint16 `json:"listen_port"`
}

type proxyNftConfig struct {
	DNS struct {
		Servers []proxyDNS `json:"servers"`
	} `json:"dns"`
	Inbounds []proxyInbound `json:"inbounds"`
}

func readProxyNftConfig(p Paths, backend string) (proxyNftConfig, error) {
	var config proxyNftConfig
	switch backend {
	case "sing-box":
		err := readJSON(filepath.Join(p.Data(), "sing-box", "config.json"), &config)
		return config, err
	case "mihomo":
		var source struct {
			DNS struct {
				Range string `json:"fake-ip-range"`
			} `json:"dns"`
			MixedPort  uint16 `json:"mixed-port"`
			RedirPort  uint16 `json:"redir-port"`
			TProxyPort uint16 `json:"tproxy-port"`
		}
		if err := readJSON(filepath.Join(p.Data(), "mihomo", "config.yaml"), &source); err != nil {
			return config, err
		}
		config.DNS.Servers = []proxyDNS{{Type: "fakeip", Range: source.DNS.Range}}
		for _, port := range []uint16{source.MixedPort, source.RedirPort, source.TProxyPort} {
			// The generated Mihomo listeners use both TCP and UDP.
			config.Inbounds = append(config.Inbounds, proxyInbound{Type: "mixed", Port: port})
		}
		return config, nil
	default:
		return config, fmt.Errorf("未知代理内核 %q", backend)
	}
}

func (config proxyNftConfig) appendFakeIP(batch *strings.Builder) error {
	for _, server := range config.DNS.Servers {
		if server.Type != "fakeip" || server.Range == "" {
			continue
		}
		if err := checkFakeIPRange(server.Range); err != nil {
			return fmt.Errorf("nftables FakeIP 网段：%w", err)
		}
		fmt.Fprintf(batch, "add element inet atom output_fakeip4 { %s }\n", strings.TrimSpace(server.Range))
	}
	return nil
}

func (config proxyNftConfig) appendProxyPorts(batch *strings.Builder) {
	for _, inbound := range config.Inbounds {
		if inbound.Port == 0 {
			continue
		}
		if inbound.Type == "mixed" || inbound.Type == "redirect" {
			fmt.Fprintf(batch, "add element inet atom_wan tcp_proxy_ports { %d }\n", inbound.Port)
		}
		if inbound.Type == "mixed" || inbound.Type == "tproxy" {
			fmt.Fprintf(batch, "add element inet atom_wan udp_proxy_ports { %d }\n", inbound.Port)
		}
	}
}
