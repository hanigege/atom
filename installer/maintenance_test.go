package main

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSignedAtomRelease(t *testing.T) {
	dir := t.TempDir()
	version := "1.2.3"
	for _, platform := range packagePlatforms {
		path := filepath.Join(dir, releasePackageName(version, platform))
		if err := os.WriteFile(path, []byte(platform), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := signUpdateRelease(dir, version, key); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, updateManifestName))
	if err != nil {
		t.Fatal(err)
	}
	release, err := verifyRelease(raw, public)
	if err != nil {
		t.Fatal(err)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		for _, distro := range []string{"debian", "ubuntu", "alpine"} {
			platform, err := updatePlatform(arch, distro)
			want := arch
			if distro == "alpine" {
				want += "-musl"
			}
			if err != nil || platform != want || release.Assets[platform].Size != int64(len(want)) {
				t.Fatalf("%s/%s: %s %v", arch, distro, platform, err)
			}
		}
	}
	var signed signedRelease
	if err := json.Unmarshal(raw, &signed); err != nil {
		t.Fatal(err)
	}
	signed.Payload = bytes.Replace(signed.Payload, []byte("1.2.3"), []byte("9.9.9"), 1)
	modified, err := json.Marshal(signed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifyRelease(modified, public); err == nil {
		t.Fatal("accepted tampered manifest")
	}
}

func TestVerifiedPackageLeavesNoBadFile(t *testing.T) {
	data := "atom candidate"
	asset := releaseAsset{Size: int64(len(data)), SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(data)))}
	target := filepath.Join(t.TempDir(), "asset.tar.gz")
	for _, invalid := range []string{"short", data + " extra", strings.Repeat("a", len(data))} {
		err := storeUpdatePackage(target, asset, strings.NewReader(invalid))
		if _, statErr := os.Stat(target); err == nil || !os.IsNotExist(statErr) {
			t.Fatalf("corrupt file result: %v; stat %v", err, statErr)
		}
	}
	if err := storeUpdatePackage(target, asset, strings.NewReader(data)); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateSourceFallbackOrder(t *testing.T) {
	sources := updateSources(18762)
	var used []string
	err := trySources(sources, &recLogger{}, func(current string) { used = append(used, current) }, func(source downloadSource) error {
		if source.prefix == updateMirrors[0] {
			return nil
		}
		return fmt.Errorf("offline or invalid package")
	})
	want := []string{"本机 mixed · 127.0.0.1:18762", "GitHub 直连", "gh-proxy.com"}
	if err != nil || !reflect.DeepEqual(used, want) {
		t.Fatalf("got %v (%v), want %v", used, err, want)
	}
}

func TestMixedSourceUsesSOCKSRemoteResolution(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	proxyResult := make(chan error, 1)
	go func() { proxyResult <- answerSOCKS(listener) }()
	port := listener.Addr().(*net.TCPAddr).Port
	err = updateSources(port)[0].fetch("http://release.invalid/asset", 2*time.Second, func(r io.Reader) error {
		body, err := io.ReadAll(r)
		if err != nil {
			return err
		}
		if string(body) != "mock release" {
			return fmt.Errorf("unexpected body %q", body)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-proxyResult; err != nil {
		t.Fatal(err)
	}
}

func answerSOCKS(listener net.Listener) error {
	conn, err := listener.Accept()
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return err
	}
	reader := bufio.NewReader(conn)
	var greeting [3]byte
	if _, err := io.ReadFull(reader, greeting[:]); err != nil {
		return err
	}
	if greeting != [3]byte{5, 1, 0} {
		return fmt.Errorf("unexpected greeting %v", greeting)
	}
	if _, err := conn.Write([]byte{5, 0}); err != nil {
		return err
	}
	var request [5]byte
	if _, err := io.ReadFull(reader, request[:]); err != nil {
		return err
	}
	if request[3] != 3 {
		return fmt.Errorf("SOCKS did not forward a domain: %v", request)
	}
	address := make([]byte, int(request[4])+2)
	if _, err := io.ReadFull(reader, address); err != nil {
		return err
	}
	if string(address[:len(address)-2]) != "release.invalid" {
		return fmt.Errorf("unexpected SOCKS target %q", address)
	}
	if _, err := conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 80}); err != nil {
		return err
	}
	r, err := http.ReadRequest(reader)
	if err != nil || r.URL.Path != "/asset" {
		return fmt.Errorf("unexpected proxied request: %v %v", r, err)
	}
	_, err = io.WriteString(conn, "HTTP/1.1 200 OK\r\nConnection: close\r\nContent-Length: 12\r\n\r\nmock release")
	return err
}

func TestNetworkTrialConfirmation(t *testing.T) {
	deadline := time.Now().Add(time.Minute)
	m := maintenance{
		job:     &maintenanceJob{ID: "trial", Phase: "awaiting_confirmation", Deadline: &deadline, NewPanel: 8088, NewIPs: []string{"192.0.2.4"}},
		confirm: make(chan struct{}, 1),
	}
	confirmation := m.confirm
	try := func(local string) int {
		r := httptest.NewRequest("POST", "/confirm", strings.NewReader(`{"id":"trial"}`))
		r.Header.Set("X-Atom-Local-Addr", local)
		w := httptest.NewRecorder()
		m.handler(w, r)
		return w.Code
	}
	for _, addr := range []string{"192.0.2.4:80", "192.0.2.5:8088", "127.0.0.1:8088"} {
		if code := try(addr); code != 409 {
			t.Fatalf("%s: got %d, want 409", addr, code)
		}
	}
	if code := try("192.0.2.4:" + strconv.Itoa(m.job.NewPanel)); code != 202 {
		t.Fatalf("new LAN endpoint: got %d, want 202", code)
	}
	select {
	case <-confirmation:
	default:
		t.Fatal("confirmation did not reach worker")
	}
	if code := try("192.0.2.4:8088"); code != 409 {
		t.Fatalf("duplicate confirmation got %d", code)
	}
}

func TestShellAndPanelShareMaintenanceLock(t *testing.T) {
	p := Paths{Root: filepath.Join(t.TempDir(), "atom")}
	held, err := operationLock(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cliOperationLock(p); err != errMaintenanceBusy {
		t.Fatalf("second operation lock: %v", err)
	}
	held.Close()
	m := maintenance{paths: p}
	w := httptest.NewRecorder()
	m.handler(w, httptest.NewRequest("POST", "/check", strings.NewReader("{}")))
	if w.Code != 503 {
		t.Fatalf("expected no installation, got %d", w.Code)
	}
}

func TestRestartRecoversUncommittedMaintenance(t *testing.T) {
	for _, tc := range []struct {
		name, kind string
		openRC     bool
	}{
		{"systemd/network", "network", false},
		{"openrc/network", "network", true},
		{"systemd/upgrade", "upgrade", false},
		{"openrc/upgrade", "upgrade", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/atom/status":
					io.WriteString(w, `{"singbox":{"running":true,"in_sync":true}}`)
				case "/api/v1/system/health":
					io.WriteString(w, `{"ready":true}`)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			base := t.TempDir()
			p := Paths{Root: filepath.Join(base, "atom"), UnitDir: base, OpenRC: tc.openRC}
			tools := filepath.Join(base, "tools")
			if err := os.Mkdir(tools, 0o755); err != nil {
				t.Fatal(err)
			}
			for tool, script := range map[string]string{
				"systemctl":  "echo active",
				"rc-service": "exit 0",
				"nft":        "exit 1",
				"sync":       "exit 0",
				"ss": `echo 'tcp LISTEN 0 128 0.0.0.0:53 0.0.0.0:* users:(("atomdns",pid=123,fd=3))'
echo 'udp UNCONN 0 0 0.0.0.0:53 0.0.0.0:* users:(("atomdns",pid=123,fd=4))'`,
			} {
				if err := os.WriteFile(filepath.Join(tools, tool), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", tools+":"+os.Getenv("PATH"))
			installed := InstallState{Version: "1.2.3", Complete: true, Topology: "bypass", PanelPort: server.Listener.Addr().(*net.TCPAddr).Port}
			if err := writeJSON(p.State(), installed); err != nil {
				t.Fatal(err)
			}
			if err := saveChanges(p, &Changes{}); err != nil {
				t.Fatal(err)
			}
			config := filepath.Join(p.Data(), "config_custom.yaml")
			userData := filepath.Join(p.Data(), "webinfo", "settings.json")
			binary := p.Bin("atomdns")
			before := map[string]string{config: "old panel", userData: "user data", binary: "old program"}
			for path, content := range before {
				if err := writeFileAtomic(path, []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			ins := &Installer{Paths: p}
			backup := backupPath(p, installed.Version)
			worker := &maintenance{paths: p, job: &maintenanceJob{Kind: tc.kind}}
			if err := worker.begin(ins, backup, nil); err != nil {
				t.Fatal(err)
			}
			trial := installed
			if tc.kind == "network" {
				if err := worker.change(func(job *maintenanceJob) { job.Phase = "awaiting_confirmation" }); err != nil {
					t.Fatal(err)
				}
				trial.PanelPort = 8888
			} else {
				trial.Version = "1.2.4"
				if err := writeFileAtomic(binary, []byte("candidate program"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := writeJSON(p.State(), trial); err != nil {
				t.Fatal(err)
			}
			if err := writeFileAtomic(config, []byte("trial panel"), 0o644); err != nil {
				t.Fatal(err)
			}

			restarted := &maintenance{paths: p}
			if err := restarted.recover(); err != nil {
				t.Fatal(err)
			}
			job := restarted.snapshot()
			if !job.Done || job.Blocking || job.Phase != "restored" {
				t.Fatalf("after restart: %+v", job)
			}
			current, err := loadState(p)
			if err != nil || current.PanelPort != installed.PanelPort || current.Version != installed.Version {
				t.Fatalf("restored state: %+v %v", current, err)
			}
			for path, expected := range before {
				if actual, err := os.ReadFile(path); err != nil || string(actual) != expected {
					t.Fatalf("%s: %q %v", path, actual, err)
				}
			}
		})
	}
}
