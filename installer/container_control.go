package main

// The control socket of the entry process (/run/atom/control.sock) and its clients: containerInit, which is the
// initSystem of the container version, and `atom-installer svc`, the command atomdns runs to reload and stop
// the kernels. The maintenance socket that atomdns uses for the panel is served here too (container_maintenance.go).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"
)

type svcRequest struct {
	Action string `json:"action"` // start | stop | restart | reload | enable
	Name   string `json:"name"`
}

func controlHandler(sup *supervisor) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /svc", func(w http.ResponseWriter, r *http.Request) {
		var req svcRequest
		if decodeMaintenance(r, &req) != nil {
			writeErr(w, 400, "无效的请求")
			return
		}
		var err error
		switch req.Action {
		case "start":
			err = sup.Start(req.Name)
		case "stop":
			err = sup.Stop(req.Name)
		case "restart":
			err = sup.Restart(req.Name)
		case "reload":
			err = sup.Reload(req.Name)
		case "enable":
			err = sup.Enable(req.Name)
		default:
			writeErr(w, 400, "无效的操作 "+req.Action)
			return
		}
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		writeResp(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /svc", func(w http.ResponseWriter, r *http.Request) {
		writeResp(w, 200, map[string]any{"services": sup.list()})
	})
	mux.HandleFunc("GET /svc/logs", func(w http.ResponseWriter, r *http.Request) {
		lines, err := sup.logs(r.URL.Query().Get("name"), 15)
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		writeResp(w, 200, map[string]any{"lines": lines})
	})
	return mux
}

// listenUnix makes a socket only root can use.
func listenUnix(path string) (net.Listener, error) {
	os.Remove(path)
	old := syscall.Umask(0o177)
	ln, err := net.Listen("unix", path)
	syscall.Umask(old)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// serveContainerSockets serves the control socket and the maintenance socket; the returned function closes both.
func serveContainerSockets(p Paths, sup *supervisor) (func(), error) {
	if err := os.MkdirAll(p.Run(""), 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(p.Run(""), 0o700); err != nil {
		return nil, err
	}
	// atomdns takes this lock shared before a write; it has to exist (maintenance.go serveMaintenance)
	if f, err := os.OpenFile(p.Run(panelWriteLock), os.O_CREATE|os.O_RDWR, 0o600); err == nil {
		f.Close()
	} else {
		return nil, err
	}
	var servers []*http.Server
	closeAll := func() {
		for _, s := range servers {
			s.Close()
		}
	}
	for _, s := range []struct {
		path    string
		handler http.Handler
	}{
		{p.Run(controlSocket), controlHandler(sup)},
		{p.Run(maintenanceSocket), http.HandlerFunc(containerMaintenance(p).handler)},
	} {
		ln, err := listenUnix(s.path)
		if err != nil {
			closeAll()
			return nil, err
		}
		srv := &http.Server{Handler: s.handler, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second, IdleTimeout: 10 * time.Second}
		servers = append(servers, srv)
		go srv.Serve(ln)
	}
	return closeAll, nil
}

// containerInit is the initSystem of the container version: it asks the entry process.
type containerInit struct{ socket string }

var containerNames = map[string]bool{"atom-dns": true, "atom-sing-box": true, "atom-mihomo": true}

func (c containerInit) client(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", c.socket)
		},
	}}
}

func (c containerInit) call(timeout time.Duration, method, path string, body, out any) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return c.callCtx(ctx, timeout, method, path, body, out)
}

func (c containerInit) callCtx(ctx context.Context, timeout time.Duration, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://atom"+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	cl := c.client(timeout)
	defer cl.CloseIdleConnections()
	resp, err := cl.Do(req)
	if err != nil {
		return fmt.Errorf("入口进程没有回答：%w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return fmt.Errorf("%s", e.Error)
		}
		return fmt.Errorf("入口进程返回 %d", resp.StatusCode)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// action does one operation on one service; the entry process is itself the maintenance service, which
// therefore needs no start, stop or enabling.
func (c containerInit) action(log Logger, action, name string) error {
	name = strings.TrimSuffix(name, ".service")
	if name == "atom-maintenance" {
		return nil
	}
	if !containerNames[name] {
		return fmt.Errorf("没有这个服务：%s", name)
	}
	log.Logf("$ atom-installer svc %s %s", action, name)
	return c.call(90*time.Second, "POST", "/svc", svcRequest{Action: action, Name: name}, nil)
}

func (containerInit) Reload(Logger) error { return nil } // there are no service files

func (c containerInit) Enable(log Logger, names ...string) error {
	for _, n := range names {
		if err := c.action(log, "enable", n); err != nil {
			return err
		}
	}
	return nil
}

func (c containerInit) DisableNow(log Logger, names ...string) error {
	var first error
	for _, n := range names {
		if err := c.action(log, "stop", n); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (c containerInit) Start(log Logger, name string) error   { return c.action(log, "start", name) }
func (c containerInit) Stop(log Logger, name string) error    { return c.action(log, "stop", name) }
func (c containerInit) Restart(log Logger, name string) error { return c.action(log, "restart", name) }
func (containerInit) Forget(Logger, ...string)                {}

func (c containerInit) state(name string) string {
	return c.stateCtx(context.Background(), name)
}

func (c containerInit) stateCtx(ctx context.Context, name string) string {
	name = strings.TrimSuffix(name, ".service")
	if name == "atom-maintenance" {
		return "running"
	}
	var out struct {
		Services []serviceInfo `json:"services"`
	}
	if err := c.callCtx(ctx, 5*time.Second, "GET", "/svc", nil, &out); err != nil {
		return "unknown"
	}
	for _, s := range out.Services {
		if s.Name == name {
			return s.State
		}
	}
	return "unknown"
}

func (c containerInit) IsActive(name string) string {
	switch c.state(name) {
	case "running":
		return "active"
	case "failed":
		return "failed"
	}
	return "inactive"
}

func (c containerInit) Logs(name string) string {
	var out struct {
		Lines []string `json:"lines"`
	}
	if c.call(5*time.Second, "GET", "/svc/logs?name="+strings.TrimSuffix(name, ".service"), nil, &out) != nil {
		return ""
	}
	return strings.Join(out.Lines, "\n")
}

// containerServiceState is serviceState (maintenance_services.go) of the container version.
func containerServiceState(ctx context.Context, p Paths, name string) string {
	return containerInit{socket: p.Run(controlSocket)}.stateCtx(ctx, name)
}

// cmdSvc is `atom-installer svc <start|stop|restart|reload|is-active> <service>`: what atomdns runs as the
// reload and stop command of a kernel, and what a person in the container may use.
func cmdSvc(args []string) int {
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: atom-installer svc <start|stop|restart|reload|is-active> <service>")
		return 2
	}
	os.Setenv("ATOM_DEPLOYMENT", "container")
	c := containerInit{socket: defaultPaths().Run(controlSocket)}
	name := strings.TrimSuffix(args[1], ".service")
	if args[0] == "is-active" {
		st := c.IsActive(name)
		fmt.Println(st)
		if st != "active" {
			return 3
		}
		return 0
	}
	switch args[0] {
	case "start", "stop", "restart", "reload":
	default:
		fmt.Fprintln(os.Stderr, "unknown action", args[0])
		return 2
	}
	if err := c.action(quietLogger{}, args[0], name); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

type quietLogger struct{}

func (quietLogger) Logf(string, ...any) {}
