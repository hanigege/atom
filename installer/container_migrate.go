package main

// The data migration of a new image (docs/container.md 8): the same steps as the upgrade of the Linux version
// (check, backup, overlay the shipped files, verify), done by the entry process before and after it starts the
// services. The programs are in the image, so there is nothing to install.

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// migration is the record of one that is not finished. It is written once the backup is complete and removed
// when the new version is verified or the backup is put back, so that a stop (or a crash) in between is seen at
// the next start.
type migration struct {
	From, To string
	Backup   string // the name in the backup directory
}

func migrationRecord(p Paths) string { return p.Etc("migrating.json") }

// recoverMigration puts the backup of a migration back that did not finish; the migration then starts again.
func recoverMigration(p Paths, log Logger) error {
	var m migration
	if err := readJSON(migrationRecord(p), &m); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("读不到迁移记录 %s：%w", migrationRecord(p), err)
	}
	log.Logf("上一次数据迁移（%s → %s）没有完成，恢复备份 %s", m.From, m.To, m.Backup)
	return rollbackMigration(p, &m, log)
}

func rollbackMigration(p Paths, m *migration, log Logger) error {
	dir := filepath.Join(p.Backup(), m.Backup)
	if _, err := os.Stat(filepath.Join(dir, absentList)); err != nil { // makeBackup writes it last
		return fmt.Errorf("备份 %s 不完整：%w", dir, err)
	}
	if err := newContainerInstaller(p).restoreBackupFiles(dir); err != nil {
		return fmt.Errorf("恢复备份 %s 失败：%w", m.Backup, err)
	}
	if err := os.Remove(migrationRecord(p)); err != nil {
		return err
	}
	log.Logf("已恢复到 %s", m.From)
	pruneBackups(p.Backup(), 3) // a container that restarts and fails again does not fill the disk
	return nil
}

// migrateData is what runs before the network is loaded: the new version has to read the data, then the
// shipped files go over the data after a backup. st gets the new version.
func migrateData(p Paths, st *InstallState, log Logger) (*migration, error) {
	ins := newContainerInstaller(p)
	ins.Version = version // the one the entry process compared
	if err := checkLegacyTor(p); err != nil {
		return nil, err
	}
	if err := ins.checkUpgradeConfig(st.PanelPort, log); err != nil {
		return nil, err
	}
	backup := backupPath(p, st.Version)
	log.Logf("备份到 %s", backup)
	if err := ins.makeBackup(backup); err != nil {
		return nil, fmt.Errorf("备份: %w", err)
	}
	m := &migration{From: st.Version, To: version, Backup: filepath.Base(backup)}
	if err := writeJSON(migrationRecord(p), m); err != nil {
		return nil, fmt.Errorf("记录迁移失败: %w", err)
	}
	err := ins.installData(st.PanelPort, log)
	if err == nil {
		err = ins.writeNetworkFiles(st, log)
	}
	if err == nil {
		next := *st
		next.Version, next.UpdatedAt = version, time.Now()
		if err = writeJSON(p.State(), &next); err != nil {
			err = fmt.Errorf("记录新版本失败: %w", err)
		} else {
			*st = next
		}
	}
	if err != nil {
		log.Logf("数据迁移失败：%v", err)
		if rerr := rollbackMigration(p, m, log); rerr != nil {
			return nil, fmt.Errorf("%w（%v）", err, rerr)
		}
		return nil, err
	}
	return m, nil
}

// finishMigration runs after the services have started: atomdns answers and the kernel runs the latest
// configuration, as after the upgrade of the Linux version. Then the migration is done.
func finishMigration(p Paths, st *InstallState, m *migration) error {
	api := newAPIClient(st.PanelPort, p.Data())
	if _, err := api.waitStatus(60*time.Second, func(*apiStatus) bool { return true }); err != nil {
		return fmt.Errorf("atomdns 的管理接口没有起来: %w", err)
	}
	if _, err := api.waitStatus(90*time.Second, func(s *apiStatus) bool { return s.SingBox.Running && s.SingBox.InSync }); err != nil {
		return fmt.Errorf("升级后 sing-box 没有处于最新配置: %w", err)
	}
	if err := os.Remove(migrationRecord(p)); err != nil {
		return err
	}
	pruneBackups(p.Backup(), 3)
	return nil
}
