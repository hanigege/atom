// atom-installer: the wizard that installs atom (atomdns + sing-box) under /opt/atom, and the commands to
// upgrade and uninstall it.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var version = "dev"

type manifest struct {
	Version string `json:"version"`
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: atom-installer <command>
  serve [--pkg DIR] [--port N] [--reconfigure]   start the installation wizard (--reconfigure: on an installed machine)
  urls                           print the wizard's addresses
  upgrade [--pkg DIR] [--force] [--keep-pkg]  upgrade an installed atom from an unpacked package
  uninstall [--purge] [--yes]    remove atom (the data stays unless --purge)
  version`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	args := os.Args[2:]
	switch os.Args[1] {
	case "serve":
		os.Exit(cmdServe(args))
	case "urls":
		os.Exit(cmdURLs())
	case "protect-wan":
		fs := flag.NewFlagSet("protect-wan", flag.ExitOnError)
		paths := defaultPaths()
		fs.StringVar(&paths.Root, "root", paths.Root, "installation root")
		start := fs.Bool("start", false, "replace old port protections before sing-box starts")
		fs.Parse(args)
		if err := protectWAN(paths, *start); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "upgrade":
		os.Exit(cmdUpgrade(args))
	case "uninstall":
		os.Exit(cmdUninstall(args))
	case "version":
		fmt.Println(version)
	default:
		usage()
		os.Exit(2)
	}
}

func newInstaller(pkg string) (*Installer, error) {
	if pkg == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, err
		}
		pkg = filepath.Dir(filepath.Dir(exe))
	}
	ins := &Installer{Paths: defaultPaths(), PkgDir: pkg, Version: version}
	var m manifest
	if err := readJSON(filepath.Join(pkg, "manifest.json"), &m); err == nil && m.Version != "" {
		ins.Version = m.Version
	}
	return ins, nil
}

type listenInfo struct {
	Port int    `json:"port"`
	PID  int    `json:"pid"`
	Code string `json:"code"`
}

func cmdServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	pkg := fs.String("pkg", "", "unpacked package (default: the directory above this program's)")
	port := fs.Int("port", defaultPort, "port to try first; a free one is chosen when it is taken")
	reconf := fs.Bool("reconfigure", false, "edit the configuration of an installed atom")
	fs.Parse(args)
	ins, err := newInstaller(*pkg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	ins.Reconfigure = *reconf
	if st, err := loadState(ins.Paths); *reconf && (err != nil || !st.Complete) {
		fmt.Fprintln(os.Stderr, "没有已完成的安装")
		return 1
	}
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		ln, err = net.Listen("tcp", ":0")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	ins.InstallerPort = ln.Addr().(*net.TCPAddr).Port
	defer ln.Close()
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	li := listenInfo{Port: ins.InstallerPort, PID: os.Getpid(), Code: fmt.Sprintf("%06d", n.Int64())}
	runFile := ins.Paths.Run("installer.json")
	data, err := json.MarshalIndent(li, "", "  ")
	if err == nil {
		err = writeFileAtomic(runFile, data, 0o600)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.Remove(runFile)

	s := newServer(ins, li.Code)
	srv := &http.Server{Handler: s.handler(), ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	go s.watchIdle()
	if os.Getenv("ATOM_WIZARD_BACKGROUND") != "1" {
		printWizardAccess(li)
	}
	<-s.quit
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
	return 0
}

func wizardURLs(port int) []string {
	var out []string
	for _, a := range privateAddrs() {
		out = append(out, fmt.Sprintf("http://%s:%d/", a, port))
	}
	if len(out) == 0 {
		out = append(out, fmt.Sprintf("http://localhost:%d/", port))
	}
	return out
}

func cmdURLs() int {
	var li listenInfo
	if err := readJSON(defaultPaths().Run("installer.json"), &li); err != nil {
		fmt.Fprintln(os.Stderr, "the wizard is not running")
		return 1
	}
	printWizardAccess(li)
	return 0
}

func printWizardAccess(li listenInfo) {
	for _, u := range wizardURLs(li.Port) {
		fmt.Println(u)
	}
	if li.Code != "" {
		fmt.Printf("\n=======================================\n  安装码  %s\n  请在浏览器输入此码。仅本次启动有效。\n=======================================\n", li.Code)
	}
}

type stdoutLogger struct{}

func (stdoutLogger) Logf(format string, args ...any) { fmt.Printf(format+"\n", args...) }

func cmdUpgrade(args []string) int {
	fs := flag.NewFlagSet("upgrade", flag.ExitOnError)
	pkg := fs.String("pkg", "", "unpacked package")
	force := fs.Bool("force", false, "upgrade to the same or an older version")
	keep := fs.Bool("keep-pkg", false, "leave the unpacked package (a reconfigure uses it next)")
	fs.Parse(args)
	cleanup := func() {
		if !*keep {
			os.RemoveAll(defaultPaths().Pkg())
		}
	}
	ins, err := newInstaller(*pkg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	st, err := loadState(ins.Paths)
	if err != nil || !st.Complete {
		fmt.Fprintln(os.Stderr, "没有已完成的安装；先安装")
		return 1
	}
	switch c := compareVersions(ins.Version, st.Version); {
	case c == 0 && !*force:
		fmt.Printf("已经是 %s\n", st.Version)
		cleanup()
		return 0
	case c < 0 && !*force:
		fmt.Fprintf(os.Stderr, "包的版本 %s 比已装的 %s 旧；要降级请加 --force\n", ins.Version, st.Version)
		cleanup()
		return 1
	}
	fmt.Printf("升级 %s → %s\n", st.Version, ins.Version)
	err = ins.upgrade(st, stdoutLogger{})
	cleanup()
	if err != nil {
		fmt.Fprintln(os.Stderr, "升级失败：", err)
		return 1
	}
	fmt.Printf("升级完成，面板端口 %d\n", st.PanelPort)
	return 0
}

func cmdUninstall(args []string) int {
	fs := flag.NewFlagSet("uninstall", flag.ExitOnError)
	purge := fs.Bool("purge", false, "also delete the data (rules, nodes, settings)")
	yes := fs.Bool("yes", false, "do not ask")
	fs.Parse(args)
	p := defaultPaths()
	if !hasRecords(p) {
		fmt.Fprintln(os.Stderr, "没有找到安装记录", p.State(), p.Changes())
		return 1
	}
	if !*yes {
		fmt.Printf("将停止并移除 atom 的服务、nft 规则、策略路由，删除 %s 下除数据外的全部文件", p.Root)
		if *purge {
			fmt.Print("，同时删除数据（规则、节点、设置）")
		}
		fmt.Print("。\n输入 yes 继续：")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if strings.TrimSpace(line) != "yes" {
			fmt.Println("已取消")
			return 1
		}
	}
	if err := uninstall(p, *purge, stdoutLogger{}); err != nil {
		fmt.Fprintln(os.Stderr, "卸载时有错误：", err)
		return 1
	}
	fmt.Println("已卸载")
	return 0
}
