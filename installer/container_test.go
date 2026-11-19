package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func containerPaths(t *testing.T) Paths {
	t.Helper()
	run, err := os.MkdirTemp("", "ac") // a short path: unix socket paths are limited to about 100 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(run) })
	return Paths{Root: t.TempDir(), Container: true, RunDir: run}
}

func shSpec(name, script string) serviceSpec {
	return serviceSpec{Name: name, Bin: "/bin/sh", Args: []string{"-c", script}, Delay: 20 * time.Millisecond}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if ok() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timeout: %s", what)
}

func testSup(t *testing.T, p Paths, specs ...serviceSpec) (*supervisor, *syncBuf) {
	t.Helper()
	out := &syncBuf{}
	s := newSupervisor(p, specs, out)
	s.ready.Store(true)
	t.Cleanup(s.shutdown)
	return s, out
}

func TestContainerPaths(t *testing.T) {
	t.Setenv("ATOM_DEPLOYMENT", "container")
	t.Setenv("ATOM_ROOT", "/x/atom")
	p := defaultPaths()
	if !p.Container || p.OpenRC {
		t.Fatalf("not a container: %+v", p)
	}
	for got, want := range map[string]string{
		p.State():                    "/x/atom/data/.atom/install.json",
		p.Etc("atom.nft"):            "/x/atom/data/.atom/atom.nft",
		p.Lib("policy-route.sh"):     "/x/atom/data/.atom/policy-route.sh",
		p.Backup():                   "/x/atom/data/.atom/backup",
		p.Bin("atomdns"):             "/x/atom/bin/atomdns",
		p.Shipped():                  "/x/atom/share",
		p.Run(controlSocket):         "/run/atom/control.sock",
		p.Run(maintenanceSocket):     "/run/atom/maintenance.sock",
		filepath.Join(p.Data(), "x"): "/x/atom/data/x",
	} {
		if got != want {
			t.Errorf("got %s, want %s", got, want)
		}
	}
	if _, ok := p.Init().(containerInit); !ok {
		t.Errorf("Init() is %T", p.Init())
	}
	t.Setenv("ATOM_DEPLOYMENT", "")
	if q := defaultPaths(); q.Container || q.RunDir != "" {
		t.Errorf("a Linux machine got %+v", q)
	}
}

func TestContainerSpecsMatchTheUnits(t *testing.T) {
	p := Paths{Root: "/opt/atom", Container: true, RunDir: "/run/atom"}
	by := map[string]serviceSpec{}
	for _, s := range containerSpecs(p) {
		by[s.Name] = s
	}
	cmd := func(n string) string { return strings.Join(append([]string{by[n].Bin}, by[n].Args...), " ") }
	if got, want := cmd("atom-dns"), "/opt/atom/bin/atomdns start -d /opt/atom/data -c config_custom.yaml"; got != want {
		t.Errorf("atom-dns: %s", got)
	}
	if got, want := cmd("atom-sing-box"), "/opt/atom/bin/sing-box run -D /opt/atom/data/sing-box -c /opt/atom/data/sing-box/config.json"; got != want {
		t.Errorf("atom-sing-box: %s", got)
	}
	if got, want := cmd("atom-mihomo"), "/opt/atom/bin/mihomo -d /opt/atom/data/mihomo -f /opt/atom/data/mihomo/config.yaml"; got != want {
		t.Errorf("atom-mihomo: %s", got)
	}
	if env := by["atom-dns"].Env; len(env) != 1 || env[0] != "ATOM_MAINTENANCE_SOCKET=/run/atom/maintenance.sock" {
		t.Errorf("atom-dns env %v", env)
	}
	if by["atom-dns"].Delay != 2*time.Second || by["atom-sing-box"].Delay != time.Second || by["atom-mihomo"].Delay != time.Second {
		t.Error("RestartSec differs from the units")
	}
	// the unit files say the same
	unit := renderProxyUnit(p, "mihomo", "config.yaml", "-d", "-f")
	if !strings.Contains(unit, "ExecStart="+cmd("atom-mihomo")+"\n") {
		t.Errorf("unit differs:\n%s", unit)
	}
	// ExecStartPre / ExecReload of a kernel: sync-nft --start, then sync-nft
	var calls []string
	old := syncNftFn
	syncNftFn = func(_ Paths, starting, loadAtom bool, backend string) error {
		calls = append(calls, strings.Join([]string{backend, boolS(starting), boolS(loadAtom)}, " "))
		return nil
	}
	defer func() { syncNftFn = old }()
	dir := t.TempDir()
	q := Paths{Root: dir, Container: true}
	sb := containerSpecs(q)[0]
	if err := sb.Pre(); err == nil {
		t.Error("a kernel without config.json started")
	}
	if err := os.MkdirAll(filepath.Join(q.Data(), "sing-box"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(q.Data(), "sing-box", "config.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := sb.Pre(); err != nil {
		t.Error(err)
	}
	if err := sb.Reload(); err != nil {
		t.Error(err)
	}
	if strings.Join(calls, ";") != "sing-box true false;sing-box false false" {
		t.Errorf("sync-nft calls: %v", calls)
	}
}

func boolS(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func TestSetReloadCommandForTheContainer(t *testing.T) {
	yaml := "  reload: [systemctl, reload-or-restart, atom-sing-box.service]\n  stop: [systemctl, stop, atom-sing-box.service]\n" +
		"    reload: [systemctl, reload-or-restart, atom-mihomo.service]\n    stop: [systemctl, stop, atom-mihomo.service]\n"
	got, err := setReloadCommand(yaml, "/opt/atom/bin/atom-installer")
	if err != nil {
		t.Fatal(err)
	}
	want := "  reload: [/opt/atom/bin/atom-installer, svc, reload, atom-sing-box]\n  stop: [/opt/atom/bin/atom-installer, svc, stop, atom-sing-box]\n" +
		"    reload: [/opt/atom/bin/atom-installer, svc, reload, atom-mihomo]\n    stop: [/opt/atom/bin/atom-installer, svc, stop, atom-mihomo]\n"
	if got != want {
		t.Errorf("got\n%s", got)
	}
	shipped, err := os.ReadFile("../atomdns/config/sub_config/atom_rules.yaml")
	if err != nil {
		t.Skip("no atomdns checkout")
	}
	out, err := setReloadCommand(string(shipped), "/opt/atom/bin/atom-installer")
	if err != nil || strings.Contains(out, "systemctl") {
		t.Errorf("the shipped rules keep systemctl (%v)", err)
	}
}

func TestSupervisorStartStopAndLogs(t *testing.T) {
	p := containerPaths(t)
	s, out := testSup(t, p, shSpec("atom-dns", `echo hello; echo oops >&2; exec sleep 30`))
	if s.list()[0].State != "stopped" {
		t.Fatal("not stopped at first")
	}
	if err := s.Start("atom-dns.service"); err != nil { // the .service suffix is accepted
		t.Fatal(err)
	}
	if st := s.list()[0]; st.State != "running" || st.PID == 0 || st.Desired != "running" {
		t.Fatalf("%+v", st)
	}
	waitFor(t, "output", func() bool { return strings.Contains(out.String(), "[atom-dns] oops") })
	if !strings.Contains(out.String(), "[atom-dns] hello\n") {
		t.Errorf("no prefixed line: %q", out.String())
	}
	lines, _ := s.logs("atom-dns", 15)
	if len(lines) != 2 {
		t.Errorf("ring: %v", lines)
	}
	if err := s.Stop("atom-dns"); err != nil {
		t.Fatal(err)
	}
	if st := s.list()[0]; st.State != "stopped" || st.Desired != "stopped" {
		t.Fatalf("%+v", st)
	}
	if err := s.Start("nope"); err == nil {
		t.Error("unknown service started")
	}
}

func TestRingKeepsTheLastLines(t *testing.T) {
	var r lineRing
	for i := 0; i < ringLines+50; i++ {
		r.add(strings.Repeat("x", 1) + string(rune('a'+i%26)))
	}
	if got := r.last(1000); len(got) != ringLines {
		t.Errorf("kept %d", len(got))
	}
}

func TestSupervisorRestartsAfterACrashButNotAfterACleanExit(t *testing.T) {
	p := containerPaths(t)
	marks := filepath.Join(t.TempDir(), "starts")
	crash := shSpec("atom-sing-box", `echo x >> `+marks+`; exec sleep 30`)
	clean := shSpec("atom-mihomo", `exit 0`)
	s, _ := testSup(t, p, crash, clean)
	if err := s.Start("atom-sing-box"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "first start", func() bool { data, _ := os.ReadFile(marks); return strings.Count(string(data), "x") == 1 })
	pid := s.list()[0].PID
	syscall.Kill(pid, syscall.SIGKILL)
	waitFor(t, "restart", func() bool { st := s.list()[0]; return st.State == "running" && st.PID != pid })
	waitFor(t, "second start", func() bool { data, _ := os.ReadFile(marks); return strings.Count(string(data), "x") == 2 })
	if err := s.Start("atom-mihomo"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "clean exit", func() bool { return s.list()[1].State == "stopped" })
	time.Sleep(100 * time.Millisecond)
	if s.list()[1].State != "stopped" {
		t.Error("a clean exit was restarted")
	}
	if s.list()[1].Desired != "running" { // it still comes back with the container
		t.Error("desired state lost")
	}
}

func TestSupervisorPreFailureAndRetry(t *testing.T) {
	p := containerPaths(t)
	var mu sync.Mutex
	ready := false
	sp := shSpec("atom-sing-box", `exec sleep 30`)
	sp.Pre = func() error {
		mu.Lock()
		defer mu.Unlock()
		if !ready {
			return os.ErrNotExist
		}
		return nil
	}
	s, _ := testSup(t, p, sp)
	if err := s.Start("atom-sing-box"); err == nil {
		t.Fatal("start did not fail")
	}
	if s.list()[0].Desired != "stopped" || s.list()[0].State != "stopped" {
		t.Errorf("%+v", s.list()[0])
	}
	// at boot the same failure is retried until it passes
	if err := os.MkdirAll(p.Etc(""), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.Etc(servicesFile), []byte(`{"atom-sing-box":"running"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s.boot()
	if st := s.list()[0].State; st != "failed" {
		t.Errorf("state %s", st)
	}
	mu.Lock()
	ready = true
	mu.Unlock()
	waitFor(t, "retry", func() bool { return s.list()[0].State == "running" })
}

func TestSupervisorReload(t *testing.T) {
	p := containerPaths(t)
	dir := t.TempDir()
	hups := filepath.Join(dir, "hups")
	var hooks []string
	sp := shSpec("atom-sing-box", `trap 'echo h >> `+hups+`' HUP; echo ready; while :; do sleep 0.05; done`)
	sp.Pre = func() error { hooks = append(hooks, "pre"); return nil }
	sp.Reload = func() error { hooks = append(hooks, "reload"); return nil }
	s, out := testSup(t, p, sp)
	// not running: a reload starts it (reload-or-restart)
	if err := s.Reload("atom-sing-box"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(hooks, ",") != "pre" || s.list()[0].State != "running" {
		t.Fatalf("hooks %v, state %s", hooks, s.list()[0].State)
	}
	waitFor(t, "trap set", func() bool { return strings.Contains(out.String(), "ready") })
	pid := s.list()[0].PID
	if err := s.Reload("atom-sing-box"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "SIGHUP", func() bool { d, _ := os.ReadFile(hups); return strings.Count(string(d), "h") == 1 })
	if strings.Join(hooks, ",") != "pre,reload" || s.list()[0].PID != pid {
		t.Errorf("hooks %v, pid changed", hooks)
	}
	// a failing hook stops the reload before the signal
	sp2 := shSpec("atom-mihomo", `exec sleep 30`)
	sp2.Reload = func() error { return os.ErrPermission }
	s2, _ := testSup(t, p, sp2)
	s2.Start("atom-mihomo")
	if err := s2.Reload("atom-mihomo"); err == nil {
		t.Error("reload ignored the failing hook")
	}
}

func TestSupervisorRestartAndDesiredState(t *testing.T) {
	p := containerPaths(t)
	s, _ := testSup(t, p, shSpec("atom-sing-box", `exec sleep 30`), shSpec("atom-dns", `exec sleep 30`))
	if got := s.desired(); !got["atom-dns"] || got["atom-sing-box"] {
		t.Errorf("default desired state %v", got)
	}
	s.Start("atom-sing-box")
	s.Start("atom-dns")
	pid := s.list()[0].PID
	if err := s.Restart("atom-sing-box"); err != nil {
		t.Fatal(err)
	}
	if st := s.list()[0]; st.State != "running" || st.PID == pid {
		t.Errorf("restart: %+v", st)
	}
	s.Stop("atom-sing-box")
	var m map[string]string
	if err := readJSON(p.Etc(servicesFile), &m); err != nil || m["atom-dns"] != "running" || m["atom-sing-box"] != "stopped" {
		t.Errorf("services.json %v %v", m, err)
	}
	// a shutdown (SIGTERM of the container) does not change what is wanted
	s.shutdown()
	if s.list()[1].State != "stopped" {
		t.Error("shutdown left atomdns running")
	}
	if err := readJSON(p.Etc(servicesFile), &m); err != nil || m["atom-dns"] != "running" {
		t.Errorf("shutdown changed services.json: %v", m)
	}
	// and a new entry process starts what was running
	s2, _ := testSup(t, p, shSpec("atom-sing-box", `exec sleep 30`), shSpec("atom-dns", `exec sleep 30`))
	s2.boot()
	if got := s2.list(); got[0].State != "stopped" || got[1].State != "running" {
		t.Errorf("after boot %+v", got)
	}
}

func TestShutdownStopsAtomdnsFirstAndKillsWhatIgnoresTerm(t *testing.T) {
	p := containerPaths(t)
	order := filepath.Join(t.TempDir(), "order")
	term := func(n string) string {
		return `trap 'echo ` + n + ` >> ` + order + `; exit 0' TERM; echo up; while :; do sleep 0.05; done`
	}
	stubborn := shSpec("atom-mihomo", `trap '' TERM; while :; do sleep 0.05; done`)
	s, out := testSup(t, p, shSpec("atom-sing-box", term("kernel")), stubborn, shSpec("atom-dns", term("dns")))
	old := stopWait
	stopWait = 300 * time.Millisecond
	defer func() { stopWait = old }()
	for _, n := range []string{"atom-sing-box", "atom-mihomo", "atom-dns"} {
		if err := s.Start(n); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "traps", func() bool { return strings.Count(out.String(), "up") == 2 })
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	s.shutdown()
	if time.Since(start) > 3*time.Second {
		t.Errorf("took %v", time.Since(start))
	}
	data, _ := os.ReadFile(order)
	if string(data) != "dns\nkernel\n" {
		t.Errorf("stop order %q", data)
	}
	for _, st := range s.list() {
		if st.State != "stopped" {
			t.Errorf("%+v", st)
		}
	}
	if !strings.Contains(out.String(), "强制结束") {
		t.Error("the stubborn service was not killed")
	}
}

func TestControlSocketAndContainerInit(t *testing.T) {
	p := containerPaths(t)
	s, _ := testSup(t, p, shSpec("atom-sing-box", `echo kernel-line; exec sleep 30`), shSpec("atom-dns", `exec sleep 30`))
	closeAll, err := serveContainerSockets(p, s)
	if err != nil {
		t.Fatal(err)
	}
	defer closeAll()
	init := p.Init()
	log := stderrLogger{}
	if got := init.IsActive("atom-sing-box.service"); got != "inactive" {
		t.Errorf("IsActive %q", got)
	}
	if err := init.Start(log, "atom-sing-box"); err != nil {
		t.Fatal(err)
	}
	if got := init.IsActive("atom-sing-box"); got != "active" {
		t.Errorf("IsActive after start %q", got)
	}
	waitFor(t, "log", func() bool { return strings.Contains(init.Logs("atom-sing-box"), "kernel-line") })
	if err := init.Restart(log, "atom-sing-box.service"); err != nil {
		t.Fatal(err)
	}
	if err := init.Enable(log, "atom-dns"); err != nil || s.desired()["atom-dns"] != true {
		t.Errorf("enable: %v %v", err, s.desired())
	}
	if init.IsActive("atom-dns") != "inactive" {
		t.Error("enable started the service")
	}
	if err := init.DisableNow(log, "atom-sing-box", "atom-dns"); err != nil {
		t.Fatal(err)
	}
	if init.IsActive("atom-sing-box") != "inactive" || s.desired()["atom-dns"] {
		t.Error("DisableNow")
	}
	// the maintenance service is the entry process itself; an unknown service is an error
	if init.IsActive(maintenanceUnitName) != "active" || init.Start(log, maintenanceUnitName) != nil || init.Restart(log, "atom-maintenance") != nil {
		t.Error("atom-maintenance")
	}
	if err := init.Start(log, "sshd"); err == nil {
		t.Error("unknown service")
	}
	if init.Reload(log) != nil {
		t.Error("Reload")
	}
	// the svc command
	t.Setenv("ATOM_DEPLOYMENT", "container")
	t.Setenv("ATOM_RUN_DIR", p.RunDir)
	if code := cmdSvc([]string{"reload", "atom-sing-box"}); code != 0 || s.list()[0].State != "running" {
		t.Errorf("svc reload: %d %+v", code, s.list()[0])
	}
	if code := cmdSvc([]string{"stop", "atom-sing-box"}); code != 0 || s.list()[0].State != "stopped" {
		t.Errorf("svc stop: %d", code)
	}
	if cmdSvc([]string{"stop", "nope"}) == 0 || cmdSvc([]string{"dance", "atom-dns"}) == 0 || cmdSvc([]string{"stop"}) == 0 {
		t.Error("bad svc arguments passed")
	}
	// without an entry process
	if err := (containerInit{socket: filepath.Join(p.RunDir, "none.sock")}).Start(log, "atom-dns"); err == nil {
		t.Error("no entry process, no error")
	}
}

func TestContainerMaintenance(t *testing.T) {
	p := containerPaths(t)
	s, _ := testSup(t, p, shSpec("atom-sing-box", `exec sleep 30`), shSpec("atom-mihomo", `exec sleep 30`), shSpec("atom-dns", `exec sleep 30`))
	closeAll, err := serveContainerSockets(p, s)
	if err != nil {
		t.Fatal(err)
	}
	defer closeAll()
	do := func(method, path, body string) (int, map[string]any) {
		cl := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", p.Run(maintenanceSocket))
		}}}
		req, _ := http.NewRequest(method, "http://m"+path, strings.NewReader(body))
		resp, err := cl.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		var m map[string]any
		json.Unmarshal(data, &m)
		return resp.StatusCode, m
	}
	if code, _ := do("GET", "/", ""); code != 503 {
		t.Errorf("overview without an installation: %d", code)
	}
	if err := writeJSON(p.State(), InstallState{Version: "1.2.3", Complete: true}); err != nil {
		t.Fatal(err)
	}
	code, m := do("GET", "/", "")
	if code != 200 || m["deployment"] != "container" || m["version"] != "1.2.3" {
		t.Errorf("overview %d %v", code, m)
	}
	for _, c := range []struct{ method, path string }{{"POST", "/upgrade"}, {"POST", "/check"}, {"POST", "/network"}, {"GET", "/network"}, {"POST", "/reset"}} {
		if code, m := do(c.method, c.path, "{}"); code != 400 || m["error"] == "" {
			t.Errorf("%s %s: %d %v", c.method, c.path, code, m)
		}
	}
	if code, _ := do("GET", "/nothing", ""); code != 404 {
		t.Errorf("unknown path %d", code)
	}
	// components: atomdns and the selected kernel only; atom-maintenance is not listed
	selectKernel(t, p, "mihomo")
	s.Start("atom-mihomo")
	code, m = do("GET", "/services", "")
	list, _ := m["services"].([]any)
	if code != 200 || len(list) != 2 {
		t.Fatalf("services %d %v", code, m)
	}
	first, second := list[0].(map[string]any), list[1].(map[string]any)
	if first["name"] != "atom-dns" || first["state"] != "stopped" || second["name"] != "atom-mihomo" || second["state"] != "running" {
		t.Errorf("services %v", list)
	}
	// restart: the other kernel and an unknown one are refused, atom-dns restarts
	if code, _ := do("POST", "/restart", `{"service":"atom-sing-box"}`); code != 400 {
		t.Errorf("restart of the unselected kernel: %d", code)
	}
	if code, _ := do("POST", "/restart", `{"service":"atom-maintenance"}`); code != 400 {
		t.Errorf("restart of atom-maintenance: %d", code)
	}
	old := restartDelay
	restartDelay = 0
	defer func() { restartDelay = old }()
	pid := s.list()[1].PID
	if code, _ := do("POST", "/restart", `{"service":"atom-mihomo"}`); code != 202 {
		t.Fatalf("restart: %d", code)
	}
	waitFor(t, "restart", func() bool { st := s.list()[1]; return st.State == "running" && st.PID != pid })
}

func TestContainerNetworkIsRenderedFromTheState(t *testing.T) {
	p := containerPaths(t)
	bin := fakeCommands(t, map[string]string{"nft": `exit 0`, "ip": `exit 0`})
	st := InstallState{Version: "1.0.0", Complete: true, Topology: "bypass", LAN: []string{"eth0"}, PanelPort: 80, NftBin: filepath.Join(bin, "nft")}
	if err := writeJSON(p.State(), st); err != nil {
		t.Fatal(err)
	}
	var got [3]any
	old := syncNftFn
	syncNftFn = func(_ Paths, starting, loadAtom bool, backend string) error {
		got = [3]any{starting, loadAtom, backend}
		return nil
	}
	defer func() { syncNftFn = old }()
	if err := loadContainerNetwork(p); err != nil {
		t.Fatal(err)
	}
	nft, err := os.ReadFile(p.Etc("atom.nft"))
	if err != nil || !strings.Contains(string(nft), `iifname != { "eth0" } return`) {
		t.Errorf("atom.nft: %v %s", err, nft)
	}
	if got != [3]any{false, true, ""} {
		t.Errorf("sync-nft %v", got)
	}
	if _, err := os.Stat(p.Lib("policy-route.sh")); err != nil {
		t.Error(err)
	}
	if _, err := os.Stat(p.DNSUnit()); err == nil {
		t.Error("a unit file was written")
	}
}

func TestNothingStartsDuringShutdown(t *testing.T) {
	p := containerPaths(t)
	s, _ := testSup(t, p, shSpec("atom-dns", `exec sleep 30`))
	s.shutdown()
	for name, err := range map[string]error{"start": s.Start("atom-dns"), "restart": s.Restart("atom-dns"), "reload": s.Reload("atom-dns")} {
		if !errors.Is(err, errClosing) {
			t.Errorf("%s during shutdown: %v", name, err)
		}
	}
	s.boot() // a boot that SIGTERM overtakes starts nothing
	if s.list()[0].State != "stopped" {
		t.Error("boot started a service after shutdown began")
	}
}

func TestOnlyOneKernelRuns(t *testing.T) {
	p := containerPaths(t)
	sb, mh := shSpec("atom-sing-box", `exec sleep 30`), shSpec("atom-mihomo", `exec sleep 30`)
	sb.Group, mh.Group = "kernel", "kernel"
	s, _ := testSup(t, p, sb, mh)
	if err := s.Start("atom-sing-box"); err != nil {
		t.Fatal(err)
	}
	if err := s.Start("atom-mihomo"); err == nil || s.list()[1].State != "stopped" {
		t.Errorf("a second kernel started: %v", err)
	}
	if err := s.Reload("atom-mihomo"); err == nil {
		t.Error("a second kernel was reloaded into running")
	}
	// the switch of atomdns: the old one is stopped, then the new one reloaded
	s.Stop("atom-sing-box")
	if err := s.Reload("atom-mihomo"); err != nil {
		t.Fatal(err)
	}
	// both marked running (hand edited, or a crash between the two writes): the selected one starts
	s.shutdown()
	p2 := containerPaths(t)
	if err := os.MkdirAll(filepath.Join(p2.Data(), "webinfo"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(p2.Data(), "webinfo", "atom_proxy.json"), []byte(`{"settings":{"backend":"mihomo"}}`), 0o600)
	os.MkdirAll(p2.Etc(""), 0o755)
	os.WriteFile(p2.Etc(servicesFile), []byte(`{"atom-sing-box":"running","atom-mihomo":"running"}`), 0o644)
	s2, _ := testSup(t, p2, sb, mh)
	s2.boot()
	if got := s2.list(); got[0].State != "stopped" || got[1].State != "running" || got[0].Desired != "stopped" {
		t.Errorf("after boot %+v", got)
	}
}

func TestReloadDuringTheRetryWaitStartsNow(t *testing.T) {
	p := containerPaths(t)
	var mu sync.Mutex
	ready := false
	sp := shSpec("atom-sing-box", `exec sleep 30`)
	sp.Delay = time.Hour // the loop would not try again by itself
	sp.Pre = func() error {
		mu.Lock()
		defer mu.Unlock()
		if !ready {
			return os.ErrNotExist
		}
		return nil
	}
	sp.Reload = func() error { return nil }
	s, _ := testSup(t, p, sp)
	os.MkdirAll(p.Etc(""), 0o755)
	os.WriteFile(p.Etc(servicesFile), []byte(`{"atom-sing-box":"running"}`), 0o644)
	s.boot()
	if s.list()[0].State != "failed" {
		t.Fatalf("state %s", s.list()[0].State)
	}
	mu.Lock()
	ready = true
	mu.Unlock()
	if err := s.Reload("atom-sing-box"); err != nil || s.list()[0].State != "running" {
		t.Errorf("reload in the wait: %v, %s", err, s.list()[0].State)
	}
}

func TestServicesJSONProblems(t *testing.T) {
	p := containerPaths(t)
	s, _ := testSup(t, p, shSpec("atom-dns", `exec sleep 30`), shSpec("atom-sing-box", `exec sleep 30`))
	os.MkdirAll(p.Etc(""), 0o755)
	// unreadable content: counts as new, and the next change rewrites it
	for _, bad := range []string{`{`, `{"atom-dns":"maybe"}`, `[1]`, `null`} {
		os.WriteFile(p.Etc(servicesFile), []byte(bad), 0o644)
		if _, err := s.loadDesired(); !errors.Is(err, errServicesCorrupt) {
			t.Errorf("%s: %v", bad, err)
		}
		if err := s.Enable("atom-sing-box"); err != nil {
			t.Errorf("%s: %v", bad, err)
		}
		var m map[string]string
		if err := readJSON(p.Etc(servicesFile), &m); err != nil || m["atom-dns"] != "running" || m["atom-sing-box"] != "running" {
			t.Errorf("rewritten: %v %v", m, err)
		}
	}
	// a file that cannot be read or written is an error of the call, not a silent default
	os.Remove(p.Etc(servicesFile))
	os.MkdirAll(p.Etc(servicesFile), 0o755) // a directory where the file should be
	s.Start("atom-dns")                     // fails to record: the start is taken back
	if s.list()[0].State != "stopped" {
		t.Error("a start that cannot be recorded stayed")
	}
	os.Remove(p.Etc(servicesFile))
	if err := s.Start("atom-dns"); err != nil {
		t.Fatal(err)
	}
	os.Remove(p.Etc(servicesFile))
	os.MkdirAll(filepath.Join(p.Etc(servicesFile), "x"), 0o755) // a directory where the file should be
	if err := s.Stop("atom-dns"); err == nil || s.list()[0].State != "running" {
		t.Errorf("a stop that cannot be recorded happened: %v %s", err, s.list()[0].State)
	}
}

func TestStopKillsWhatTheServiceStarted(t *testing.T) {
	p := containerPaths(t)
	child := filepath.Join(t.TempDir(), "child")
	s, _ := testSup(t, p, shSpec("atom-dns", `sleep 30 & echo $! > `+child+`; wait`))
	if err := s.Start("atom-dns"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "child", func() bool { d, _ := os.ReadFile(child); return len(d) > 0 })
	d, _ := os.ReadFile(child)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(d)))
	s.Stop("atom-dns")
	waitFor(t, "child gone", func() bool { return syscall.Kill(pid, 0) != nil })
}

func TestSetReloadCommandForTheContainerNeedsAllFourCommands(t *testing.T) {
	yaml := "  reload: [systemctl, reload-or-restart, atom-sing-box.service]\n  stop: [systemctl, stop, atom-sing-box.service]\n"
	if _, err := setReloadCommand(yaml, "/opt/atom/bin/atom-installer"); err == nil {
		t.Error("rules without the mihomo commands were accepted")
	}
}

func TestOverlappingKernelStartsDoNotBothRun(t *testing.T) {
	p := containerPaths(t)
	in := make(chan struct{}, 2)
	release := make(chan struct{})
	mk := func(n string) serviceSpec {
		sp := shSpec(n, `exec sleep 30`)
		sp.Group = "kernel"
		sp.Pre = func() error { in <- struct{}{}; <-release; return nil }
		return sp
	}
	s, _ := testSup(t, p, mk("atom-sing-box"), mk("atom-mihomo"))
	errs := make(chan error, 2)
	go func() { errs <- s.Start("atom-sing-box") }()
	go func() { errs <- s.Start("atom-mihomo") }()
	<-in // one is inside its hook; the other must be waiting for the slot, not in its hook
	select {
	case <-in:
		t.Fatal("both starts were in their hooks")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	e1, e2 := <-errs, <-errs
	if (e1 == nil) == (e2 == nil) {
		t.Errorf("results %v %v: exactly one start must win", e1, e2)
	}
	n := 0
	for _, st := range s.list() {
		if st.State == "running" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d kernels run", n)
	}
}

func TestFailedReloadInTheRetryWaitKeepsRetrying(t *testing.T) {
	p := containerPaths(t)
	var mu sync.Mutex
	ready := false
	sp := shSpec("atom-sing-box", `exec sleep 30`)
	sp.Delay = 30 * time.Millisecond
	sp.Pre = func() error {
		mu.Lock()
		defer mu.Unlock()
		if !ready {
			return os.ErrNotExist
		}
		return nil
	}
	sp.Reload = func() error { return nil }
	s, _ := testSup(t, p, sp)
	os.MkdirAll(p.Etc(""), 0o755)
	os.WriteFile(p.Etc(servicesFile), []byte(`{"atom-sing-box":"running"}`), 0o644)
	s.boot()
	if err := s.Reload("atom-sing-box"); err == nil {
		t.Fatal("reload with a failing hook succeeded")
	}
	if s.list()[0].State != "failed" {
		t.Errorf("the retry loop was lost: %s", s.list()[0].State)
	}
	mu.Lock()
	ready = true
	mu.Unlock()
	waitFor(t, "recovery without another request", func() bool { return s.list()[0].State == "running" })
}

func TestRequestsBeforeTheBootAreRefused(t *testing.T) {
	p := containerPaths(t)
	s := newSupervisor(p, []serviceSpec{shSpec("atom-dns", `exec sleep 30`)}, &syncBuf{})
	defer s.shutdown()
	if err := s.Start("atom-dns"); !errors.Is(err, errNotReady) {
		t.Errorf("start before the boot: %v", err)
	}
	if err := s.Reload("atom-dns"); !errors.Is(err, errNotReady) {
		t.Errorf("reload before the boot: %v", err)
	}
}
