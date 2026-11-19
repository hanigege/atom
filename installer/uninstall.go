package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
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
	ch, failed, err := takeBack(p, false, log)
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
// strict is the reset's removal: stopForReset has stopped and disabled every service already, so the service
// files only go (whatever the records say: an earlier attempt may have removed install.json already), and one
// that cannot be removed is an error, so that nothing is deleted after it.
func takeBack(p Paths, strict bool, log Logger) (*Changes, []string, error) {
	ch, err := loadChanges(p)
	if err != nil {
		return nil, nil, err
	}
	if err := p.removeMaintenance(log, !strict); err != nil {
		return nil, nil, err
	}
	teardownNetwork(p, ch, !strict, log)
	units := []string{p.DNSUnit(), p.SingBoxUnit(), p.MihomoUnit()}
	if strict {
		for _, f := range units {
			if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
				return nil, nil, fmt.Errorf("删除服务文件：%w", err)
			}
		}
		p.Init().Forget(log, dnsUnitName, singBoxUnitName, mihomoUnitName)
	} else if _, err := os.Stat(p.State()); err == nil { // the units are written after install.json
		for _, f := range units {
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
//
// Unlike uninstall, it stops before it deletes anything when atom's services are not verifiably stopped (an old
// DNS service would keep port 53 and the data it serves) or a service file cannot be removed; and etc/ (the
// install record and changes.json) goes last, so that a removal that failed half way can be tried again in full.
func uninstallForReset(p Paths, keep []string, log Logger) (warning string, err error) {
	if err := stopForReset(p, log); err != nil {
		return "", fmt.Errorf("旧的服务没有停下，未删除任何数据：%w", err)
	}
	ch, failed, err := takeBack(p, true, log)
	if err != nil {
		return "", err
	}
	if err := removeOpenRCLogs(p); err != nil {
		log.Logf("（忽略）%v", err)
	}
	keepOrig := ch.ResolvConfSaved && ch.ResolvConfSavedLink == ""
	// what was put back is no longer to do, for a retry after a removal below that fails
	if err := saveChanges(p, ch); err != nil {
		return "", fmt.Errorf("保存未还原的记录：%w", err)
	}
	entries, err := os.ReadDir(p.Root)
	if err != nil {
		return "", err
	}
	// etc/ last: while anything else is left, the records that a retry needs stay
	slices.SortStableFunc(entries, func(a, b os.DirEntry) int {
		return cmpBool(a.Name() == "etc", b.Name() == "etc")
	})
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

func cmpBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	}
	return -1
}

// atomServices are the services of an installation, the maintenance service first (it can start the others).
var atomServices = []string{maintenanceUnitName, dnsUnitName, singBoxUnitName, mihomoUnitName}

// atomPrograms are the programs of the installation that run as its services.
var atomPrograms = []string{"atomdns", "sing-box", "mihomo"}

// serviceStopWait is how long the processes of a stopped service may take to go away, and the limit of one
// round of status queries (a query that takes longer is an error); a variable for the tests.
var serviceStopWait = 10 * time.Second

// stopForReset stops atom's services and checks that they are stopped, before the reset deletes anything: a
// stop or disable that fails, a service the service manager still reports running, or a process of atom's
// programs that is still there is an error. It can be repeated: what an earlier attempt stopped and disabled
// passes. The stops have their own time limits (runCmd); the status queries of the services whose files are gone
// share one deadline, and the wait after the stops, every query in it included, ends at another.
func stopForReset(p Paths, log Logger) error {
	in := p.Init()
	probe, cancelProbe := context.WithTimeout(context.Background(), serviceStopWait)
	defer cancelProbe()
	var failed []string
	for _, n := range atomServices {
		var err error
		if _, statErr := os.Lstat(p.unitFile(n)); statErr == nil {
			err = in.DisableNow(log, n)
		} else {
			running, rerr := serviceRunning(probe, p, n)
			if rerr != nil {
				err = rerr
			} else if running { // its file went in an earlier attempt, the service did not
				err = in.Stop(log, n)
			}
		}
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s（%v）", n, err))
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("停止失败：%s", strings.Join(failed, "；"))
	}
	deadline := time.Now().Add(serviceStopWait)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	var last error
	for {
		err := checkStopped(ctx, p)
		switch {
		case err == nil:
			return nil
		case ctx.Err() != nil && last != nil:
			return last // the deadline cut this round short: the last full answer says what still runs
		case ctx.Err() != nil:
			return err
		}
		last = err
		wait := time.Until(deadline)
		if wait <= 0 {
			return err
		}
		time.Sleep(min(wait, 200*time.Millisecond))
	}
}

// checkStopped: none of atom's services runs, by the service manager's account (asked within ctx) and by the
// processes there.
func checkStopped(ctx context.Context, p Paths) error {
	left, err := stillRunning(ctx, p)
	if err != nil {
		return err
	}
	if len(left) > 0 {
		return fmt.Errorf("仍在运行：%s", strings.Join(left, "、"))
	}
	return nil
}

// stillRunning names atom's services that the service manager reports running and the processes of atom's
// programs that are there; a service manager that does not answer is an error.
func stillRunning(ctx context.Context, p Paths) ([]string, error) {
	var left []string
	for _, n := range atomServices {
		running, err := serviceRunning(ctx, p, n)
		if err != nil {
			return nil, err
		}
		if running {
			left = append(left, n)
		}
	}
	return append(left, programProcesses(p)...), nil
}

// programProcesses lists the processes that run one of atom's programs from bin/ (also once the file is deleted).
func programProcesses(p Paths) []string {
	progs := map[string]string{}
	dirs := []string{p.Bin("")}
	if real, err := filepath.EvalSymlinks(p.Bin("")); err == nil {
		dirs = append(dirs, real)
	}
	for _, d := range dirs {
		for _, n := range atomPrograms {
			progs[filepath.Join(d, n)] = n
		}
	}
	entries, _ := os.ReadDir("/proc")
	var found []string
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		exe, err := os.Readlink(filepath.Join("/proc", e.Name(), "exe"))
		if err != nil {
			continue
		}
		if n, ok := progs[strings.TrimSuffix(exe, " (deleted)")]; ok {
			found = append(found, fmt.Sprintf("%s（进程 %d）", n, pid))
		}
	}
	return found
}
