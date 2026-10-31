package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bitvibe/kestrel/internal/netx"
)

// headTailBuffer keeps the first and last bytes of streamed output so a
// runaway process cannot exhaust memory while the model still sees both the
// beginning and the end (including errors).
type headTailBuffer struct {
	mu      sync.Mutex
	head    []byte
	tail    []byte
	headMax int
	tailMax int
	total   int
}

func newHeadTailBuffer(headMax, tailMax int) *headTailBuffer {
	if headMax <= 0 {
		headMax = 32 * 1024
	}
	if tailMax <= 0 {
		tailMax = 16 * 1024
	}
	return &headTailBuffer{headMax: headMax, tailMax: tailMax}
}

func (b *headTailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	orig := len(p)
	b.total += len(p)
	if len(b.head) < b.headMax {
		n := b.headMax - len(b.head)
		if n > len(p) {
			n = len(p)
		}
		b.head = append(b.head, p[:n]...)
		p = p[n:]
	}
	if len(p) > 0 {
		b.tail = append(b.tail, p...)
		if len(b.tail) > b.tailMax {
			b.tail = append([]byte(nil), b.tail[len(b.tail)-b.tailMax:]...)
		}
	}
	return orig, nil
}

func (b *headTailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	omitted := b.total - len(b.head) - len(b.tail)
	var sb strings.Builder
	sb.Write(b.head)
	if omitted > 0 {
		fmt.Fprintf(&sb, "\n\n... [%d bytes omitted] ...\n\n", omitted)
	}
	sb.Write(b.tail)
	return sb.String()
}

func (b *headTailBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.total
}

type shellArgs struct {
	Command        string `json:"command"`
	WorkingDir     string `json:"working_dir,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
	Background     bool   `json:"background,omitempty"`
}

// ShellTool runs shell commands through bash.
type ShellTool struct {
	Deps      Deps
	Policy    ApprovalPolicy
	Processes *ProcessManager
}

func (t *ShellTool) Name() string { return "shell" }

func (t *ShellTool) Description() string {
	return "Run a shell command in bash. Returns combined stdout/stderr, the exit code, and kills the whole process group on timeout. Use background=true for servers or long jobs, then poll with the process tool. Working directory defaults to the workspace."
}

func (t *ShellTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"command":{"type":"string","description":"Bash command to run."},
			"working_dir":{"type":"string","description":"Directory to run in. Defaults to the workspace."},
			"timeout_seconds":{"type":"integer","description":"Timeout in seconds, capped by configuration."},
			"background":{"type":"boolean","description":"Start as a background process and return immediately."}
		},
		"required":["command"]
	}`)
}

func (t *ShellTool) Execute(ctx context.Context, raw json.RawMessage) (Result, error) {
	var a shellArgs
	if err := DecodeArgs(raw, &a); err != nil {
		return ErrorResult("%v", err), nil
	}
	a.Command = strings.TrimSpace(a.Command)
	if a.Command == "" {
		return ErrorResult("command is required"), nil
	}
	info, _ := SessionFromContext(ctx)
	ok, reason := t.Policy.Authorize(ctx, info, a.Command)
	if !ok {
		return ErrorResult("%s", reason), nil
	}
	workdir := t.workdir(a.WorkingDir)
	if strings.TrimSpace(a.WorkingDir) == "" && info.HomeDir != "" {
		workdir = t.workdir(info.HomeDir)
	}
	if st, err := os.Stat(workdir); err != nil || !st.IsDir() {
		return ErrorResult("working directory %q is not accessible", workdir), nil
	}
	maxOut := t.Deps.Config.Tools.MaxOutputBytes
	if maxOut <= 0 {
		maxOut = 64 * 1024
	}
	if a.Background {
		if t.Processes == nil {
			return ErrorResult("background processes are disabled"), nil
		}
		id, err := t.Processes.Start(a.Command, workdir)
		if err != nil {
			return ErrorResult("start background process: %v", err), nil
		}
		return TextResult("started background process %s\nUse process {action:\"output\",id:%q} to poll and {action:\"kill\",id:%q} to stop it.", id, id, id), nil
	}
	timeout := t.Deps.Config.Tools.CommandTimeout.D()
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	if a.TimeoutSeconds > 0 {
		requested := time.Duration(a.TimeoutSeconds) * time.Second
		if requested < timeout {
			timeout = requested
		}
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	buf := newHeadTailBuffer(maxOut/2, maxOut/2)
	code, runErr := runForeground(cctx, a.Command, workdir, t.Deps.childEnv(), buf)
	out := buf.String()
	var head string
	switch {
	case cctx.Err() == context.DeadlineExceeded:
		head = fmt.Sprintf("command timed out after %s (process group killed)\n", timeout)
	case cctx.Err() == context.Canceled:
		head = "command cancelled (process group killed)\n"
	case code != 0:
		head = fmt.Sprintf("exit code: %d\n", code)
	default:
		head = "exit code: 0\n"
	}
	if runErr != nil && code < 0 {
		head += "error: " + runErr.Error() + "\n"
	}
	if out == "" {
		out = "(no output)"
	}
	res := Result{Content: head + out}
	if code != 0 || runErr != nil {
		res.IsError = true
	}
	return res, nil
}

func (t *ShellTool) workdir(dir string) string {
	def := ""
	if t.Deps.Config != nil {
		def = t.Deps.Config.Workspace
	}
	if strings.TrimSpace(dir) == "" {
		dir = def
	}
	if dir == "" {
		dir, _ = os.Getwd()
	}
	if !filepath.IsAbs(dir) && def != "" {
		dir = filepath.Join(def, dir)
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	return dir
}

// Close stops background processes.
func (t *ShellTool) Close() error {
	if t.Processes != nil {
		return t.Processes.Close()
	}
	return nil
}

func (d Deps) childEnv() []string { return buildChildEnv(d.ProxySpec) }

// buildChildEnv filters obvious credentials while preserving proxy settings,
// then overlays the resolved proxy for curl/git/pip/npm.
func buildChildEnv(proxySpec string) []string {
	base := os.Environ()
	out := make([]string, 0, len(base)+12)
	overlayProxy := strings.TrimSpace(proxySpec) != ""
	for _, kv := range base {
		i := strings.IndexByte(kv, '=')
		if i <= 0 {
			continue
		}
		name := strings.ToUpper(kv[:i])
		if isProxyEnv(name) {
			if !overlayProxy {
				out = append(out, kv)
			}
			continue
		}
		if isSecretEnv(name) {
			continue
		}
		out = append(out, kv)
	}
	out = append(out,
		"TERM=dumb", "NO_COLOR=1", "PAGER=cat", "GIT_PAGER=cat",
		"DEBIAN_FRONTEND=noninteractive", "PYTHONUNBUFFERED=1",
	)
	out = append(out, netx.SubprocessEnv(proxySpec)...)
	return out
}

func isProxyEnv(name string) bool {
	switch name {
	case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY":
		return true
	}
	return false
}

func isSecretEnv(name string) bool {
	for _, needle := range []string{"TOKEN", "API_KEY", "APIKEY", "SECRET", "PASSWORD", "PASSWD", "CREDENTIAL", "PRIVATE_KEY"} {
		if strings.Contains(name, needle) {
			return true
		}
	}
	return false
}

// ProcessInfo describes a background process.
type ProcessInfo struct {
	ID      string
	Command string
	Running bool
	Started time.Time
	Output  string
}

type bgProcess struct {
	id      string
	command string
	cmd     *exec.Cmd
	out     *headTailBuffer
	done    chan struct{}
	started time.Time
	mu      sync.Mutex
	err     error
}

func (p *bgProcess) running() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

// ProcessManager supervises background shell processes.
type ProcessManager struct {
	mu     sync.Mutex
	procs  map[string]*bgProcess
	seq    int
	max    int
	work   string
	proxy  string
	closed bool
}

// NewProcessManager creates a manager.
func NewProcessManager(workdir, proxySpec string, max int) *ProcessManager {
	if max <= 0 {
		max = 8
	}
	return &ProcessManager{procs: make(map[string]*bgProcess), max: max, work: workdir, proxy: proxySpec}
}

// Start launches bash -lc command in the background.
func (m *ProcessManager) Start(command, workdir string) (string, error) {
	if m == nil {
		return "", fmt.Errorf("background processes are disabled")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return "", fmt.Errorf("process manager is closed")
	}
	running := 0
	for _, p := range m.procs {
		if p.running() {
			running++
		}
	}
	if running >= m.max {
		return "", fmt.Errorf("background process limit reached (%d)", m.max)
	}
	if workdir == "" {
		workdir = m.work
	}
	cmd := exec.Command(shellBinary(), "-lc", command)
	cmd.Dir = workdir
	cmd.Env = buildChildEnv(m.proxy)
	configureProcessGroup(cmd)
	buf := newHeadTailBuffer(8*1024, 8*1024)
	cmd.Stdout = buf
	cmd.Stderr = buf
	if err := cmd.Start(); err != nil {
		return "", err
	}
	m.seq++
	id := "proc_" + strconv.Itoa(m.seq)
	p := &bgProcess{id: id, command: command, cmd: cmd, out: buf, done: make(chan struct{}), started: time.Now()}
	m.procs[id] = p
	go func() {
		err := cmd.Wait()
		p.mu.Lock()
		p.err = err
		p.mu.Unlock()
		close(p.done)
	}()
	return id, nil
}

// Get returns a process snapshot.
func (m *ProcessManager) Get(id string) (ProcessInfo, bool) {
	m.mu.Lock()
	p, ok := m.procs[id]
	m.mu.Unlock()
	if !ok {
		return ProcessInfo{}, false
	}
	p.mu.Lock()
	err := p.err
	p.mu.Unlock()
	info := ProcessInfo{ID: p.id, Command: p.command, Running: p.running(), Started: p.started, Output: p.out.String()}
	if !info.Running && err != nil {
		info.Output += "\n(process ended with error: " + err.Error() + ")"
	}
	return info, true
}

// List returns all background processes.
func (m *ProcessManager) List() []ProcessInfo {
	m.mu.Lock()
	ids := make([]string, 0, len(m.procs))
	for id := range m.procs {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	out := make([]ProcessInfo, 0, len(ids))
	for _, id := range ids {
		if info, ok := m.Get(id); ok {
			out = append(out, info)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Kill terminates a process group and waits briefly.
func (m *ProcessManager) Kill(id string) error {
	m.mu.Lock()
	p, ok := m.procs[id]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown process %q", id)
	}
	if p.running() {
		killProcessGroup(p.cmd)
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
			_ = p.cmd.Process.Kill()
			select {
			case <-p.done:
			case <-time.After(2 * time.Second):
			}
		}
	}
	return nil
}

// Close kills every background process.
func (m *ProcessManager) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	procs := make([]*bgProcess, 0, len(m.procs))
	for _, p := range m.procs {
		procs = append(procs, p)
	}
	m.mu.Unlock()
	for _, p := range procs {
		if p.running() {
			killProcessGroup(p.cmd)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for _, p := range procs {
		remain := time.Until(deadline)
		if remain <= 0 {
			break
		}
		select {
		case <-p.done:
		case <-time.After(remain):
		}
	}
	return nil
}

// ProcessTool polls and kills background shell processes.
type ProcessTool struct {
	Deps      Deps
	Processes *ProcessManager
}

func (t *ProcessTool) Name() string { return "process" }
func (t *ProcessTool) Description() string {
	return "List, inspect or kill background processes started with shell background=true."
}
func (t *ProcessTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"action":{"type":"string","enum":["list","output","kill"]},
			"id":{"type":"string","description":"Process ID for output/kill."}
		},
		"required":["action"]
	}`)
}

func (t *ProcessTool) Execute(ctx context.Context, raw json.RawMessage) (Result, error) {
	var a struct {
		Action string `json:"action"`
		ID     string `json:"id"`
	}
	if err := DecodeArgs(raw, &a); err != nil {
		return ErrorResult("%v", err), nil
	}
	if t.Processes == nil {
		return ErrorResult("background processes are disabled"), nil
	}
	switch a.Action {
	case "list":
		procs := t.Processes.List()
		if len(procs) == 0 {
			return TextResult("no background processes"), nil
		}
		var b strings.Builder
		for _, p := range procs {
			fmt.Fprintf(&b, "%s  running=%v  started=%s  %s\n", p.ID, p.Running, p.Started.Format(time.RFC3339), p.Command)
		}
		return Result{Content: b.String()}, nil
	case "output":
		p, ok := t.Processes.Get(a.ID)
		if !ok {
			return ErrorResult("unknown process %q", a.ID), nil
		}
		status := "running"
		if !p.Running {
			status = "exited"
		}
		return TextResult("process %s (%s): %s\n\n%s", p.ID, status, p.Command, p.Output), nil
	case "kill":
		if err := t.Processes.Kill(a.ID); err != nil {
			return ErrorResult("%v", err), nil
		}
		return TextResult("process %s killed", a.ID), nil
	default:
		return ErrorResult("action must be list, output or kill"), nil
	}
}

func (t *ProcessTool) Close() error {
	if t.Processes != nil {
		return t.Processes.Close()
	}
	return nil
}

func shellBinary() string {
	if p, err := exec.LookPath("bash"); err == nil {
		return p
	}
	if p, err := exec.LookPath("sh"); err == nil {
		return p
	}
	return "bash"
}

// runForeground runs a command with a process group, bounded output and
// prompt group termination on cancellation.
func runForeground(ctx context.Context, command, workdir string, env []string, out *headTailBuffer) (int, error) {
	cmd := exec.Command(shellBinary(), "-lc", command)
	cmd.Dir = workdir
	cmd.Env = env
	cmd.Stdout = out
	cmd.Stderr = out
	configureProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		return -1, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			return 0, nil
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode(), err
		}
		return -1, err
	case <-ctx.Done():
		killProcessGroup(cmd)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
		}
		return -1, ctx.Err()
	}
}
