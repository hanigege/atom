package main

// The container version (docs/container.md): `atom-installer container` is the entry process (PID 1) of the
// image. It does what systemd does on a Linux machine: loads the network rules, runs atomdns and the proxy
// kernel, restarts them when they die, and reloads the kernel when atomdns asks. The other processes of the
// image (`atom-installer svc ...`, which atomdns runs as the reload and stop commands) talk to it over the
// control socket.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	controlSocket = "control.sock"
	servicesFile  = "services.json"
	ringLines     = 200 // the lines of each service kept for Logs()
	maxLogLine    = 16 << 10
)

var (
	stopWait = 4 * time.Second // per service: SIGTERM, then SIGKILL; atomdns and one kernel stop within the 10 s of `docker stop`
	// syncNftFn is what the kernels' hooks and the network load call; a variable for the tests
	syncNftFn = syncNft
)

// serviceSpec is one service the entry process runs: what its systemd unit (renderDNSUnit, renderProxyUnit) says.
type serviceSpec struct {
	Name  string
	Bin   string
	Args  []string
	Env   []string
	Delay time.Duration // RestartSec
	// Group: of the services of one group (the proxy kernels, which share their ports and the nft sets) at most
	// one runs.
	Group string
	// Pre is ExecStartPre (and ConditionPathExists): a start does not happen when it fails.
	Pre func() error
	// Reload is ExecReload before the SIGHUP; nil: the service has no reload and is restarted.
	Reload func() error
}

// containerSpecs are the three services, in the order they are started (the kernels before atomdns).
func containerSpecs(p Paths) []serviceSpec {
	specs := []serviceSpec{
		{Name: "atom-sing-box"}, {Name: "atom-mihomo"}, {Name: "atom-dns"},
	}
	for i, backend := range []string{"sing-box", "mihomo"} {
		dir := filepath.Join(p.Data(), backend)
		config, work, cfgArg := "config.json", "run -D", "-c"
		if backend == "mihomo" {
			config, work, cfgArg = "config.yaml", "-d", "-f"
		}
		path := filepath.Join(dir, config)
		specs[i].Bin = p.Bin(backend)
		specs[i].Args = append(strings.Fields(work), dir, cfgArg, path)
		specs[i].Delay = time.Second
		specs[i].Group = "kernel"
		specs[i].Pre = func() error {
			if _, err := os.Stat(path); err != nil {
				return fmt.Errorf("没有 %s", path)
			}
			return syncNftFn(p, true, false, backend)
		}
		specs[i].Reload = func() error { return syncNftFn(p, false, false, backend) }
	}
	specs[2].Bin = p.Bin("atomdns")
	specs[2].Args = []string{"start", "-d", p.Data(), "-c", "config_custom.yaml"}
	specs[2].Env = []string{"ATOM_MAINTENANCE_SOCKET=" + p.Run(maintenanceSocket)}
	specs[2].Delay = 2 * time.Second
	specs[2].Pre = func() error { return loadContainerNetwork(p) }
	return specs
}

// loadContainerNetwork is what renderDNSUnit does before atomdns starts: the base nft rules, the policy routing
// and, for the single-NIC option that sends the answers through the main router, accept_redirects=0. The rules
// are rendered from install.json each time.
func loadContainerNetwork(p Paths) error {
	st, err := loadState(p)
	if err != nil {
		return err
	}
	if err := (&Installer{Paths: p}).writeNetworkFiles(st, stderrLogger{}); err != nil {
		return err
	}
	if err := syncNftFn(p, false, true, ""); err != nil {
		return err
	}
	if st.ReplyVia != "" {
		for _, dev := range []string{"all", st.LAN[0]} {
			f := "/proc/sys/net/ipv4/conf/" + dev + "/accept_redirects"
			if v, err := os.ReadFile(f); err == nil && strings.TrimSpace(string(v)) == "0" {
				continue
			}
			if err := os.WriteFile(f, []byte("0"), 0o644); err != nil {
				return fmt.Errorf("不能关闭 %s（%v）：在 compose 的 sysctls 里加 net.ipv4.conf.%s.accept_redirects=0", f, err, dev)
			}
		}
	}
	out, err := exec.Command("/bin/sh", p.Lib("policy-route.sh")).CombinedOutput()
	if err != nil {
		return fmt.Errorf("策略路由：%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// lineRing keeps the last lines of a service.
type lineRing struct {
	mu    sync.Mutex
	lines []string
}

func (r *lineRing) add(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, line)
	if len(r.lines) > ringLines {
		r.lines = append([]string(nil), r.lines[len(r.lines)-ringLines:]...)
	}
}

func (r *lineRing) last(n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n > len(r.lines) {
		n = len(r.lines)
	}
	return append([]string(nil), r.lines[len(r.lines)-n:]...)
}

// service is one supervised service. A loop goroutine exists from a start until a stop (or a clean exit of the
// process); it restarts the process when it dies.
type service struct {
	spec serviceSpec
	sup  *supervisor
	ring lineRing
	op   sync.Mutex // one start, stop or reload at a time

	mu   sync.Mutex
	cur  *exec.Cmd     // the running process
	stop chan struct{} // closed to end the loop; nil without a loop
	done chan struct{} // closed when the loop has ended
}

type supervisor struct {
	p    Paths
	svcs []*service
	out  io.Writer
	outM sync.Mutex

	stateMu sync.Mutex
	closing atomic.Bool // the container is stopping: nothing starts any more
	ready   atomic.Bool // the network is loaded and the services are being started: requests are served
	groupMu sync.Mutex  // one start of a group (the kernels) at a time
}

var (
	errClosing  = errors.New("容器正在停止")
	errNotReady = errors.New("容器还在启动")
)

func newSupervisor(p Paths, specs []serviceSpec, out io.Writer) *supervisor {
	s := &supervisor{p: p, out: out}
	for _, sp := range specs {
		s.svcs = append(s.svcs, &service{spec: sp, sup: s})
	}
	return s
}

func (s *supervisor) find(name string) *service {
	name = strings.TrimSuffix(name, ".service")
	for _, v := range s.svcs {
		if v.spec.Name == name {
			return v
		}
	}
	return nil
}

// Logf is a line of the entry process itself.
func (s *supervisor) Logf(format string, args ...any) { s.line("atom", fmt.Sprintf(format, args...)) }

func (s *supervisor) line(prefix, text string) {
	s.outM.Lock()
	defer s.outM.Unlock()
	fmt.Fprintf(s.out, "[%s] %s\n", prefix, text)
}

// pump copies the output of a service to the container's output, with its name in front, and keeps its last lines.
func (s *supervisor) pump(v *service, r io.ReadCloser) {
	defer r.Close()
	br := bufio.NewReaderSize(r, 4096)
	for {
		var line []byte
		for {
			part, more, err := br.ReadLine()
			if len(line) < maxLogLine {
				line = append(line, part...)
			}
			if err != nil {
				if len(line) > 0 {
					v.emit(string(line))
				}
				return
			}
			if !more {
				break
			}
		}
		v.emit(string(line))
	}
}

func (v *service) emit(line string) {
	v.ring.add(line)
	v.sup.line(v.spec.Name, line)
}

var errServicesCorrupt = errors.New("格式不对")

// loadDesired reads services.json: the services that are to run when the container starts. Without the file (a
// new installation) atomdns runs and starts the kernel itself.
func (s *supervisor) loadDesired() (map[string]string, error) {
	var m map[string]string
	if err := readJSON(s.p.Etc(servicesFile), &m); err != nil {
		if os.IsNotExist(err) {
			return map[string]string{"atom-dns": "running"}, nil
		}
		var se *json.SyntaxError
		var te *json.UnmarshalTypeError
		if errors.As(err, &se) || errors.As(err, &te) {
			return nil, fmt.Errorf("%s %w", servicesFile, errServicesCorrupt)
		}
		return nil, err
	}
	if m == nil { // null
		return nil, fmt.Errorf("%s %w", servicesFile, errServicesCorrupt)
	}
	for n, v := range m {
		if v != "running" && v != "stopped" {
			return nil, fmt.Errorf("%s %w：%s 的值是 %q", servicesFile, errServicesCorrupt, n, v)
		}
	}
	return m, nil
}

// desired is loadDesired for the readers that can do without it: what cannot be read counts as a new installation.
func (s *supervisor) desired() map[string]bool {
	m, err := s.loadDesired()
	if err != nil {
		m = map[string]string{"atom-dns": "running"}
	}
	out := map[string]bool{}
	for n, v := range m {
		out[n] = v == "running"
	}
	return out
}

func (s *supervisor) setDesired(name string, run bool) error {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	m, err := s.loadDesired()
	if errors.Is(err, errServicesCorrupt) {
		s.Logf("%v，按新安装处理并重写", err)
		m, err = map[string]string{"atom-dns": "running"}, nil
	}
	if err != nil {
		return fmt.Errorf("读取 %s：%w", servicesFile, err)
	}
	m[name] = stateWord(run)
	if err := writeJSON(s.p.Etc(servicesFile), m); err != nil {
		return fmt.Errorf("保存 %s：%w", servicesFile, err)
	}
	return nil
}

func stateWord(run bool) string {
	if run {
		return "running"
	}
	return "stopped"
}

// state is running (a process), failed (it died and will be restarted, or cannot start) or stopped.
func (v *service) state() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	switch {
	case v.cur != nil:
		return "running"
	case v.stop != nil:
		return "failed"
	}
	return "stopped"
}

func (v *service) pid() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.cur != nil && v.cur.Process != nil {
		return v.cur.Process.Pid
	}
	return 0
}

// spawn runs the process. The caller holds v.mu.
func (v *service) spawnLocked() error {
	cmd := exec.Command(v.spec.Bin, v.spec.Args...)
	cmd.Env = append(os.Environ(), v.spec.Env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	pr, pw, err := os.Pipe()
	if err != nil {
		return err
	}
	cmd.Stdout, cmd.Stderr = pw, pw
	err = cmd.Start()
	pw.Close()
	if err != nil {
		pr.Close()
		return err
	}
	go v.sup.pump(v, pr)
	v.cur = cmd
	return nil
}

// active: a process, or a loop that is to start one again.
func (v *service) active() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.cur != nil || v.stop != nil
}

// begin runs the start hook and the process; it fails when either does. With the process running it does
// nothing; with the loop waiting to try again, the start is that try, and when it fails the loop goes on trying.
// The caller holds v.op.
func (v *service) begin() error {
	if v.sup.closing.Load() {
		return errClosing
	}
	v.mu.Lock()
	running, retrying := v.cur != nil, v.stop != nil
	v.mu.Unlock()
	if running {
		return nil
	}
	if retrying {
		v.end()
	}
	err := v.tryStart()
	if err != nil && retrying && !v.sup.closing.Load() && !errors.Is(err, errGroupBusy) {
		v.mu.Lock()
		if v.stop == nil {
			v.startLoopLocked()
		}
		v.mu.Unlock()
	}
	return err
}

var errGroupBusy = errors.New("同时只能运行一个代理内核")

// begin's start: a first start (which makes the loop), or the retry of the loop itself.
func (v *service) tryStart() error { return v.launch(nil) }

// launch is the start hook and the process. loopStop is the stop channel of the loop that is retrying, nil for a
// first start, which makes the loop.
func (v *service) launch(loopStop chan struct{}) error {
	if v.spec.Group != "" {
		// one proxy kernel at a time, also for starts that overlap: the slot is held through the hook
		v.sup.groupMu.Lock()
		defer v.sup.groupMu.Unlock()
		for _, o := range v.sup.svcs {
			if o != v && o.spec.Group == v.spec.Group && o.active() {
				return fmt.Errorf("%s 在运行或等着重启：%w", o.spec.Name, errGroupBusy)
			}
		}
	}
	if v.spec.Pre != nil {
		if err := v.spec.Pre(); err != nil {
			return fmt.Errorf("%s 启动前的准备失败：%w", v.spec.Name, err)
		}
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.sup.closing.Load() { // shutdown began while the hook ran: it ends what it finds, so nothing starts here
		return errClosing
	}
	if loopStop != nil {
		select {
		case <-loopStop:
			return errClosing
		default:
		}
	}
	if err := v.spawnLocked(); err != nil {
		return err
	}
	if loopStop == nil {
		v.startLoopLocked()
	}
	return nil
}

func (v *service) startLoopLocked() {
	v.stop, v.done = make(chan struct{}), make(chan struct{})
	go v.loop(v.stop, v.done)
}

// loop waits for the process and starts it again after Delay when it did not exit cleanly (Restart=on-failure).
// With no process (a start that failed at boot) it begins with the retry. v.stop stays set until the loop is over,
// so that a stop that comes while another one is under way can wait for the same end.
func (v *service) loop(stop, done chan struct{}) {
	defer func() {
		v.mu.Lock()
		if v.done == done {
			v.stop = nil
		}
		v.mu.Unlock()
		close(done)
	}()
	for {
		v.mu.Lock()
		cmd := v.cur
		v.mu.Unlock()
		if cmd != nil {
			err := cmd.Wait()
			// what the process left behind (it was the leader of its group) goes with it, as with a systemd cgroup
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			v.mu.Lock()
			v.cur = nil
			select {
			case <-stop:
				v.mu.Unlock()
				return
			default:
			}
			v.mu.Unlock()
			if err == nil { // it ended by itself and well: stopped, not restarted
				v.sup.Logf("%s 已退出", v.spec.Name)
				return
			}
			v.sup.Logf("%s 异常退出：%v，%s 后重启", v.spec.Name, err, v.spec.Delay)
		}
		select {
		case <-stop:
			return
		case <-time.After(v.spec.Delay):
		}
		if v.sup.closing.Load() {
			return
		}
		if err := v.launch(stop); err != nil {
			if errors.Is(err, errClosing) {
				return
			}
			v.sup.Logf("%s 启动失败：%v，%s 后再试", v.spec.Name, err, v.spec.Delay)
		}
	}
}

// end stops the loop and the process: SIGTERM, and SIGKILL after stopWait. It gives up on a loop that does not end
// (a start hook that hangs) after stopWait more. A second caller waits for the same end. It does not take v.op.
func (v *service) end() {
	v.mu.Lock()
	stop, done, cmd := v.stop, v.done, v.cur
	if stop == nil {
		v.mu.Unlock()
		return
	}
	select {
	case <-stop:
	default:
		close(stop)
	}
	v.mu.Unlock()
	if cmd != nil {
		signalGroup(cmd, syscall.SIGTERM)
	}
	select {
	case <-done:
		return
	case <-time.After(stopWait):
	}
	if cmd != nil {
		v.sup.Logf("%s 没有在 %s 内停下，强制结束", v.spec.Name, stopWait)
		signalGroup(cmd, syscall.SIGKILL)
	}
	select {
	case <-done:
	case <-time.After(stopWait):
		v.sup.Logf("%s 的循环没有结束，不再等待", v.spec.Name)
	}
}

// signalGroup signals the process and what it started (it leads a process group of its own).
func signalGroup(cmd *exec.Cmd, sig syscall.Signal) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, sig)
	}
}

// Start, Stop, Restart and Reload are what the control socket offers; each one also records whether the service
// is to run when the container starts again.

// mutating is the common start of the operations that change a service: the container is up, not going down.
func (s *supervisor) mutating(name string) (*service, error) {
	v := s.find(name)
	switch {
	case v == nil:
		return nil, fmt.Errorf("没有这个服务：%s", name)
	case s.closing.Load():
		return nil, errClosing
	case !s.ready.Load():
		return nil, errNotReady
	}
	return v, nil
}

// committed records that a service that has just been started is to run; when that cannot be saved the start is
// taken back, so that the service is as the file says.
func (s *supervisor) committed(v *service, wasRunning bool) error {
	if err := s.setDesired(v.spec.Name, true); err != nil {
		if !wasRunning {
			v.end()
		}
		return err
	}
	return nil
}

func (s *supervisor) Start(name string) error {
	v, err := s.mutating(name)
	if err != nil {
		return err
	}
	v.op.Lock()
	defer v.op.Unlock()
	was := v.state() == "running"
	if err := v.begin(); err != nil {
		return err
	}
	return s.committed(v, was)
}

// Stop saves first: a stop that cannot be saved does not happen, or the service would come back with the container.
func (s *supervisor) Stop(name string) error {
	v := s.find(name)
	if v == nil {
		return fmt.Errorf("没有这个服务：%s", name)
	}
	v.op.Lock()
	defer v.op.Unlock()
	if !s.closing.Load() {
		if err := s.setDesired(v.spec.Name, false); err != nil {
			return err
		}
	}
	v.end()
	return nil
}

func (s *supervisor) Restart(name string) error {
	v, err := s.mutating(name)
	if err != nil {
		return err
	}
	v.op.Lock()
	defer v.op.Unlock()
	v.end()
	if err := v.begin(); err != nil {
		return err
	}
	return s.committed(v, false)
}

// Reload is `systemctl reload-or-restart`: the service runs, so it gets its hook and SIGHUP; else it starts.
func (s *supervisor) Reload(name string) error {
	v, err := s.mutating(name)
	if err != nil {
		return err
	}
	if v.spec.Reload == nil {
		return s.Restart(name)
	}
	v.op.Lock()
	defer v.op.Unlock()
	v.mu.Lock()
	cmd := v.cur
	v.mu.Unlock()
	if cmd == nil {
		if err := v.begin(); err != nil {
			return err
		}
		return s.committed(v, false)
	}
	if err := v.spec.Reload(); err != nil {
		return err
	}
	// SIGHUP goes to the main process only (ExecReload=kill -HUP $MAINPID). One that has died on the way is
	// started again by the loop, with the files the hook just wrote.
	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return s.setDesired(v.spec.Name, true)
}

// Enable records that the service is to run when the container starts; it does not start it.
func (s *supervisor) Enable(name string) error {
	v, err := s.mutating(name)
	if err != nil {
		return err
	}
	return s.setDesired(v.spec.Name, true)
}

// boot starts what services.json says. A service that cannot start now is retried like one that died.
func (s *supervisor) boot() {
	if _, err := s.loadDesired(); err != nil {
		s.Logf("%v，按新安装处理", err)
	}
	want := s.desired()
	// the kernels share ports and rules: of two that are marked running only the selected one starts
	if want["atom-sing-box"] && want["atom-mihomo"] {
		backend, _, _ := s.p.proxyService()
		other := "atom-mihomo"
		if backend == "mihomo" {
			other = "atom-sing-box"
		}
		s.Logf("%s 和 %s 都记为运行，只启动选中的内核", "atom-sing-box", "atom-mihomo")
		want[other] = false
		if err := s.setDesired(other, false); err != nil {
			s.Logf("%v", err)
		}
	}
	s.ready.Store(true)
	for _, v := range s.svcs {
		if s.closing.Load() {
			return
		}
		if !want[v.spec.Name] {
			continue
		}
		v.op.Lock()
		// what a request changed since the first read counts: the file is read again, under the operation lock
		if !s.desired()[v.spec.Name] {
			v.op.Unlock()
			continue
		}
		if err := v.begin(); err != nil && !errors.Is(err, errClosing) && !errors.Is(err, errGroupBusy) {
			s.Logf("%v，%s 后再试", err, v.spec.Delay)
			v.mu.Lock()
			if v.stop == nil && !s.closing.Load() {
				v.startLoopLocked()
			}
			v.mu.Unlock()
		}
		v.op.Unlock()
	}
}

// shutdown stops atomdns first, then the kernels, and leaves services.json as it is. Nothing starts after it
// has begun. Both steps are bounded by stopWait, so that `docker stop` (10 s) is enough.
func (s *supervisor) shutdown() {
	s.closing.Store(true)
	// end does not wait for the operation in progress: a start sees the flag before it spawns
	s.svcs[len(s.svcs)-1].end()
	var wg sync.WaitGroup
	for _, v := range s.svcs[:len(s.svcs)-1] {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v.end()
		}()
	}
	wg.Wait()
}

func (s *supervisor) logs(name string, n int) ([]string, error) {
	v := s.find(name)
	if v == nil {
		return nil, fmt.Errorf("没有这个服务：%s", name)
	}
	return v.ring.last(n), nil
}

type serviceInfo struct {
	Name    string `json:"name"`
	State   string `json:"state"`
	PID     int    `json:"pid,omitempty"`
	Desired string `json:"desired"`
}

func (s *supervisor) list() []serviceInfo {
	want := s.desired()
	out := make([]serviceInfo, 0, len(s.svcs))
	for _, v := range s.svcs {
		out = append(out, serviceInfo{Name: v.spec.Name, State: v.state(), PID: v.pid(), Desired: stateWord(want[v.spec.Name])})
	}
	return out
}

// cmdContainer is the entry process.
func cmdContainer([]string) int {
	os.Setenv("ATOM_DEPLOYMENT", "container") // the services and `svc` run from here see the same
	p := defaultPaths()
	raiseFileLimit()
	sup := newSupervisor(p, containerSpecs(p), os.Stdout)
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	closeSockets, err := serveContainerSockets(p, sup)
	if err != nil {
		sup.Logf("%v", err)
		return 1
	}
	defer closeSockets()

	st, err := loadState(p)
	if err != nil || !st.Complete {
		sup.Logf("%s 没有已完成的安装；安装向导还没有实现", p.State())
		return 1
	}
	if c := compareVersions(version, st.Version); version != "dev" && c < 0 {
		sup.Logf("镜像版本 %s 比数据的版本 %s 旧，拒绝启动；请换回新版镜像", version, st.Version)
		return 1
	} else if version != "dev" && c > 0 {
		sup.Logf("镜像版本 %s 比数据的版本 %s 新；数据迁移还没有实现，数据照原样使用", version, st.Version)
	}
	// the network and the start run beside the signal wait: a SIGTERM in the middle of them is answered
	failed := make(chan struct{})
	go func() {
		if err := loadContainerNetwork(p); err != nil {
			sup.Logf("加载网络失败（检查 NET_ADMIN 和内核的 nft_tproxy、nft_socket）：%v", err)
			close(failed)
			return
		}
		sup.Logf("atom %s 启动，面板端口 %d", st.Version, st.PanelPort)
		sup.boot()
	}()
	select {
	case <-failed:
		return 1
	case sig := <-sigs:
		sup.Logf("收到 %v，停止服务", sig)
	}
	start := time.Now()
	sup.shutdown()
	sup.Logf("已停止（%s）", time.Since(start).Round(time.Millisecond))
	return 0
}

// raiseFileLimit is LimitNOFILE=1048576 of the units; the services inherit it. It is best effort.
func raiseFileLimit() {
	lim := syscall.Rlimit{Cur: 1048576, Max: 1048576}
	if syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lim) != nil {
		if syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim) == nil {
			lim.Cur = lim.Max
			_ = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lim)
		}
	}
}
