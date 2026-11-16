package main

import (
	"fmt"
	"os"
	"path/filepath"
)

func (p Paths) writeMaintenanceService(log Logger) error {
	unit := fmt.Sprintf(`[Unit]
Description=Atom maintenance
After=network-online.target

[Service]
ExecStart=%s maintenance
Restart=on-failure
RestartSec=2

[Install]
WantedBy=multi-user.target
`, p.Bin("atom-installer"))
	if p.OpenRC {
		unit = fmt.Sprintf(`#!/sbin/openrc-run
description="Atom maintenance"
supervisor=supervise-daemon
command=%s
command_args="maintenance"
respawn_delay=2
respawn_max=0
output_log=%s/atom-maintenance.log
error_log=%s/atom-maintenance.log

depend() {
	need net
}

start_pre() {
	checkpath -d -m 0755 %s
}
`, p.Bin("atom-installer"), openRCLogDir, openRCLogDir, openRCLogDir)
	}
	if err := writeFileAtomic(p.unitFile(maintenanceUnitName), []byte(unit), p.unitMode()); err != nil {
		return err
	}
	return p.Init().Reload(log)
}

func (p Paths) enableMaintenance(log Logger) error {
	if err := p.writeMaintenanceService(log); err != nil {
		return err
	}
	if err := p.Init().Enable(log, maintenanceUnitName); err != nil {
		return err
	}
	return p.Init().Restart(log, maintenanceUnitName)
}

// Staging belongs to one shell invocation. Keep support for the original pkg
// path so older install.sh versions can still bootstrap a maintenance release.
func (p Paths) managedPackage(pkg string) bool {
	path, err := filepath.Abs(pkg)
	if err != nil || filepath.Dir(path) != p.Root {
		return false
	}
	ok, _ := filepath.Match("pkg.*", filepath.Base(path))
	return path == p.Pkg() || ok
}

func (p Paths) removeMaintenance(log Logger) error {
	unit := p.unitFile(maintenanceUnitName)
	if _, err := os.Stat(unit); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := p.Init().DisableNow(log, maintenanceUnitName); err != nil {
		return err
	}
	if err := os.Remove(unit); err != nil {
		return err
	}
	p.Init().Forget(log, maintenanceUnitName)
	return nil
}
