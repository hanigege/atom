package main

// The container version (docs/container.md): `atom-installer container` is the entry process (PID 1) of the
// image. It does what systemd does on a Linux machine: loads the network rules, runs atomdns and the proxy
// kernel, restarts them when they die, and reloads the kernel when atomdns asks. The other processes of the
// image (`atom-installer svc ...`, which atomdns runs as the reload and stop commands) talk to it over the
// control socket.

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
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
		go sup.Logf("收到 %v，停止服务", sig) // a log sink that does not read must not hold the shutdown
	}
	start := time.Now()
	sup.shutdown()
	done := make(chan struct{})
	go func() { sup.Logf("已停止（%s）", time.Since(start).Round(time.Millisecond)); close(done) }()
	select {
	case <-done:
	case <-time.After(300 * time.Millisecond):
	}
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
