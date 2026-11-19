package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// shippedPackage writes a minimal unpacked package of version.
func shippedPackage(t *testing.T, version string) string {
	t.Helper()
	pkg := t.TempDir()
	for path, content := range map[string]string{
		"config/config_custom.yaml":         "api:\n  http: \"0.0.0.0:80\"\n",
		"config/sub_config/atom_rules.yaml": "rules: []\n",
		"atom-webui.tar.gz":                 "webui",
		"manifest.json":                     `{"version":"` + version + `","arch":"amd64"}`,
	} {
		if err := writeFileAtomic(filepath.Join(pkg, path), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return pkg
}

func TestResetPackageComesFromTheInstalledVersion(t *testing.T) {
	p := Paths{Root: filepath.Join(t.TempDir(), "atom")}
	if _, err := prepareResetPackage(p, "1.2.3"); err == nil || !strings.Contains(err.Error(), "安装包副本") {
		t.Fatalf("without share/ the reset cannot start: %v", err)
	}
	ins := &Installer{Paths: p, PkgDir: shippedPackage(t, "1.2.3"), Version: "1.2.3"}
	if err := ins.keepShipped(); err != nil {
		t.Fatal(err)
	}
	// a second copy replaces the first (an upgrade)
	if err := ins.keepShipped(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"atom-installer", "atomdns", "sing-box"} {
		if err := writeFileAtomic(p.Bin(name), []byte(name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := prepareResetPackage(p, "1.2.4"); err == nil {
		t.Fatal("a copy of another version must not be used")
	}
	pkg, err := prepareResetPackage(p, "1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	if !p.managedPackage(pkg) || !strings.HasPrefix(filepath.Base(pkg), "pkg.reset-") {
		t.Fatalf("the wizard removes only packages it manages: %s", pkg)
	}
	for path, want := range map[string]string{
		"bin/atomdns": "atomdns", "bin/atom-installer": "atom-installer", "atom-webui.tar.gz": "webui",
		"config/sub_config/atom_rules.yaml": "rules: []\n",
	} {
		if got, err := os.ReadFile(filepath.Join(pkg, path)); err != nil || string(got) != want {
			t.Errorf("%s: %q %v", path, got, err)
		}
	}
	if _, err := os.Stat(filepath.Join(pkg, "bin", "mihomo")); !os.IsNotExist(err) {
		t.Errorf("mihomo is optional: %v", err)
	}
	reloaded, err := newInstaller(pkg)
	if err != nil || reloaded.Version != "1.2.3" {
		t.Fatalf("the package names its version: %+v %v", reloaded, err)
	}
	if !slices.ContainsFunc((&Installer{Paths: p}).backupItems(), func(it backupItem) bool { return it.path == p.Shipped() && it.exact }) {
		t.Error("a restored backup must bring back the copy of its own version")
	}
}

func TestUninstallForResetKeepsThePackageBackupsAndWizard(t *testing.T) {
	p := Paths{Root: t.TempDir(), UnitDir: t.TempDir()}
	fakeSystemctl(t, "")
	if err := writeJSON(p.State(), &InstallState{Version: "1.2.3", Complete: true}); err != nil {
		t.Fatal(err)
	}
	// a service that cannot be started again stays recorded, and so do the packages atom brought
	if err := saveChanges(p, &Changes{PkgInstalled: []string{"nftables"},
		DisabledUnits: []UnitState{{Unit: "x-atom-test.service", WasActive: true}}}); err != nil {
		t.Fatal(err)
	}
	files := []string{"bin/atomdns", "lib/policy-route.sh", "share/manifest.json", "data/webinfo/atom_proxy.json",
		"run/maintenance-job.json", "run/installer.json", "pkg.reset-1/bin/atomdns", "backup/reset-1/data/x",
		"backup/1.2.2-20260101-000000/bin/atomdns"}
	for _, f := range files {
		if err := writeFileAtomic(filepath.Join(p.Root, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	warning, err := uninstallForReset(p, []string{"pkg.reset-1", "backup"}, &recLogger{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(warning, "x-atom-test.service") {
		t.Errorf("what could not be put back is reported: %q", warning)
	}
	for _, f := range files {
		_, err := os.Stat(filepath.Join(p.Root, f))
		kept := strings.HasPrefix(f, "pkg.reset-1/") || strings.HasPrefix(f, "backup/") || f == "run/installer.json"
		if kept != (err == nil) {
			t.Errorf("%s: kept %v, stat %v", f, kept, err)
		}
	}
	if _, err := os.Stat(p.State()); !os.IsNotExist(err) {
		t.Errorf("install.json goes: the wizard installs a new machine: %v", err)
	}
	ch, err := loadChanges(p)
	if err != nil || !slices.Equal(ch.PkgInstalled, []string{"nftables"}) || len(ch.DisabledUnits) != 1 {
		t.Fatalf("the next installation takes over what was not put back: %+v %v", ch, err)
	}
}

func TestResetBackupIsNeverPruned(t *testing.T) {
	p := Paths{Root: t.TempDir()}
	writeJSON(p.State(), &InstallState{Version: "1.2.3"})
	writeFileAtomic(filepath.Join(p.Data(), "webinfo", "atom_auth.json"), []byte(`{"username":"a"}`), 0o600)
	backup, err := backupForReset(p, &recLogger{})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"data/webinfo/atom_auth.json", "etc/install.json"} {
		if _, err := os.Stat(filepath.Join(backup, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	if info, err := os.Stat(filepath.Join(backup, "data", "webinfo", "atom_auth.json")); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the login stays private: %v %v", info, err)
	}
	pruneBackups(p.Backup(), 0)
	if _, err := os.Stat(backup); err != nil {
		t.Fatalf("the reset backup is the only copy of the old data: %v", err)
	}

	previous := Config{Topology: "gateway", LAN: []string{"eth1"}, WAN: "eth0", AcceptGateway: true, PanelPort: 8080}
	saveChanges(p, &Changes{ResolvedDropIn: true, DisabledUnits: []UnitState{{Unit: "dnsmasq.service"}}})
	plan, err := planReset(p, previous, backup)
	if err != nil {
		t.Fatal(err)
	}
	c := plan.cfg
	if c.Topology != "gateway" || c.WAN != "eth0" || c.PanelPort != 8080 || c.Backend != "sing-box" ||
		c.Domestic != defaultDomestic || c.FakeIPRange != defaultFakeIP || c.MixedPort != defaultMixed {
		t.Errorf("the previous network with default settings: %+v", c)
	}
	if c.panelAuth != filepath.Join(backup, "data", "webinfo", "atom_auth.json") {
		t.Errorf("the panel login is taken over: %q", c.panelAuth)
	}
	if !plan.freeResolved || !slices.Equal(plan.freeUnits, []string{"dnsmasq.service"}) {
		t.Errorf("port 53 is freed from the same services: %+v", plan)
	}
}

func resetServer(t *testing.T) *Server {
	t.Helper()
	p := Paths{Root: t.TempDir()}
	if err := writeJSON(p.State(), &InstallState{Version: "1.2.3", Complete: true, Topology: "bypass", LAN: []string{"eth0"}, PanelPort: 80}); err != nil {
		t.Fatal(err)
	}
	r, err := newResetRun(p, "job1", listenInfo{Port: 9996, PID: os.Getpid(), Code: "012345"}, p.Run("installer.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := newServer(&Installer{Paths: p}, "012345")
	s.reset = r
	fakeSystemctl(t, "") // the removal asks whether atom's services still run; none does
	return s
}

// fakeCommands puts stand-ins for commands (systemctl, rc-service, nft...) first in PATH. Each records its
// arguments in <dir>/calls; dir is returned so that a test can change the files a stand-in reads.
func fakeCommands(t *testing.T, scripts map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range scripts {
		script := "#!/bin/sh\nD=\"$(dirname \"$0\")\"\necho \"" + name + " $*\" >> \"$D/calls\"\n" + body + "\nexit 0\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

// fakeSystemctl stands in for systemctl, nft and ip: a service named in <dir>/running is active, one named in
// <dir>/stuck cannot be disabled; start and enable fail (a service of others cannot be started again).
func fakeSystemctl(t *testing.T, running string) string {
	t.Helper()
	dir := fakeCommands(t, map[string]string{
		"systemctl": `case "$1" in
is-active) if grep -qx "$2" "$D/running" 2>/dev/null; then echo active; exit 0; fi; echo inactive; exit 3;;
disable) for u in "$@"; do if grep -qx "$u" "$D/stuck" 2>/dev/null; then echo "Failed to disable unit $u: Access denied" >&2; exit 1; fi; done;;
start|enable) exit 1;;
esac`,
		"nft": "", "ip": "",
	})
	if err := os.WriteFile(filepath.Join(dir, "running"), []byte(running), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// installedWithServices writes an installation with its service definitions (systemd) under p.
func installedWithServices(t *testing.T, p Paths) {
	t.Helper()
	for path, content := range map[string]string{
		"etc/install.json":             `{"version":"1.2.3","complete":true}`,
		"data/.atom-data":              `{"version":"1.2.3"}`,
		"data/webinfo/atom_proxy.json": `{}`,
		"bin/atomdns":                  "x",
	} {
		if err := writeFileAtomic(filepath.Join(p.Root, path), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, u := range []string{maintenanceUnitName, dnsUnitName, singBoxUnitName} {
		if err := writeFileAtomic(p.unitFile(u), []byte("[Service]\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := saveChanges(p, &Changes{Network: true}); err != nil {
		t.Fatal(err)
	}
}

func noStopWait(t *testing.T) {
	old := serviceStopWait
	serviceStopWait = 0
	t.Cleanup(func() { serviceStopWait = old })
}

func TestResetWizardWaitsThenHoldsUntilInstalled(t *testing.T) {
	s := resetServer(t)
	if !s.busyOrClosing() {
		t.Error("nothing installs before the reset")
	}
	if v := s.state().Reset; v == nil || v.Phase != resetWaiting || v.Previous.LAN[0] != "eth0" || v.PanelPort != 80 {
		t.Fatalf("the page sees the waiting reset: %+v", v)
	}
	// the code alone does not reset: only the reset request does
	post := func(path string) int {
		r := httptest.NewRequest("POST", "http://127.0.0.1"+path, strings.NewReader("{}"))
		r.RemoteAddr = "127.0.0.1:8888"
		r.Header.Set("X-Atom-Install-Code", "012345")
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.handler().ServeHTTP(w, r)
		return w.Code
	}
	s.mu.Lock()
	s.reset.deadline = time.Now().Add(-time.Second)
	s.mu.Unlock()
	if code := post("/api/reset"); code != 409 {
		t.Fatalf("a reset asked for too late: %d", code)
	}
	if s.state().Reset.Phase != resetWaiting {
		t.Fatal("a late request changes nothing")
	}
	if !s.idleClose() || !s.closing {
		t.Fatal("a reset nobody opened in time ends the wizard")
	}

	s = resetServer(t)
	s.mu.Lock()
	s.reset.phase = resetPurging
	s.mu.Unlock()
	if s.requestClose() {
		t.Fatal("the wizard cannot be closed while it resets")
	}
	s.setResetPhase(resetReady, "")
	var li listenInfo
	if err := readJSON(s.reset.runFile, &li); err != nil || li.Reset != resetReady || li.ResetJob != "job1" || li.Code != "012345" {
		t.Fatalf("the maintenance service reads the phase from the run file: %+v %v", li, err)
	}
	s.mu.Lock()
	s.seen = time.Now().Add(-2 * idleLimit)
	s.mu.Unlock()
	if s.busyOrClosing() {
		t.Error("after the reset the wizard takes an installation whenever it comes")
	}
	if s.requestClose() || post("/api/finish") != 409 {
		t.Error("without a successful installation the machine has no DNS: the wizard stays")
	}
	if v := s.state().Reset; v.AutoInstall == nil || v.AutoInstall.Sub(time.Now()) < idleLimit-time.Minute {
		t.Errorf("the default installation is due idleLimit after the reset: %+v", v.AutoInstall)
	}
	// a failed installation moves the default installation idleLimit after it
	s.job = newJob([]string{"a"})
	s.job.finish(os.ErrNotExist, 0)
	s.mu.Lock()
	at, due := s.autoInstallAt()
	s.mu.Unlock()
	if !due || at.Before(time.Now().Add(idleLimit-time.Minute)) {
		t.Errorf("after a failed installation: %v %v", at, due)
	}
	s.job = newJob([]string{"a"})
	s.job.finish(nil, 80)
	if !s.requestClose() {
		t.Error("an installed machine lets the wizard go")
	}
}

func TestResetRefusedBeforeAnyChangeEndsTheWizard(t *testing.T) {
	s := resetServer(t)
	// changes.json that cannot be read: the reset must stop before the backup
	if err := os.WriteFile(s.ins.Paths.Changes(), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.reset.phase = resetBackingUp
	s.mu.Unlock()
	s.runReset()
	v := s.state().Reset
	if v.Phase != resetFailed || !strings.Contains(v.Error, "未做任何更改") {
		t.Fatalf("%+v", v)
	}
	if _, err := os.Stat(s.ins.Paths.State()); err != nil {
		t.Fatalf("nothing was removed: %v", err)
	}
	if _, err := os.Stat(s.ins.Paths.Backup()); !os.IsNotExist(err) {
		t.Fatalf("no backup either: %v", err)
	}
}

func TestMaintenanceFollowsTheResetWizard(t *testing.T) {
	p := Paths{Root: filepath.Join(t.TempDir(), "atom")}
	deadline := time.Now().Add(time.Minute)
	m := &maintenance{paths: p}
	start := func() {
		// the test binary (atom-installer.test) plays the wizard
		if err := m.save(maintenanceJob{ID: "job1", Kind: "reset", Phase: "awaiting_browser", Deadline: &deadline,
			WizardPID: os.Getpid(), WizardCode: "012345", WizardPort: 9996}); err != nil {
			t.Fatal(err)
		}
	}
	wizard := func(phase, msg string) {
		li := listenInfo{Port: 9996, PID: os.Getpid(), Code: "012345", ResetJob: "job1", Reset: phase, ResetError: msg}
		if err := writeJSON(p.Run("installer.json"), li); err != nil {
			t.Fatal(err)
		}
	}

	start()
	wizard(resetPurging, "")
	m.watchReset()
	if job := m.snapshot(); !job.Done || job.Phase != "reset" || job.WizardCode != "" || job.Blocking {
		t.Fatalf("the reset began: %+v", job)
	}

	start()
	wizard(resetFailed, "无法读取恢复记录。未做任何更改")
	m.watchReset()
	if job := m.snapshot(); job.Phase != "failed" || !strings.Contains(job.Error, "无法读取恢复记录") {
		t.Fatalf("a refused reset: %+v", job)
	}

	start()
	os.Remove(p.Run("installer.json"))
	m.watchReset()
	if job := m.snapshot(); job.Phase != "cancelled" || job.Error == "" {
		t.Fatalf("a wizard that closed: %+v", job)
	}

	// a restarted maintenance service follows a wizard that still waits, and ends a reset whose wizard is gone
	start()
	wizard(resetWaiting, "")
	restarted := &maintenance{paths: p}
	if err := restarted.recover(); err != nil {
		t.Fatal(err)
	}
	if job := restarted.snapshot(); job.Done {
		t.Fatalf("the waiting wizard is followed again: %+v", job)
	}
	wizard(resetReady, "")
	for i := 0; i < 40 && !restarted.snapshot().Done; i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if job := restarted.snapshot(); job.Phase != "reset" {
		t.Fatalf("%+v", job)
	}
	start()
	os.Remove(p.Run("installer.json")) // the wizard is gone
	testWizardStarting = func() bool { return false }
	defer func() { testWizardStarting = nil }()
	m.change(func(job *maintenanceJob) {
		job.WizardPID = 0
		job.Phase = "preparing"
		job.Package = filepath.Join(p.Root, "pkg.reset-x")
	})
	os.MkdirAll(filepath.Join(p.Root, "pkg.reset-x"), 0o755)
	restarted = &maintenance{paths: p}
	if err := restarted.recover(); err != nil {
		t.Fatal(err)
	}
	if job := restarted.snapshot(); job.Phase != "cancelled" {
		t.Fatalf("%+v", job)
	}
	if _, err := os.Stat(filepath.Join(p.Root, "pkg.reset-x")); !os.IsNotExist(err) {
		t.Errorf("the package nobody got is removed: %v", err)
	}
}

func TestResetJobLockAndRequest(t *testing.T) {
	p := Paths{Root: filepath.Join(t.TempDir(), "atom")}
	m := &maintenance{paths: p}
	if err := m.save(maintenanceJob{ID: "job1", Kind: "reset", Phase: "awaiting_browser"}); err != nil {
		t.Fatal(err)
	}
	lock, err := resetOperationLock(p, "job1")
	if err != nil {
		t.Fatal(err)
	}
	lock.Close()
	if _, err := resetOperationLock(p, "other"); err == nil {
		t.Error("a wizard for another job must not start")
	}
	m.finish("cancelled", nil)
	if _, err := resetOperationLock(p, "job1"); err == nil {
		t.Error("a wizard for a finished job must not start")
	}

	w := httptest.NewRecorder()
	m.handler(w, httptest.NewRequest("POST", "/reset", strings.NewReader(`{"keep":true}`)))
	if w.Code != 400 {
		t.Errorf("the reset takes no options: %d", w.Code)
	}
	m.job.Done = false
	w = httptest.NewRecorder()
	m.handler(w, httptest.NewRequest("POST", "/reset", strings.NewReader(`{}`)))
	if w.Code != 409 {
		t.Errorf("a reset already running: %d", w.Code)
	}
	var body map[string]string
	json.Unmarshal(w.Body.Bytes(), &body)
	if body["error"] == "" {
		t.Errorf("%s", w.Body.String())
	}
}

// Review finding: every write of the run file (it holds the code) is 0600, not only the first one.
func TestResetRunFileAndLogStayPrivate(t *testing.T) {
	s := resetServer(t)
	runFile := s.reset.runFile
	// a run file that somehow has a wider mode is narrowed by the next phase
	if err := os.MkdirAll(filepath.Dir(runFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runFile, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{resetBackingUp, resetPurging, resetPurgeFailed, resetReady, resetFailed} {
		s.setResetPhase(phase, "")
		info, err := os.Stat(runFile)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s: the run file with the code is root only: %v %v", phase, info.Mode(), err)
		}
		var li listenInfo
		if err := readJSON(runFile, &li); err != nil || li.Code != "012345" || li.Reset != phase {
			t.Fatalf("%s: %+v %v", phase, li, err)
		}
	}
	// the OpenRC wizard log, where the reset wizard prints its code
	log := filepath.Join(t.TempDir(), "run", "installer.log")
	if err := privateLog(log); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(log); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("a new log is root only: %v %v", info, err)
	}
	os.Chmod(log, 0o644)
	os.WriteFile(log, []byte("安装码  111111\n"), 0o644)
	if err := privateLog(log); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(log); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("an existing log is narrowed: %v %v", info, err)
	}
	if got, _ := os.ReadFile(log); string(got) != "安装码  111111\n" {
		t.Errorf("the log is appended to, not truncated: %q", got)
	}
	script, err := os.ReadFile("scripts/install.sh.in")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(script), `chmod 600 "$ROOT/run/installer.log"`) {
		t.Error("install.sh creates the OpenRC wizard log root only as well")
	}
}

// installedForReset gives the reset server an installation with data, its marker and the install record.
func installedForReset(t *testing.T, s *Server) {
	t.Helper()
	p := s.ins.Paths
	for path, content := range map[string]string{
		"data/webinfo/atom_proxy.json": `{"settings":{"backend":"sing-box"}}`,
		"data/.atom-data":              `{"version":"1.2.3"}`,
		"bin/atomdns":                  "x",
	} {
		if err := writeFileAtomic(filepath.Join(p.Root, path), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := saveChanges(p, &Changes{}); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.reset.phase = resetBackingUp
	s.mu.Unlock()
}

func waitResetPhase(t *testing.T, s *Server, want string) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if s.state().Reset.Phase == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("phase %q, want %q", s.state().Reset.Phase, want)
}

// Review finding: a removal that failed (or left the data) must not end in ready, where the default
// installation would take the old data over; the page can try the removal again.
func TestResetRemovalFailureIsRetryableAndNeverKeepsData(t *testing.T) {
	s := resetServer(t)
	p := s.ins.Paths
	installedForReset(t, s)
	defer func() { resetUninstall = uninstallForReset }()

	calls := 0
	resetUninstall = func(Paths, []string, Logger) (string, error) {
		calls++
		return "", os.ErrPermission // e.g. stopping the maintenance service failed: nothing was removed
	}
	s.runReset()
	v := s.state().Reset
	if v.Phase != resetPurgeFailed || !strings.Contains(v.Error, "移除旧的安装没有完成") || v.Backup == "" {
		t.Fatalf("a failed removal is reported, not ready: %+v", v)
	}
	if v.Job.Steps[1].Status != "fail" {
		t.Errorf("the removal step failed: %+v", v.Job.Steps)
	}
	var li listenInfo
	if readJSON(s.reset.runFile, &li); li.Reset != resetPurgeFailed {
		t.Errorf("the maintenance service sees it too: %+v", li)
	}
	if !hasData(p) {
		t.Fatal("the test expects the old data to be there")
	}
	if !s.busyOrClosing() || s.requestClose() {
		t.Error("no installation and no closing while the old installation is still there")
	}
	s.mu.Lock()
	s.seen = time.Now().Add(-2 * idleLimit)
	s.mu.Unlock()
	if closed, hold := s.resetIdle(); closed || !hold || s.job != nil {
		t.Errorf("the idle timer neither closes nor installs: %v %v %v", closed, hold, s.job)
	}
	if s.state().Reset.AutoInstall != nil {
		t.Error("no default installation is announced")
	}

	// a removal that reports success but leaves the data is not a reset either
	resetUninstall = func(Paths, []string, Logger) (string, error) { calls++; return "", nil }
	post := func() int {
		r := httptest.NewRequest("POST", "http://127.0.0.1/api/reset", strings.NewReader("{}"))
		r.RemoteAddr = "127.0.0.1:8888"
		r.Header.Set("X-Atom-Install-Code", "012345")
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.handler().ServeHTTP(w, r)
		return w.Code
	}
	if code := post(); code != 202 {
		t.Fatalf("the page retries the removal: %d", code)
	}
	waitResetPhase(t, s, resetPurgeFailed)
	if e := s.state().Reset.Error; !strings.Contains(e, p.Data()) {
		t.Errorf("the leftover is named: %q", e)
	}

	// the retry that removes everything reaches ready; the backup is not taken again
	backup := s.state().Reset.Backup
	resetUninstall = func(p Paths, keep []string, log Logger) (string, error) {
		calls++
		for _, d := range []string{"data", "bin", "etc"} {
			os.RemoveAll(filepath.Join(p.Root, d))
		}
		return "", nil
	}
	if code := post(); code != 202 {
		t.Fatalf("retry: %d", code)
	}
	waitResetPhase(t, s, resetReady)
	if v := s.state().Reset; v.Backup != backup || v.AutoInstall == nil || v.Job.Steps[1].Status != "ok" {
		t.Errorf("after the retry: %+v", v)
	}
	if calls != 3 {
		t.Errorf("removal attempts: %d", calls)
	}
	// even with a data marker back (a failed installation, say), a reset installation never keeps data
	writeFileAtomic(dataMarker(p), []byte("{}"), 0o600)
	if s.keepExistingData() {
		t.Error("after a reset the installation starts from the default settings")
	}
	if code := post(); code != 200 {
		t.Errorf("once ready, the reset request only reports: %d", code)
	}
}

// Review finding: the panel's writes are drained before the backup and refused until the old installation is gone.
func TestResetHoldsPanelWritesFromBackupToRemoval(t *testing.T) {
	s := resetServer(t)
	p := s.ins.Paths
	installedForReset(t, s)
	defer func() { resetUninstall = uninstallForReset }()

	// a panel write in progress: it holds the shared lock and finishes after the reset asked for the backup
	if err := os.MkdirAll(p.Run(""), 0o755); err != nil {
		t.Fatal(err)
	}
	panel, err := os.OpenFile(p.Run(panelWriteLock), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer panel.Close()
	if err := syscall.Flock(int(panel.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	refused := 0
	resetUninstall = func(p Paths, keep []string, log Logger) (string, error) {
		// a new panel write between the backup and the removal is refused
		f, err := os.Open(p.Run(panelWriteLock))
		if err != nil {
			t.Error(err)
			return "", err
		}
		defer f.Close()
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
			refused++
		}
		for _, d := range []string{"data", "bin", "etc"} {
			os.RemoveAll(filepath.Join(p.Root, d))
		}
		return "", nil
	}
	done := make(chan struct{})
	go func() { s.runReset(); close(done) }()
	time.Sleep(300 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("the reset waited for the write in progress")
	default:
	}
	if err := os.WriteFile(filepath.Join(p.Data(), "webinfo", "late.json"), []byte("late"), 0o600); err != nil {
		t.Fatal(err)
	}
	syscall.Flock(int(panel.Fd()), syscall.LOCK_UN)
	<-done
	v := s.state().Reset
	if v.Phase != resetReady {
		t.Fatalf("%+v", v)
	}
	if got, err := os.ReadFile(filepath.Join(v.Backup, "data", "webinfo", "late.json")); err != nil || string(got) != "late" {
		t.Errorf("the write that was in progress is in the backup: %q %v", got, err)
	}
	if refused != 1 {
		t.Error("panel writes are refused until the old installation is gone")
	}
	if s.reset.writes != nil {
		t.Error("the lock is let go once the old installation is gone")
	}
}

// Review finding: a maintenance service that restarts after it launched the wizard and before it recorded the
// wizard's process must not remove the package the wizard runs from; it follows that wizard instead.
func TestMaintenanceRestartWhileTheResetWizardStarts(t *testing.T) {
	p := Paths{Root: filepath.Join(t.TempDir(), "atom")}
	pkg := filepath.Join(p.Root, "pkg.reset-x")
	defer func() { testWizardStarting, recoverWizardWait = nil, 20*time.Second }()
	preparing := func() {
		m := &maintenance{paths: p}
		if err := m.save(maintenanceJob{ID: "job1", Kind: "reset", Phase: "preparing", Package: pkg}); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(pkg, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	wizard := func(job, phase string) {
		li := listenInfo{Port: 9996, PID: os.Getpid(), Code: "012345", ResetJob: job, Reset: phase}
		if err := writeJSON(p.Run("installer.json"), li); err != nil {
			t.Fatal(err)
		}
	}

	// the run file of the job's wizard is there: it is taken over
	preparing()
	wizard("job1", resetWaiting)
	testWizardStarting = func() bool { return true }
	m := &maintenance{paths: p}
	if err := m.recover(); err != nil {
		t.Fatal(err)
	}
	job := m.snapshot()
	if job.Done || job.WizardPID != os.Getpid() || job.WizardCode != "012345" || job.Phase != "awaiting_browser" {
		t.Fatalf("the running wizard is followed: %+v", job)
	}
	if _, err := os.Stat(pkg); err != nil {
		t.Fatalf("the wizard's package stays: %v", err)
	}
	wizard("job1", resetReady)
	for i := 0; i < 40 && !m.snapshot().Done; i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if job := m.snapshot(); job.Phase != "reset" {
		t.Fatalf("and its reset is recorded: %+v", job)
	}

	// the wizard was launched but has not written its run file yet: the service waits for it
	preparing()
	os.Remove(p.Run("installer.json"))
	recoverWizardWait = 5 * time.Second
	go func() {
		time.Sleep(300 * time.Millisecond)
		wizard("job1", resetWaiting)
	}()
	m = &maintenance{paths: p}
	if err := m.recover(); err != nil {
		t.Fatal(err)
	}
	if job := m.snapshot(); job.Done || job.WizardPID != os.Getpid() {
		t.Fatalf("the starting wizard is waited for: %+v", job)
	}
	if _, err := os.Stat(pkg); err != nil {
		t.Fatalf("the wizard's package stays: %v", err)
	}
	wizard("job1", resetFailed)
	for i := 0; i < 40 && !m.snapshot().Done; i++ {
		time.Sleep(100 * time.Millisecond)
	}

	// a wizard of another job is not this reset's: no wizard got the package
	preparing()
	wizard("other", resetWaiting)
	testWizardStarting = func() bool { return false }
	m = &maintenance{paths: p}
	if err := m.recover(); err != nil {
		t.Fatal(err)
	}
	if job := m.snapshot(); job.Phase != "cancelled" {
		t.Fatalf("%+v", job)
	}
	if _, err := os.Stat(pkg); !os.IsNotExist(err) {
		t.Errorf("the package no wizard got is removed: %v", err)
	}

	// a wizard process that never writes its run file: the service waits, then cancels
	preparing()
	os.Remove(p.Run("installer.json"))
	testWizardStarting = func() bool { return true }
	recoverWizardWait = 500 * time.Millisecond
	m = &maintenance{paths: p}
	if err := m.recover(); err != nil {
		t.Fatal(err)
	}
	if job := m.snapshot(); job.Phase != "cancelled" {
		t.Fatalf("%+v", job)
	}
	if _, err := os.Stat(pkg); err != nil {
		t.Errorf("a process that may still run from the package keeps it (it removes it when it exits): %v", err)
	}
}

// Review finding (round 2): a DNS service that cannot be stopped (`systemctl disable --now` fails, it keeps
// running) stops the reset before anything is deleted: reset_failed with the data and the backup there and the
// panel's writes still held; the retry, once the service stops, reaches ready.
func TestResetStopsWhileTheOldDNSServiceRuns(t *testing.T) {
	s := resetServer(t)
	s.ins.Paths.UnitDir = t.TempDir()
	p := s.ins.Paths
	installedForReset(t, s)
	installedWithServices(t, p)
	noStopWait(t)
	stub := fakeSystemctl(t, dnsUnitName)
	os.WriteFile(filepath.Join(stub, "stuck"), []byte(dnsUnitName), 0o644)

	s.runReset()
	v := s.state().Reset
	if v.Phase != resetPurgeFailed || !strings.Contains(v.Error, dnsUnitName) || v.Backup == "" {
		t.Fatalf("a DNS service that did not stop is not ready: %+v", v)
	}
	if !hasData(p) || !fileExists(p.State()) || !fileExists(p.DNSUnit()) || !fileExists(p.Bin("atomdns")) {
		t.Fatal("nothing is deleted while the old service runs")
	}
	if _, err := os.Stat(filepath.Join(v.Backup, "data", ".atom-data")); err != nil {
		t.Errorf("the backup is kept: %v", err)
	}
	if s.reset.writes == nil {
		t.Error("the panel's writes stay held")
	}

	// a stop that reports success while the service still runs is not enough either
	os.Remove(filepath.Join(stub, "stuck"))
	if _, err := uninstallForReset(p, []string{"backup"}, &recLogger{}); err == nil || !strings.Contains(err.Error(), "仍在运行") || !hasData(p) {
		t.Fatalf("a service still active after its stop: %v (data kept: %v)", err, hasData(p))
	}

	os.WriteFile(filepath.Join(stub, "running"), nil, 0o644)
	r := httptest.NewRequest("POST", "http://127.0.0.1/api/reset", strings.NewReader("{}"))
	r.RemoteAddr = "127.0.0.1:8888"
	r.Header.Set("X-Atom-Install-Code", "012345")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, r)
	if w.Code != 202 {
		t.Fatalf("retry: %d", w.Code)
	}
	waitResetPhase(t, s, resetReady)
	if err := verifyPurged(p); err != nil || s.reset.writes != nil {
		t.Errorf("after the retry: %v %v", err, s.reset.writes)
	}
	// the check before ready also asks: an old service still running is not a removal
	os.WriteFile(filepath.Join(stub, "running"), []byte(dnsUnitName), 0o644)
	if err := verifyPurged(p); err == nil || !strings.Contains(err.Error(), dnsUnitName) {
		t.Errorf("a service still running: %v", err)
	}
}

// A program of atom still running (the service manager lost track of it) stops the reset as well.
func TestResetStopsWhileAProgramOfAtomRuns(t *testing.T) {
	p := Paths{Root: t.TempDir(), UnitDir: t.TempDir()}
	installedWithServices(t, p)
	noStopWait(t)
	fakeSystemctl(t, "")
	sleep, err := os.ReadFile("/bin/sleep")
	if err != nil {
		t.Skip(err)
	}
	if err := writeFileAtomic(p.Bin("atomdns"), sleep, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(p.Bin("atomdns"), "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	_, err = uninstallForReset(p, []string{"backup"}, &recLogger{})
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("atomdns（进程 %d）", cmd.Process.Pid)) {
		t.Fatalf("the process is named: %v", err)
	}
	if !hasData(p) || !fileExists(p.State()) {
		t.Fatal("nothing is deleted while it runs")
	}
	cmd.Process.Kill()
	cmd.Wait()
	if _, err := uninstallForReset(p, []string{"backup"}, &recLogger{}); err != nil {
		t.Fatal(err)
	}
	if err := verifyPurged(p); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir()) // no systemctl that answers
	if err := checkStopped(p); err == nil {
		t.Error("a service manager that does not answer is not an answer")
	}
}

// OpenRC: a stop that fails, or a service that is still starting, stops the reset; stopped (3) or crashed (32,
// the daemon is gone) lets it go on. A service manager that cannot be asked is never taken for "stopped".
func TestResetChecksOpenRCServicesStopped(t *testing.T) {
	p := Paths{Root: t.TempDir(), UnitDir: t.TempDir(), OpenRC: true}
	for _, u := range atomServices {
		if err := writeFileAtomic(p.unitFile(u), []byte("#!/sbin/openrc-run\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	noStopWait(t)
	stub := fakeCommands(t, map[string]string{
		"rc-service": `case "$2" in
stop) [ -e "$D/stop-fails" ] && exit 1;;
status) [ "$1" = atom-dns ] && exit "$(cat "$D/dns-status")"; exit 3;;
esac`,
		"rc-update": "",
	})
	set := func(name, v string) { os.WriteFile(filepath.Join(stub, name), []byte(v), 0o644) }
	set("stop-fails", "")
	set("dns-status", "0")
	if err := stopForReset(p, &recLogger{}); err == nil || !strings.Contains(err.Error(), "停止失败") {
		t.Fatalf("a failed stop: %v", err)
	}
	os.Remove(filepath.Join(stub, "stop-fails"))
	for status, stopped := range map[string]bool{"0": false, "8": false, "4": false, "3": true, "32": true} {
		set("dns-status", status)
		if err := stopForReset(p, &recLogger{}); (err == nil) != stopped {
			t.Errorf("status %s: %v", status, err)
		}
	}
	t.Setenv("PATH", t.TempDir()) // no rc-service at all
	if err := checkStopped(p); err == nil {
		t.Error("a service manager that does not answer is not an answer")
	}
}

// Review finding (round 2): a service file that could not be removed keeps the install record, and a retry
// removes the service files whatever the records say (as on a machine where an earlier build lost them).
func TestResetRetryRemovesTheServiceFilesLeftBehind(t *testing.T) {
	p := Paths{Root: t.TempDir(), UnitDir: t.TempDir()}
	installedWithServices(t, p)
	fakeSystemctl(t, "")
	// an obstacle that also holds for root: a non-empty directory where the file is
	os.Remove(p.DNSUnit())
	if err := writeFileAtomic(filepath.Join(p.DNSUnit(), "x"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := uninstallForReset(p, []string{"backup"}, &recLogger{}); err == nil || !strings.Contains(err.Error(), "删除服务文件") {
		t.Fatalf("a service file that cannot be removed: %v", err)
	}
	if !fileExists(p.State()) || !hasData(p) {
		t.Fatal("the install record and the data stay for the retry")
	}
	// the obstacle goes; the file is still there, and install.json is gone as an earlier build left it
	os.RemoveAll(p.DNSUnit())
	writeFileAtomic(p.DNSUnit(), []byte("[Service]\n"), 0o644)
	os.Remove(p.State())
	if _, err := uninstallForReset(p, []string{"backup"}, &recLogger{}); err != nil {
		t.Fatal(err)
	}
	if err := verifyPurged(p); err != nil {
		t.Fatalf("the retry reaches ready: %v", err)
	}
}

// etc/ (install.json, changes.json) is removed last: a removal that fails on the way keeps the records.
func TestResetKeepsTheRecordsUntilTheRestIsGone(t *testing.T) {
	p := Paths{Root: t.TempDir(), UnitDir: t.TempDir()}
	installedWithServices(t, p)
	fakeSystemctl(t, "")
	// share/ (after etc/ by name) cannot be removed: a directory the test cannot write into
	if err := writeFileAtomic(filepath.Join(p.Shipped(), "sub", "x"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if os.Getuid() == 0 {
		t.Skip("root removes anything")
	}
	os.Chmod(filepath.Join(p.Shipped(), "sub"), 0o555)
	defer os.Chmod(filepath.Join(p.Shipped(), "sub"), 0o755)
	if _, err := uninstallForReset(p, []string{"backup"}, &recLogger{}); err == nil {
		t.Fatal("share/ was not removed")
	}
	if !fileExists(p.State()) || !fileExists(p.Changes()) {
		t.Fatal("the records stay while share/ is there")
	}
}
