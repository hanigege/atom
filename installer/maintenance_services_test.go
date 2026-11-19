package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// servicesSystemd stands in for systemctl: is-active answers the line in <dir>/state-<unit> (no file: "unknown"
// with exit 4, as for a unit systemd does not know) and sleeps the seconds in <dir>/slow first; restart sleeps
// the seconds in <dir>/restart-slow.
func servicesSystemd(t *testing.T, states map[string]string) (Paths, string) {
	t.Helper()
	p := Paths{Root: t.TempDir(), UnitDir: t.TempDir()}
	installedWithServices(t, p)
	dir := fakeCommands(t, map[string]string{"systemctl": `case "$1" in
is-active) [ -e "$D/slow" ] && sleep "$(cat "$D/slow")"
  [ -e "$D/state-$2" ] || { echo unknown; exit 4; }
  s="$(cat "$D/state-$2")"; echo "$s"; [ "$s" = active ] && exit 0; exit 3;;
restart) [ -e "$D/restart-slow" ] && sleep "$(cat "$D/restart-slow")";;
esac`})
	for unit, state := range states {
		if err := os.WriteFile(filepath.Join(dir, "state-"+unit), []byte(state+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := restartDelay
	restartDelay = 0
	t.Cleanup(func() { restartDelay = old })
	return p, dir
}

func getServices(t *testing.T, m *maintenance) []serviceStatus {
	t.Helper()
	w := httptest.NewRecorder()
	m.handler(w, httptest.NewRequest("GET", "/services", nil))
	var body struct {
		Services []serviceStatus `json:"services"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	return body.Services
}

func TestServicesSystemd(t *testing.T) {
	p, _ := servicesSystemd(t, map[string]string{
		"atom-maintenance.service": "active", "atom-dns.service": "failed", "atom-sing-box.service": "inactive",
	})
	got := getServices(t, &maintenance{paths: p})
	want := []serviceStatus{{"atom-maintenance", "running"}, {"atom-dns", "failed"}, {"atom-sing-box", "stopped"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("only the installed services, in order: %+v", got)
	}
}

func TestServicesQueryHasADeadline(t *testing.T) {
	p, dir := servicesSystemd(t, map[string]string{"atom-dns.service": "active"})
	os.WriteFile(filepath.Join(dir, "slow"), []byte("5"), 0o644)
	old := serviceQueryWait
	serviceQueryWait = 300 * time.Millisecond
	t.Cleanup(func() { serviceQueryWait = old })
	start := time.Now()
	for _, s := range getServices(t, &maintenance{paths: p}) {
		if s.State != "unknown" {
			t.Errorf("a query cut short says nothing: %+v", s)
		}
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("the queries share one deadline: %v", time.Since(start))
	}
}

func TestServicesOpenRC(t *testing.T) {
	p, stub := openRCMachine(t)
	for name, code := range map[string]string{"atom-dns": "32", "atom-sing-box": "3", "atom-mihomo": "16"} {
		os.WriteFile(filepath.Join(stub, "status-"+name), []byte(code), 0o644)
	}
	got := getServices(t, &maintenance{paths: p})
	want := []serviceStatus{{"atom-maintenance", "running"}, {"atom-dns", "failed"}, {"atom-sing-box", "stopped"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%+v", got)
	}
}

// Of the two kernels only the selected one is shown and may be restarted: the other shares its ports and nft
// sets.
func TestServicesOnlyTheSelectedKernel(t *testing.T) {
	p, stub := openRCMachine(t)
	m := &maintenance{paths: p}
	w := postRestart(m, `{"service":"atom-mihomo"}`)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "不是当前使用的代理内核") {
		t.Errorf("%d %s", w.Code, w.Body.String())
	}
	if err := writeFileAtomic(filepath.Join(p.Data(), "webinfo", "atom_proxy.json"), []byte(`{"settings":{"backend":"mihomo"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got := getServices(t, m)
	want := []serviceStatus{{"atom-maintenance", "running"}, {"atom-dns", "running"}, {"atom-mihomo", "running"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%+v", got)
	}
	if w := postRestart(m, `{"service":"atom-sing-box"}`); w.Code != 400 {
		t.Errorf("%d %s", w.Code, w.Body.String())
	}
	if w := postRestart(m, `{"service":"atom-mihomo"}`); w.Code != 202 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	waitCall(t, stub, "rc-service atom-mihomo restart")
	waitUnlocked(t, p)
}

func postRestart(m *maintenance, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	m.handler(w, httptest.NewRequest("POST", "/restart", strings.NewReader(body)))
	return w
}

// waitCall waits for the stand-ins in dir to have made the call line.
func waitCall(t *testing.T, dir, line string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); calls(dir, line) == 0; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			data, _ := os.ReadFile(filepath.Join(dir, "calls"))
			t.Fatalf("no %q in:\n%s", line, data)
		}
	}
}

func TestRestartRequests(t *testing.T) {
	p, _ := servicesSystemd(t, nil)
	m := &maintenance{paths: p}
	for _, body := range []string{`{"service":"sshd"}`, `{"service":"atom-mihomo"}`, `{}`, `{"service":"atom-dns","x":1}`, `nonsense`} {
		if w := postRestart(m, body); w.Code != 400 {
			t.Errorf("%s: %d %s", body, w.Code, w.Body.String())
		}
	}
	if err := m.save(maintenanceJob{ID: "job1", Kind: "upgrade", Phase: "applying"}); err != nil {
		t.Fatal(err)
	}
	if w := postRestart(m, `{"service":"atom-dns"}`); w.Code != 409 {
		t.Errorf("a maintenance job runs: %d", w.Code)
	}
	m.finish("failed", nil)
	m.job.Blocking = true
	if w := postRestart(m, `{"service":"atom-dns"}`); w.Code != 409 {
		t.Errorf("a job waits for its recovery: %d", w.Code)
	}
	m.job.Blocking = false
	lock, err := operationLock(p)
	if err != nil {
		t.Fatal(err)
	}
	if w := postRestart(m, `{"service":"atom-dns"}`); w.Code != 409 {
		t.Errorf("an installer command runs: %d", w.Code)
	}
	lock.Close()
}

func TestRestartAnswersThenRestarts(t *testing.T) {
	p, dir := servicesSystemd(t, nil)
	os.WriteFile(filepath.Join(dir, "restart-slow"), []byte("1"), 0o644)
	m := &maintenance{paths: p}
	w := postRestart(m, `{"service":"atom-dns"}`)
	if w.Code != 202 || strings.TrimSpace(w.Body.String()) != `{"ok":true}` {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	waitCall(t, dir, "systemctl restart atom-dns.service")
	if w := postRestart(m, `{"service":"atom-sing-box"}`); w.Code != 409 {
		t.Errorf("one restart at a time: %d", w.Code)
	}
	waitUnlocked(t, p)
}

// waitUnlocked waits for a restart to let the operation lock go.
func waitUnlocked(t *testing.T, p Paths) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		lock, err := operationLock(p)
		if err == nil {
			lock.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
	}
}

func TestRestartTheMaintenanceServiceItself(t *testing.T) {
	old := selfRestartHold
	selfRestartHold = 500 * time.Millisecond
	t.Cleanup(func() { selfRestartHold = old })
	p, dir := servicesSystemd(t, nil)
	m := &maintenance{paths: p}
	if w := postRestart(m, `{"service":"atom-maintenance"}`); w.Code != 202 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	waitCall(t, dir, "systemctl restart --no-block atom-maintenance.service")
	// systemd only took the job: until it stops this process, nothing else may start
	if w := postRestart(m, `{"service":"atom-dns"}`); w.Code != 409 {
		t.Errorf("while the restart is pending: %d", w.Code)
	}
	waitUnlocked(t, p)

	p, stub := openRCMachine(t)
	if w := postRestart(&maintenance{paths: p}, `{"service":"atom-maintenance"}`); w.Code != 202 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	waitCall(t, stub, "rc-service atom-maintenance restart")
	// rc-service came back without stopping this process: the restart did not happen, the lock goes
	waitUnlocked(t, p)
}
