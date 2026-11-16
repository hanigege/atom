package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	maintenanceSocket = "maintenance.sock"
	maintenanceRecord = "maintenance-job.json"
	panelWriteLock    = "maintenance-writes.lock"
)

var errMaintenanceBusy = errors.New("已有安装或维护任务正在进行")

func exclusiveLock(path string, nonblocking bool) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	mode := syscall.LOCK_EX
	if nonblocking {
		mode |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(f.Fd()), mode); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errMaintenanceBusy
		}
		return nil, err
	}
	return f, nil
}

func operationLock(p Paths) (*os.File, error) {
	// The inode stays in place across uninstall/reinstall, so racing commands
	// cannot lock different inodes when the installation root is removed.
	return exclusiveLock(p.Root+".lock", true)
}

func cliOperationLock(p Paths) (*os.File, error) {
	lock, err := operationLock(p)
	if err != nil {
		return nil, err
	}
	var job maintenanceJob
	if err := readJSON(p.Run(maintenanceRecord), &job); (err == nil && job.Blocking && job.Phase != "recovery_failed") ||
		(err != nil && !os.IsNotExist(err)) {
		lock.Close()
		return nil, fmt.Errorf("维护记录有待恢复，请等待维护服务完成恢复")
	}
	return lock, nil
}

type maintenanceJob struct {
	ID         string     `json:"id"`
	Kind       string     `json:"kind"`
	Phase      string     `json:"phase"`
	Source     string     `json:"source,omitempty"`
	Version    string     `json:"version,omitempty"`
	Error      string     `json:"error,omitempty"`
	Backup     string     `json:"backup,omitempty"`
	Blocking   bool       `json:"blocking"`
	Done       bool       `json:"done"`
	Updated    time.Time  `json:"updated_at"`
	Deadline   *time.Time `json:"deadline,omitempty"`
	OldURLs    []string   `json:"old_urls,omitempty"`
	NewURLs    []string   `json:"new_urls,omitempty"`
	NewPanel   int        `json:"new_panel,omitempty"`
	NewIPs     []string   `json:"new_ips,omitempty"`
	SameAccess bool       `json:"same_access,omitempty"`
}

type maintenance struct {
	paths     Paths
	mu        sync.Mutex
	job       *maintenanceJob
	confirm   chan struct{}
	reload    chan struct{}
	reloading bool
}

func (m *maintenance) save(job maintenanceJob) error {
	job.Updated = time.Now()
	data, err := json.Marshal(job)
	if err == nil {
		err = writeFile(m.paths.Run(maintenanceRecord), data, 0o600, true)
	}
	if err == nil {
		m.job = &job
	}
	return err
}

func (m *maintenance) change(edit func(*maintenanceJob)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	job := *m.job
	edit(&job)
	return m.save(job)
}

func (m *maintenance) currentSource(source string) {
	if err := m.change(func(job *maintenanceJob) { job.Source = source }); err != nil {
		stderrLogger{}.Logf("记录下载来源：%v", err)
	}
}

func (m *maintenance) finish(phase string, failure error) error {
	return m.change(func(job *maintenanceJob) {
		job.Done, job.Phase = true, phase
		job.Blocking = phase == "recovery_failed"
		job.Deadline = nil
		job.Error = ""
		if failure != nil {
			job.Error = failure.Error()
		}
	})
}

func (m *maintenance) failed(failure error) {
	stderrLogger{}.Logf("维护任务：%v", failure)
	if err := m.finish("failed", failure); err != nil {
		stderrLogger{}.Logf("保存任务结果：%v", err)
	}
}

func (m *maintenance) overview(w http.ResponseWriter) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, err := loadState(m.paths)
	if err != nil || !st.Complete {
		writeErr(w, 503, "没有已完成的 Atom 安装")
		return
	}
	stVersion := st.Version
	if m.job != nil && m.job.Blocking && m.job.Backup != "" {
		var old InstallState
		if readJSON(filepath.Join(m.paths.Backup(), m.job.Backup, "etc", "install.json"), &old) == nil {
			stVersion = old.Version
		}
	}
	view := map[string]any{"version": stVersion, "online": updatePublicKey != ""}
	if m.job != nil {
		view["job"] = m.job
	}
	writeResp(w, 200, view)
}

func (m *maintenance) handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	switch {
	case r.Method == "GET" && r.URL.Path == "/":
		m.overview(w)
	case r.Method == "GET" && r.URL.Path == "/network":
		m.network(w)
	case r.Method == "POST" && r.URL.Path == "/confirm":
		m.confirmNetwork(w, r)
	case r.Method == "POST" && (r.URL.Path == "/check" || r.URL.Path == "/upgrade" || r.URL.Path == "/network"):
		m.start(w, r)
	default:
		writeErr(w, 404, "没有这个维护接口")
	}
}

func decodeMaintenance(r *http.Request, out any) error {
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return fmt.Errorf("请求包含额外数据")
	}
	return nil
}

func (m *maintenance) start(w http.ResponseWriter, r *http.Request) {
	kind := strings.TrimPrefix(r.URL.Path, "/")
	var upgrade struct {
		Version string `json:"version"`
	}
	var network networkRequest
	switch kind {
	case "check":
		if decodeMaintenance(r, &struct{}{}) != nil {
			writeErr(w, 400, "无效的检查请求")
			return
		}
	case "upgrade":
		if decodeMaintenance(r, &upgrade) != nil || !releaseVersion.MatchString(upgrade.Version) {
			writeErr(w, 400, "无效的升级版本")
			return
		}
	case "network":
		if decodeMaintenance(r, &network) != nil {
			writeErr(w, 400, "无效的网络配置")
			return
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.reloading || (m.job != nil && (!m.job.Done || m.job.Blocking)) {
		writeErr(w, 409, "已有维护任务进行中或等待恢复")
		return
	}
	lock, err := operationLock(m.paths)
	if err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	started := false
	defer func() {
		if !started {
			lock.Close()
		}
	}()
	st, err := loadState(m.paths)
	if err != nil || !st.Complete {
		writeErr(w, 503, "没有已完成的 Atom 安装")
		return
	}
	if kind == "upgrade" && compareVersions(upgrade.Version, st.Version) <= 0 {
		writeErr(w, 409, "请选择比当前版本更新的正式发布")
		return
	}

	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		writeErr(w, 500, "无法创建维护任务")
		return
	}
	job := maintenanceJob{ID: hex.EncodeToString(random[:]), Kind: kind, Phase: "checking", Version: upgrade.Version}
	if kind == "network" {
		if network.Revision != networkRevision(st) {
			writeErr(w, 409, "网络配置已更新，请重新读取")
			return
		}
		prepareNetworkJob(&job, st, &network)
	}
	if err := m.save(job); err != nil {
		writeErr(w, 500, "无法记录维护任务")
		return
	}
	started = true
	go func() {
		defer lock.Close()
		switch kind {
		case "network":
			m.runNetwork(st, network)
		case "check", "upgrade":
			m.runUpdate(st, job, lock)
		}
	}()
	writeResp(w, 202, m.job)
}

// holdWrites drains HTTP mutations already in progress. Later requests see
// either the exclusive lock or the durable transaction record.
func (m *maintenance) holdWrites() (*os.File, error) {
	for deadline := time.Now().Add(30 * time.Second); ; {
		lock, err := exclusiveLock(m.paths.Run(panelWriteLock), true)
		if !errors.Is(err, errMaintenanceBusy) || time.Now().After(deadline) {
			return lock, err
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (m *maintenance) begin(ins *Installer, backup string, prepare func(*maintenanceJob)) error {
	if err := ins.makeBackup(backup); err != nil {
		return fmt.Errorf("备份：%w", err)
	}
	if err := syncBackup(backup); err != nil {
		return fmt.Errorf("同步备份：%w", err)
	}
	return m.change(func(job *maintenanceJob) {
		job.Backup = filepath.Base(backup)
		job.Blocking, job.Phase = true, "applying"
		if prepare != nil {
			prepare(job)
		}
	})
}

func (m *maintenance) rollback(job maintenanceJob, failure error) {
	log := stderrLogger{}
	log.Logf("维护任务失败，恢复备份：%v", failure)
	if err := m.change(func(job *maintenanceJob) { job.Phase = "restoring" }); err != nil {
		log.Logf("记录恢复状态：%v", err)
	}
	backup := filepath.Join(m.paths.Backup(), job.Backup)
	var restoreErr error
	if job.Backup == "" || !backupName.MatchString(job.Backup) {
		restoreErr = fmt.Errorf("备份记录无效")
	} else {
		ins := &Installer{Paths: m.paths}
		restoreErr = ins.restoreMaintenanceBackup(backup, job.Kind == "network", log)
	}
	phase := "restored"
	if restoreErr != nil {
		phase = "recovery_failed"
		failure = fmt.Errorf("%v；恢复失败：%w", failure, restoreErr)
	}
	if err := m.finish(phase, failure); err != nil {
		log.Logf("记录恢复结果：%v", err)
	}
}

func (m *maintenance) snapshot() maintenanceJob {
	m.mu.Lock()
	defer m.mu.Unlock()
	return *m.job
}

func (m *maintenance) recover() error {
	var job maintenanceJob
	if err := readJSON(m.paths.Run(maintenanceRecord), &job); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	m.job = &job
	if job.Done && !job.Blocking {
		return nil
	}
	failure := fmt.Errorf("维护服务中断，任务未完成")
	if job.Blocking {
		deadline := time.Now().Add(3 * time.Minute)
		for {
			lock, err := operationLock(m.paths)
			if err == nil {
				defer lock.Close()
				break
			}
			if !errors.Is(err, errMaintenanceBusy) || time.Now().After(deadline) {
				return err
			}
			time.Sleep(time.Second)
		}
		m.rollback(job, failure)
		return nil
	}
	return m.finish("failed", failure)
}

func serveMaintenance(p Paths) error {
	m := &maintenance{paths: p, reload: make(chan struct{}, 1)}
	if err := os.MkdirAll(p.Run(""), 0o755); err != nil {
		return err
	}
	instance, err := exclusiveLock(p.Run("maintenance-process.lock"), true)
	if err != nil {
		return err
	}
	defer instance.Close()
	// This file must exist before the panel can take shared mutation locks.
	f, err := os.OpenFile(p.Run(panelWriteLock), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	f.Close()
	if err := m.recover(); err != nil {
		return err
	}
	if stale, err := filepath.Glob(p.Run("update-*")); err == nil {
		for _, dir := range stale {
			_ = os.RemoveAll(dir)
		}
	}
	socket := p.Run(maintenanceSocket)
	os.Remove(socket)
	oldMask := syscall.Umask(0o177)
	listener, err := net.Listen("unix", socket)
	syscall.Umask(oldMask)
	if err != nil {
		return err
	}
	defer listener.Close()
	if err := os.Chmod(socket, 0o600); err != nil {
		return err
	}
	server := &http.Server{
		Handler: http.HandlerFunc(m.handler), ReadHeaderTimeout: 3 * time.Second,
		ReadTimeout: 5 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 10 * time.Second,
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case err := <-done:
		return err
	case <-m.reload:
		server.Close()
		<-done
		in := p.Bin("atom-installer")
		return syscall.Exec(in, []string{in, "maintenance"}, os.Environ())
	}
}
