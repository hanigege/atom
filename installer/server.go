package main

import (
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"fmt"
	"mime"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web/index.html
var indexHTML []byte

const (
	idleLimit      = 30 * time.Minute
	afterDoneLimit = 5 * time.Minute
)

// Server is the wizard: a page and a small JSON API. It only answers private and loopback addresses.
type Server struct {
	ins       *Installer
	mu        sync.Mutex
	job       *Job
	seen      time.Time
	code      string
	nextGuess time.Time
	quit      chan struct{}
	once      sync.Once

	closing bool       // finish was accepted: no new job
	opMu    sync.Mutex // one free action at a time, and not while a job starts

	reset *resetRun // the wizard resets the installed atom first (reset.go); nil otherwise

	// container.go: the first installation of a container. The wizard has no idle limit and only a successful
	// installation closes it; it holds the port of the panel, which atomdns takes over afterwards.
	firstRun bool
	probe    *probeCache // the capability probe of the environment step (container)
}

func newServer(ins *Installer, code string) *Server {
	return &Server{ins: ins, code: code, seen: time.Now(), quit: make(chan struct{}), probe: &probeCache{env: realProbeEnv()}}
}

func isPrivateRemote(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

// hostAllowed refuses a Host header that is not an address, localhost or this machine's own name: a web page
// that makes the browser reach the wizard through a name of its own (DNS rebinding) is not the user.
func hostAllowed(host string) bool {
	h := host
	if hh, _, err := net.SplitHostPort(host); err == nil {
		h = hh
	}
	h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	if net.ParseIP(h) != nil || strings.EqualFold(h, "localhost") {
		return true
	}
	name, _ := os.Hostname()
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	h = strings.ToLower(strings.TrimSuffix(h, "."))
	return name != "" && (h == name || h == name+".local" || h == name+".lan")
}

// sameOriginRequest keeps other web pages from driving the wizard through the user's browser: a request that
// changes something must be JSON (which a cross-site page cannot send without a preflight the server never
// grants) and must not name another origin.
func sameOriginRequest(r *http.Request) bool {
	if origins := r.Header.Values("Origin"); len(origins) != 0 &&
		(len(origins) != 1 || origins[0] != "http://"+r.Host) {
		return false
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return ct == "application/json"
}

func (s *Server) authorize(code string) int {
	if len(code) != 6 {
		return http.StatusUnauthorized
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if now.Before(s.nextGuess) {
		return http.StatusTooManyRequests
	}
	if subtle.ConstantTimeCompare([]byte(code), []byte(s.code)) != 1 {
		s.nextGuess = now.Add(2 * time.Second)
		return http.StatusUnauthorized
	}
	return http.StatusOK
}

func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Write(indexHTML)
	})
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) { writeResp(w, 200, s.state()) })
	mux.HandleFunc("POST /api/free", s.handleFree)
	mux.HandleFunc("POST /api/validate", s.handleValidate)
	mux.HandleFunc("POST /api/install", s.handleInstall)
	mux.HandleFunc("GET /api/job", s.handleJob)
	mux.HandleFunc("POST /api/finish", s.handleFinish)
	mux.HandleFunc("POST /api/reset", s.handleReset)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Atom-Wizard", "1") // the page tells the wizard from the panel that takes its port
		if !isPrivateRemote(r.RemoteAddr) || !hostAllowed(r.Host) || !sameOriginRequest(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if r.URL.Path != "/" {
			w.Header().Set("Cache-Control", "no-store")
			switch s.authorize(r.Header.Get("X-Atom-Install-Code")) {
			case http.StatusUnauthorized:
				writeErr(w, http.StatusUnauthorized, "请输入启动终端上显示的安装码")
				return
			case http.StatusTooManyRequests:
				w.Header().Set("Retry-After", "2")
				writeErr(w, http.StatusTooManyRequests, "请等两秒再试")
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}

func writeResp(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	data, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "cannot encode response", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(code)
	_, _ = w.Write(data)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeResp(w, code, map[string]string{"error": msg})
}

type defaults struct {
	Domestic    string `json:"domestic"`
	Foreign     string `json:"foreign"`
	FakeIPRange string `json:"fakeip_range"`
	MixedPort   int    `json:"mixed_port"`
	PanelPort   int    `json:"panel_port"`
}

type stateResp struct {
	Deployment    string            `json:"deployment,omitempty"` // "container" in the container version
	Container     *containerProbe   `json:"container,omitempty"`  // what the environment step shows there
	Version       string            `json:"version"`
	System        SystemInfo        `json:"system"`
	Ifaces        []Iface           `json:"ifaces"`
	Routes        []ClientRoute     `json:"routes"`   // routes via a gateway the machine has: the wizard fills the LAN side from them
	Gateways      map[string]string `json:"gateways"` // default gateway by interface: the wizard fills the main router from it
	Addrs         []string          `json:"addrs"`
	Port53        []Occupant        `json:"port53"`
	ScanError     string            `json:"scan_error,omitempty"`
	Defaults      defaults          `json:"defaults"`
	InstallerPort int               `json:"installer_port"`
	Installed     *struct {
		Version   string `json:"version"`
		PanelPort int    `json:"panel_port"`
	} `json:"installed,omitempty"`
	Running bool `json:"running"`
	HasData bool `json:"has_data"`
	// Reconfigure: the wizard edits the installed configuration; Current is what is installed now and Login
	// the panel login (nil: unknown).
	Reconfigure bool    `json:"reconfigure,omitempty"`
	Current     *Config `json:"current,omitempty"`
	Login       *struct {
		Enabled  bool   `json:"enabled"`
		Username string `json:"username"`
	} `json:"login,omitempty"`
	// Reset: the wizard was started by the panel's reset (reset.go).
	Reset *resetView `json:"reset,omitempty"`
}

// container says whether this is the container version.
func (s *Server) container() bool { return s.ins.Paths.Container }

// system is checkSystem for the machine, and for the container the capability probe: what is wrong with it is
// listed as the problems of the system, which the page shows and does not go on with.
func (s *Server) system(fresh bool) (SystemInfo, *containerProbe) {
	if !s.container() {
		return checkSystem(), nil
	}
	info := SystemInfo{Arch: runtime.GOARCH, Problems: []string{}}
	info.Hostname, _ = os.Hostname()
	data, _ := os.ReadFile("/etc/os-release")
	info.OS = parseOSRelease(string(data))["PRETTY_NAME"]
	pr := s.probe.get(fresh)
	info.Problems = append(info.Problems, pr.Problems...)
	return info, &pr
}

// validateConfig is Config.Validate with what the container changes: the topology is always the single NIC, the
// only interface is the LAN, nobody else listens on a port, and the port of the wizard is free for the panel
// (the installation moves it, see Installer.PanelTemp).
func (s *Server) validateConfig(cfg *Config) []FieldError {
	ifaces := listInterfaces()
	if !s.container() {
		return cfg.Validate(ifaces, s.ins.InstallerPort, s.busy())
	}
	cfg.Topology, cfg.WAN, cfg.LANGateway, cfg.ClientNets = "bypass", "", "", ""
	if len(cfg.LAN) == 0 && len(ifaces) == 1 {
		cfg.LAN = []string{ifaces[0].Name}
	}
	installerPort := s.ins.InstallerPort
	if s.firstRun {
		installerPort = 0
	}
	return cfg.Validate(ifaces, installerPort, func(int) string { return "" })
}

func (s *Server) state() stateResp {
	sys, probe := s.system(false)
	r := stateResp{Version: s.ins.Version, System: sys, Container: probe, Ifaces: listInterfaces(), Addrs: privateAddrs(), InstallerPort: s.ins.InstallerPort,
		Defaults: defaults{defaultDomestic, defaultForeign, defaultFakeIP, defaultMixed, defaultPanel}}
	if s.container() {
		r.Deployment = "container"
	}
	if r.Ifaces == nil {
		r.Ifaces = []Iface{}
	}
	if r.Addrs == nil {
		r.Addrs = []string{}
	}
	out, _ := quietCmd("ip", "-4", "route", "show")
	r.Routes = parseViaRoutes(out)
	r.Gateways = parseDefaultVia(out)
	if s.container() { // the container has a network namespace of its own: nobody else is on port 53
	} else if occ, err := portOccupants(53); err != nil {
		r.ScanError = err.Error()
		r.System.Problems = append(r.System.Problems, "需要 ss 命令（iproute2）来检查端口")
	} else {
		r.Port53 = occ
	}
	if r.Port53 == nil {
		r.Port53 = []Occupant{}
	}
	if st, err := loadState(s.ins.Paths); err == nil && st.Complete {
		r.Installed = &struct {
			Version   string `json:"version"`
			PanelPort int    `json:"panel_port"`
		}{st.Version, st.PanelPort}
	}
	r.HasData = hasData(s.ins.Paths)
	if s.ins.Reconfigure {
		r.Reconfigure = true
		if st, err := loadState(s.ins.Paths); err == nil {
			cur := configFromState(st)
			r.Current = &cur
			var l struct {
				Enabled  bool   `json:"enabled"`
				Username string `json:"username"`
			}
			if newAPIClient(st.PanelPort, s.ins.Paths.Data()).call("GET", "/auth", nil, &l) == nil {
				r.Login = &l
			}
		}
	}
	s.mu.Lock()
	r.Running = s.job != nil && !s.job.view(0).Done
	r.Reset = s.resetState()
	s.mu.Unlock()
	return r
}

func (s *Server) handleFree(w http.ResponseWriter, r *http.Request) {
	if s.container() {
		writeErr(w, 409, "容器里没有 53 端口的占用者")
		return
	}
	var req struct {
		Kind string `json:"kind"`
		Unit string `json:"unit"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil {
		writeErr(w, 400, "bad request")
		return
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if s.busyOrClosing() {
		writeErr(w, 409, "正在安装或已结束")
		return
	}
	occ, err := portOccupants(53)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	var target *Occupant
	for i := range occ {
		if occ[i].Kind == req.Kind && (req.Kind == "resolved" || occ[i].Unit == req.Unit) {
			target = &occ[i]
		}
	}
	if target == nil {
		writeErr(w, 409, "53 端口上已经没有这个占用者")
		return
	}
	switch req.Kind {
	case "resolved":
		err = freeResolvedStub(s.ins.Paths, stderrLogger{})
	case "openrc":
		err = freeOpenRCService(s.ins.Paths, target.Unit, stderrLogger{})
	default:
		err = freeUnit(s.ins.Paths, target.Unit, stderrLogger{})
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	time.Sleep(time.Second) // let the listeners go away before the next scan
	writeResp(w, 200, s.state())
}

func (s *Server) busy() func(port int) string {
	ls, err := listeners()
	if err != nil {
		return func(int) string { return "" }
	}
	return func(port int) string {
		unitOf, openrc := serviceOfPID()
		if occ := occupantsOf(ls, port, unitOf, openrc); len(occ) > 0 {
			return occ[0].Proc
		}
		return ""
	}
}

func (c *Config) fillDefaults() {
	if strings.TrimSpace(c.Domestic) == "" {
		c.Domestic = defaultDomestic
	}
	if strings.TrimSpace(c.Foreign) == "" {
		c.Foreign = defaultForeign
	}
	if strings.TrimSpace(c.FakeIPRange) == "" {
		c.FakeIPRange = defaultFakeIP
	}
	c.FakeIPRange = strings.TrimSpace(c.FakeIPRange)
	c.Topology = strings.TrimSpace(c.Topology)
}

// currentLoginUser is the user of the panel login of an installed machine ("" without one or off reconfigure).
func (s *Server) currentLoginUser() string {
	if !s.ins.Reconfigure {
		return ""
	}
	st, err := loadState(s.ins.Paths)
	if err != nil {
		return ""
	}
	var l struct {
		Username string `json:"username"`
	}
	_ = newAPIClient(st.PanelPort, s.ins.Paths.Data()).call("GET", "/auth", nil, &l)
	return l.Username
}

func (s *Server) handleValidate(w http.ResponseWriter, r *http.Request) {
	var cfg Config
	if json.NewDecoder(r.Body).Decode(&cfg) != nil {
		writeErr(w, 400, "bad request")
		return
	}
	cfg.fillDefaults()
	cfg.keepData = s.keepExistingData()
	cfg.currentUser = s.currentLoginUser()
	errs := s.validateConfig(&cfg)
	if errs == nil {
		errs = []FieldError{}
	}
	writeResp(w, 200, map[string]any{"errors": errs})
}

// panelTemp is the port atomdns runs on during the installation when the panel's port is the wizard's own (the
// first wizard of a container holds it until it closes); 0: the panel's own.
func (s *Server) panelTemp(panelPort int) (int, error) {
	if !s.firstRun || panelPort != s.ins.InstallerPort {
		return 0, nil
	}
	return freePort()
}

func (s *Server) handleInstall(w http.ResponseWriter, r *http.Request) {
	var cfg Config
	if json.NewDecoder(r.Body).Decode(&cfg) != nil {
		writeErr(w, 400, "bad request")
		return
	}
	s.opMu.Lock() // waits for a free action that is under way; nothing else changes the machine meanwhile
	defer s.opMu.Unlock()
	if s.busyOrClosing() {
		writeErr(w, 409, "正在安装或已结束")
		return
	}
	cfg.fillDefaults()
	cfg.keepData = s.keepExistingData()
	cfg.currentUser = s.currentLoginUser()
	sys, _ := s.system(true)
	if probs := sys.Problems; len(probs) > 0 {
		writeErr(w, 400, strings.Join(probs, "；"))
		return
	}
	if occ, err := portOccupants(53); !s.container() && (err != nil || len(occ) > 0) {
		writeErr(w, 409, "53 端口被占用")
		return
	}
	if errs := s.validateConfig(&cfg); len(errs) > 0 {
		writeResp(w, 400, map[string]any{"error": "配置不对", "errors": errs})
		return
	}
	tmp, err := s.panelTemp(cfg.PanelPort)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.ins.PanelTemp = tmp
	job := newJob(s.ins.stepNames())
	s.mu.Lock()
	s.job = job
	s.mu.Unlock()
	go s.ins.runInstall(cfg, job)
	writeResp(w, 202, map[string]bool{"started": true})
}

// keepExistingData: the installation takes over the data of an earlier one. Never after a reset: the reset is
// the promise that the next installation starts from the default settings.
func (s *Server) keepExistingData() bool {
	return s.reset == nil && hasData(s.ins.Paths)
}

// busyOrClosing: an installation is running, or finish was accepted. A reset wizard takes nothing before the
// reset is done, and after it no deadline applies: it stays until an installation succeeded.
func (s *Server) busyOrClosing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	expired := !s.firstRun && time.Since(s.seen) >= idleLimit
	if s.reset != nil {
		if s.reset.phase != resetReady {
			return true
		}
		expired = false
	}
	return s.closing || expired || (s.job != nil && !s.job.view(0).Done)
}

func (s *Server) handleJob(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	job := s.job
	s.mu.Unlock()
	if job == nil {
		writeResp(w, 200, map[string]bool{"idle": true})
		return
	}
	since := 0
	fmt.Sscanf(r.URL.Query().Get("since"), "%d", &since)
	writeResp(w, 200, job.view(since))
}

// requestClose is the one way the wizard ends, for the finish request (the page and the bootstrap script) and
// the idle timer alike: refused while an installation runs; otherwise the wizard is marked closing, after
// which no installation or free action is accepted.
func (s *Server) requestClose() bool {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job != nil && !s.job.view(0).Done {
		return false
	}
	if s.firstRun && (s.job == nil || !s.job.view(0).OK) { // nothing runs in the container until it is installed
		return false
	}
	// after a reset began, only a successful installation ends the wizard: the machine has no DNS until then
	if r := s.reset; r != nil && r.phase != resetWaiting && r.phase != resetFailed && (s.job == nil || !s.job.view(0).OK) {
		return false
	}
	s.closing = true
	return true
}

func (s *Server) handleFinish(w http.ResponseWriter, r *http.Request) {
	if !s.requestClose() {
		if s.reset != nil {
			writeErr(w, 409, "已开始重置，安装完成前安装向导不会关闭")
			return
		}
		writeErr(w, 409, "正在安装")
		return
	}
	writeResp(w, 200, map[string]bool{"ok": true})
	go func() {
		time.Sleep(500 * time.Millisecond)
		s.stop(true)
	}()
}

// stop ends the wizard; after a successful installation it also removes the unpacked package.
func (s *Server) stop(cleanup bool) {
	s.once.Do(func() {
		if cleanup {
			s.mu.Lock()
			ok := s.job != nil && s.job.view(0).OK
			s.mu.Unlock()
			if ok && s.ins.Paths.managedPackage(s.ins.PkgDir) {
				os.RemoveAll(s.ins.PkgDir)
			}
		}
		close(s.quit)
	})
}

// idleClose ends the wizard after the startup deadline, or shortly after success.
// A running installation can finish even if it outlives the deadline.
func (s *Server) idleClose() bool {
	if s.reset != nil {
		if closed, hold := s.resetIdle(); hold {
			return closed
		}
	}
	s.mu.Lock()
	expired := !s.firstRun && time.Since(s.seen) >= idleLimit
	var done, ok bool
	var sinceFinish time.Duration
	if s.job != nil {
		v := s.job.view(0)
		done, ok = v.Done, v.OK
		if done {
			sinceFinish = time.Since(s.job.finishedAt())
		}
	}
	running := s.job != nil && !done
	s.mu.Unlock()
	if running || (done && sinceFinish < afterDoneLimit) || !(expired || (done && ok)) {
		return false
	}
	if !s.requestClose() { // an installation started in the meantime
		return false
	}
	s.stop(done && ok)
	return true
}

func (s *Server) watchIdle() {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.quit:
			return
		case <-t.C:
			s.idleClose()
		}
	}
}
