package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const logrotateMarker = "# Written by atom-installer.\n"

func (p Paths) logrotateConfig() string {
	return filepath.Join(filepath.Dir(p.UnitDir), "logrotate.d", "atom-installer")
}

func configureOpenRCLogs(p Paths, log Logger) error {
	path := p.logrotateConfig()
	if data, err := os.ReadFile(path); err == nil {
		if !strings.HasPrefix(string(data), logrotateMarker) {
			return fmt.Errorf("%s 已经存在且不是安装器创建的", path)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if _, err := exec.LookPath("logrotate"); err != nil {
		ch, err := loadChanges(p)
		if err != nil {
			return err
		}
		if err := runCmd(log, 10*time.Minute, "apk", "add", "--no-cache", "logrotate"); err != nil {
			return err
		}
		ch.PkgInstalled = append(ch.PkgInstalled, "logrotate")
		if err := saveChanges(p, ch); err != nil {
			return err
		}
	}
	config := fmt.Sprintf(`%s%s/*.log {
    size 5M
    rotate 3
    missingok
    notifempty
    compress
    copytruncate
}
`, logrotateMarker, openRCLogDir)
	return writeFileAtomic(path, []byte(config), 0o644)
}

func removeOpenRCLogRotation(p Paths) error {
	path := p.logrotateConfig()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if strings.HasPrefix(string(data), logrotateMarker) {
		return os.Remove(path)
	}
	return nil
}
