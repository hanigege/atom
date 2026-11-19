package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// compareVersions compares dotted numeric versions ("0.1.0"); anything after a non-digit is ignored.
func compareVersions(a, b string) int {
	pa, pb := versionParts(a), versionParts(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func versionParts(v string) [3]int {
	var out [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	for i, s := range strings.SplitN(v, ".", 3) {
		n := 0
		for _, r := range s {
			if r < '0' || r > '9' {
				break
			}
			n = n*10 + int(r-'0')
		}
		out[i] = n
	}
	return out
}

func fileSHA256(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// upgrade replaces the programs and the shipped configuration, keeps the data, and goes back to the
// backup when the new version does not come up.
func (ins *Installer) upgrade(st *InstallState, log Logger) error {
	p := ins.Paths
	if _, err := loadChanges(p); err != nil {
		return err
	}
	if err := checkLegacyTor(p); err != nil {
		return err
	}
	if err := ins.checkUpgradeConfig(st.PanelPort, log); err != nil {
		return err
	}
	backup := backupPath(p, st.Version)
	log.Logf("备份到 %s", backup)
	if err := ins.makeBackup(backup); err != nil {
		return fmt.Errorf("备份: %w", err)
	}
	backend, _, _ := p.proxyService()
	oldProxy := fileSHA256(p.Bin(backend))

	oldVersion := st.Version
	err := ins.applyUpgrade(st, oldProxy, log)
	if err == nil {
		st.Version, st.UpdatedAt = ins.Version, time.Now()
		if err = writeJSON(p.State(), st); err != nil {
			err = fmt.Errorf("记录新版本失败: %w", err)
		}
	}
	if err != nil {
		st.Version = oldVersion
		log.Logf("升级失败：%v", err)
		log.Logf("恢复备份")
		if rerr := ins.restoreBackup(backup, st.PanelPort, log); rerr != nil {
			return fmt.Errorf("%w（恢复备份也失败了：%v，备份在 %s）", err, rerr, backup)
		}
		return fmt.Errorf("%w（已恢复到 %s）", err, st.Version)
	}
	pruneBackups(p.Backup(), 3)
	return nil
}

func (ins *Installer) checkUpgradeConfig(panelPort int, log Logger) error {
	root, err := os.MkdirTemp("", "atom-check-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)
	// Stage the same overlay as the upgrade. The checker only reads retained
	// data, so link it instead of copying large lists and caches.
	data, err := filepath.Abs(ins.Paths.Data())
	if err != nil {
		return err
	}
	if err := filepath.WalkDir(data, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(data, path)
		if err != nil {
			return err
		}
		target := filepath.Join(root, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return os.Symlink(path, target)
	}); err != nil {
		return fmt.Errorf("准备升级检查：%w", err)
	}
	if err := ins.refreshConfig(root, panelPort); err != nil {
		return fmt.Errorf("准备升级检查：%w", err)
	}
	if err := runCmd(log, 2*time.Minute, filepath.Join(ins.PkgDir, "bin", "atomdns"), "check", "-d", root, "-c", "config_custom.yaml"); err != nil {
		return fmt.Errorf("候选版本无法读取现有配置，当前服务继续运行：%w", err)
	}
	return nil
}

// Check both the saved nodes and the config sing-box can start from on its own.
func checkLegacyTor(p Paths) error {
	type outbound struct {
		Type string `json:"type"`
		Tag  string `json:"tag"`
	}
	for _, path := range []string{filepath.Join(p.Data(), "webinfo", "atom_proxy.json"), filepath.Join(p.Data(), "sing-box", "config.json")} {
		var config struct {
			Nodes []struct {
				ID       string   `json:"id"`
				Outbound outbound `json:"outbound"`
			} `json:"nodes"`
			Outbounds []outbound `json:"outbounds"`
		}
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err == nil {
			err = json.Unmarshal(data, &config)
		}
		if err != nil {
			return fmt.Errorf("检查旧节点 %s：%w", path, err)
		}
		for _, node := range config.Nodes {
			if node.Outbound.Type == "tor" {
				return fmt.Errorf("%s 中有 Tor 节点 %q，请先从旧面板移除后再继续", path, node.ID)
			}
		}
		for _, item := range config.Outbounds {
			if item.Type == "tor" {
				return fmt.Errorf("%s 中仍有 Tor 节点 %q，请先移除后再继续", path, item.Tag)
			}
		}
	}
	return nil
}

func (ins *Installer) applyUpgrade(st *InstallState, oldProxy string, log Logger) error {
	if err := ins.installBinaries(log, compareVersions(ins.Version, st.Version) < 0); err != nil {
		return err
	}
	if err := ins.installData(st.PanelPort, log); err != nil {
		return err
	}
	if err := ins.writeNetworkFiles(st, log); err != nil {
		return err
	}
	if err := ins.Paths.Init().Restart(log, dnsUnitName); err != nil {
		return err
	}
	api := newAPIClient(st.PanelPort, ins.Paths.Data())
	if _, err := api.waitStatus(60*time.Second, func(*apiStatus) bool { return true }); err != nil {
		return fmt.Errorf("atomdns 的管理接口没有起来: %w", err)
	}
	backend, unit, _ := ins.Paths.proxyService()
	if fileSHA256(ins.Paths.Bin(backend)) != oldProxy {
		log.Logf("%s 程序变了，重启它", backend)
		if err := ins.Paths.Init().Restart(log, unit); err != nil {
			return err
		}
	}
	if _, err := api.waitStatus(90*time.Second, func(s *apiStatus) bool { return s.SingBox.Running && s.SingBox.InSync }); err != nil {
		return fmt.Errorf("升级后 sing-box 没有处于最新配置: %w", err)
	}
	return nil
}

// backupItem is something the upgrade may replace. exact items are put back as they were (a file the new
// version added is removed); the others are laid over.
type backupItem struct {
	path, name string
	file       bool
	exact      bool
	mode       os.FileMode // of a file item when put back; 0 is 0644
}

// backed up: programs, the state and network files, the shipped configuration (not the data), the WebUI, the panel login,
// and the copy of the shipped package a reset installs from.
func (ins *Installer) backupItems() []backupItem {
	p := ins.Paths
	items := []backupItem{
		{path: filepath.Join(p.Root, "bin"), name: "bin"},
		{path: filepath.Join(p.Root, "etc"), name: "etc"},
		{path: p.Etc("atom-wan.nft"), name: "gateway/atom-wan.nft", file: true},
		{path: p.Etc("atom-gateway.nft"), name: "gateway/atom-gateway.nft", file: true},
		{path: filepath.Join(p.Root, "lib"), name: "lib"},
		{path: p.Shipped(), name: "share", exact: true},
		{path: filepath.Join(p.Data(), "sub_config"), name: "data/sub_config", exact: true},
		{path: filepath.Join(p.Data(), "ui", "main"), name: "data/ui/main", exact: true},
		{path: filepath.Join(p.Data(), "config_custom.yaml"), name: "data/config_custom.yaml", file: true},
		{path: filepath.Join(p.Data(), "mihomo", "config.yaml"), name: "data/mihomo/config.yaml", file: true, mode: 0o600},
		{path: filepath.Join(p.Data(), "sing-box", "config.json"), name: "data/sing-box/config.json", file: true, mode: 0o600},
		{path: filepath.Join(p.Data(), "rule", "atom_catalog.json"), name: "data/rule/atom_catalog.json", file: true},
		{path: filepath.Join(p.Data(), "webinfo", "atom_auth.json"), name: "data/webinfo/atom_auth.json", file: true, mode: 0o600}, // the panel login: a failed reconfigure puts it back
		{path: p.DNSUnit(), name: "units/" + filepath.Base(p.DNSUnit()), file: true, mode: p.unitMode()},
		{path: p.MihomoUnit(), name: "units/" + filepath.Base(p.MihomoUnit()), file: true, mode: p.unitMode()},
		{path: p.SingBoxUnit(), name: "units/" + filepath.Base(p.SingBoxUnit()), file: true, mode: p.unitMode()},
		{path: p.unitFile(maintenanceUnitName), name: "units/" + filepath.Base(p.unitFile(maintenanceUnitName)), file: true, mode: p.unitMode()},
	}
	if p.OpenRC {
		items = append(items, backupItem{path: p.logrotateConfig(), name: "logrotate/atom-installer", file: true})
	}
	return items
}

const absentList = "absent.txt"

func (ins *Installer) makeBackup(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	var absent []string
	for _, it := range ins.backupItems() {
		if _, err := os.Stat(it.path); os.IsNotExist(err) {
			absent = append(absent, it.name)
			continue
		} else if err != nil {
			return err
		}
		var err error
		if it.file {
			mode := it.mode
			if mode == 0 {
				mode = 0o644
			}
			err = copyFileAtomic(it.path, filepath.Join(dir, it.name), mode)
		} else {
			err = copyTree(it.path, filepath.Join(dir, it.name))
		}
		if err != nil {
			return err
		}
	}
	return writeFileAtomic(filepath.Join(dir, absentList), []byte(strings.Join(absent, "\n")+"\n"), 0o644)
}

func (ins *Installer) restoreBackup(dir string, panelPort int, log Logger) error {
	absent := map[string]bool{}
	if data, err := os.ReadFile(filepath.Join(dir, absentList)); err == nil {
		for _, n := range strings.Fields(string(data)) {
			absent[n] = true
		}
	}
	for _, it := range ins.backupItems() {
		src := filepath.Join(dir, it.name)
		if _, err := os.Stat(src); os.IsNotExist(err) {
			if !absent[it.name] {
				// An older backup may not have tracked this item.
				continue
			}
			if it.file || it.exact {
				if err := os.RemoveAll(it.path); err != nil {
					return err
				}
			}
			continue
		} else if err != nil {
			return err
		}
		var err error
		if it.file {
			mode := it.mode
			if mode == 0 {
				mode = 0o644
			}
			err = copyFileAtomic(src, it.path, mode)
		} else {
			if it.exact {
				if err := os.RemoveAll(it.path); err != nil {
					return err
				}
			}
			err = copyTree(src, it.path)
		}
		if err != nil {
			return err
		}
	}
	if absent["gateway/atom-wan.nft"] {
		if _, err := quietCmd("nft", "list", "table", "inet", "atom_wan"); err == nil {
			if err := runCmd(log, 15*time.Second, "nft", "delete", "table", "inet", "atom_wan"); err != nil {
				return err
			}
		}
	}
	init := ins.Paths.Init()
	if err := init.Reload(log); err != nil {
		return err
	}
	// Bring the backed-up generated proxy config up before DNS reconciles it.
	_, unit, config := ins.Paths.proxyService()
	if _, err := os.Stat(config); err == nil {
		if err := init.Restart(log, unit); err != nil {
			return err
		}
	}
	if err := init.Restart(log, dnsUnitName); err != nil {
		return err
	}
	if _, err := newAPIClient(panelPort, ins.Paths.Data()).waitStatus(60*time.Second, func(s *apiStatus) bool { return s.SingBox.Running && s.SingBox.InSync }); err != nil {
		return fmt.Errorf("恢复后服务没有起来: %w", err)
	}
	log.Logf("已恢复，服务正常")
	return nil
}

const backupTimeFormat = "20060102-150405"

var backupName = regexp.MustCompile(`^(?:reconfigure|dev|v?\d+\.\d+\.\d+(?:[-+][a-zA-Z0-9.+-]+)?)-(\d{8}-\d{6}(?:\.\d{9})?)$`)

func backupPath(p Paths, prefix string) string {
	return filepath.Join(p.Backup(), prefix+"-"+time.Now().Format(backupTimeFormat+".000000000"))
}

func pruneBackups(dir string, keep int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type backup struct{ name, timestamp string }
	var backups []backup
	for _, e := range entries {
		if stamp := backupName.FindStringSubmatch(e.Name()); e.IsDir() && stamp != nil {
			if _, err := time.Parse(backupTimeFormat, stamp[1]); err == nil {
				backups = append(backups, backup{e.Name(), stamp[1]})
			}
		}
	}
	sort.SliceStable(backups, func(i, j int) bool { return backups[i].timestamp > backups[j].timestamp })
	for _, backup := range backups[min(keep, len(backups)):] {
		os.RemoveAll(filepath.Join(dir, backup.name))
	}
}
