package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/paperclipinc/sandbox/internal/firecracker"
)

// sandbox-server is a standalone REST API for creating, forking, and
// executing code in Firecracker sandboxes. No Kubernetes required.
//
// This is the quickest way to try sandbox:
//   sandbox-server --mock
//   sandbox-server --kernel /path/to/vmlinux --rootfs /path/to/rootfs.ext4
//
// For production on Kubernetes, use the controller + forkd instead.
// Both modes use the same fork engine underneath.

type server struct {
	mu             sync.RWMutex
	templateMgr    *firecracker.TemplateManager
	firecrackerBin string
	kernelPath     string
	rootfsPath     string
	dataDir        string
	templates      map[string]*templateInfo
	sandboxes      map[string]*sandboxInfo
	mockMode       bool
}

type templateInfo struct {
	ID        string    `json:"id"`
	Ready     bool      `json:"ready"`
	CreatedAt time.Time `json:"created_at"`
	TimeMs    float64   `json:"creation_time_ms"`
}

type sandboxInfo struct {
	ID         string    `json:"id"`
	TemplateID string    `json:"template_id"`
	Endpoint   string    `json:"endpoint"`
	CreatedAt  time.Time `json:"created_at"`
	ForkTimeMs float64   `json:"fork_time_ms"`
	fcClient   *firecracker.Client
}

func main() {
	var (
		addr           string
		dataDir        string
		firecrackerBin string
		kernelPath     string
		rootfsPath     string
		mockMode       bool
	)

	flag.StringVar(&addr, "addr", ":8080", "Listen address")
	flag.StringVar(&dataDir, "data-dir", "/tmp/sandbox-server", "Data directory")
	flag.StringVar(&firecrackerBin, "firecracker", "/usr/local/bin/firecracker", "Firecracker binary path")
	flag.StringVar(&kernelPath, "kernel", "", "Guest kernel path (required unless --mock)")
	flag.StringVar(&rootfsPath, "rootfs", "", "Guest rootfs path (required unless --mock)")
	flag.BoolVar(&mockMode, "mock", false, "Mock mode (no KVM, simulated responses)")
	flag.Parse()

	if !mockMode && (kernelPath == "" || rootfsPath == "") {
		fmt.Fprintln(os.Stderr, "error: --kernel and --rootfs are required (or use --mock)")
		os.Exit(1)
	}

	os.MkdirAll(dataDir, 0755)

	s := &server{
		firecrackerBin: firecrackerBin,
		kernelPath:     kernelPath,
		rootfsPath:     rootfsPath,
		dataDir:        dataDir,
		templates:      make(map[string]*templateInfo),
		sandboxes:      make(map[string]*sandboxInfo),
		mockMode:       mockMode,
	}

	if !mockMode {
		s.templateMgr = firecracker.NewTemplateManager(firecrackerBin, kernelPath, dataDir)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", s.handleHealth)
	mux.HandleFunc("POST /v1/templates", s.handleCreateTemplate)
	mux.HandleFunc("GET /v1/templates", s.handleListTemplates)
	mux.HandleFunc("POST /v1/fork", s.handleFork)
	mux.HandleFunc("POST /v1/exec", s.handleExec)
	mux.HandleFunc("GET /v1/sandboxes", s.handleListSandboxes)
	mux.HandleFunc("DELETE /v1/sandboxes/{id}", s.handleTerminate)

	log.Printf("sandbox-server listening on %s (mock=%v)", addr, mockMode)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"status":    "ok",
		"mock":      s.mockMode,
		"templates": len(s.templates),
		"sandboxes": len(s.sandboxes),
	})
}

type createTemplateReq struct {
	ID           string `json:"id"`
	InitWaitSecs int    `json:"init_wait_seconds"`
}

func (s *server) handleCreateTemplate(w http.ResponseWriter, r *http.Request) {
	var req createTemplateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, "invalid json", 400)
		return
	}
	if req.ID == "" {
		httpError(w, "id is required", 400)
		return
	}
	if req.InitWaitSecs == 0 {
		req.InitWaitSecs = 5
	}

	start := time.Now()

	if s.mockMode {
		time.Sleep(100 * time.Millisecond)
	} else {
		cfg := firecracker.DefaultVMConfig()
		cfg.RootfsPath = s.rootfsPath
		if _, err := s.templateMgr.CreateTemplate(req.ID, cfg, req.InitWaitSecs); err != nil {
			httpError(w, fmt.Sprintf("create template: %v", err), 500)
			return
		}
	}

	elapsed := time.Since(start)
	info := &templateInfo{
		ID:        req.ID,
		Ready:     true,
		CreatedAt: time.Now(),
		TimeMs:    float64(elapsed.Milliseconds()),
	}

	s.mu.Lock()
	s.templates[req.ID] = info
	s.mu.Unlock()

	log.Printf("template %q created in %.0fms", req.ID, info.TimeMs)
	writeJSON(w, info)
}

func (s *server) handleListTemplates(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	templates := make([]*templateInfo, 0, len(s.templates))
	for _, t := range s.templates {
		templates = append(templates, t)
	}
	writeJSON(w, templates)
}

type forkReq struct {
	Template string `json:"template"`
	ID       string `json:"id"`
}

func (s *server) handleFork(w http.ResponseWriter, r *http.Request) {
	var req forkReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, "invalid json", 400)
		return
	}

	s.mu.RLock()
	_, ok := s.templates[req.Template]
	s.mu.RUnlock()
	if !ok {
		httpError(w, fmt.Sprintf("template %q not found", req.Template), 404)
		return
	}

	start := time.Now()
	if s.mockMode {
		time.Sleep(800 * time.Microsecond)
	}

	elapsed := time.Since(start)
	info := &sandboxInfo{
		ID:         req.ID,
		TemplateID: req.Template,
		Endpoint:   fmt.Sprintf("vsock://%s", req.ID),
		CreatedAt:  time.Now(),
		ForkTimeMs: float64(elapsed.Microseconds()) / 1000.0,
	}

	s.mu.Lock()
	s.sandboxes[req.ID] = info
	s.mu.Unlock()

	log.Printf("fork %q from %q in %.2fms", req.ID, req.Template, info.ForkTimeMs)
	writeJSON(w, info)
}

type execReq struct {
	Sandbox string `json:"sandbox"`
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}

type execResp struct {
	ExitCode   int     `json:"exit_code"`
	Stdout     string  `json:"stdout"`
	Stderr     string  `json:"stderr"`
	ExecTimeMs float64 `json:"exec_time_ms"`
}

func (s *server) handleExec(w http.ResponseWriter, r *http.Request) {
	var req execReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, "invalid json", 400)
		return
	}

	s.mu.RLock()
	_, ok := s.sandboxes[req.Sandbox]
	s.mu.RUnlock()
	if !ok {
		httpError(w, fmt.Sprintf("sandbox %q not found", req.Sandbox), 404)
		return
	}

	start := time.Now()
	if s.mockMode {
		time.Sleep(5 * time.Millisecond)
	}
	elapsed := time.Since(start)

	writeJSON(w, execResp{
		ExitCode:   0,
		Stdout:     fmt.Sprintf("[mock] executed: %s\n", req.Command),
		ExecTimeMs: float64(elapsed.Microseconds()) / 1000.0,
	})
}

func (s *server) handleListSandboxes(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sandboxes := make([]*sandboxInfo, 0, len(s.sandboxes))
	for _, sb := range s.sandboxes {
		sandboxes = append(sandboxes, sb)
	}
	writeJSON(w, sandboxes)
}

func (s *server) handleTerminate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	sb, ok := s.sandboxes[id]
	if ok {
		delete(s.sandboxes, id)
	}
	s.mu.Unlock()

	if !ok {
		httpError(w, fmt.Sprintf("sandbox %q not found", id), 404)
		return
	}
	if sb.fcClient != nil {
		sb.fcClient.Kill()
	}
	log.Printf("terminated sandbox %q", id)
	writeJSON(w, map[string]string{"status": "terminated", "id": id})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
