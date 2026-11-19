package main

// The reset from the panel (「设置 → 系统 → 重置所有设置」): atom goes back to a new installation.
//
// The maintenance service only prepares: it copies the installed programs and the kept copy of the shipped
// package (share/) to a package directory, starts the wizard in reset mode from it and records the wizard's
// port and code in the job, which the panel reads and hands to the browser. Nothing is changed until a browser
// has authorized with that code and asked for the reset; a wizard nobody opens within resetWait exits and the
// job ends as cancelled.
//
// The wizard then holds the panel's writes, copies the data to backup/reset-<time>/ (kept, never pruned), takes
// atom off the machine as `uninstall --purge` does (keeping the package copy, the backups and its own run file)
// and, once it has checked that the old installation is gone, continues as the installation wizard of a new
// machine. A removal that did not complete stays in reset_failed, from which the page tries it again. A wizard that is left alone after the reset installs with the previous
// network (topology, interfaces, panel port, panel login) and the default settings, so the LAN gets its DNS back.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// resetWait is how long the wizard waits for a browser before the reset is cancelled.
const resetWait = 3 * time.Minute

// The phases the reset wizard publishes in installer.json for the maintenance service.
const (
	resetWaiting   = "waiting"   // for a browser with the code; nothing changed
	resetBackingUp = "resetting" // checks and the data backup; nothing changed yet
	resetPurging   = "purging"   // atom is being taken off the machine
	resetFailed    = "failed"    // refused before anything changed; the wizard exits
	// resetPurgeFailed: the data is backed up but atom is not (all) off the machine; the wizard stays, the page
	// offers to try the removal again (POST /api/reset) and the idle timer tries it every resetRetryEvery. No
	// installation is possible until it succeeded.
	resetPurgeFailed = "reset_failed"
	resetReady       = "ready" // atom is verified gone; a new installation follows
)

// resetRetryEvery is how often the wizard tries a removal that did not complete again by itself, so that a
// passing failure does not keep the network down when nobody is at the page; a variable for the tests.
var resetRetryEvery = time.Minute

// resetUninstall takes atom off the machine for the reset; a variable for the tests.
var resetUninstall = uninstallForReset

// keepShipped keeps the shipped configuration, WebUI and manifest of the version being installed in share/;
// a reset installs from them again, together with the programs in bin/.
func (ins *Installer) keepShipped() error {
	p := ins.Paths
	share := p.Shipped()
	if filepath.Clean(ins.PkgDir) == share {
		return nil
	}
	next := share + ".new"
	os.RemoveAll(next)
	err := func() error {
		if err := copyTree(filepath.Join(ins.PkgDir, "config"), filepath.Join(next, "config")); err != nil {
			return err
		}
		if err := copyFileAtomic(filepath.Join(ins.PkgDir, "atom-webui.tar.gz"), filepath.Join(next, "atom-webui.tar.gz"), 0o644); err != nil {
			return err
		}
		m := manifest{Version: ins.Version}
		if data, err := os.ReadFile(filepath.Join(ins.PkgDir, "manifest.json")); err == nil {
			return writeFileAtomic(filepath.Join(next, "manifest.json"), data, 0o644)
		}
		return writeJSON(filepath.Join(next, "manifest.json"), m)
	}()
	if err != nil {
		os.RemoveAll(next)
		return err
	}
	old := share + ".old"
	os.RemoveAll(old)
	if _, err := os.Lstat(share); err == nil {
		if err := os.Rename(share, old); err != nil {
			os.RemoveAll(next)
			return err
		}
	}
	if err := os.Rename(next, share); err != nil {
		os.Rename(old, share)
		return err
	}
	os.RemoveAll(old)
	return nil
}

// prepareResetPackage assembles an unpacked package of the installed version under the root (pkg.reset-*, which
// the wizard removes when it exits): the programs from bin/, the rest from share/.
func prepareResetPackage(p Paths, version string) (string, error) {
	var m manifest
	if err := readJSON(filepath.Join(p.Shipped(), "manifest.json"), &m); err != nil {
		return "", fmt.Errorf("没有找到已安装版本的安装包副本，请先升级一次或重新运行安装命令（%v）", err)
	}
	if m.Version != version {
		return "", fmt.Errorf("安装包副本的版本 %s 与已安装的 %s 不一致", m.Version, version)
	}
	dir, err := os.MkdirTemp(p.Root, "pkg.reset-")
	if err != nil {
		return "", err
	}
	err = func() error {
		if err := copyTree(filepath.Join(p.Shipped(), "config"), filepath.Join(dir, "config")); err != nil {
			return err
		}
		for _, f := range []string{"atom-webui.tar.gz", "manifest.json"} {
			if err := copyFileAtomic(filepath.Join(p.Shipped(), f), filepath.Join(dir, f), 0o644); err != nil {
				return err
			}
		}
		for _, name := range installedBinaries {
			src := p.Bin(name)
			if _, err := os.Stat(src); os.IsNotExist(err) && name == "mihomo" {
				continue
			}
			if err := copyFileAtomic(src, filepath.Join(dir, "bin", name), 0o755); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
		return nil
	}()
	if err != nil {
		os.RemoveAll(dir)
		return "", fmt.Errorf("准备安装包：%w", err)
	}
	return dir, nil
}

// processAlive: pid is a running atom-installer.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	if err := syscall.Kill(pid, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	return err == nil && strings.Contains(string(cmdline), "atom-installer")
}

// runningWizard reads the run file of a wizard whose process is still there.
func runningWizard(p Paths) (listenInfo, bool) {
	var li listenInfo
	if err := readJSON(p.Run("installer.json"), &li); err != nil {
		return li, false
	}
	if p.Container { // the wizard is a part of the entry process
		return li, activeWizard(li)
	}
	return li, processAlive(li.PID)
}

// wizardRunning: a wizard is running (its run file, or on systemd its unit), so another one cannot start.
func wizardRunning(p Paths) bool {
	if _, ok := runningWizard(p); ok {
		return true
	}
	if p.Container {
		return false
	}
	if !p.OpenRC {
		if _, err := quietCmd("systemctl", "is-active", "--quiet", "atom-installer.service"); err == nil {
			return true
		}
	}
	return false
}

// privateLog creates the log the wizard appends to with mode 0600, or narrows an existing one to it.
func privateLog(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Chmod(0o600)
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// launchResetWizard starts the wizard as its own service, as install.sh does, so that it outlives the
// maintenance service it removes.
func launchResetWizard(p Paths, pkg, id string, log Logger) error {
	args := []string{filepath.Join(pkg, "bin", "atom-installer"), "serve", "--pkg", pkg, "--reset", id}
	env := []string{"ATOM_WIZARD_BACKGROUND=1"}
	for _, name := range []string{"ATOM_ROOT", "ATOM_UNIT_DIR"} {
		if v := os.Getenv(name); v != "" {
			env = append(env, name+"="+v)
		}
	}
	if p.OpenRC {
		// The wizard prints its code to this log (for a lost browser tab): root only, created so.
		if err := privateLog(p.Run("installer.log")); err != nil {
			return err
		}
		pidfile := p.Run("installer.pid")
		os.Remove(pidfile)
		var script []string
		for _, e := range env {
			script = append(script, "export "+shellQuote(e)+";")
		}
		var quoted []string
		for _, a := range args {
			quoted = append(quoted, shellQuote(a))
		}
		script = append(script, "exec", strings.Join(quoted, " "), ">>", shellQuote(p.Run("installer.log")), "2>&1")
		return runCmd(log, 30*time.Second, "start-stop-daemon", "--start", "--background", "--make-pidfile",
			"--pidfile", pidfile, "--exec", "/bin/sh", "--", "-c", strings.Join(script, " "))
	}
	quietCmd("systemctl", "reset-failed", "atom-installer.service")
	run := []string{"--quiet", "--unit=atom-installer", "--collect"}
	for _, e := range env {
		run = append(run, "--setenv="+e)
	}
	return runCmd(log, 30*time.Second, "systemd-run", append(run, args...)...)
}

// runReset prepares the reset and hands it to the wizard. operation is released just before the wizard
// starts, which takes it for the rest of the reset.
func (m *maintenance) runReset(st *InstallState, operation *os.File) {
	p := m.paths
	log := stderrLogger{}
	pkg := "" // the container installs from the image: no package to assemble
	if !p.Container {
		var err error
		if pkg, err = prepareResetPackage(p, st.Version); err != nil {
			m.failed(err)
			return
		}
		if err := m.change(func(job *maintenanceJob) { job.Package = pkg }); err != nil {
			os.RemoveAll(pkg)
			m.failed(err)
			return
		}
	}
	id := m.snapshot().ID
	os.Remove(p.Run("installer.json"))
	operation.Close()
	var err error
	if p.Container {
		err = startContainerReset(p, id)
	} else {
		err = launchResetWizard(p, pkg, id, log)
	}
	if err != nil {
		if pkg != "" {
			os.RemoveAll(pkg)
		}
		m.failed(fmt.Errorf("启动安装向导：%w", err))
		return
	}
	var li listenInfo
	started := false
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
		if got, ok := runningWizard(p); ok && got.ResetJob == id && got.Code != "" {
			li, started = got, true
			break
		}
	}
	if !started {
		// A wizard that starts after this checks the job, finds it finished and exits (removing the package).
		m.failed(fmt.Errorf("安装向导没有启动，未做任何更改（详情见 atom-installer 的日志）"))
		return
	}
	m.adoptWizard(st.LAN, li)
	m.watchReset()
}

// adoptWizard records the wizard of the reset job (its port, code and process) for the panel.
func (m *maintenance) adoptWizard(lan []string, li listenInfo) {
	deadline := time.Now().Add(resetWait)
	urls := wizardURLs(li.Port)
	if ips := panelIPs(lan, listInterfaces()); len(ips) > 0 {
		urls = nil
		for _, ip := range ips {
			urls = append(urls, "http://"+net.JoinHostPort(ip, strconv.Itoa(li.Port))+"/")
		}
	}
	if err := m.change(func(job *maintenanceJob) {
		job.Phase, job.Deadline = "awaiting_browser", &deadline
		if li.Reset == resetBackingUp {
			job.Phase = "resetting"
		}
		job.WizardPort, job.WizardCode, job.WizardPID, job.WizardURLs = li.Port, li.Code, li.PID, urls
	}); err != nil {
		stderrLogger{}.Logf("记录重置任务：%v", err)
	}
}

// watchReset follows the wizard through its run file until the reset began, failed, or the wizard exited.
// Once the wizard takes atom off the machine it also stops this service; the record goes with the data.
func (m *maintenance) watchReset() {
	for {
		job := m.snapshot()
		if job.Done {
			return
		}
		li, alive := runningWizard(m.paths)
		if alive && (li.PID != job.WizardPID || (m.paths.Container && li.ResetJob != job.ID)) {
			alive = false
		}
		var err error
		switch {
		case !alive && job.Phase == "resetting":
			err = m.finish("failed", fmt.Errorf("安装向导在开始改动前退出，未做任何更改"))
		case !alive && job.Deadline != nil && time.Now().After(*job.Deadline):
			err = m.finish("cancelled", fmt.Errorf("%d 分钟内没有在浏览器打开安装向导，重置已取消，未做任何更改", int(resetWait/time.Minute)))
		case !alive:
			err = m.finish("cancelled", fmt.Errorf("安装向导已关闭，重置已取消，未做任何更改"))
		case li.Reset == resetBackingUp && job.Phase != "resetting":
			err = m.change(func(job *maintenanceJob) { job.Phase = "resetting" })
		case li.Reset == resetFailed:
			err = m.finish("failed", fmt.Errorf("%s", li.ResetError))
		case li.Reset == resetPurging || li.Reset == resetPurgeFailed || li.Reset == resetReady:
			err = m.finish("reset", nil)
		}
		if err != nil {
			stderrLogger{}.Logf("记录重置任务：%v", err)
		}
		if !alive && job.Package != "" && m.paths.managedPackage(job.Package) {
			os.RemoveAll(job.Package) // a wizard that was killed did not remove it
		}
		if m.snapshot().Done {
			m.mu.Lock()
			if m.job.WizardCode != "" { // the code is of no use to the panel any more
				job := *m.job
				job.WizardCode = ""
				m.save(job)
			}
			m.mu.Unlock()
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// recoverWizardWait is how long a restarted maintenance service waits for a wizard that was launched for the
// job but has not written its run file yet; a variable for the tests.
var recoverWizardWait = 20 * time.Second

// recoverReset picks up an unfinished reset after this service restarted: a wizard that still waits or works is
// followed again; otherwise the reset did not happen. The run file decides, not the job record: the service may
// have stopped after it launched the wizard and before it recorded the wizard's process (phase "preparing"), and
// such a wizard runs from the job's package, which must then not be removed.
func (m *maintenance) recoverReset() error {
	job := *m.job
	p := m.paths
	ours := func(li listenInfo) bool { return li.ResetJob == job.ID && li.Code != "" }
	li, ok := runningWizard(p)
	if ok && job.WizardPID != 0 && li.PID == job.WizardPID {
		go m.watchReset()
		return nil
	}
	// A wizard process without a run file may be this job's, still starting; a live wizard with another run file
	// is not (the job was only created while no wizard ran), and this job's wizard cannot start beside it.
	starting := func() bool { return !ok && wizardStarting(p) }
	if job.WizardPID == 0 && job.Phase == "preparing" && starting() {
		for deadline := time.Now().Add(recoverWizardWait); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
			if li, ok = runningWizard(p); ok || !wizardStarting(p) {
				break
			}
		}
	}
	if ok && ours(li) && job.WizardPID == 0 {
		var lan []string
		if st, err := loadState(p); err == nil {
			lan = st.LAN
		}
		m.adoptWizard(lan, li)
		go m.watchReset()
		return nil
	}
	if job.Phase == "preparing" && job.Package != "" && p.managedPackage(job.Package) && !(ok && ours(li)) && !starting() {
		os.RemoveAll(job.Package) // no wizard got it
	}
	return m.finish("cancelled", fmt.Errorf("维护服务中断，重置没有开始，未做任何更改"))
}

// wizardStarting: a wizard process exists, which may not have written its run file yet (its systemd unit is
// active, or on OpenRC the process in its pid file is there).
func wizardStarting(p Paths) bool {
	if p.OpenRC {
		data, err := os.ReadFile(p.Run("installer.pid"))
		if err != nil {
			return false
		}
		pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
		return processAlive(pid)
	}
	if testWizardStarting != nil {
		return testWizardStarting()
	}
	_, err := quietCmd("systemctl", "is-active", "--quiet", "atom-installer.service")
	return err == nil
}

// testWizardStarting replaces the systemd check in the tests.
var testWizardStarting func() bool

// resetOperationLock is the wizard's lock in reset mode: the operation lock that the maintenance service released
// just before, for the reset job that started this wizard and is still open.
func resetOperationLock(p Paths, id string) (*os.File, error) {
	var lock *os.File
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		var err error
		if lock, err = operationLock(p); err == nil {
			break
		}
		if !errors.Is(err, errMaintenanceBusy) || time.Now().After(deadline) {
			return nil, err
		}
	}
	var job maintenanceJob
	if err := readJSON(p.Run(maintenanceRecord), &job); err != nil || job.ID != id || job.Kind != "reset" || job.Done {
		lock.Close()
		return nil, fmt.Errorf("没有等待中的重置任务")
	}
	return lock, nil
}

// resetRun is the reset a wizard in reset mode does (Server.reset).
type resetRun struct {
	jobID    string
	deadline time.Time // a browser must ask for the reset before this
	phase    string
	err      string
	previous Config    // the installed network, shown in the wizard and used by the default installation
	panel    int       // the panel to go back to while nothing changed
	steps    *Job      // the backup and the removal, shown while they run
	backup   string    // the copy of the data
	plan     resetPlan // the default installation
	purgedAt time.Time
	failedAt time.Time // the last removal that did not complete

	listen  listenInfo // published in runFile for the maintenance service
	runFile string
	// writes holds the panel's write lock (maintenance-writes.lock) from before the backup until the old
	// installation is gone, so the backup is what the reset takes off the machine.
	writes *os.File
}

var resetSteps = []string{"备份数据", "移除旧的安装"}

func newResetRun(p Paths, id string, li listenInfo, runFile string) (*resetRun, error) {
	st, err := loadState(p)
	if err != nil || !st.Complete {
		return nil, fmt.Errorf("没有已完成的安装")
	}
	li.ResetJob, li.Reset = id, resetWaiting
	return &resetRun{jobID: id, deadline: time.Now().Add(resetWait), phase: resetWaiting, previous: configFromState(st),
		panel: st.PanelPort, steps: newJob(resetSteps), listen: li, runFile: runFile}, nil
}

// resetView is the reset in the wizard's state.
type resetView struct {
	Phase    string    `json:"phase"`
	Error    string    `json:"error,omitempty"`
	Deadline time.Time `json:"deadline"`
	// PanelPort: the panel, which keeps running while nothing has been changed.
	PanelPort int     `json:"panel_port"`
	Previous  Config  `json:"previous"`
	Backup    string  `json:"backup,omitempty"`
	Job       JobView `json:"job"`
	// AutoInstall is when the wizard installs with the previous network and default settings by itself.
	AutoInstall *time.Time `json:"auto_install,omitempty"`
}

// resetState is the view for the wizard page; with s.mu held.
func (s *Server) resetState() *resetView {
	r := s.reset
	if r == nil {
		return nil
	}
	v := &resetView{Phase: r.phase, Error: r.err, Deadline: r.deadline, PanelPort: r.panel, Previous: r.previous,
		Backup: r.backup, Job: r.steps.view(0)}
	if r.phase == resetReady {
		if at, ok := s.autoInstallAt(); ok {
			v.AutoInstall = &at
		}
	}
	return v
}

// autoInstallAt is when the default installation starts: idleLimit after the reset or after the last failed
// installation, while none runs or succeeded. With s.mu held.
func (s *Server) autoInstallAt() (time.Time, bool) {
	from := s.reset.purgedAt
	if s.job != nil {
		v := s.job.view(0)
		if !v.Done || v.OK {
			return time.Time{}, false
		}
		from = s.job.finishedAt()
	}
	return from.Add(idleLimit), true
}

// publishReset writes the phase to the run file, where the maintenance service reads it.
func (s *Server) publishReset() {
	s.mu.Lock()
	li := s.reset.listen
	li.Reset, li.ResetError = s.reset.phase, s.reset.err
	runFile := s.reset.runFile
	s.mu.Unlock()
	// the run file holds the code: root only in every write, as when the wizard started
	if err := writePrivateJSON(runFile, li); err != nil {
		stderrLogger{}.Logf("记录重置状态：%v", err)
	}
}

func (s *Server) setResetPhase(phase, msg string) {
	s.mu.Lock()
	s.reset.phase, s.reset.err = phase, msg
	switch phase {
	case resetReady:
		s.reset.purgedAt = time.Now()
	case resetPurgeFailed:
		s.reset.failedAt = time.Now()
	}
	s.mu.Unlock()
	s.publishReset()
}

// retryPurge tries the removal again from resetPurgeFailed (the data is backed up already: only the removal
// runs); false when the reset is not there. With s.opMu held.
func (s *Server) retryPurge() bool {
	s.mu.Lock()
	retry := s.reset.phase == resetPurgeFailed && !s.closing
	if retry {
		s.reset.phase, s.reset.err = resetPurging, ""
	}
	s.mu.Unlock()
	if retry {
		s.publishReset()
		go s.purgeForReset()
	}
	return retry
}

// handleReset starts the reset: the browser has the code, so this is the user who confirmed it in the panel.
func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	if s.reset == nil {
		writeErr(w, 404, "不是重置")
		return
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if s.retryPurge() {
		s.mu.Lock()
		v := s.resetState()
		s.mu.Unlock()
		writeResp(w, 202, v)
		return
	}
	s.mu.Lock()
	phase, closing, late := s.reset.phase, s.closing, time.Now().After(s.reset.deadline)
	if phase == resetWaiting && !closing && !late {
		s.reset.phase = resetBackingUp
	}
	s.mu.Unlock()
	switch {
	case phase != resetWaiting:
		s.mu.Lock()
		v := s.resetState()
		s.mu.Unlock()
		writeResp(w, 200, v)
		return
	case closing || late:
		writeErr(w, 409, "重置已取消，未做任何更改")
		return
	}
	s.publishReset()
	go s.runReset()
	s.mu.Lock()
	v := s.resetState()
	s.mu.Unlock()
	writeResp(w, 202, v)
}

// runReset backs the data up and takes atom off the machine. A failure before anything changed ends the wizard
// (the panel keeps running). A removal that did not complete leaves the wizard in resetPurgeFailed, from which
// the page tries it again; only a verified removal leads to a new installation.
func (s *Server) runReset() {
	p := s.ins.Paths
	j := s.reset.steps
	refuse := func(err error) {
		j.Logf("重置没有开始：%v", err)
		j.finish(err, 0)
		s.setResetPhase(resetFailed, err.Error()+"。未做任何更改")
		// let the page show it, then exit: the maintenance service has read it by then
		time.AfterFunc(10*time.Second, func() {
			s.mu.Lock()
			s.closing = true
			s.mu.Unlock()
			s.stop(false)
		})
	}
	j.setStep(0, "running")
	if _, err := loadChanges(p); err != nil {
		j.setStep(0, "fail")
		refuse(err)
		return
	}
	// The panel's writes are drained and held off as for an upgrade (maintenance.holdWrites), until the old
	// installation is gone: a change made after the backup would be lost with the data.
	writes, err := holdPanelWrites(p, 30*time.Second)
	if err != nil {
		j.setStep(0, "fail")
		refuse(fmt.Errorf("暂停面板的写入：%w", err))
		return
	}
	backup, err := backupForReset(p, j)
	if err == nil {
		var plan resetPlan
		if plan, err = planReset(p, s.reset.previous, backup); err == nil {
			s.mu.Lock()
			s.reset.backup, s.reset.plan, s.reset.writes = backup, plan, writes
			s.mu.Unlock()
		}
	} else {
		err = fmt.Errorf("备份数据：%w", err)
	}
	if err != nil {
		writes.Close()
		j.setStep(0, "fail")
		refuse(err)
		return
	}
	j.setStep(0, "ok")
	s.purgeForReset()
}

// holdPanelWrites takes the panel's write lock exclusively, as maintenance.holdWrites does: writes in progress
// finish first, later ones are refused while it is held.
func holdPanelWrites(p Paths, wait time.Duration) (*os.File, error) {
	for deadline := time.Now().Add(wait); ; time.Sleep(100 * time.Millisecond) {
		lock, err := exclusiveLock(p.Run(panelWriteLock), true)
		if !errors.Is(err, errMaintenanceBusy) || time.Now().After(deadline) {
			return lock, err
		}
	}
}

// purgeForReset takes atom off the machine (after the backup, and again for each retry from the page) and
// checks that it is gone before the wizard offers a new installation.
func (s *Server) purgeForReset() {
	p := s.ins.Paths
	j := s.reset.steps
	s.mu.Lock()
	backup := s.reset.backup
	s.mu.Unlock()
	j.setStep(1, "running")
	s.setResetPhase(resetPurging, "")
	// The maintenance service records the reset before it is removed with the rest.
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
		var job maintenanceJob
		if readJSON(p.Run(maintenanceRecord), &job) != nil || job.ID != s.reset.jobID || job.Done {
			break
		}
	}
	warning, err := resetUninstall(p, []string{filepath.Base(s.ins.PkgDir), "backup"}, j)
	if err == nil {
		err = verifyPurged(p)
	}
	if err != nil {
		j.Logf("移除旧的安装没有完成：%v", err)
		j.setStep(1, "fail")
		s.setResetPhase(resetPurgeFailed, fmt.Sprintf("移除旧的安装没有完成：%v。数据已备份在 %s；处理好之后可以重试", err, backup))
		return
	}
	if warning != "" {
		j.warn("%s", warning)
	}
	j.setStep(1, "ok")
	j.finish(nil, 0)
	j.Logf("已重置，数据备份在 %s", backup)
	s.mu.Lock()
	if s.reset.writes != nil {
		s.reset.writes.Close() // the panel is gone with the rest
		s.reset.writes = nil
	}
	s.mu.Unlock()
	s.setResetPhase(resetReady, "")
}

// verifyPurged checks that nothing of the old installation that a new one could take over is left: the data
// (and its marker), the install record, the programs and the service definitions, and that none of its
// services or programs still runs.
func verifyPurged(p Paths) error {
	if p.Container {
		return verifyPurgedContainer(p)
	}
	left := []string{p.Data(), dataMarker(p), p.State(), filepath.Join(p.Root, "bin"), p.Shipped(), p.Lib("")}
	if p.UnitDir != "" {
		left = append(left, p.unitFile(maintenanceUnitName), p.DNSUnit(), p.SingBoxUnit(), p.MihomoUnit())
	}
	var found []string
	for _, f := range left {
		if _, err := os.Lstat(f); err == nil || !os.IsNotExist(err) {
			found = append(found, f)
		}
	}
	if len(found) > 0 {
		return fmt.Errorf("仍然存在：%s", strings.Join(found, "、"))
	}
	// an old service still running would keep port 53 from the new installation; the check is bounded too
	ctx, cancel := context.WithTimeout(context.Background(), serviceStopWait)
	defer cancel()
	return checkStopped(ctx, p)
}

// backupForReset copies the data and the install records to backup/reset-<time>/. The name is not one that
// pruneBackups knows: the copy stays until the user removes it (or uninstalls).
func backupForReset(p Paths, log Logger) (string, error) {
	copyData := func(dir string) error { return copyTree(p.Data(), dir) }
	if p.Container { // the backups are inside the data directory: not a part of what is copied
		copyData = func(dir string) error { return copyTreeExcept(p.Data(), dir, keptByReset(p)) }
	}
	dir := filepath.Join(p.Backup(), "reset-"+time.Now().Format(backupTimeFormat))
	if _, err := os.Stat(dir); err == nil {
		dir += "-" + strconv.FormatInt(time.Now().UnixNano()%1e9, 10)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	_, statErr := os.Stat(p.Data())
	if p.Container && statErr != nil && !os.IsNotExist(statErr) {
		return "", statErr // the data cannot be read: it is not deleted either
	}
	if statErr == nil {
		if err := copyData(filepath.Join(dir, "data")); err != nil {
			return "", err
		}
	}
	for _, f := range []string{p.State(), p.Changes()} {
		if _, err := os.Stat(f); err == nil {
			if err := copyFileAtomic(f, filepath.Join(dir, "etc", filepath.Base(f)), 0o600); err != nil {
				return "", err
			}
		}
	}
	if err := syncBackup(dir); err != nil {
		return "", err
	}
	log.Logf("数据 → %s", dir)
	return dir, nil
}

// resetPlan is the installation the wizard does by itself when nobody installs after the reset: the previous
// network (topology, interfaces, routes, panel port) and panel login, everything else default.
type resetPlan struct {
	cfg Config
	// freeResolved, freeUnits: what the previous installation had freed port 53 from, freed again.
	freeResolved bool
	freeUnits    []string
}

func planReset(p Paths, previous Config, backup string) (resetPlan, error) {
	ch, err := loadChanges(p)
	if err != nil {
		return resetPlan{}, err
	}
	cfg := previous
	cfg.LAN = slices.Clone(previous.LAN)
	cfg.Backend = "sing-box"
	cfg.MixedPort = defaultMixed
	cfg.fillDefaults()
	if auth := filepath.Join(backup, "data", "webinfo", "atom_auth.json"); fileExists(auth) {
		cfg.panelAuth = auth
	}
	plan := resetPlan{cfg: cfg, freeResolved: ch.ResolvedDropIn}
	for _, u := range ch.DisabledUnits {
		plan.freeUnits = append(plan.freeUnits, u.Unit)
	}
	return plan, nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// freeForDefault frees port 53 again from what the previous installation had freed it from; anything else on
// it stops the default installation.
func freeForDefault(p Paths, plan resetPlan, log Logger) error {
	if p.Container { // the container has no one else on port 53
		return nil
	}
	occ, err := portOccupants(53)
	if err != nil {
		return err
	}
	if len(occ) == 0 {
		return nil
	}
	for _, o := range occ {
		switch {
		case o.Kind == "resolved" && plan.freeResolved:
			err = freeResolvedStub(p, log)
		case o.Kind == "unit" && slices.Contains(plan.freeUnits, o.Unit):
			err = freeUnit(p, o.Unit, log)
		case o.Kind == "openrc" && slices.Contains(plan.freeUnits, o.Unit):
			err = freeOpenRCService(p, o.Unit, log)
		default:
			err = fmt.Errorf("53 端口被 %s 占用", o.Proc)
		}
		if err != nil {
			return err
		}
	}
	time.Sleep(time.Second) // let the listeners go away before the next scan
	if occ, err := portOccupants(53); err != nil || len(occ) > 0 {
		return fmt.Errorf("53 端口仍被占用")
	}
	return nil
}

// resetIdle decides for the idle timer while the wizard resets: it keeps the wizard open (tries a removal that
// did not complete again, and starts the default installation when it is due) until an installation succeeded; a reset nobody asked for in time ends
// the wizard (closed). hold false: the normal rules apply.
func (s *Server) resetIdle() (closed, hold bool) {
	s.mu.Lock()
	phase, deadline, failedAt := s.reset.phase, s.reset.deadline, s.reset.failedAt
	installed := s.job != nil && s.job.view(0).Done && s.job.view(0).OK
	at, due := time.Time{}, false
	if phase == resetReady {
		at, due = s.autoInstallAt()
	}
	s.mu.Unlock()
	switch phase {
	case resetWaiting:
		if time.Now().After(deadline) && s.requestClose() {
			stderrLogger{}.Logf("%d 分钟内没有浏览器打开安装向导，重置已取消", int(resetWait/time.Minute))
			s.stop(false)
			return true, true
		}
	case resetPurgeFailed:
		if time.Since(failedAt) >= resetRetryEvery {
			s.opMu.Lock()
			if s.retryPurge() {
				s.reset.steps.Logf("自动重试移除旧的安装")
			}
			s.opMu.Unlock()
		}
	case resetReady:
		if installed {
			return false, false
		}
		if due && !time.Now().Before(at) {
			s.startDefaultInstall()
		}
	}
	return false, true
}

// startDefaultInstall installs with the previous network and the default settings (resetPlan).
func (s *Server) startDefaultInstall() {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if s.busyOrClosing() {
		return
	}
	s.mu.Lock()
	plan := s.reset.plan
	job := newJob(s.ins.stepNames())
	s.job = job
	s.mu.Unlock()
	go func() {
		job.Logf("重置后 %d 分钟内没有完成安装：按原来的网络设置和默认配置安装", int(idleLimit/time.Minute))
		cfg := plan.cfg
		cfg.LAN = slices.Clone(plan.cfg.LAN)
		// A reset installs the default settings, never the old ones; this runs only in resetReady, which
		// purgeForReset reaches once the old installation was verified gone.
		cfg.keepData = false
		if sys, _ := s.system(true); len(sys.Problems) > 0 {
			job.finish(fmt.Errorf("%s", strings.Join(sys.Problems, "；")), 0)
			return
		}
		if err := freeForDefault(s.ins.Paths, plan, job); err != nil {
			job.Logf("安装失败：%v", err)
			job.finish(err, 0)
			return
		}
		if errs := s.validateConfig(&cfg); len(errs) > 0 {
			err := fmt.Errorf("原来的网络设置不能用（%s）：%s", errs[0].Field, errs[0].Msg)
			job.Logf("安装失败：%v", err)
			job.finish(err, 0)
			return
		}
		s.ins.runInstall(cfg, job)
	}()
}
