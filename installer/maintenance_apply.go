package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
)

func (m *maintenance) runUpdate(st *InstallState, job maintenanceJob, operation *os.File) {
	if err := m.update(st, job, operation); err != nil {
		current := m.snapshot()
		if current.Blocking {
			m.rollback(current, err)
		} else {
			m.failed(err)
		}
	}
}

func (m *maintenance) update(st *InstallState, job maintenanceJob, operation *os.File) error {
	p := m.paths
	log := stderrLogger{}
	settings, _ := installedProxySettings(p, st.PanelPort)
	sources := updateSources(settings.MixedPort)
	route := "latest/download/"
	if job.Kind == "upgrade" {
		route = "download/v" + job.Version + "/"
	}
	release, err := fetchUpdateRelease(releaseBase+route+updateManifestName, job.Version, sources, log, m.currentSource)
	if err != nil {
		return err
	}
	osRelease, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return err
	}
	platform, err := updatePlatform(runtime.GOARCH, parseOSRelease(string(osRelease))["ID"])
	if err != nil {
		return err
	}
	if job.Kind == "check" {
		if err := m.change(func(job *maintenanceJob) {
			if compareVersions(release.Version, st.Version) > 0 {
				job.Version = release.Version
			}
		}); err != nil {
			return err
		}
		return m.finish("success", nil)
	}

	stage, err := os.MkdirTemp(p.Run(""), "update-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	asset := release.Assets[platform]
	archive := filepath.Join(stage, asset.Name)
	if err := m.change(func(job *maintenanceJob) { job.Phase = "downloading" }); err != nil {
		return err
	}
	if err := fetchUpdatePackage(releaseBase+route+asset.Name, archive, asset, sources, log, m.currentSource); err != nil {
		return err
	}
	if err := m.change(func(job *maintenanceJob) { job.Phase = "validating" }); err != nil {
		return err
	}
	if err := extractTarGz(archive, stage); err != nil {
		return err
	}
	pkg := filepath.Join(stage, "atom-"+release.Version)
	var manifest struct {
		Version string `json:"version"`
		Arch    string `json:"arch"`
		Libc    string `json:"libc"`
	}
	if err := readJSON(filepath.Join(pkg, "manifest.json"), &manifest); err != nil {
		return err
	}
	expectedLibc := "glibc"
	if platform != runtime.GOARCH {
		expectedLibc = "musl"
	}
	if manifest.Version != release.Version || manifest.Arch != runtime.GOARCH || manifest.Libc != expectedLibc {
		return fmt.Errorf("升级包内部的版本或平台与发布清单不符")
	}
	ins := &Installer{Paths: p, Version: release.Version, PkgDir: pkg}
	if _, err := loadChanges(p); err != nil {
		return err
	}

	// Drain panel writes before the candidate checks the data it will actually
	// start with. A crash after the backup is committed is handled by recover.
	writes, err := m.holdWrites()
	if err != nil {
		return err
	}
	defer writes.Close()
	if err := checkLegacyTor(p); err != nil {
		return err
	}
	if err := ins.checkUpgradeConfig(st.PanelPort, log); err != nil {
		return err
	}
	// Check the nested WebUI archive before activation as well.
	if err := installWebUI(filepath.Join(pkg, "atom-webui.tar.gz"), filepath.Join(stage, "ui-check")); err != nil {
		return err
	}
	backup := backupPath(p, st.Version)
	if err := m.begin(ins, backup, nil); err != nil {
		return err
	}
	if err := applyUpdatePackage(p, pkg, operation); err != nil {
		m.rollback(m.snapshot(), err)
		return nil
	}
	if err := runCmd(log, 30*time.Second, "sync"); err != nil {
		m.rollback(m.snapshot(), err)
		return nil
	}
	st.Version, st.UpdatedAt = ins.Version, time.Now()
	if err := writeJSON(p.State(), st); err != nil {
		m.rollback(m.snapshot(), err)
		return nil
	}
	m.mu.Lock()
	m.reloading = true
	m.mu.Unlock()
	if err := m.finish("success", nil); err != nil {
		m.mu.Lock()
		m.reloading = false
		m.mu.Unlock()
		m.rollback(m.snapshot(), err)
		return nil
	}
	pruneBackups(p.Backup(), 3)
	// Drop this work directory before replacing the current process.
	os.RemoveAll(stage)
	m.reload <- struct{}{}
	return nil
}

// Use the new installer's templates and apply logic. The inherited file keeps
// competing operations out even if the supervising maintenance process dies.
func applyUpdatePackage(p Paths, pkg string, operation *os.File) error {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(pkg, "bin", "atom-installer"), "apply-panel-upgrade", pkg)
	cmd.Env = append(os.Environ(), "ATOM_ROOT="+p.Root, "ATOM_UNIT_DIR="+p.UnitDir)
	cmd.ExtraFiles = []*os.File{operation}
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("候选 Atom 安装或自检失败：%w（详情见维护服务日志）", err)
	}
	return nil
}

func cmdApplyPanelUpgrade(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("没有候选安装包")
	}
	p := defaultPaths()
	var job maintenanceJob
	if err := readJSON(p.Run(maintenanceRecord), &job); err != nil || job.Kind != "upgrade" || job.Phase != "applying" {
		return fmt.Errorf("没有进行中的升级事务")
	}
	lock := os.NewFile(3, "maintenance operation")
	if lock == nil {
		return fmt.Errorf("没有维护操作锁")
	}
	defer lock.Close()
	inherited, err := lock.Stat()
	if err != nil {
		return err
	}
	onDisk, err := os.Stat(p.Root + ".lock")
	if err != nil || !os.SameFile(inherited, onDisk) {
		return fmt.Errorf("无效的维护操作锁")
	}
	st, err := loadState(p)
	if err != nil {
		return err
	}
	ins, err := newInstaller(args[0])
	if err != nil {
		return err
	}
	if ins.Version != job.Version || compareVersions(ins.Version, st.Version) <= 0 {
		return fmt.Errorf("候选包和升级事务不匹配")
	}
	log := stderrLogger{}
	if err := ins.applyUpgrade(st, fileSHA256(p.Bin("sing-box")), log); err != nil {
		return err
	}
	if err := p.writeMaintenanceService(log); err != nil {
		return err
	}
	if err := maintenanceHealthy(p, st.PanelPort, log); err != nil {
		return err
	}
	return panelReady(p, st.PanelPort, true)
}

func panelReady(p Paths, port int, requireUI bool) error {
	api := newAPIClient(port, p.Data())
	api.http.Timeout = 4 * time.Second
	api.base = strings.TrimSuffix(api.base, "/atom")
	var health struct {
		Ready bool `json:"ready"`
		UI    []struct {
			Name    string `json:"name"`
			Mounted bool   `json:"mounted"`
		} `json:"external_ui"`
	}
	if err := api.callOnce("GET", "/system/health", nil, &health); err != nil {
		return err
	}
	if !health.Ready {
		return fmt.Errorf("DNS 服务未就绪")
	}
	if !requireUI {
		return nil
	}
	for _, entry := range health.UI {
		if entry.Name == "main" && entry.Mounted {
			return nil
		}
	}
	return fmt.Errorf("候选版本的面板未挂载")
}

// requireServicesActive checks atom-dns and the selected proxy backend; the two backends share their
// ports, so only one of them runs at a time.
func requireServicesActive(init initSystem, p Paths) error {
	_, proxyUnit, _ := p.proxyService()
	for _, name := range []string{dnsUnitName, proxyUnit} {
		if out := init.IsActive(name); out != "active" {
			return fmt.Errorf("%s 不是 active（%s）", name, out)
		}
	}
	return nil
}

func maintenanceHealthy(p Paths, port int, log Logger) error {
	api := newAPIClient(port, p.Data())
	api.http.Timeout = 4 * time.Second
	if _, err := api.waitStatus(45*time.Second, func(s *apiStatus) bool {
		return s.SingBox.Running && s.SingBox.InSync
	}); err != nil {
		return fmt.Errorf("DNS 管理接口未就绪或代理配置未同步：%w", err)
	}
	if err := requireServicesActive(p.Init(), p); err != nil {
		return err
	}
	ls, err := listeners()
	if err != nil {
		return err
	}
	var tcp, udp bool
	for _, listener := range ls {
		if listener.Port == 53 && listener.Proc == "atomdns" {
			tcp = tcp || listener.Proto == "tcp"
			udp = udp || listener.Proto == "udp"
		}
	}
	if !tcp || !udp {
		return fmt.Errorf("DNS 未同时监听 TCP 和 UDP 53 端口")
	}
	if err := panelReady(p, port, false); err != nil {
		return err
	}
	log.Logf("DNS 已就绪，代理内核配置已同步")
	return nil
}

func syncBackup(backup string) error {
	var dirs []string
	err := filepath.WalkDir(backup, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			dirs = append(dirs, path)
			return nil
		}
		return syncDir(path)
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := syncDir(dirs[i]); err != nil {
			return err
		}
	}
	if err := syncDir(filepath.Dir(backup)); err != nil {
		return err
	}
	return syncDir(filepath.Dir(filepath.Dir(backup)))
}

func (ins *Installer) restoreMaintenanceBackup(backup string, network bool, log Logger) error {
	var st InstallState
	if err := readJSON(filepath.Join(backup, "etc", "install.json"), &st); err != nil {
		return err
	}
	p := ins.Paths
	for _, name := range []string{dnsUnitName, singBoxUnitName, mihomoUnitName} {
		if p.Init().IsActive(name) == "active" {
			if err := p.Init().Stop(log, name); err != nil {
				return err
			}
		}
	}
	var trial, old Changes
	if network {
		if err := readJSON(filepath.Join(backup, "etc", "changes.json"), &old); err != nil {
			return err
		}
		trialPath := filepath.Join(backup, "trial-changes.json")
		if err := readJSON(trialPath, &trial); os.IsNotExist(err) {
			next, err := loadChanges(p)
			if err != nil {
				return err
			}
			trial = *next
			// Keep the trial's ownership records for recovery retries even
			// after restoreBackup puts the old changes.json back.
			if err := writeJSON(trialPath, trial); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		rc := reconfigureRun{oldSt: st, oldCh: old}
		undo := trial
		rc.undoAdditions(&undo, log)
	}
	if err := ins.restoreBackup(backup, st.PanelPort, log); err != nil {
		return err
	}
	if err := maintenanceHealthy(p, st.PanelPort, log); err != nil {
		return err
	}
	if network {
		if err := checkRestoredNetwork(st, old, trial); err != nil {
			return err
		}
	}
	return runCmd(log, 30*time.Second, "sync")
}

func checkRestoredNetwork(st InstallState, old, trial Changes) error {
	if st.Topology != "gateway" && trial.IPForwardBefore != "" && old.IPForwardBefore == "" {
		for _, table := range [][]string{{"inet", "atom_wan"}, {"ip", "atom_nat"}} {
			if _, err := quietCmd("nft", "list", "table", table[0], table[1]); err == nil {
				return fmt.Errorf("试用防火墙表 %s 未移除", table[1])
			}
		}
		forward, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward")
		if err != nil || strings.TrimSpace(string(forward)) != trial.IPForwardBefore {
			return fmt.Errorf("ip_forward 没有恢复到试用前的值")
		}
	}
	if st.ReplyVia == "" && trial.Reply {
		rules, err := quietCmd("ip", "rule", "show")
		if err != nil || strings.Contains(rules, "fwmark "+replyMark+" lookup "+replyTable) {
			return fmt.Errorf("试用的回包策略未移除")
		}
	}
	for _, r := range trial.Routes {
		if slices.Contains(old.Routes, r) {
			continue
		}
		out, err := quietCmd("ip", "-4", "route", "show", "exact", r.Net)
		if err != nil || mine([]ClientRoute{r}, r.Net, out) {
			return fmt.Errorf("试用路由 %s 未移除", r.Net)
		}
	}
	return nil
}
