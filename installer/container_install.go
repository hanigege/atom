package main

// What the wizard does differently in the container (docs/container.md 6): the network step writes the rules and
// loads them in this network namespace, a failure takes back only what was loaded, and the reset backs up and
// empties the data directory instead of removing a system installation.

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// stepNetworkContainer records the network settings, writes the rules and loads them. There is no changes.json:
// what is loaded lives in the network namespace of the container and goes with it.
func (ins *Installer) stepNetworkContainer(cfg Config, j *Job) error {
	st := &InstallState{Version: ins.Version, InstalledAt: time.Now(), UpdatedAt: time.Now(), Topology: "bypass", LAN: cfg.LAN,
		PanelPort: cfg.PanelPort, NftBin: lookPathOr("nft", "/usr/sbin/nft"), SysctlBin: lookPathOr("sysctl", "/usr/sbin/sysctl")}
	if cfg.ReplyVia {
		st.ReplyVia = strings.TrimSpace(cfg.Router)
	}
	if old, err := loadState(ins.Paths); err == nil { // a reconfigure keeps what the first installation recorded
		st.InstalledAt, st.Complete = old.InstalledAt, old.Complete
	}
	if err := writeJSON(ins.Paths.State(), st); err != nil {
		return err
	}
	if !cfg.ReplyVia {
		removeReplyRouting() // an installation before this one had the option: its rule and table go
	}
	if err := loadContainerNetwork(ins.Paths); err != nil {
		return err
	}
	j.Logf("规则已加载：%s", ins.Paths.Etc("atom.nft"))
	return nil
}

// removeReplyRouting takes the rule and the default route of the reply option away; both may be absent.
func removeReplyRouting() {
	for _, args := range [][]string{
		{"rule", "del", "fwmark", replyMark, "lookup", replyTable, "priority", replyPriority},
		{"route", "flush", "table", replyTable},
	} {
		_ = runCmd(quietLogger{}, 30*time.Second, "ip", args...)
	}
}

// containerTeardown is the rollback of a failed installation: the services stop and the nft table and the policy
// routing this installation loaded go. The files stay, so that the wizard can go back and try again.
func containerTeardown(p Paths, log Logger) {
	log.Logf("撤销本次加载的网络规则")
	in := p.Init()
	for _, n := range []string{dnsUnitName, singBoxUnitName, mihomoUnitName} {
		if err := in.Stop(log, n); err != nil {
			log.Logf("  （忽略）%v", err)
		}
	}
	ignore := func(name string, args ...string) {
		if err := runCmd(log, 60*time.Second, name, args...); err != nil {
			log.Logf("  （忽略）%v", err)
		}
	}
	ignore("nft", "delete", "table", "inet", "atom")
	ignore("ip", "rule", "del", "fwmark", fwmark, "lookup", routeTable, "priority", routePriority)
	ignore("ip", "route", "flush", "table", routeTable)
	removeReplyRouting()
}

// containerBackupItems is backupItems of the container: the programs and the shipped files are in the image, so
// only the records and rules in data/.atom and the files of the data a reconfigure changes.
func (ins *Installer) containerBackupItems() []backupItem {
	p := ins.Paths
	return []backupItem{
		{path: p.State(), name: "etc/install.json", file: true},
		{path: p.Etc("atom.nft"), name: "etc/atom.nft", file: true},
		{path: p.Lib("policy-route.sh"), name: "lib/policy-route.sh", file: true, mode: 0o755},
		{path: filepath.Join(p.Data(), "sub_config"), name: "data/sub_config", exact: true},
		{path: filepath.Join(p.Data(), "ui", "main"), name: "data/ui/main", exact: true},
		{path: filepath.Join(p.Data(), "config_custom.yaml"), name: "data/config_custom.yaml", file: true},
		{path: filepath.Join(p.Data(), "mihomo", "config.yaml"), name: "data/mihomo/config.yaml", file: true, mode: 0o600},
		{path: filepath.Join(p.Data(), "sing-box", "config.json"), name: "data/sing-box/config.json", file: true, mode: 0o600},
		{path: filepath.Join(p.Data(), "rule", "atom_catalog.json"), name: "data/rule/atom_catalog.json", file: true},
		{path: filepath.Join(p.Data(), "webinfo", "atom_auth.json"), name: "data/webinfo/atom_auth.json", file: true, mode: 0o600},
	}
}

// ---- the reset ----

// keptByReset is what the reset leaves in the data directory: the backups.
func keptByReset(p Paths) string { return filepath.Join(p.Data(), ".atom", "backup") }

// copyTreeExcept is copyTree without the path skip (and what is under it).
func copyTreeExcept(src, dst, skip string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == skip {
			return filepath.SkipDir
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return copyFileAtomic(path, target, info.Mode().Perm())
	})
}

// containerUninstallForReset is uninstallForReset of the container: the services stop (and are checked to be
// stopped before anything is deleted), the network rules go, and the data directory is emptied except for the
// backups. The programs and the shipped files are the image's and stay.
func containerUninstallForReset(p Paths, log Logger) (string, error) {
	in := p.Init()
	var failed []string
	for _, n := range []string{dnsUnitName, singBoxUnitName, mihomoUnitName} {
		if err := in.Stop(log, n); err != nil {
			failed = append(failed, fmt.Sprintf("%s（%v）", n, err))
		}
	}
	if len(failed) == 0 {
		if left := containerRunning(context.Background(), p); len(left) > 0 {
			failed = left
		}
	}
	if len(failed) > 0 {
		return "", fmt.Errorf("旧的服务没有停下，未删除任何数据：%s", strings.Join(failed, "、"))
	}
	containerTeardown(p, log)
	// the install record goes last: while anything else is left, a retry still finds it
	data := p.Data()
	keep := keptByReset(p)
	entries, err := os.ReadDir(data)
	if err != nil {
		return "", err
	}
	slices.SortStableFunc(entries, func(a, b os.DirEntry) int { return cmpBool(a.Name() == ".atom", b.Name() == ".atom") })
	for _, e := range entries {
		path := filepath.Join(data, e.Name())
		if path == filepath.Dir(keep) { // data/.atom: everything but the backups, the install record last
			inner, err := os.ReadDir(path)
			if err != nil {
				return "", err
			}
			slices.SortStableFunc(inner, func(a, b os.DirEntry) int { return cmpBool(a.Name() == "install.json", b.Name() == "install.json") })
			for _, f := range inner {
				if filepath.Join(path, f.Name()) == keep {
					continue
				}
				if err := os.RemoveAll(filepath.Join(path, f.Name())); err != nil {
					return "", err
				}
			}
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			return "", err
		}
	}
	log.Logf("已清空 %s，备份留在 %s", data, keep)
	return "", nil
}

// containerRunning names the services of atom that the entry process says run.
func containerRunning(ctx context.Context, p Paths) []string {
	var left []string
	for _, n := range []string{"atom-dns", "atom-sing-box", "atom-mihomo"} {
		switch st := containerServiceState(ctx, p, n); st {
		case "stopped":
		default:
			left = append(left, n+"（"+st+"）")
		}
	}
	return left
}

// verifyPurgedContainer checks that nothing the next installation could take over is left in the data
// directory, and that no service runs.
func verifyPurgedContainer(p Paths) error {
	var found []string
	entries, _ := os.ReadDir(p.Data())
	for _, e := range entries {
		path := filepath.Join(p.Data(), e.Name())
		if e.Name() != ".atom" {
			found = append(found, path)
			continue
		}
		inner, _ := os.ReadDir(path)
		for _, f := range inner {
			if filepath.Join(path, f.Name()) != keptByReset(p) {
				found = append(found, filepath.Join(path, f.Name()))
			}
		}
	}
	if len(found) > 0 {
		return fmt.Errorf("仍然存在：%s", strings.Join(found, "、"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), serviceStopWait)
	defer cancel()
	if left := containerRunning(ctx, p); len(left) > 0 {
		return fmt.Errorf("仍在运行：%s", strings.Join(left, "、"))
	}
	return nil
}
