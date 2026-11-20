package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecoverMigrationPutsTheBackupBack(t *testing.T) {
	p := containerPaths(t)
	cfg := filepath.Join(p.Data(), "config_custom.yaml")
	for path, text := range map[string]string{cfg: "old", p.State(): `{"version":"0.1.0"}`, filepath.Join(p.Data(), "webinfo", "x.json"): "mine"} {
		if err := writeFileAtomic(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := recoverMigration(p, stderrLogger{}); err != nil { // nothing recorded: nothing to do
		t.Fatal(err)
	}
	backup := backupPath(p, "0.1.0")
	if err := newContainerInstaller(p).makeBackup(backup); err != nil {
		t.Fatal(err)
	}
	// the migration got as far as the new files and the new version, and was stopped
	os.WriteFile(cfg, []byte("new"), 0o644)
	os.WriteFile(p.State(), []byte(`{"version":"0.2.0"}`), 0o644)
	if err := writeJSON(migrationRecord(p), &migration{From: "0.1.0", To: "0.2.0", Backup: filepath.Base(backup)}); err != nil {
		t.Fatal(err)
	}
	var log syncBuf
	if err := recoverMigration(p, newSupervisor(p, nil, &log)); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(cfg); string(b) != "old" {
		t.Fatalf("config_custom.yaml = %q", b)
	}
	if st, err := loadState(p); err != nil || st.Version != "0.1.0" {
		t.Fatalf("state %+v %v", st, err)
	}
	if b, _ := os.ReadFile(filepath.Join(p.Data(), "webinfo", "x.json")); string(b) != "mine" {
		t.Fatalf("webinfo changed: %q", b)
	}
	if _, err := os.Stat(migrationRecord(p)); !os.IsNotExist(err) {
		t.Fatalf("record is still there: %v", err)
	}
	if !strings.Contains(log.String(), "没有完成") {
		t.Fatalf("log: %s", log.String())
	}
	// a record whose backup is gone is an error and stays
	writeJSON(migrationRecord(p), &migration{From: "0.1.0", To: "0.2.0", Backup: "none"})
	os.WriteFile(cfg, []byte("new"), 0o644)
	if err := recoverMigration(p, stderrLogger{}); err == nil {
		t.Fatal("a missing backup is not a restore")
	}
	if _, err := os.Stat(migrationRecord(p)); err != nil {
		t.Fatal("the record has to stay")
	}
}

func TestMigrationChecksTheConfigBeforeChangingAnything(t *testing.T) {
	p := containerPaths(t)
	old := version
	version = "0.2.0"
	defer func() { version = old }()
	st := &InstallState{Version: "0.1.0", PanelPort: 9996, Complete: true}
	cfg := filepath.Join(p.Data(), "config_custom.yaml")
	os.MkdirAll(filepath.Join(p.Root, "bin"), 0o755)
	if err := os.WriteFile(p.Bin("atomdns"), []byte("#!/bin/sh\necho bad config >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(p.Shipped(), "config"), 0o755)
	os.WriteFile(filepath.Join(p.Shipped(), "config", "config_custom.yaml"), []byte("api:\n  http: \"0.0.0.0:9996\"\n"), 0o644)
	writeFileAtomic(cfg, []byte("old"), 0o644)
	writeJSON(p.State(), st)
	if _, err := migrateData(p, st, stderrLogger{}); err == nil || !strings.Contains(err.Error(), "候选版本无法读取现有配置") {
		t.Fatalf("err = %v", err)
	}
	if b, _ := os.ReadFile(cfg); string(b) != "old" {
		t.Fatalf("config changed: %q", b)
	}
	if _, err := os.Stat(migrationRecord(p)); !os.IsNotExist(err) {
		t.Fatal("a record was written")
	}
	if e, _ := os.ReadDir(p.Backup()); len(e) != 0 {
		t.Fatalf("a backup was made: %v", e)
	}
	if st.Version != "0.1.0" {
		t.Fatalf("version %s", st.Version)
	}
}
