package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Protect the ports in the generated config before starting or reloading
// sing-box. On reload old sockets can still be serving while the signal is
// handled, so retain their entries until a full restart.
func protectWAN(p Paths, starting bool) error {
	state, err := loadState(p)
	if err != nil {
		return err
	}
	if state.Topology != "gateway" {
		return nil
	}
	rules, err := os.ReadFile(p.Etc("atom-wan.nft"))
	if err != nil {
		return err
	}
	var batch strings.Builder
	batch.Write(rules)
	if starting {
		batch.WriteString("flush set inet atom_wan tcp_proxy_ports\nflush set inet atom_wan udp_proxy_ports\n")
	}
	path := filepath.Join(p.Data(), "sing-box", "config.json")
	if err := appendProxyPorts(&batch, path); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, state.NftBin, "-f", "-")
	cmd.Stdin = strings.NewReader(batch.String())
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("更新 WAN 防火墙：%w（%s）", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func appendProxyPorts(batch *strings.Builder, path string) error {
	var config struct {
		Inbounds []struct {
			Type string `json:"type"`
			Port uint16 `json:"listen_port"`
		} `json:"inbounds"`
	}
	if err := readJSON(path, &config); err != nil {
		if os.IsNotExist(err) {
			return nil // fresh DNS start, before AtomDNS generates the first config
		}
		return err
	}
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
	return nil
}
