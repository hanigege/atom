package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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
	// selfRestartHold keeps the operation lock while this service restarts itself: the stop ends this process
	// and the lock with it; should it never come, the lock goes after this long.
	selfRestartHold = 2 * time.Minute
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
		case code == 32 || code == 64: // crashed, or unsupervised after a crash
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

// installedServices are the panel services whose service definition is there. Of the two proxy kernels only
// the selected one counts: the other shares its ports and nft sets and must stay stopped.
func installedServices(p Paths) []string {
	kernel := selectedKernel(p)
	var names []string
	for _, n := range panelServices {
		unit := n + ".service"
		if isKernel(unit) && unit != kernel {
			continue
		}
		if _, err := os.Lstat(p.unitFile(unit)); err == nil {
			names = append(names, n)
		}
	}
	return names
}

func isKernel(unit string) bool { return unit == singBoxUnitName || unit == mihomoUnitName }

// selectedKernel is the unit of the proxy kernel atom_proxy.json selects; no backend there is sing-box, as for
// installations before the choice. A file that cannot be read or a backend it does not know: "", neither.
func selectedKernel(p Paths) string {
	var state struct {
		Settings struct {
			Backend string `json:"backend"`
		} `json:"settings"`
	}
	data, err := os.ReadFile(filepath.Join(p.Data(), "webinfo", "atom_proxy.json"))
	if err != nil || json.Unmarshal(data, &state) != nil {
		return ""
	}
	switch state.Settings.Backend {
	case "", "sing-box":
		return singBoxUnitName
	case "mihomo":
		return mihomoUnitName
	}
	return ""
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
		if unit := req.Service + ".service"; isKernel(unit) {
			if selectedKernel(m.paths) == "" {
				writeErr(w, 400, "无法确定当前使用的代理内核")
			} else {
				writeErr(w, 400, "不是当前使用的代理内核")
			}
			return
		}
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
	delay, hold := restartDelay, selfRestartHold
	go func() {
		defer lock.Close()
		time.Sleep(delay)
		if err := restartUnit(m.paths, req.Service+".service", hold); err != nil {
			stderrLogger{}.Logf("重启 %s：%v", req.Service, err)
		}
	}()
}

// restartUnit restarts one of atom's services. For the maintenance service itself the service manager stops
// this process on the way: systemd gets the job without waiting for it, OpenRC's rc-service runs on its own.
// Either way the caller's lock is kept until the stop ends this process, or for hold at most.
func restartUnit(p Paths, unit string, hold time.Duration) error {
	log := stderrLogger{}
	if unit != maintenanceUnitName {
		return p.Init().Restart(log, unit)
	}
	done := make(chan error, 1)
	if !p.OpenRC {
		if err := runCmd(log, 30*time.Second, "systemctl", "restart", "--no-block", sysdName(unit)); err != nil {
			return err
		}
	} else {
		// in a session of its own rc-service outlives this process, which its stop ends; if it returns first,
		// the restart did not happen
		cmd := exec.Command("rc-service", rcName(unit), "restart")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			return err
		}
		go func() { done <- cmd.Wait() }()
	}
	select {
	case err := <-done:
		if err == nil {
			err = errors.New("rc-service 返回了，维护服务没有被停掉")
		}
		return err
	case <-time.After(hold):
		return errors.New("维护服务没有被停掉")
	}
}
