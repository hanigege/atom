package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// hasRecords: install.json or changes.json is there, so there is something to take back.
func hasRecords(p Paths) bool {
	for _, f := range []string{p.State(), p.Changes()} {
		if _, err := os.Stat(f); err == nil {
			return true
		}
	}
	return false
}

// uninstall takes atom off the machine: services, nft tables, policy routing, ip_forward, what the wizard
// changed (resolved, stopped services), the units and /opt/atom. The data directory stays unless purge.
// When something the installer changed cannot be put back, the records and the program stay so that the
// command can be run again once the cause is fixed.
func uninstall(p Paths, purge bool, log Logger) error {
	ch, failed, err := takeBack(p, log)
	if err != nil {
		return err
	}
	if len(failed) > 0 {
		if err := saveChanges(p, ch); err != nil {
			log.Logf("记录保存失败：%v", err)
		}
		return fmt.Errorf("没有还原：%s。记录和程序都还在，处理好之后可以再运行一次 uninstall", strings.Join(failed, "、"))
	}
	if err := removeOpenRCLogs(p); err != nil {
		return err
	}
	if len(ch.PkgInstalled) > 0 {
		log.Logf("安装时装过的软件包没有卸载：%v", ch.PkgInstalled)
	}
	if purge {
		log.Logf("删除 %s（含数据）", p.Root)
		return os.RemoveAll(p.Root)
	}
	for _, d := range []string{"bin", "etc", "lib", "share", "run", "pkg", "backup"} {
		os.RemoveAll(filepath.Join(p.Root, d))
	}
	log.Logf("数据保留在 %s", p.Data())
	return nil
}

// takeBack undoes what the installer did outside /opt/atom: the maintenance service, the network (when the
// record says it is the installer's), the units, resolved, resolv.conf and the services it stopped. The returned
// record keeps what could not be put back (failed names it) and the packages the installer brought.
func takeBack(p Paths, log Logger) (*Changes, []string, error) {
	ch, err := loadChanges(p)
	if err != nil {
		return nil, nil, err
	}
	if err := p.removeMaintenance(log); err != nil {
		return nil, nil, err
	}
	teardownNetwork(p, ch, log)
	if _, err := os.Stat(p.State()); err == nil { // the units are written after install.json
		for _, f := range []string{p.DNSUnit(), p.SingBoxUnit(), p.MihomoUnit()} {
			os.Remove(f)
		}
		p.Init().Forget(log, dnsUnitName, singBoxUnitName, mihomoUnitName)
	}

	var failed []string
	if err := restoreResolved(p, ch, log); err != nil {
		log.Logf("还原 systemd-resolved 失败：%v", err)
		failed = append(failed, "systemd-resolved")
	}
	if err := restoreResolvConf(p, ch, log); err != nil {
		log.Logf("还原 /etc/resolv.conf 失败：%v", err)
		failed = append(failed, "/etc/resolv.conf")
	}
	var left []UnitState
	for _, u := range ch.DisabledUnits {
		if err := restoreUnit(p, u, log); err != nil {
			log.Logf("还原 %s 失败：%v", u.Unit, err)
			left = append(left, u)
			failed = append(failed, u.Unit)
		}
	}
	ch.DisabledUnits = left
	return ch, failed, nil
}

// removeOpenRCLogs removes the log rotation and the logs (OpenRC): they are not data, gone with or without purge.
func removeOpenRCLogs(p Paths) error {
	if !p.OpenRC {
		return nil
	}
	if err := removeOpenRCLogRotation(p); err != nil {
		return fmt.Errorf("移除日志轮转配置：%w", err)
	}
	os.RemoveAll(openRCLogDir)
	return nil
}

// resetFiles are the files of the run directory that belong to the wizard; a reset leaves them to it.
var resetFiles = []string{"installer.json", "installer.pid", "installer.log"}

// uninstallForReset is `uninstall --purge` for the reset from the panel: everything under the root goes, data
// included, except the entries named in keep (the copy of the package the wizard installs from, the backups)
// and the wizard's own files in run/. What could not be put back is not a reason to stop half way: it stays
// recorded in a new changes.json (with resolv.conf.orig when it is needed), which the next installation takes
// over, and is returned as a warning.
func uninstallForReset(p Paths, keep []string, log Logger) (warning string, err error) {
	ch, failed, err := takeBack(p, log)
	if err != nil {
		return "", err
	}
	if err := removeOpenRCLogs(p); err != nil {
		log.Logf("（忽略）%v", err)
	}
	keepOrig := ch.ResolvConfSaved && ch.ResolvConfSavedLink == ""
	entries, err := os.ReadDir(p.Root)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		name := e.Name()
		path := filepath.Join(p.Root, name)
		switch {
		case slices.Contains(keep, name):
		case name == "run":
			removeExcept(path, resetFiles)
		case name == "etc" && keepOrig:
			removeExcept(path, []string{"resolv.conf.orig"})
		default:
			if err := os.RemoveAll(path); err != nil {
				return "", err
			}
		}
	}
	log.Logf("已删除 %s 下的程序、配置和数据", p.Root)
	if data, _ := json.Marshal(ch); string(data) != "{}" {
		if err := saveChanges(p, ch); err != nil {
			return "", fmt.Errorf("保存未还原的记录：%w", err)
		}
	}
	if len(failed) > 0 {
		warning = fmt.Sprintf("没有还原：%s。记录已留给下一次安装，卸载时会再试", strings.Join(failed, "、"))
		log.Logf("! %s", warning)
	}
	return warning, nil
}

func removeExcept(dir string, keep []string) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if !slices.Contains(keep, e.Name()) {
			os.RemoveAll(filepath.Join(dir, e.Name()))
		}
	}
}
