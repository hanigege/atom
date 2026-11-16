package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"
)

type networkConfig struct {
	Topology      string   `json:"topology"`
	LAN           []string `json:"lan"`
	WAN           string   `json:"wan"`
	AcceptGateway bool     `json:"accept_gateway"`
	LANGateway    string   `json:"lan_gateway"`
	ClientNets    string   `json:"client_nets"`
	ReplyVia      bool     `json:"reply_via"`
	Router        string   `json:"router"`
	PanelPort     int      `json:"panel_port"`
}

type networkRequest struct {
	networkConfig
	Revision string `json:"revision"`
}

func networkRevision(st *InstallState) string { return st.UpdatedAt.Format(time.RFC3339Nano) }

func (m *maintenance) network(w http.ResponseWriter) {
	st, err := loadState(m.paths)
	if err != nil || !st.Complete {
		writeErr(w, 503, "没有已完成的 Atom 安装")
		return
	}
	c := configFromState(st)
	ifaces := listInterfaces()
	if ifaces == nil {
		ifaces = []Iface{}
	}
	writeResp(w, 200, map[string]any{
		"revision": networkRevision(st), "ifaces": ifaces,
		"config": networkConfig{
			Topology: c.Topology, LAN: c.LAN, WAN: c.WAN, AcceptGateway: c.AcceptGateway,
			LANGateway: c.LANGateway, ClientNets: c.ClientNets, ReplyVia: c.ReplyVia,
			Router: c.Router, PanelPort: c.PanelPort,
		},
	})
}

func panelIPs(lan []string, ifaces []Iface) []string {
	var ips []string
	for _, iface := range ifaces {
		if !slices.Contains(lan, iface.Name) {
			continue
		}
		for _, address := range iface.Addrs {
			if ip, _, err := net.ParseCIDR(address); err == nil && ip.To4() != nil {
				ips = append(ips, ip.String())
			}
		}
	}
	return ips
}

func panelURLs(ips []string, port int) []string {
	var urls []string
	for _, ip := range ips {
		urls = append(urls, "http://"+net.JoinHostPort(ip, strconv.Itoa(port))+"/#/settings")
	}
	return urls
}

func prepareNetworkJob(job *maintenanceJob, st *InstallState, request *networkRequest) {
	ifaces := listInterfaces()
	job.NewPanel = request.PanelPort
	job.NewIPs = panelIPs(request.LAN, ifaces)
	job.OldURLs = panelURLs(panelIPs(st.LAN, ifaces), st.PanelPort)
	job.NewURLs = panelURLs(job.NewIPs, request.PanelPort)
	job.SameAccess = st.PanelPort == request.PanelPort && slices.Equal(st.LAN, request.LAN)
}

func (m *maintenance) runNetwork(st *InstallState, request networkRequest) {
	if err := m.applyNetwork(st, request); err != nil {
		current := m.snapshot()
		if current.Blocking {
			m.rollback(current, err)
		} else {
			m.failed(err)
		}
	}
}

func (m *maintenance) applyNetwork(st *InstallState, request networkRequest) error {
	p := m.paths
	log := stderrLogger{}
	writes, err := m.holdWrites()
	if err != nil {
		return err
	}
	defer writes.Close()

	settings, err := installedProxySettings(p, st.PanelPort)
	if err != nil {
		return fmt.Errorf("读取当前 FakeIP 和 mixed 设置：%w", err)
	}
	n := request.networkConfig
	cfg := Config{
		Topology: n.Topology, LAN: n.LAN, WAN: n.WAN, AcceptGateway: n.AcceptGateway,
		LANGateway: n.LANGateway, ClientNets: n.ClientNets, ReplyVia: n.ReplyVia,
		Router: n.Router, PanelPort: n.PanelPort, FakeIPRange: settings.FakeIPRange, keepData: true,
	}
	if n.Topology == "bypass" {
		cfg.WAN = ""
	}
	ifaces := listInterfaces()
	if len(panelIPs(cfg.LAN, ifaces)) == 0 {
		return fmt.Errorf("需要选择至少一个具有 IPv4 地址的 LAN 网卡")
	}
	ls, err := listeners()
	if err != nil {
		return fmt.Errorf("检查面板端口占用：%w", err)
	}
	busy := func(port int) string {
		if port == st.PanelPort {
			return ""
		}
		if port == settings.MixedPort {
			return "mixed 代理"
		}
		for _, l := range ls {
			if l.Port == port {
				if l.Proc == "" {
					return "其他监听器"
				}
				return l.Proc
			}
		}
		return ""
	}
	if errs := cfg.Validate(ifaces, 0, busy); len(errs) != 0 {
		return fmt.Errorf("%s", errs[0].Msg)
	}
	ins := &Installer{Paths: p, Version: st.Version, Reconfigure: true}
	if err := ins.checkRouteTable(cfg); err != nil {
		return err
	}
	// Change only the listening port in the active configuration. Reusing the
	// package overlay would overwrite configuration unrelated to network setup.
	config := filepath.Join(p.Data(), "config_custom.yaml")
	data, err := os.ReadFile(config)
	if err != nil {
		return err
	}
	nextConfig, err := setPanelPort(string(data), cfg.PanelPort)
	if err != nil {
		return err
	}

	j := newJob(nil)
	if err := ins.stepDeps(cfg, j); err != nil {
		return err
	}
	backup := backupPath(p, "reconfigure")
	if err := m.begin(ins, backup, func(job *maintenanceJob) { prepareNetworkJob(job, st, &request) }); err != nil {
		return err
	}
	fail := func(err error) error {
		m.rollback(m.snapshot(), err)
		return nil
	}
	if err := ins.stepNetwork(cfg, j); err != nil {
		return fail(err)
	}
	if err := writeFileAtomic(config, []byte(nextConfig), 0o644); err != nil {
		return fail(err)
	}
	if err := ins.stepStart(cfg, j); err != nil {
		return fail(err)
	}
	if err := maintenanceHealthy(p, cfg.PanelPort, log); err != nil {
		return fail(err)
	}
	deadline := time.Now().Add(3 * time.Minute)
	confirm := make(chan struct{}, 1)
	m.mu.Lock()
	m.confirm = confirm
	job := *m.job
	job.Phase, job.Deadline = "awaiting_confirmation", &deadline
	err = m.save(job)
	m.mu.Unlock()
	if err != nil {
		return fail(err)
	}

	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-confirm:
	case <-timer.C:
		select {
		case <-confirm:
		default:
			return fail(fmt.Errorf("未在 3 分钟内从新面板确认"))
		}
	}
	if err := runCmd(log, 30*time.Second, "sync"); err != nil {
		return fail(err)
	}
	if err := m.finish("success", nil); err != nil {
		return fail(err)
	}
	pruneBackups(p.Backup(), 3)
	return nil
}

func (m *maintenance) confirmNetwork(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := decodeMaintenance(r, &req); err != nil {
		writeErr(w, 400, "无效的确认请求")
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	job := m.job
	if job == nil || job.ID != req.ID || job.Phase != "awaiting_confirmation" ||
		job.Deadline == nil || !time.Now().Before(*job.Deadline) || m.confirm == nil {
		writeErr(w, 409, "网络任务已结束或正在恢复")
		return
	}
	host, port, err := net.SplitHostPort(r.Header.Get("X-Atom-Local-Addr"))
	ip := net.ParseIP(host)
	if err != nil || port != strconv.Itoa(job.NewPanel) || ip == nil ||
		!(slices.Contains(job.NewIPs, ip.String()) || (job.SameAccess && ip.IsLoopback())) {
		writeErr(w, 409, "请通过新配置的 LAN 面板地址确认")
		return
	}
	m.confirm <- struct{}{}
	m.confirm = nil
	writeResp(w, 202, map[string]bool{"ok": true})
}
