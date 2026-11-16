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
// config sing-box will load. Keep old WAN ports until a full sing-box restart.
func syncNft(p Paths, starting, loadAtom bool) error {
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

	path := filepath.Join(p.Data(), "sing-box", "config.json")
	var config proxyNftConfig
	if err := readJSON(path, &config); err != nil && !os.IsNotExist(err) {
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

type proxyNftConfig struct {
	DNS struct {
		Servers []struct {
			Type  string `json:"type"`
			Range string `json:"inet4_range"`
		} `json:"servers"`
	} `json:"dns"`
	Inbounds []struct {
		Type string `json:"type"`
		Port uint16 `json:"listen_port"`
	} `json:"inbounds"`
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
