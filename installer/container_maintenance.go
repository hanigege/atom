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
	case r.Method == "GET" && r.URL.Path == "/network":
		c.network(w)
	case r.URL.Path == "/network":
		writeErr(w, 400, "容器版的网络设置在容器里运行 atom-installer reconfigure 修改")
	case r.Method == "POST" && r.URL.Path == "/reset":
		c.start(w, r) // the same job as on a machine; the wizard it starts is the entry process's own (container_wizard.go)
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
	view := map[string]any{"version": st.Version, "online": false, "deployment": "container"}
	// A reset that began has taken this panel down with the rest: a panel that answers is a later one, which has
	// nothing to show of it.
	if job := c.currentJob(); job != nil && !(job.Done && job.Kind == "reset" && job.Phase == "reset") {
		view["job"] = job
	}
	writeResp(w, 200, view)
}

func (c containerMaint) currentJob() *maintenanceJob {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.job == nil {
		return nil
	}
	job := *c.job
	return &job
}
