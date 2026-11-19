package main

import "net/http"

// containerMaint is the maintenance service of the container version, which the entry process serves on
// maintenance.sock (container.md 5.3): the overview, the components table and the restart of a component. An
// update and a change of the network are the business of a new image and of `atom-installer reconfigure`.
type containerMaint struct{ *maintenance }

func containerMaintenance(p Paths) containerMaint {
	return containerMaint{&maintenance{paths: p, reload: make(chan struct{}, 1)}}
}

func (c containerMaint) handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	switch {
	case r.Method == "GET" && r.URL.Path == "/":
		c.overview(w)
	case r.Method == "GET" && r.URL.Path == "/services":
		c.services(w)
	case r.Method == "POST" && r.URL.Path == "/restart":
		c.restartService(w, r)
	case r.URL.Path == "/upgrade" || r.URL.Path == "/check":
		writeErr(w, 400, "容器版通过更换镜像更新")
	case r.URL.Path == "/network":
		writeErr(w, 400, "容器版的网络设置在容器里运行 atom-installer reconfigure 修改")
	case r.URL.Path == "/reset":
		writeErr(w, 400, "容器版的重置还没有实现")
	default:
		writeErr(w, 404, "没有这个维护接口")
	}
}

func (c containerMaint) overview(w http.ResponseWriter) {
	st, err := loadState(c.paths)
	if err != nil || !st.Complete {
		writeErr(w, 503, "没有已完成的 Atom 安装")
		return
	}
	writeResp(w, 200, map[string]any{"version": st.Version, "online": false, "deployment": "container"})
}
