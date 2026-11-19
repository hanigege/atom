package main

// The supervisor of the entry process is an actor: one goroutine (run) owns all state, the services' phases, their
// processes, the retry timers, the kernel slot and the shutdown. Everything else is an event posted to it: control
// requests, process exits, timers, the completion of a start hook, SIGTERM. The loop never waits for a command: hooks
// and process starts run in goroutines and post their result back. Each start, retry and stop has a generation
// number; a result that is not the current one (the service was stopped or started again meanwhile) is dropped, and
// a process it brought is killed.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

var (
	errServicesCorrupt = errors.New("格式不对")
	errGroupBusy       = errors.New("同时只能运行一个代理内核")
	errClosing         = errors.New("容器正在停止")
	errNotReady        = errors.New("容器还在启动")
	errSuperseded      = errors.New("被后来的操作取代")
)

// phases of a service
const (
	phStopped  = "stopped"
	phStarting = "starting" // the start hook runs and the process is being started
	phRunning  = "running"
	phBackoff  = "backoff"  // it died, or could not start: waiting for the next try
	phStopping = "stopping" // SIGTERM sent
)

type proc struct{ cmd *exec.Cmd }

type service struct {
	spec serviceSpec
	ring lineRing // has its own lock: Logs() reads it without the loop

	// owned by the loop
	phase   string
	gen     uint64 // of the current start, retry wait or stop; results carry the one they were made for
	proc    *proc
	retry   bool // a failed or dead service is tried again (it was started by the boot, or has run)
	timer   *time.Timer
	waiters []func(error) // answered when the current start ends
	// startAfterStop: a start came while the service was stopping
	startAfterStop bool
	stopWaiters    []func()
}

type supervisor struct {
	p    Paths
	svcs []*service // in start order: the kernels, then atomdns
	out  io.Writer
	outM sync.Mutex

	ev   chan func()
	quit chan struct{} // closed when the loop ends: after the shutdown

	// owned by the loop
	ready, closing  bool
	kernelsSignaled bool
	shutdownAt      time.Time
}

func newSupervisor(p Paths, specs []serviceSpec, out io.Writer) *supervisor {
	s := &supervisor{p: p, out: out, ev: make(chan func(), 64), quit: make(chan struct{})}
	for _, sp := range specs {
		s.svcs = append(s.svcs, &service{spec: sp, phase: phStopped})
	}
	go func() {
		for {
			select {
			case f := <-s.ev:
				f()
			case <-s.quit:
				return
			}
			select { // after the end nothing more runs
			case <-s.quit:
				return
			default:
			}
		}
	}()
	return s
}

// post hands an event to the loop; once the loop has ended it is dropped.
func (s *supervisor) post(f func()) {
	select {
	case s.ev <- f:
	case <-s.quit:
	}
}

// do runs f in the loop and waits for the answer f gives through reply (possibly much later). A request whose ctx
// is over before the loop gets to it is dropped. After the loop has ended nothing is served: errClosing.
func (s *supervisor) do(ctx context.Context, f func(reply func(error))) error {
	res := make(chan error, 1)
	run := func() {
		if err := ctx.Err(); err != nil {
			res <- err
			return
		}
		f(func(err error) { res <- err })
	}
	select {
	case s.ev <- run:
	case <-s.quit:
		return errClosing
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-res:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-s.quit:
		select {
		case err := <-res:
			return err
		default:
			return errClosing
		}
	}
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
					s.emit(v, string(line))
				}
				return
			}
			if !more {
				break
			}
		}
		s.emit(v, string(line))
	}
}

func (s *supervisor) emit(v *service, line string) {
	v.ring.add(line)
	s.line(v.spec.Name, line)
}

// services.json: the services that are to run when the container starts. Without the file (a new installation)
// atomdns runs and starts the kernel itself.

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
	m, err := s.loadDesired()
	if errors.Is(err, errServicesCorrupt) {
		s.Logf("%v，按新安装处理并重写", err)
		m, err = map[string]string{"atom-dns": "running"}, nil
	}
	if err != nil {
		return fmt.Errorf("读取 %s：%w", servicesFile, err)
	}
	if run {
		m[name] = "running"
	} else {
		m[name] = "stopped"
	}
	if err := writeJSON(s.p.Etc(servicesFile), m); err != nil {
		return fmt.Errorf("保存 %s：%w", servicesFile, err)
	}
	return nil
}

// signalGroup signals a process and what it started (it leads a process group of its own).
func signalGroup(p *proc, sig syscall.Signal) {
	if p.cmd.Process != nil {
		_ = syscall.Kill(-p.cmd.Process.Pid, sig)
	}
}

// spawn starts the process of a service with its output in the pump.
func (s *supervisor) spawn(v *service) (*proc, error) {
	cmd := exec.Command(v.spec.Bin, v.spec.Args...)
	cmd.Env = append(os.Environ(), v.spec.Env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = pw, pw
	err = cmd.Start()
	pw.Close()
	if err != nil {
		pr.Close()
		return nil, err
	}
	go s.pump(v, pr)
	return &proc{cmd}, nil
}

// ---- everything below runs in the loop ----

// begin starts the service: the hook and the process in a goroutine, the result comes back as an event.
func (s *supervisor) begin(v *service) {
	if v.spec.Group != "" {
		for _, o := range s.svcs { // the kernel slot is taken from the moment a start is under way until it is stopped
			if o != v && o.spec.Group == v.spec.Group && o.phase != phStopped {
				s.failStart(v, fmt.Errorf("%s 在运行或等着重启：%w", o.spec.Name, errGroupBusy))
				return
			}
		}
	}
	s.stopTimer(v)
	v.gen++
	g := v.gen
	v.phase = phStarting
	go func() {
		var p *proc
		var err error
		if v.spec.Pre != nil {
			if err = v.spec.Pre(); err != nil {
				err = fmt.Errorf("%s 启动前的准备失败：%w", v.spec.Name, err)
			}
		}
		if err == nil {
			p, err = s.spawn(v)
		}
		s.post(func() { s.started(v, g, p, err) })
	}()
}

func (s *supervisor) started(v *service, g uint64, p *proc, err error) {
	if v.gen != g || v.phase != phStarting { // dropped: the service was stopped or started again meanwhile
		if p != nil {
			signalGroup(p, syscall.SIGKILL)
			go p.cmd.Wait()
		}
		return
	}
	if err != nil {
		s.failStart(v, err)
		return
	}
	v.proc, v.phase, v.retry = p, phRunning, true
	go func() {
		err := p.cmd.Wait()
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL) // what it left behind goes with it, as in a systemd cgroup
		s.post(func() { s.exited(v, p, err) })
	}()
	s.answer(v, nil)
}

// answer tells the requests that wait for the end of a start.
func (s *supervisor) answer(v *service, err error) {
	ws := v.waiters
	v.waiters = nil
	for _, w := range ws {
		w(err)
	}
}

func (s *supervisor) failStart(v *service, err error) {
	s.Logf("%v", err)
	switch {
	case s.closing:
		v.phase = phStopped
	case v.retry:
		s.backoff(v)
	default:
		v.phase = phStopped
	}
	s.answer(v, err)
	s.settled()
}

func (s *supervisor) backoff(v *service) {
	s.stopTimer(v)
	v.gen++
	g := v.gen
	v.phase = phBackoff
	v.timer = time.AfterFunc(v.spec.Delay, func() {
		s.post(func() {
			if v.gen == g && v.phase == phBackoff && !s.closing {
				s.begin(v)
			}
		})
	})
}

func (s *supervisor) stopTimer(v *service) {
	if v.timer != nil {
		v.timer.Stop()
		v.timer = nil
	}
}

func (s *supervisor) exited(v *service, p *proc, err error) {
	if v.proc != p {
		return
	}
	v.proc = nil
	switch {
	case v.phase == phStopping || s.closing:
		s.stopped(v)
	case err == nil: // it ended by itself and well: stopped, not restarted
		s.Logf("%s 已退出", v.spec.Name)
		v.phase, v.retry = phStopped, false
		s.settled()
	default:
		s.Logf("%s 异常退出：%v，%s 后重启", v.spec.Name, err, v.spec.Delay)
		s.backoff(v)
	}
}

// stopped: the service is down. A start that came meanwhile goes on.
func (s *supervisor) stopped(v *service) {
	v.phase, v.retry = phStopped, false
	ws := v.stopWaiters
	v.stopWaiters = nil
	for _, w := range ws {
		w()
	}
	if v.startAfterStop && !s.closing {
		v.startAfterStop = false
		s.begin(v)
		return
	}
	s.settled()
}

// stop takes the service down: SIGTERM, SIGKILL after wait. w, if any, is called when it is down.
func (s *supervisor) stop(v *service, wait time.Duration, w func()) {
	v.retry = false
	switch v.phase {
	case phStopped:
		if w != nil {
			w()
		}
	case phStarting, phBackoff: // nothing runs: the result of the start in progress is dropped when it comes
		s.stopTimer(v)
		v.gen++
		v.phase = phStopped
		s.answer(v, errSuperseded)
		if w != nil {
			w()
		}
		s.settled()
	case phRunning:
		v.phase = phStopping
		v.gen++
		p := v.proc
		if w != nil {
			v.stopWaiters = append(v.stopWaiters, w)
		}
		signalGroup(p, syscall.SIGTERM)
		g := v.gen
		time.AfterFunc(wait, func() {
			s.post(func() {
				if v.gen == g && v.proc == p && v.phase == phStopping {
					s.Logf("%s 没有在 %s 内停下，强制结束", v.spec.Name, wait)
					signalGroup(p, syscall.SIGKILL)
				}
			})
		})
	case phStopping:
		if w != nil {
			v.stopWaiters = append(v.stopWaiters, w)
		}
	}
}

// requestStart: w is answered when the service runs (or cannot be started).
func (s *supervisor) requestStart(v *service, w func(error)) {
	switch v.phase {
	case phRunning:
		w(nil)
	case phStarting:
		v.waiters = append(v.waiters, w)
	case phStopping:
		v.startAfterStop = true
		v.waiters = append(v.waiters, w)
	default: // stopped, or waiting to try again: this is the try
		v.waiters = append(v.waiters, w)
		s.begin(v)
	}
}

// settled: after a service has come to rest. During the shutdown it moves on: atomdns first, then the kernels.
func (s *supervisor) settled() {
	if !s.closing {
		return
	}
	dns := s.svcs[len(s.svcs)-1]
	if dns.phase == phStopped && !s.kernelsSignaled {
		s.kernelsSignaled = true
		left := time.Until(s.shutdownAt) - 100*time.Millisecond // the kill has time to take effect before the end
		if left > stopWait {
			left = stopWait
		}
		if left < 0 {
			left = 0
		}
		for _, v := range s.svcs[:len(s.svcs)-1] {
			s.stop(v, left, nil)
		}
	}
	for _, v := range s.svcs {
		if v.phase != phStopped {
			return
		}
	}
	s.finish()
}

func (s *supervisor) finish() {
	select {
	case <-s.quit:
	default:
		close(s.quit)
	}
}

// ---- the operations ----

// mutating checks what every request that changes a service needs.
func (s *supervisor) mutating(name string) (*service, error) {
	v := s.find(name)
	switch {
	case v == nil:
		return nil, fmt.Errorf("没有这个服务：%s", name)
	case s.closing:
		return nil, errClosing
	case !s.ready:
		return nil, errNotReady
	}
	return v, nil
}

// recorded is the answer of a start request: the service is to run when the container starts again; when that
// cannot be saved, the start is taken back (unless the service ran before).
func (s *supervisor) recorded(v *service, was bool, reply func(error)) func(error) {
	return func(err error) {
		if err == nil {
			if err = s.setDesired(v.spec.Name, true); err != nil && !was {
				s.stop(v, stopWait, func() { reply(err) })
				return
			}
		}
		reply(err)
	}
}

func (s *supervisor) Start(ctx context.Context, name string) error {
	return s.do(ctx, func(reply func(error)) {
		v, err := s.mutating(name)
		if err != nil {
			reply(err)
			return
		}
		s.requestStart(v, s.recorded(v, v.phase == phRunning, reply))
	})
}

// Stop records first: a stop that cannot be recorded does not happen, or the service would come back with the container.
func (s *supervisor) Stop(ctx context.Context, name string) error {
	return s.do(ctx, func(reply func(error)) {
		v := s.find(name)
		switch {
		case v == nil:
			reply(fmt.Errorf("没有这个服务：%s", name))
		case s.closing:
			reply(errClosing)
		default:
			if err := s.setDesired(v.spec.Name, false); err != nil {
				reply(err)
				return
			}
			s.stop(v, stopWait, func() { reply(nil) })
		}
	})
}

func (s *supervisor) Restart(ctx context.Context, name string) error {
	return s.do(ctx, func(reply func(error)) {
		v, err := s.mutating(name)
		if err != nil {
			reply(err)
			return
		}
		if v.phase == phRunning {
			// stop, then start again: the start waits in v.waiters for the stop to be done
			v.waiters = append(v.waiters, s.recorded(v, false, reply))
			v.startAfterStop = true
			s.stop(v, stopWait, nil)
			return
		}
		s.requestStart(v, s.recorded(v, false, reply))
	})
}

// Reload is `systemctl reload-or-restart`: the service runs, so it gets its hook and SIGHUP; else it starts.
func (s *supervisor) Reload(ctx context.Context, name string) error {
	return s.do(ctx, func(reply func(error)) {
		v, err := s.mutating(name)
		if err != nil {
			reply(err)
			return
		}
		if v.spec.Reload == nil {
			if v.phase == phRunning {
				v.waiters = append(v.waiters, s.recorded(v, false, reply))
				v.startAfterStop = true
				s.stop(v, stopWait, nil)
				return
			}
			s.requestStart(v, s.recorded(v, false, reply))
			return
		}
		if v.phase != phRunning {
			s.requestStart(v, s.recorded(v, false, reply))
			return
		}
		g, p := v.gen, v.proc
		go func() {
			herr := v.spec.Reload()
			s.post(func() {
				switch {
				case herr != nil:
					reply(herr)
				case v.gen != g || v.proc != p || v.phase != phRunning:
					reply(errSuperseded)
				default:
					// SIGHUP goes to the main process only (ExecReload=kill -HUP $MAINPID)
					if err := p.cmd.Process.Signal(syscall.SIGHUP); err != nil && !errors.Is(err, os.ErrProcessDone) {
						reply(err)
						return
					}
					reply(s.setDesired(v.spec.Name, true))
				}
			})
		}()
	})
}

// Enable records that the service is to run when the container starts; it does not start it.
func (s *supervisor) Enable(ctx context.Context, name string) error {
	return s.do(ctx, func(reply func(error)) {
		v, err := s.mutating(name)
		if err != nil {
			reply(err)
			return
		}
		reply(s.setDesired(v.spec.Name, true))
	})
}

// boot starts what services.json says, the kernels first and atomdns when they have started (or failed: they are
// then tried again). It returns when that is done.
func (s *supervisor) boot() {
	done := make(chan struct{})
	s.post(func() {
		if s.closing {
			close(done)
			return
		}
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
			s.Logf("atom-sing-box 和 atom-mihomo 都记为运行，只启动选中的内核")
			if err := s.setDesired(other, false); err != nil {
				s.Logf("%v", err)
			}
		}
		s.ready = true
		var next func(i int)
		next = func(i int) {
			for ; i < len(s.svcs); i++ {
				v := s.svcs[i]
				if s.closing || !s.desired()[v.spec.Name] {
					continue
				}
				v.retry = true
				s.requestStart(v, func(error) { next(i + 1) })
				return
			}
			close(done)
		}
		next(0)
	})
	select {
	case <-done:
	case <-s.quit:
	}
}

// shutdown stops atomdns first, then the kernels, and leaves services.json as it is. Nothing starts or stops by
// request after it has begun; what is starting is dropped. The whole takes at most 2*stopWait (8 s: `docker stop`
// allows 10): a hook that hangs is not waited for.
func (s *supervisor) shutdown() {
	s.post(func() {
		if s.closing {
			return
		}
		s.closing = true
		s.shutdownAt = time.Now().Add(2 * stopWait)
		time.AfterFunc(2*stopWait, func() { s.post(s.finish) })
		for _, v := range s.svcs {
			v.startAfterStop = false
			if v.phase == phStarting || v.phase == phBackoff {
				s.stop(v, 0, nil)
			}
			s.answer(v, errClosing)
		}
		s.stop(s.svcs[len(s.svcs)-1], stopWait, nil)
		s.settled()
	})
	<-s.quit
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

// list: running (a process), failed (it died and will be tried again) or stopped.
func (s *supervisor) list() []serviceInfo {
	var out []serviceInfo
	read := func() {
		want := s.desired()
		for _, v := range s.svcs {
			st := phStopped
			switch v.phase {
			case phRunning, phStopping:
				st = "running"
			case phBackoff:
				st = "failed"
			}
			info := serviceInfo{Name: v.spec.Name, State: st, Desired: "stopped"}
			if want[v.spec.Name] {
				info.Desired = "running"
			}
			if v.proc != nil && v.proc.cmd.Process != nil {
				info.PID = v.proc.cmd.Process.Pid
			}
			out = append(out, info)
		}
	}
	var once sync.Once
	done := make(chan struct{})
	select {
	case s.ev <- func() { once.Do(read); close(done) }:
		select {
		case <-done:
		case <-s.quit:
			once.Do(read) // the loop ended before it came to this
		}
	case <-s.quit:
		once.Do(read)
	}
	return out
}
