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
		if err := removeReplyRouting(); err != nil { // an installation before this one had the option: its rule and table go
			return err
		}
	}
	ins.redirectsBefore = nil // per attempt: what a failed one changed goes back
	if cfg.ReplyVia && len(st.LAN) > 0 {
		ins.redirectsBefore = readRedirects(st.LAN[0])
	}
	if err := loadContainerNetwork(ins.Paths); err != nil {
		return err
	}
	j.Logf("规则已加载：%s", ins.Paths.Etc("atom.nft"))
	return nil
}

// absentErr: the command failed because what it was to remove is not there, which is what was wanted.
func absentErr(err error) bool {
	if err == nil {
		return false
	}
	for _, s := range []string{"No such file or directory", "does not exist", "No such process"} {
		if strings.Contains(err.Error(), s) {
			return true
		}
	}
	return false
}

// removeReplyRouting takes the rule of the reply option and its default route away, when the rule is exactly
// the installer's: another rule or table 234 is not touched.
func removeReplyRouting() error {
	rules, err := quietCmd("ip", "rule", "show")
	if err != nil {
		return fmt.Errorf("ip rule show：%w: %s", err, rules)
	}
	ours := false
	for _, l := range strings.Split(rules, "\n") {
		ours = ours || strings.TrimSpace(l) == replyRule
	}
	if !ours {
		return nil
	}
	var errs []string
	// the route first: while the rule is there, a retry finds it and does both again
	for _, args := range [][]string{
		{"route", "del", "default", "table", replyTable},
		{"rule", "del", "fwmark", replyMark, "lookup", replyTable, "priority", replyPriority},
	} {
		if err := runCmd(quietLogger{}, 30*time.Second, "ip", args...); err != nil && !absentErr(err) {
			errs = append(errs, err.Error())
			break // the rule stays, so that the next try finds it
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "；"))
	}
	return nil
}

// containerTeardown is the rollback of a failed installation: the services stop and the nft table and the policy
// routing this installation loaded go. The files stay, so that the wizard can go back and try again. What cannot
// be taken away is returned (what is already gone is not an error).
func containerTeardown(p Paths, log Logger) error {
	log.Logf("撤销本次加载的网络规则")
	in := p.Init()
	for _, n := range []string{dnsUnitName, singBoxUnitName, mihomoUnitName} {
		if err := in.Stop(log, n); err != nil {
			log.Logf("  （忽略）%v", err)
		}
	}
	var errs []string
	del := func(name string, args ...string) {
		if err := runCmd(log, 60*time.Second, name, args...); err != nil && !absentErr(err) {
			errs = append(errs, err.Error())
		}
	}
	del("nft", "delete", "table", "inet", "atom")
	del("ip", "rule", "del", "fwmark", fwmark, "lookup", routeTable, "priority", routePriority)
	del("ip", "route", "flush", "table", routeTable)
	if err := removeReplyRouting(); err != nil {
		errs = append(errs, err.Error())
	}
	if len(errs) > 0 {
		return fmt.Errorf("没有撤销网络规则：%s", strings.Join(errs, "；"))
	}
	return nil
}

// readRedirects is the accept_redirects values the reply option is going to change, by /proc path.
func readRedirects(lan string) map[string]string {
	out := map[string]string{}
	for _, dev := range []string{"all", lan} {
		f := "/proc/sys/net/ipv4/conf/" + dev + "/accept_redirects"
		if v, err := os.ReadFile(f); err == nil {
			out[f] = strings.TrimSpace(string(v))
		}
	}
	return out
}

// undoRedirects puts the values back that the installation read before it turned them off (where it could).
func (ins *Installer) undoRedirects(log Logger) {
	for f, v := range ins.redirectsBefore {
		if cur, err := os.ReadFile(f); err == nil && strings.TrimSpace(string(cur)) == v {
			continue
		}
		if err := os.WriteFile(f, []byte(v), 0o644); err != nil {
			log.Logf("  （忽略）恢复 %s: %v", f, err)
		}
	}
	ins.redirectsBefore = nil
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

// copyTreeExcept is copyTree without the path skip (and what is under it), and with the links kept as links: what
// the reset deletes next, it has to have saved. Sockets and the like are runtime objects, not data.
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
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o755)
		case d.Type()&fs.ModeSymlink != 0:
			dest, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			os.Remove(target)
			return os.Symlink(dest, target)
		case !d.Type().IsRegular():
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return copyFileAtomic(path, target, info.Mode().Perm())
	})
}

// resetPending is the record that a reset is deleting the data: an entry process that is stopped in the middle
// finishes the deletion when it starts again, and does not start a half deleted installation.
func resetPending(p Paths) string { return filepath.Join(keptByReset(p), ".reset-pending") }

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
	if err := containerTeardown(p, log); err != nil {
		return "", fmt.Errorf("%w，未删除任何数据", err)
	}
	if err := requireRealDirs(p); err != nil {
		return "", fmt.Errorf("%w，未删除任何数据", err)
	}
	if err := os.MkdirAll(keptByReset(p), 0o700); err != nil {
		return "", err
	}
	if err := writeFile(resetPending(p), []byte(time.Now().Format(time.RFC3339)+"\n"), 0o600, true); err != nil {
		return "", fmt.Errorf("记录重置：%w，未删除任何数据", err)
	}
	if err := purgeData(p, log); err != nil {
		return "", err
	}
	if err := os.Remove(resetPending(p)); err != nil && !os.IsNotExist(err) {
		return "", err
	}
	return "", nil
}

// purgeData empties the data directory except for the backups; the install record goes last, so that a removal
// that failed half way can be tried again in full.
func purgeData(p Paths, log Logger) error {
	if err := requireRealDirs(p); err != nil {
		return err
	}
	data := p.Data()
	keep := keptByReset(p)
	entries, err := os.ReadDir(data)
	if err != nil {
		return err
	}
	slices.SortStableFunc(entries, func(a, b os.DirEntry) int { return cmpBool(a.Name() == ".atom", b.Name() == ".atom") })
	for _, e := range entries {
		path := filepath.Join(data, e.Name())
		if path != filepath.Dir(keep) {
			if err := os.RemoveAll(path); err != nil {
				return err
			}
			continue
		}
		inner, err := os.ReadDir(path) // data/.atom: everything but the backups
		if err != nil {
			return err
		}
		slices.SortStableFunc(inner, func(a, b os.DirEntry) int { return cmpBool(a.Name() == "install.json", b.Name() == "install.json") })
		for _, f := range inner {
			if filepath.Join(path, f.Name()) == keep {
				continue
			}
			if err := os.RemoveAll(filepath.Join(path, f.Name())); err != nil {
				return err
			}
		}
	}
	log.Logf("已清空 %s，备份留在 %s", data, keep)
	return nil
}

// requireRealDirs: data/.atom and its backup directory are directories, not links to somewhere else, so that what
// is deleted below them is what was backed up.
func requireRealDirs(p Paths) error {
	for _, d := range []string{filepath.Join(p.Data(), ".atom"), keptByReset(p)} {
		info, err := os.Lstat(d)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("%s 不是目录（是链接或文件）", d)
		}
	}
	return nil
}

// resumeReset finishes a reset that the entry process did not (it was stopped while the data was deleted).
func resumeReset(p Paths, log Logger) error {
	if _, err := os.Stat(resetPending(p)); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("读不到重置记录 %s：%w", resetPending(p), err)
	}
	log.Logf("上一次重置没有做完，继续清空数据")
	if err := purgeData(p, log); err != nil {
		return err
	}
	return os.Remove(resetPending(p))
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
