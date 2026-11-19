package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// initSystem is what the installer needs of the service manager, for atom's own two services and for the
// services it stops to free a port. Names may be given with or without the systemd ".service" suffix.
type initSystem interface {
	Reload(log Logger) error // pick up changed service files
	Enable(log Logger, names ...string) error
	DisableNow(log Logger, names ...string) error
	Start(log Logger, name string) error
	Stop(log Logger, name string) error
	Restart(log Logger, name string) error
	IsActive(name string) string // "active" when running
	Logs(name string) string     // the last lines of its log
	Forget(log Logger, names ...string)
}

// useOpenRC says which service manager this machine has: systemd when it runs, else OpenRC. ATOM_INIT
// (systemd or openrc) exists for the tests.
func useOpenRC() bool {
	switch os.Getenv("ATOM_INIT") {
	case "openrc":
		return true
	case "systemd":
		return false
	}
	if _, err := os.Stat("/run/systemd/system"); err == nil {
		return false
	}
	if _, err := os.Stat("/run/openrc"); err == nil {
		return true
	}
	_, err := os.Stat("/sbin/openrc-run")
	return err == nil
}

type systemdInit struct{}

func sysdName(n string) string {
	if !strings.HasSuffix(n, ".service") {
		return n + ".service"
	}
	return n
}

func sysdNames(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = sysdName(n)
	}
	return out
}

func (systemdInit) Reload(log Logger) error {
	return runCmd(log, 30*time.Second, "systemctl", "daemon-reload")
}

func (systemdInit) Enable(log Logger, names ...string) error {
	return runCmd(log, 30*time.Second, "systemctl", append([]string{"enable"}, sysdNames(names)...)...)
}

func (systemdInit) DisableNow(log Logger, names ...string) error {
	return runCmd(log, 60*time.Second, "systemctl", append([]string{"disable", "--now"}, sysdNames(names)...)...)
}

func (systemdInit) Start(log Logger, name string) error {
	return runCmd(log, 60*time.Second, "systemctl", "start", sysdName(name))
}

func (systemdInit) Stop(log Logger, name string) error {
	return runCmd(log, 60*time.Second, "systemctl", "stop", sysdName(name))
}

func (systemdInit) Restart(log Logger, name string) error {
	return runCmd(log, 60*time.Second, "systemctl", "restart", sysdName(name))
}

func (systemdInit) IsActive(name string) string {
	out, _ := quietCmd("systemctl", "is-active", sysdName(name))
	return out
}

// IsEnabled is for the services of others that the wizard stops (free.go).
func (systemdInit) IsEnabled(name string) bool {
	out, _ := quietCmd("systemctl", "is-enabled", sysdName(name))
	return strings.HasPrefix(out, "enabled")
}

func (systemdInit) Logs(name string) string {
	out, _ := quietCmd("journalctl", "-u", sysdName(name), "-n", "15", "--no-pager")
	return out
}

func (systemdInit) Forget(log Logger, names ...string) {
	_ = runCmd(log, 30*time.Second, "systemctl", "daemon-reload")
	_ = runCmd(log, 30*time.Second, "systemctl", append([]string{"reset-failed"}, sysdNames(names)...)...)
}

// openRCInit runs atom's services in the default runlevel. The log of each is /var/log/atom/<name>.log.
type openRCInit struct{}

var openRCLogDir = "/var/log/atom" // a variable for the tests

func rcName(n string) string { return strings.TrimSuffix(n, ".service") }

func (openRCInit) Reload(Logger) error { return nil }

func (openRCInit) Enable(log Logger, names ...string) error {
	for _, n := range names {
		if err := runCmd(log, 30*time.Second, "rc-update", "add", rcName(n), "default"); err != nil {
			return err
		}
	}
	return nil
}

// DisableNow can be repeated: a service that is already stopped and a service that is not (or no longer) in the
// default runlevel are what it asks for, not errors (`rc-update del` fails for a service that is not there).
func (o openRCInit) DisableNow(log Logger, names ...string) error {
	var first error
	for _, n := range names {
		if err := o.Stop(log, n); err != nil {
			// an older openrc-run may refuse to stop what is stopped already: that is a stopped service
			ctx, cancel := context.WithTimeout(context.Background(), serviceStopWait)
			code, qerr := openRCStatus(ctx, n)
			cancel()
			if qerr == nil && openRCStopped(code) {
				log.Logf("%s 已经停止", rcName(n))
			} else if first == nil {
				first = err
			}
		}
		if !inRunlevel(n, "default") {
			continue
		}
		if err := runCmd(log, 30*time.Second, "rc-update", "del", rcName(n), "default"); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// inRunlevel: the service is in the runlevel (rc-update keeps it as /etc/runlevels/<level>/<name>).
func inRunlevel(name, level string) bool {
	_, err := os.Lstat(filepath.Join(runlevelsDir, level, rcName(name)))
	return err == nil
}

func (openRCInit) Start(log Logger, name string) error {
	return runCmd(log, 60*time.Second, "rc-service", rcName(name), "start")
}

func (openRCInit) Stop(log Logger, name string) error {
	return runCmd(log, 60*time.Second, "rc-service", rcName(name), "stop")
}

func (openRCInit) Restart(log Logger, name string) error {
	return runCmd(log, 60*time.Second, "rc-service", rcName(name), "restart")
}

func (openRCInit) IsActive(name string) string {
	if _, err := quietCmd("rc-service", rcName(name), "status"); err == nil {
		return "active"
	}
	return "inactive"
}

func (openRCInit) Logs(name string) string {
	out, _ := quietCmd("tail", "-n", "15", openRCLogDir+"/"+rcName(name)+".log")
	return out
}

func (openRCInit) Forget(Logger, ...string) {}

// queryCmd runs a status query of the service manager within ctx. A query cut short by ctx is an error, never
// an answer: its exit code says nothing about the service.
func queryCmd(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = 200 * time.Millisecond // a child that keeps the output open does not hold the query either
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return "", fmt.Errorf("%s %s：没有在时限内回答", name, strings.Join(args, " "))
	}
	return string(out), err
}

// openRCStatus is the exit code of `rc-service <name> status`; err is a query that gave no exit code.
func openRCStatus(ctx context.Context, name string) (int, error) {
	out, err := queryCmd(ctx, "rc-service", rcName(name), "status")
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &ee):
		return ee.ExitCode(), nil
	default:
		return -1, fmt.Errorf("%v %s", err, strings.TrimSpace(out))
	}
}

// openRCStopped: the exit codes of openrc-run status that mean nothing runs: 3 stopped, 16 inactive, 32 crashed
// (started, but the daemon is gone). 0 is started, 4 stopping, 8 starting.
func openRCStopped(code int) bool { return code == 3 || code == 16 || code == 32 }

// serviceRunning asks the service manager, within ctx, whether one of atom's services runs (or is starting or
// stopping). An answer that cannot be had is an error, never "stopped": the reset deletes the data only once it
// is sure.
func serviceRunning(ctx context.Context, p Paths, name string) (bool, error) {
	if p.OpenRC {
		code, err := openRCStatus(ctx, name)
		switch {
		case err != nil:
			return false, fmt.Errorf("查询 %s 的状态：%v", name, err)
		case code == 0 || code == 4 || code == 8:
			return true, nil
		case openRCStopped(code):
			return false, nil
		}
		// rc-service fails (1) for a service it does not know: stopped only when its script is really gone
		if _, serr := os.Lstat(p.unitFile(name)); code == 1 && os.IsNotExist(serr) {
			return false, nil
		}
		return false, fmt.Errorf("查询 %s 的状态：rc-service status 的退出码 %d 不是已知的状态", name, code)
	}
	out, err := queryCmd(ctx, "systemctl", "is-active", sysdName(name))
	if err == nil {
		return true, nil
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	switch state := strings.TrimSpace(lines[len(lines)-1]); state {
	case "inactive", "failed", "unknown": // unknown: an older systemd, for a unit it does not know
		return false, nil
	case "activating", "deactivating", "reloading", "refreshing":
		return true, nil
	default:
		return false, fmt.Errorf("查询 %s 的状态：%v %s", name, err, strings.TrimSpace(out))
	}
}
