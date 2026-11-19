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
	"testing"
	"time"
)

// ---- the capability probe ----

func fakeProbeEnv(capEff string, nft map[string]string, ipErr string, total, withIP int) (probeEnv, *[]string) {
	var calls []string
	return probeEnv{
		capEff: func() (string, error) { return capEff, nil },
		run: func(stdin, name string, args ...string) (string, error) {
			calls = append(calls, name+" "+strings.Join(args, " "))
			base := filepath.Base(name)
			if base == "nft" && len(args) > 0 && args[0] == "-f" {
				for key, msg := range nft {
					if strings.Contains(stdin, key) {
						return msg, errors.New("exit 1")
					}
				}
			}
			if base == "ip" && ipErr != "" && len(args) > 1 && args[1] == "add" {
				return ipErr, errors.New("exit 2")
			}
			return "", nil
		},
		nics: func() (int, int) { return total, withIP },
	}, &calls
}

func TestProbeAllGood(t *testing.T) {
	bin := fakeCommands(t, map[string]string{"nft": "", "ip": ""})
	_ = bin
	env, calls := fakeProbeEnv("0000003fffffffff", nil, "", 1, 1)
	pr := env.probe()
	if len(pr.Problems) != 0 || len(pr.Checks) != 4 {
		t.Fatalf("%+v", pr)
	}
	for _, c := range pr.Checks {
		if !c.OK {
			t.Errorf("check %s failed: %s", c.ID, c.Reason)
		}
	}
	// the probe table is deleted after each load, the rule after its add
	joined := strings.Join(*calls, "\n")
	if strings.Count(joined, "delete table inet "+probeTable) != 3 || !strings.Contains(joined, "rule del fwmark 0x2ff") {
		t.Errorf("cleanup missing:\n%s", joined)
	}
}

func TestProbeWithoutNetAdmin(t *testing.T) {
	// CAP_NET_ADMIN is bit 12: 0xfff has bits 0-11 only
	env, calls := fakeProbeEnv("0000000000000fff", nil, "", 1, 1)
	pr := env.probe()
	if len(pr.Problems) != 1 || pr.Problems[0] != "缺少 NET_ADMIN" {
		t.Fatalf("%+v", pr)
	}
	if len(*calls) != 0 {
		t.Errorf("nothing is tried without the capability: %v", *calls)
	}
	if !hasCap("1000", 12) || hasCap("fff", 12) || hasCap("zz", 12) {
		t.Error("hasCap")
	}
}

func TestProbeNamesTheMissingModule(t *testing.T) {
	bin := fakeCommands(t, map[string]string{"nft": "", "ip": ""})
	_ = bin
	env, _ := fakeProbeEnv("1fffffffff", map[string]string{"tproxy ip to": "Error: Could not process rule: Operation not supported\nadd rule ..."}, "", 1, 1)
	pr := env.probe()
	if len(pr.Problems) != 1 || pr.Problems[0] != "内核缺少 nft_tproxy" {
		t.Fatalf("%+v", pr)
	}
	for _, c := range pr.Checks {
		if c.ID == "nft" && (c.OK || c.Reason != "内核缺少 nft_tproxy") {
			t.Errorf("%+v", c)
		}
		if c.ID != "nft" && !c.OK {
			t.Errorf("only the nft line fails: %+v", c)
		}
	}
}

func TestProbeIPRuleAndInterfaces(t *testing.T) {
	bin := fakeCommands(t, map[string]string{"nft": "", "ip": ""})
	_ = bin
	env, _ := fakeProbeEnv("1fffffffff", nil, "RTNETLINK answers: Operation not supported", 0, 0)
	pr := env.probe()
	if len(pr.Problems) != 2 || !strings.HasPrefix(pr.Problems[0], "不能添加 ip rule") || pr.Problems[1] != "没有网卡" {
		t.Fatalf("%+v", pr)
	}
	env, _ = fakeProbeEnv("1fffffffff", nil, "", 2, 0)
	if pr := env.probe(); len(pr.Problems) != 1 || pr.Problems[0] != "网卡没有 IPv4 地址" {
		t.Fatalf("%+v", pr)
	}
}

func TestProbeCacheKeepsTheAnswerBriefly(t *testing.T) {
	n := 0
	env, _ := fakeProbeEnv("1fffffffff", nil, "", 1, 1)
	env.nics = func() (int, int) { n++; return 1, 1 }
	c := &probeCache{env: env}
	c.get(false)
	c.get(false)
	if n != 1 {
		t.Errorf("probed %d times", n)
	}
	c.get(true)
	if n != 2 {
		t.Errorf("a fresh probe was not made: %d", n)
	}
}

// ---- the wizard server in the container ----

func containerServer(t *testing.T, p Paths, firstRun bool) (*Server, func(method, path, body string) (int, map[string]any)) {
	t.Helper()
	fakeCommands(t, map[string]string{"nft": "", "ip": ""}) // the probe looks for them
	ins := &Installer{Paths: p, PkgDir: p.Shipped(), Version: "1.2.3", InstallerPort: 4321}
	s := newServer(ins, "123456")
	s.firstRun = firstRun
	env, _ := fakeProbeEnv("1fffffffff", nil, "", 1, 1)
	s.probe = &probeCache{env: env}
	h := s.handler()
	do := func(method, path, body string) (int, map[string]any) {
		req, _ := http.NewRequest(method, path, strings.NewReader(body))
		req.RemoteAddr = "10.0.0.9:5555"
		req.Host = "10.0.0.201"
		req.Header.Set("X-Atom-Install-Code", "123456")
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := &recorder{header: http.Header{}}
		h.ServeHTTP(rec, req)
		var m map[string]any
		json.Unmarshal(rec.body.Bytes(), &m)
		return rec.code, m
	}
	return s, do
}

type recorder struct {
	header http.Header
	body   bytes.Buffer
	code   int
}

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) Write(b []byte) (int, error) { return r.body.Write(b) }
func (r *recorder) WriteHeader(c int)           { r.code = c }

func TestContainerStateAndEnvironment(t *testing.T) {
	p := containerPaths(t)
	_, do := containerServer(t, p, true)
	code, m := do("GET", "/api/state", "")
	if code != 200 || m["deployment"] != "container" {
		t.Fatalf("%d %v", code, m)
	}
	c, _ := m["container"].(map[string]any)
	if checks, _ := c["checks"].([]any); len(checks) != 4 {
		t.Errorf("checks %v", c)
	}
	sys, _ := m["system"].(map[string]any)
	if probs, _ := sys["problems"].([]any); len(probs) != 0 {
		t.Errorf("problems %v", probs)
	}
	if p53, _ := m["port53"].([]any); len(p53) != 0 {
		t.Errorf("port 53 is not looked at: %v", p53)
	}
	if code, _ := do("POST", "/api/free", `{"kind":"unit","unit":"x"}`); code != 409 {
		t.Errorf("free: %d", code)
	}
}

func TestContainerEnvironmentProblemsStopTheInstallation(t *testing.T) {
	p := containerPaths(t)
	s, do := containerServer(t, p, true)
	env, _ := fakeProbeEnv("0", nil, "", 1, 1)
	s.probe = &probeCache{env: env}
	code, m := do("GET", "/api/state", "")
	sys, _ := m["system"].(map[string]any)
	if probs, _ := sys["problems"].([]any); code != 200 || len(probs) != 1 || probs[0] != "缺少 NET_ADMIN" {
		t.Fatalf("%d %v", code, m)
	}
	code, m = do("POST", "/api/install", `{"lan":["eth0"],"panel_port":80,"mixed_port":7890,"backend":"sing-box"}`)
	if code != 400 || !strings.Contains(m["error"].(string), "缺少 NET_ADMIN") {
		t.Errorf("install: %d %v", code, m)
	}
}

func TestContainerValidationIsSingleNIC(t *testing.T) {
	p := containerPaths(t)
	s, _ := containerServer(t, p, true)
	cfg := Config{Topology: "gateway", WAN: "eth1", LANGateway: "1.1.1.1", ClientNets: "10.0.0.0/24", LAN: []string{"nope0"},
		PanelPort: 80, MixedPort: 7890, Backend: "sing-box", FakeIPRange: defaultFakeIP, Domestic: defaultDomestic, Foreign: defaultForeign}
	errs := s.validateConfig(&cfg)
	if cfg.Topology != "bypass" || cfg.WAN != "" || cfg.ClientNets != "" {
		t.Errorf("not forced to the single NIC: %+v", cfg)
	}
	for _, e := range errs {
		if e.Field == "topology" || e.Field == "wan" || e.Field == "client_nets" || e.Field == "lan_gateway" || e.Field == "panel_port" {
			t.Errorf("unexpected %v", e)
		}
	}
	if len(errs) != 1 || errs[0].Field != "lan" {
		t.Errorf("only the unknown interface is wrong: %v", errs)
	}
	// the first wizard's own port is free for the panel; another wizard's is not
	cfg.LAN = nil
	if ifs := listInterfaces(); len(ifs) == 1 {
		if errs := s.validateConfig(&cfg); len(errs) != 0 || cfg.LAN[0] != ifs[0].Name {
			t.Errorf("the only interface is the LAN: %v %v", errs, cfg.LAN)
		}
	}
	s.ins.InstallerPort = 80
	cfg.LAN = []string{"nope0"}
	has := func(errs []FieldError, field string) bool {
		for _, e := range errs {
			if e.Field == field {
				return true
			}
		}
		return false
	}
	if has(s.validateConfig(&cfg), "panel_port") {
		t.Error("the first wizard's port was refused for the panel")
	}
	s.firstRun = false
	if !has(s.validateConfig(&cfg), "panel_port") {
		t.Error("another wizard's port was accepted for the panel")
	}
}

func TestPanelTempOnlyWhenTheWizardHoldsThePort(t *testing.T) {
	p := containerPaths(t)
	s, _ := containerServer(t, p, true)
	s.ins.InstallerPort = 80
	if port, err := s.panelTemp(8080); err != nil || port != 0 {
		t.Errorf("another port: %d %v", port, err)
	}
	port, err := s.panelTemp(80)
	if err != nil || port == 0 || port == 80 {
		t.Errorf("same port: %d %v", port, err)
	}
	s.firstRun = false
	if port, _ := s.panelTemp(80); port != 0 {
		t.Errorf("a wizard beside the panel: %d", port)
	}
}

func TestFirstWizardClosesOnlyAfterAnInstallation(t *testing.T) {
	p := containerPaths(t)
	s, _ := containerServer(t, p, true)
	s.seen = time.Now().Add(-24 * time.Hour)
	if s.busyOrClosing() {
		t.Error("a container wizard has no idle limit")
	}
	if s.requestClose() || s.idleClose() {
		t.Error("closed without an installation")
	}
	s.job = newJob(containerInstallSteps)
	s.job.finish(nil, 80)
	if !s.requestClose() {
		t.Error("an installation is done: it may close")
	}
}

func TestContainerInstallStepsHaveNoDependenciesOrPrograms(t *testing.T) {
	p := containerPaths(t)
	ins := &Installer{Paths: p}
	if got := strings.Join(ins.stepNames(), ","); got != "配置,网络,启动,设置,自检" {
		t.Errorf("%s", got)
	}
	if got := len((&Installer{Paths: Paths{Root: "/x"}}).stepNames()); got != 7 {
		t.Errorf("%d steps on a machine", got)
	}
}

// ---- the network step and its rollback ----

func TestNetworkStepWritesStateAndLoadsRules(t *testing.T) {
	p := containerPaths(t)
	bin := fakeCommands(t, map[string]string{"nft": "", "ip": ""})
	old := syncNftFn
	var loaded bool
	syncNftFn = func(_ Paths, _, loadAtom bool, _ string) error { loaded = loadAtom; return nil }
	defer func() { syncNftFn = old }()
	ins := &Installer{Paths: p, Version: "1.2.3"}
	j := newJob(containerInstallSteps)
	err := ins.stepNetworkContainer(Config{LAN: []string{"eth0"}, PanelPort: 8080}, j)
	if err != nil {
		t.Fatal(err)
	}
	st, err := loadState(p)
	if err != nil || st.Topology != "bypass" || st.LAN[0] != "eth0" || st.PanelPort != 8080 || st.Complete || st.ReplyVia != "" {
		t.Errorf("%+v %v", st, err)
	}
	if !loaded {
		t.Error("the rules were not loaded")
	}
	if _, err := os.Stat(p.Changes()); err == nil {
		t.Error("the container keeps no changes.json")
	}
	calls, _ := os.ReadFile(filepath.Join(bin, "calls"))
	if !strings.Contains(string(calls), "ip rule del fwmark "+replyMark) {
		t.Errorf("the reply routing of an earlier installation was not taken away:\n%s", calls)
	}
}

func TestNetworkStepFailsWhereAcceptRedirectsCannotBeWritten(t *testing.T) {
	// /proc/sys is read-only in Docker: the error names the compose line
	p := containerPaths(t)
	fakeCommands(t, map[string]string{"nft": "", "ip": ""})
	old := syncNftFn
	syncNftFn = func(Paths, bool, bool, string) error { return nil }
	defer func() { syncNftFn = old }()
	ins := &Installer{Paths: p, Version: "1.2.3"}
	err := ins.stepNetworkContainer(Config{LAN: []string{"no-such-nic0"}, PanelPort: 80, ReplyVia: true, Router: "10.0.0.1"}, newJob(containerInstallSteps))
	if err == nil || !strings.Contains(err.Error(), "sysctls") || !strings.Contains(err.Error(), "accept_redirects=0") {
		t.Fatalf("%v", err)
	}
}

func TestTeardownRemovesOnlyWhatTheInstallationLoaded(t *testing.T) {
	p := containerPaths(t)
	bin := fakeCommands(t, map[string]string{"nft": "", "ip": ""})
	s, _ := testSup(t, p, shSpec("atom-sing-box", `exec sleep 30`), shSpec("atom-mihomo", `exec sleep 30`), shSpec("atom-dns", `exec sleep 30`))
	closeAll, err := serveContainerSockets(p, s, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeAll()
	if err := s.Start(bg, "atom-dns"); err != nil {
		t.Fatal(err)
	}
	containerTeardown(p, stderrLogger{})
	if st := s.list()[2].State; st != "stopped" {
		t.Errorf("atom-dns %s", st)
	}
	calls, _ := os.ReadFile(filepath.Join(bin, "calls"))
	for _, want := range []string{"nft delete table inet atom", "ip rule del fwmark " + fwmark + " lookup " + routeTable, "ip route flush table " + routeTable, "ip rule del fwmark " + replyMark} {
		if !strings.Contains(string(calls), want) {
			t.Errorf("missing %q in\n%s", want, calls)
		}
	}
	if strings.Contains(string(calls), "atom_wan") || strings.Contains(string(calls), "ip_forward") {
		t.Errorf("a gateway's things were touched:\n%s", calls)
	}
}

func TestContainerBackupItemsLeaveTheImageAlone(t *testing.T) {
	p := containerPaths(t)
	if err := os.MkdirAll(p.Data()+"/sub_config", 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(p.State(), InstallState{Version: "1.0.0", Complete: true, LAN: []string{"eth0"}})
	os.WriteFile(p.Etc("atom.nft"), []byte("table inet atom\n"), 0o644)
	os.WriteFile(p.Data()+"/sub_config/atom_rules.yaml", []byte("x"), 0o644)
	os.MkdirAll(p.Backup()+"/reset-old/data", 0o755) // an earlier backup must not be copied into this one
	os.WriteFile(p.Backup()+"/reset-old/data/big", []byte("x"), 0o644)
	ins := &Installer{Paths: p}
	dir := filepath.Join(p.Backup(), "reconfigure-test")
	if err := ins.makeBackup(dir); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"etc/install.json", "etc/atom.nft", "data/sub_config/atom_rules.yaml", absentList} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	for _, f := range []string{"bin", "share", "units", "reset-old"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			t.Errorf("%s was backed up", f)
		}
	}
}

func TestEnsurePanelPort(t *testing.T) {
	p := containerPaths(t)
	os.MkdirAll(p.Data(), 0o755)
	cfg := "api:\n  http: \"0.0.0.0:41234\"\n"
	os.WriteFile(p.Data()+"/config_custom.yaml", []byte(cfg), 0o644)
	changed, err := ensurePanelPort(p, &InstallState{PanelPort: 80})
	if err != nil || !changed {
		t.Fatalf("%v %v", changed, err)
	}
	data, _ := os.ReadFile(p.Data() + "/config_custom.yaml")
	if !strings.Contains(string(data), `"0.0.0.0:80"`) {
		t.Errorf("%s", data)
	}
	if changed, _ := ensurePanelPort(p, &InstallState{PanelPort: 80}); changed {
		t.Error("changed again")
	}
}

// ---- reconfigure ----

func installedContainer(t *testing.T, p Paths) {
	t.Helper()
	if err := writeJSON(p.State(), InstallState{Version: "1.2.3", Complete: true, Topology: "bypass", LAN: []string{"eth0"}, PanelPort: 8080, ReplyVia: "10.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(p.Data()+"/webinfo", 0o755)
	os.WriteFile(p.Data()+"/webinfo/atom_auth.json", []byte(`{"key":"k"}`), 0o600)
	os.WriteFile(p.Data()+"/webinfo/atom_proxy.json", []byte(`{"nodes":[]}`), 0o600)
	os.WriteFile(p.Data()+"/.atom-data", []byte(`{"version":"1.2.3"}`), 0o644)
	os.WriteFile(p.Data()+"/config_custom.yaml", []byte("api:\n  http: \"0.0.0.0:8080\"\n"), 0o644)
}

func TestReconfigureOpensAWizardBesideThePanel(t *testing.T) {
	p := containerPaths(t)
	installedContainer(t, p)
	s, _ := testSup(t, p, shSpec("atom-sing-box", `exec sleep 30`), shSpec("atom-mihomo", `exec sleep 30`), shSpec("atom-dns", `exec sleep 30`))
	host := newWizardHost(p, s)
	setWizardHost(host)
	defer setWizardHost(nil)
	closeAll, err := serveContainerSockets(p, s, host)
	if err != nil {
		t.Fatal(err)
	}
	defer closeAll()
	c := containerInit{socket: p.Run(controlSocket)}
	var first, second reconfigureReply
	if err := c.call(5*time.Second, "POST", "/reconfigure", map[string]bool{}, &first); err != nil {
		t.Fatal(err)
	}
	defer host.cur.server.stop(false)
	if first.Port == 0 || len(first.Code) != 6 || len(first.URLs) == 0 {
		t.Fatalf("%+v", first)
	}
	if err := c.call(5*time.Second, "POST", "/reconfigure", map[string]bool{}, &second); err != nil || second.Port != first.Port || second.Code != first.Code {
		t.Errorf("a second run must show the same wizard: %+v %v", second, err)
	}
	// the wizard answers, with the installed network as its current one
	req, _ := http.NewRequest("GET", "http://127.0.0.1:"+strconv.Itoa(first.Port)+"/api/state", nil)
	req.Header.Set("X-Atom-Install-Code", first.Code)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var st struct {
		Deployment  string `json:"deployment"`
		Reconfigure bool   `json:"reconfigure"`
		Current     Config `json:"current"`
	}
	if err := json.Unmarshal(data, &st); err != nil || st.Deployment != "container" || !st.Reconfigure ||
		st.Current.PanelPort != 8080 || !st.Current.ReplyVia || st.Current.Router != "10.0.0.1" || st.Current.LAN[0] != "eth0" {
		t.Errorf("%v %s", err, data)
	}
	// the operation lock is held: a reset cannot start meanwhile
	if _, err := operationLock(p); !errors.Is(err, errMaintenanceBusy) {
		t.Errorf("operation lock: %v", err)
	}
}

func TestReconfigureNeedsAnInstallation(t *testing.T) {
	p := containerPaths(t)
	s, _ := testSup(t, p, shSpec("atom-dns", `exec sleep 30`))
	host := newWizardHost(p, s)
	if _, err := host.reconfigure(); err == nil || !strings.Contains(err.Error(), "没有已完成的安装") {
		t.Errorf("%v", err)
	}
	if _, err := operationLock(p); err != nil {
		t.Errorf("the lock was kept: %v", err)
	}
}

// ---- the reset ----

func TestResetFromThePanelHandsOverToTheContainerWizard(t *testing.T) {
	p := containerPaths(t)
	fakeCommands(t, map[string]string{"nft": "", "ip": ""})
	installedContainer(t, p)
	os.MkdirAll(p.Data()+"/rule", 0o755)
	os.WriteFile(p.Data()+"/rule/geosite.dat", []byte("big"), 0o644)
	os.MkdirAll(p.Backup()+"/reconfigure-20250101-000000.000000000", 0o755)
	os.WriteFile(p.Backup()+"/reconfigure-20250101-000000.000000000/keep", []byte("x"), 0o644)
	s, _ := testSup(t, p, shSpec("atom-sing-box", `exec sleep 30`), shSpec("atom-mihomo", `exec sleep 30`), shSpec("atom-dns", `exec sleep 30`))
	host := newWizardHost(p, s)
	setWizardHost(host)
	defer setWizardHost(nil)
	closeAll, err := serveContainerSockets(p, s, host)
	if err != nil {
		t.Fatal(err)
	}
	defer closeAll()
	if err := s.Start(bg, "atom-dns"); err != nil {
		t.Fatal(err)
	}
	maint := &http.Client{Transport: unixTransport(p.Run(maintenanceSocket))}
	call := func(method, path string) (int, map[string]any) {
		req, _ := http.NewRequest(method, "http://m"+path, strings.NewReader("{}"))
		resp, err := maint.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		var m map[string]any
		json.Unmarshal(data, &m)
		return resp.StatusCode, m
	}
	if code, m := call("POST", "/reset"); code != 202 {
		t.Fatalf("reset: %d %v", code, m)
	}
	// the panel finds the wizard in the job: its port, code and addresses
	var job map[string]any
	waitFor(t, "awaiting_browser", func() bool {
		_, m := call("GET", "/")
		job, _ = m["job"].(map[string]any)
		return job != nil && job["phase"] == "awaiting_browser"
	})
	port, code := int(job["wizard_port"].(float64)), job["wizard_code"].(string)
	if port == 0 || len(code) != 6 || job["kind"] != "reset" {
		t.Fatalf("%v", job)
	}
	defer host.cur.server.stop(false)
	if urls, _ := job["wizard_urls"].([]any); len(urls) == 0 {
		t.Errorf("no addresses: %v", job)
	}
	// a second reset while this one waits is refused
	if c, _ := call("POST", "/reset"); c != 409 {
		t.Errorf("second reset: %d", c)
	}
	// the browser confirms
	wizard := func(method, path string) map[string]any {
		req, _ := http.NewRequest(method, "http://127.0.0.1:"+strconv.Itoa(port)+path, strings.NewReader("{}"))
		req.Header.Set("X-Atom-Install-Code", code)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		var m map[string]any
		json.Unmarshal(data, &m)
		return m
	}
	wizard("POST", "/api/reset")
	waitFor(t, "reset ready", func() bool {
		r, _ := wizard("GET", "/api/state")["reset"].(map[string]any)
		return r != nil && r["phase"] == "ready"
	})
	r := wizard("GET", "/api/state")["reset"].(map[string]any)
	backup := r["backup"].(string)
	if !strings.HasPrefix(backup, p.Backup()+"/reset-") {
		t.Errorf("backup %s", backup)
	}
	for _, f := range []string{"data/rule/geosite.dat", "data/webinfo/atom_auth.json", "data/.atom/install.json", "etc/install.json"} {
		if _, err := os.Stat(filepath.Join(backup, f)); err != nil {
			t.Errorf("backup lacks %s: %v", f, err)
		}
	}
	if _, err := os.Stat(filepath.Join(backup, "data/.atom/backup")); err == nil {
		t.Error("the backup holds the backups")
	}
	// the data is empty but for the backups, and nothing runs
	var left []string
	filepath.Walk(p.Data(), func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && !strings.HasPrefix(path, p.Backup()) {
			left = append(left, path)
		}
		return nil
	})
	if len(left) != 0 {
		t.Errorf("left in the data: %v", left)
	}
	if _, err := os.Stat(p.Backup() + "/reconfigure-20250101-000000.000000000/keep"); err != nil {
		t.Errorf("an old backup went: %v", err)
	}
	for _, sv := range s.list() {
		if sv.State != "stopped" {
			t.Errorf("%s runs", sv.Name)
		}
	}
	// the wizard starts from the network of the installation before
	prev := r["previous"].(map[string]any)
	if prev["panel_port"].(float64) != 8080 || prev["reply_via"] != true || prev["router"] != "10.0.0.1" {
		t.Errorf("previous %v", prev)
	}
	if code, m := call("GET", "/"); code != 503 {
		t.Errorf("overview after the reset: %d %v", code, m) // no installation any more
	}
}

func unixTransport(path string) *http.Transport {
	return &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}
}
