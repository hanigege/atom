package main

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

// panelServices are the services the panel shows and may restart, in this order.
var panelServices = []string{"atom-maintenance", "atom-dns", "atom-sing-box", "atom-mihomo"}

var (
	serviceQueryWait = 3 * time.Second        // all status queries of one GET /services together
	restartDelay     = 300 * time.Millisecond // lets the 202 get through atomdns before a restart cuts it off
)

type serviceStatus struct {
	Name  string `json:"name"`
	State string `json:"state"` // running | stopped | failed | unknown
}

// serviceState asks the service manager within ctx; an answer that cannot be had is "unknown".
func serviceState(ctx context.Context, p Paths, name string) string {
	if p.OpenRC {
		code, err := openRCStatus(ctx, name)
		switch {
		case err != nil:
			return "unknown"
		case code == 0 || code == 4 || code == 8:
			return "running"
		case code == 3 || code == 16:
			return "stopped"
		case code == 32: // crashed
			return "failed"
		}
		return "unknown"
	}
	out, _ := queryCmd(ctx, "systemctl", "is-active", sysdName(name)) // no answer: no line, "unknown"
	lines := strings.Split(strings.TrimSpace(out), "\n")
	switch strings.TrimSpace(lines[len(lines)-1]) {
	case "active", "activating", "deactivating", "reloading", "refreshing":
		return "running"
	case "inactive":
		return "stopped"
	case "failed":
		return "failed"
	}
	return "unknown"
}

// installedServices are the panel services whose service definition is there.
func installedServices(p Paths) []string {
	var names []string
	for _, n := range panelServices {
		if _, err := os.Lstat(p.unitFile(n + ".service")); err == nil {
			names = append(names, n)
		}
	}
	return names
}

func (m *maintenance) services(w http.ResponseWriter) {
	ctx, cancel := context.WithTimeout(context.Background(), serviceQueryWait)
	defer cancel()
	names := installedServices(m.paths)
	list := make([]serviceStatus, len(names))
	var wg sync.WaitGroup
	for i, n := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			list[i] = serviceStatus{Name: n, State: serviceState(ctx, m.paths, n)}
		}()
	}
	wg.Wait()
	writeResp(w, 200, map[string]any{"services": list})
}

func (m *maintenance) restartService(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Service string `json:"service"`
	}
	if decodeMaintenance(r, &req) != nil || !slices.Contains(panelServices, req.Service) {
		writeErr(w, 400, "无效的服务")
		return
	}
	if !slices.Contains(installedServices(m.paths), req.Service) {
		writeErr(w, 400, "没有安装这个服务")
		return
	}
	m.mu.Lock()
	if m.reloading || (m.job != nil && (!m.job.Done || m.job.Blocking)) {
		m.mu.Unlock()
		writeErr(w, 409, "已有维护任务进行中或等待恢复")
		return
	}
	// held until the restart is done: an upgrade, reset or other restart started meanwhile is refused
	lock, err := operationLock(m.paths)
	m.mu.Unlock()
	if err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	writeResp(w, 202, map[string]bool{"ok": true})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	go func() {
		defer lock.Close()
		time.Sleep(restartDelay)
		if err := restartUnit(m.paths, req.Service+".service"); err != nil {
			stderrLogger{}.Logf("重启 %s：%v", req.Service, err)
		}
	}()
}

// restartUnit restarts one of atom's services. The maintenance service restarts itself without waiting: the
// service manager stops this process on the way.
func restartUnit(p Paths, unit string) error {
	log := stderrLogger{}
	if unit != maintenanceUnitName {
		return p.Init().Restart(log, unit)
	}
	if !p.OpenRC {
		return runCmd(log, 30*time.Second, "systemctl", "restart", "--no-block", sysdName(unit))
	}
	// rc-service waits for the stop; in a session of its own it outlives this process, which the stop ends
	cmd := exec.Command("rc-service", rcName(unit), "restart")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}
