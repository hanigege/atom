package main

// The wizard inside the entry process (docs/container.md 6). There are three: the first installation (no
// install.json yet, on the panel's port), a reconfigure (started by `atom-installer reconfigure` from a shell in
// the container, on a port of its own) and the reset (started by the panel's maintenance request, likewise). The
// wizard is the same Server as the Linux one; what is container only is in container_install.go.

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type wizardKind string

const (
	wizardFirst       wizardKind = "install"
	wizardReconfigure wizardKind = "reconfigure"
	wizardReset       wizardKind = "reset"
)

// containerWizard is one wizard of the entry process.
type containerWizard struct {
	kind      wizardKind
	server    *Server
	ins       *Installer
	port      int
	code      string
	done      chan struct{} // closed when the wizard has closed
	runFile   string
	operation *os.File
}

type wizardHost struct {
	p   Paths
	log Logger
	mu  sync.Mutex
	cur *containerWizard
}

func newWizardHost(p Paths, log Logger) *wizardHost { return &wizardHost{p: p, log: log} }

// the host of this process: the maintenance code that follows a reset wizard through its run file asks it
// whether the wizard is still there (on a machine it asks whether the process is).
var (
	hostMu        sync.Mutex
	containerHost *wizardHost
)

func setWizardHost(h *wizardHost) {
	hostMu.Lock()
	containerHost = h
	hostMu.Unlock()
}

func activeWizard(li listenInfo) bool {
	hostMu.Lock()
	h := containerHost
	hostMu.Unlock()
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cur != nil && h.cur.port == li.Port && h.cur.code == li.Code && !isClosed(h.cur.done)
}

func isClosed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// startContainerReset is launchResetWizard of the container.
func startContainerReset(p Paths, id string) error {
	hostMu.Lock()
	h := containerHost
	hostMu.Unlock()
	if h == nil {
		return fmt.Errorf("入口进程没有安装向导的宿主")
	}
	_, err := h.start(wizardReset, id)
	return err
}

// newContainerInstaller is the installer of the image: it installs from the shipped files in share/.
func newContainerInstaller(p Paths) *Installer {
	ins := &Installer{Paths: p, PkgDir: p.Shipped(), Version: version}
	var m manifest
	if err := readJSON(p.Shipped()+"/manifest.json", &m); err == nil && m.Version != "" {
		ins.Version = m.Version
	}
	return ins
}

func newInstallCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// freePort is a port nobody listens on now.
func freePort() (int, error) {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// start opens a wizard; id is the maintenance job of a reset. One wizard at a time: a reconfigure that is open is
// returned again (the command may be run twice), any other open wizard is an error.
func (h *wizardHost) start(kind wizardKind, id string) (*containerWizard, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cur != nil && !isClosed(h.cur.done) {
		if kind == wizardReconfigure && h.cur.kind == wizardReconfigure {
			return h.cur, nil
		}
		return nil, fmt.Errorf("安装向导已在运行，请先关闭它")
	}
	p := h.p
	w := &containerWizard{kind: kind, ins: newContainerInstaller(p), done: make(chan struct{}), runFile: p.Run("installer.json")}
	w.ins.Reconfigure = kind == wizardReconfigure
	var err error
	switch kind {
	case wizardReset:
		w.operation, err = resetOperationLock(p, id)
	case wizardReconfigure:
		w.operation, err = cliOperationLock(p)
		if st, serr := loadState(p); err == nil && (serr != nil || !st.Complete) {
			w.operation.Close()
			err = fmt.Errorf("没有已完成的安装")
		}
	}
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*containerWizard, error) {
		if w.operation != nil {
			w.operation.Close()
		}
		return nil, err
	}
	// the first wizard is the panel's address; the others are beside the running panel
	var ln net.Listener
	if kind == wizardFirst {
		ln, err = net.Listen("tcp", fmt.Sprintf(":%d", defaultPanel))
	} else {
		ln, err = net.Listen("tcp", fmt.Sprintf(":%d", defaultPort))
	}
	if err != nil {
		ln, err = net.Listen("tcp", ":0")
	}
	if err != nil {
		return fail(err)
	}
	w.port = ln.Addr().(*net.TCPAddr).Port
	w.ins.InstallerPort = w.port
	if w.code, err = newInstallCode(); err != nil {
		ln.Close()
		return fail(err)
	}
	li := listenInfo{Port: w.port, PID: os.Getpid(), Code: w.code}
	s := newServer(w.ins, w.code)
	s.firstRun = kind == wizardFirst
	if kind == wizardReset {
		if s.reset, err = newResetRun(p, id, li, w.runFile); err != nil {
			ln.Close()
			return fail(err)
		}
		li = s.reset.listen
	}
	if err := writePrivateJSON(w.runFile, li); err != nil {
		ln.Close()
		return fail(err)
	}
	w.server = s
	srv := &http.Server{Handler: s.handler(), ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	go s.watchIdle()
	go func() {
		<-s.quit
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		srv.Shutdown(ctx)
		cancel()
		os.Remove(w.runFile)
		if w.operation != nil {
			w.operation.Close()
		}
		close(w.done)
	}()
	h.cur = w
	return w, nil
}

// urls are the addresses of the wizard; port 80 is left out of them.
func containerWizardURLs(port int) []string {
	var out []string
	for _, a := range privateAddrs() {
		if port == 80 {
			out = append(out, fmt.Sprintf("http://%s/", a))
		} else {
			out = append(out, fmt.Sprintf("http://%s/", net.JoinHostPort(a, strconv.Itoa(port))))
		}
	}
	if len(out) == 0 {
		out = append(out, fmt.Sprintf("http://localhost:%d/", port))
	}
	return out
}

// announce prints where the wizard is and its code, in the log of the container.
func (w *containerWizard) announce(log Logger) {
	title := "安装向导"
	if w.kind == wizardReconfigure {
		title = "重新配置向导"
	}
	log.Logf("%s：%s", title, strings.Join(containerWizardURLs(w.port), "  "))
	log.Logf("安装码 %s（在浏览器输入；仅本次启动有效）", w.code)
}

// ---- the first installation ----

// runFirstWizard serves the wizard of a container that has no installation, until it has installed. The services
// the installation started keep running. ok is false when the wizard ended without an installation or stop was
// closed first.
func runFirstWizard(p Paths, sup *supervisor, h *wizardHost, stop <-chan struct{}) error {
	sup.markReady()
	w, err := h.start(wizardFirst, "")
	if err != nil {
		return fmt.Errorf("安装向导没有启动：%w", err)
	}
	w.announce(sup)
	select {
	case <-w.done:
	case <-stop:
		return nil
	}
	st, err := loadState(p)
	if err != nil || !st.Complete {
		return fmt.Errorf("安装向导已关闭，但没有完成的安装")
	}
	if w.ins.PanelTemp != 0 {
		return handOverPanel(p, sup, st)
	}
	sup.Logf("安装完成，面板端口 %d", st.PanelPort)
	return nil
}

// ensurePanelPort makes the panel port of config_custom.yaml the one install.json says; it returns whether the
// file had to change. It is what moves the panel from the temporary port of the installation to the real one,
// and what a container that was stopped before that does at its next start.
func ensurePanelPort(p Paths, st *InstallState) (bool, error) {
	path := p.Data() + "/config_custom.yaml"
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	out, err := setPanelPort(string(data), st.PanelPort)
	if err != nil {
		return false, err
	}
	if out == string(data) {
		return false, nil
	}
	return true, writeFileAtomic(path, []byte(out), 0o644)
}

// handOverPanel: the wizard has closed and its port is free; atomdns moves to it.
func handOverPanel(p Paths, sup *supervisor, st *InstallState) error {
	if _, err := ensurePanelPort(p, st); err != nil {
		return fmt.Errorf("面板端口：%w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := sup.Restart(ctx, "atom-dns"); err != nil {
		return fmt.Errorf("atomdns 改用面板端口 %d 时没有起来：%w", st.PanelPort, err)
	}
	sup.Logf("安装完成，面板端口 %d", st.PanelPort)
	return nil
}

// ---- reconfigure ----

type reconfigureReply struct {
	Port int      `json:"port"`
	Code string   `json:"code"`
	URLs []string `json:"urls"`
}

func (h *wizardHost) reconfigure() (reconfigureReply, error) {
	w, err := h.start(wizardReconfigure, "")
	if err != nil {
		return reconfigureReply{}, err
	}
	w.announce(h.log)
	return reconfigureReply{Port: w.port, Code: w.code, URLs: containerWizardURLs(w.port)}, nil
}

// cmdReconfigure is `atom-installer reconfigure`: it asks the entry process for a wizard and prints where it is.
func cmdReconfigure([]string) int {
	os.Setenv("ATOM_DEPLOYMENT", "container")
	p := defaultPaths()
	var out reconfigureReply
	if err := (containerInit{socket: p.Run(controlSocket)}).call(30*time.Second, "POST", "/reconfigure", map[string]bool{}, &out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	for _, u := range out.URLs {
		fmt.Println(u)
	}
	fmt.Printf("\n=======================================\n  安装码  %s\n  请在浏览器输入此码。\n=======================================\n", out.Code)
	return 0
}
