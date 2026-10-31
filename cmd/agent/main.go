package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bitvibe/kestrel/internal/agent"
	"github.com/bitvibe/kestrel/internal/config"
	"github.com/bitvibe/kestrel/internal/gateway/telegram"
	"github.com/bitvibe/kestrel/internal/logx"
	"github.com/bitvibe/kestrel/internal/netx"
	"github.com/bitvibe/kestrel/internal/provider"
	"github.com/bitvibe/kestrel/internal/sched"
	"github.com/bitvibe/kestrel/internal/store"
	"github.com/bitvibe/kestrel/internal/tools"
	"gopkg.in/yaml.v3"
)

var version = "dev"
var commit = "unknown"
var buildDate = "unknown"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "agent:", logx.Redact(err.Error()))
		os.Exit(1)
	}
}

func run(args []string) error {
	global := flag.NewFlagSet("agent", flag.ContinueOnError)
	global.SetOutput(os.Stderr)
	path := global.String("config", "", "configuration file (default: $AGENT_CONFIG or $AGENT_DATA_DIR/config.yaml)")
	if err := global.Parse(args); err != nil {
		return err
	}
	args = global.Args()
	if len(args) == 0 {
		printUsage()
		return nil
	}
	switch args[0] {
	case "init":
		return initConfig(*path, args[1:])
	case "gateway":
		return runGateway(*path)
	case "chat":
		return runChat(*path)
	case "provider":
		return providerCommand(*path, args[1:])
	case "config":
		return configCommand(*path, args[1:])
	case "version", "--version", "-v":
		fmt.Printf("Kestrel %s (%s, %s) go=%s\n", version, commit, buildDate, runtime.Version())
		return nil
	case "help", "-h", "--help":
		printUsage()
		return nil
	default:
		return fmt.Errorf("unknown command %q (run agent help)", args[0])
	}
}

func printUsage() {
	fmt.Println(`Kestrel autonomous agent

Usage: agent [-config FILE] COMMAND [options]

Commands:
  init              Create a config file (interactive or flag-driven)
  gateway           Run the Telegram bot
  chat              Local terminal REPL
  provider          Add, list, remove or test providers
  config            Show, validate or locate configuration
  version           Show build information`)
}

func initConfig(path string, args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	token := fs.String("telegram-token", "", "Telegram bot token")
	users := fs.String("allowed-users", "", "comma-separated Telegram user IDs")
	name := fs.String("provider-name", "default", "provider name")
	base := fs.String("base-url", "", "OpenAI-compatible base URL")
	key := fs.String("api-key", "", "provider API key (or ${ENV_VAR})")
	model := fs.String("model", "", "default model")
	proxy := fs.String("proxy", "", "global HTTP/HTTPS/SOCKS5 proxy")
	force := fs.Bool("force", false, "overwrite an existing config file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if path == "" {
		path = filepath.Join(envOr("AGENT_DATA_DIR", "./data"), "config.yaml")
	}
	if _, err := os.Stat(path); err == nil && !*force {
		return fmt.Errorf("%s already exists (use init -force to overwrite)", path)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if *base == "" && *model == "" && *token == "" && *users == "" {
		reader := bufio.NewReader(os.Stdin)
		ask := func(label string) string {
			fmt.Print(label, ": ")
			line, _ := reader.ReadString('\n')
			return strings.TrimSpace(line)
		}
		*token = ask("Telegram bot token")
		*users = ask("Your Telegram user ID")
		*base = ask("Provider base URL (e.g. https://api.openai.com/v1)")
		*key = ask("Provider API key (or ${ENV_VAR})")
		*model = ask("Default model")
	}
	ids, err := config.ParseIDs(*users)
	if err != nil {
		return fmt.Errorf("allowed-users: %w", err)
	}
	if *base == "" || *model == "" || len(ids) == 0 || *token == "" {
		return errors.New("init requires -telegram-token, -allowed-users, -base-url and -model")
	}
	cfg := config.Default()
	cfg.ConfigPath = path
	cfg.Telegram.Token = *token
	cfg.Telegram.AllowedUsers = ids
	cfg.Proxy = *proxy
	cfg.Providers = []config.Provider{{Name: *name, BaseURL: *base, APIKey: *key, DefaultModel: *model}}
	cfg.Active = *name + ":" + *model
	if err := cfg.ValidateGateway(); err != nil {
		return err
	}
	if err := cfg.WriteFile(path); err != nil {
		return err
	}
	fmt.Println("Wrote", path)
	return nil
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func configCommand(path string, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: agent config show|validate|path")
	}
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	switch args[0] {
	case "path":
		fmt.Println(cfg.ConfigPath)
	case "validate":
		if err := cfg.Validate(); err != nil {
			return err
		}
		fmt.Println("Configuration is valid:", cfg.ConfigPath)
	case "show":
		copyCfg := *cfg
		copyCfg.Telegram.Token = masked(copyCfg.Telegram.Token)
		copyCfg.Telegram.PairingCode = masked(copyCfg.Telegram.PairingCode)
		copyCfg.Telegram.WebhookSecret = masked(copyCfg.Telegram.WebhookSecret)
		copyCfg.Tools.SearchAPIKey = masked(copyCfg.Tools.SearchAPIKey)
		copyCfg.Voice.APIKey = masked(copyCfg.Voice.APIKey)
		copyCfg.Proxy = netx.Redact(copyCfg.Proxy)
		copyCfg.Telegram.Proxy = netx.Redact(copyCfg.Telegram.Proxy)
		copyCfg.Voice.Proxy = netx.Redact(copyCfg.Voice.Proxy)
		copyCfg.Providers = append([]config.Provider(nil), cfg.Providers...)
		for i := range copyCfg.Providers {
			p := &copyCfg.Providers[i]
			p.APIKey = masked(p.APIKey)
			p.Proxy = netx.Redact(p.Proxy)
			p.Headers = nil
		}
		b, err := yaml.Marshal(&copyCfg)
		if err != nil {
			return err
		}
		fmt.Print(string(b))
	default:
		return fmt.Errorf("unknown config command %q", args[0])
	}
	return nil
}

func masked(s string) string {
	if s == "" {
		return ""
	}
	return "***"
}

// modifyProviders edits only the providers node, preserving ${ENV} references
// and all other config keys without serializing expanded secrets back to disk.
func modifyProviders(path string, change func(*yaml.Node, *yaml.Node) error) error {
	if path == "" {
		path = filepath.Join(envOr("AGENT_DATA_DIR", "./data"), "config.yaml")
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		raw = []byte("{}\n")
	} else if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return err
	}
	if len(doc.Content) == 0 {
		doc.Content = []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return errors.New("configuration root must be a YAML mapping")
	}
	var providers *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "providers" {
			providers = root.Content[i+1]
			break
		}
	}
	if providers == nil {
		providers = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "providers"}, providers)
	}
	if providers.Kind != yaml.SequenceNode {
		return errors.New("providers must be a YAML list")
	}
	if err := change(root, providers); err != nil {
		return err
	}
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path+".tmp", out, 0o600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

func yamlField(node *yaml.Node, key string) string {
	if node.Kind != yaml.MappingNode {
		return ""
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1].Value
		}
	}
	return ""
}

func providerCommand(path string, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: agent provider add|list|remove|test")
	}
	switch args[0] {
	case "add":
		fs := flag.NewFlagSet("provider add", flag.ContinueOnError)
		name := fs.String("name", "", "provider name")
		base := fs.String("base-url", "", "base URL")
		key := fs.String("api-key", "", "API key or ${ENV_VAR}")
		model := fs.String("model", "", "default model")
		proxy := fs.String("proxy", "", "proxy override")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *name == "" || *base == "" || *model == "" {
			return errors.New("provider add requires -name, -base-url and -model")
		}
		return modifyProviders(path, func(root, providers *yaml.Node) error {
			for _, p := range providers.Content {
				if yamlField(p, "name") == *name {
					return fmt.Errorf("provider %q already exists", *name)
				}
			}
			p := config.Provider{Name: *name, BaseURL: *base, APIKey: *key, DefaultModel: *model, Proxy: *proxy}
			var node yaml.Node
			if err := node.Encode(p); err != nil {
				return err
			}
			providers.Content = append(providers.Content, &node)
			return nil
		})
	case "remove":
		if len(args) < 2 {
			return errors.New("usage: agent provider remove NAME")
		}
		name := args[1]
		return modifyProviders(path, func(root, providers *yaml.Node) error {
			for i, p := range providers.Content {
				if yamlField(p, "name") == name {
					providers.Content = append(providers.Content[:i], providers.Content[i+1:]...)
					if strings.HasPrefix(yamlField(root, "active"), name+":") {
						return fmt.Errorf("provider %q is active; switch the active model before removing it", name)
					}
					return nil
				}
			}
			return fmt.Errorf("provider %q not found", name)
		})
	case "list":
		cfg, err := config.Load(path)
		if err != nil {
			return err
		}
		for _, p := range cfg.Providers {
			fmt.Printf("%s\t%s\t%s\n", p.Name, p.DefaultModel, p.BaseURL)
		}
		return nil
	case "test":
		if len(args) < 2 {
			return errors.New("usage: agent provider test NAME [MODEL]")
		}
		cfg, err := config.Load(path)
		if err != nil {
			return err
		}
		log := logx.New(cfg.LogLevel, cfg.LogFormat)
		factory := newHTTPFactory(cfg)
		defer factory.CloseIdle()
		reg, err := buildProviders(cfg, factory, log)
		if err != nil {
			return err
		}
		p, ok := reg.Get(args[1])
		if !ok {
			return fmt.Errorf("unknown provider %q", args[1])
		}
		probe, ok := p.(interface {
			Probe(context.Context, string) (provider.ProbeResult, error)
		})
		if !ok {
			return errors.New("this provider does not support probing")
		}
		model := ""
		if len(args) > 2 {
			model = args[2]
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		result, err := probe.Probe(ctx, model)
		if err != nil {
			return err
		}
		fmt.Printf("provider=%s model=%s latency=%s native_tools=%t tool_calls=%d reply=%q\n",
			args[1], result.Model, result.Latency, result.NativeTools, len(result.ToolCalls), truncate(result.Content, 150))
		return nil
	default:
		return fmt.Errorf("unknown provider command %q", args[0])
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func newHTTPFactory(cfg *config.Config) *netx.Factory {
	return netx.NewFactory(netx.Config{
		UserAgent: cfg.HTTP.UserAgent, MaxIdleConns: cfg.HTTP.MaxIdleConns,
		IdleConnTimeout: cfg.HTTP.IdleConnTimeout.D(), TLSHandshake: cfg.HTTP.TLSHandshakeTimout.D(),
	})
}

func buildProviders(cfg *config.Config, factory *netx.Factory, log *slog.Logger) (*provider.Registry, error) {
	reg := provider.NewRegistry()
	for _, p := range cfg.Providers {
		spec := cfg.ResolveProxy("", p.Proxy)
		hc, err := factory.Client(spec, 0)
		if err != nil {
			return nil, fmt.Errorf("provider %s proxy: %w", p.Name, err)
		}
		client, err := provider.NewClient(provider.Options{
			Name: p.Name, BaseURL: p.BaseURL, APIKey: p.APIKey, DefaultModel: p.DefaultModel,
			ChatPath: p.ChatPath, AudioPath: p.AudioPath, Headers: p.Headers,
			Timeout: p.Timeout.D(), NativeTools: p.ToolsEnabled(), Streaming: p.StreamingEnabled(),
			Vision: p.VisionEnabled(), ContextWindow: p.ContextWindow, MaxTokens: p.MaxTokens,
			Temperature: p.Temperature, ExtraBody: p.ExtraBody, ProxySpec: spec,
		}, hc, log)
		if err != nil {
			return nil, err
		}
		log.Info("provider configured", "name", p.Name, "model", p.DefaultModel, "proxy", netx.Redact(spec))
		reg.Add(client)
	}
	return reg, nil
}

func openStore(cfg *config.Config, log *slog.Logger) (*store.Store, error) {
	return store.Open(store.Config{
		Path: cfg.Store.Path, ReadConns: cfg.Store.ReadConns, BusyTimeoutMS: cfg.Store.BusyTimeoutMS,
		MaxBatch: cfg.Store.MaxBatch, FlushInterval: time.Duration(cfg.Store.FlushIntervalMS) * time.Millisecond,
		Logger: log,
	})
}

func runGateway(path string) error {
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	if err := cfg.ValidateGateway(); err != nil {
		return err
	}
	if err := cfg.Prepare(); err != nil {
		return err
	}
	setMemoryLimit()
	log := logx.New(cfg.LogLevel, cfg.LogFormat)
	st, err := openStore(cfg, log)
	if err != nil {
		return err
	}
	factory := newHTTPFactory(cfg)
	defer factory.CloseIdle()
	reg, err := buildProviders(cfg, factory, log)
	if err != nil {
		_ = st.Close(context.Background())
		return err
	}
	g, err := telegram.New(cfg, st, log, version)
	if err != nil {
		_ = st.Close(context.Background())
		return err
	}
	toolReg := tools.NewBuiltins(tools.Deps{
		Config: cfg, Store: st, HTTP: factory, ProxySpec: cfg.ResolveProxy("", ""),
		Logger: log, Approver: g.Approver(),
	})
	engine, err := agent.NewEngine(cfg, st, toolReg, reg, log)
	if err != nil {
		_ = toolReg.Close()
		_ = st.Close(context.Background())
		return err
	}
	engine.LoadRuntimeOverride(context.Background())
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	manager := agent.NewManager(ctx, engine, g.Resolver(), log)
	g.Attach(engine, manager)
	var schedulerWG sync.WaitGroup
	if cfg.Cron.Enabled {
		schedulerWG.Add(1)
		go func() {
			defer schedulerWG.Done()
			job := &sched.Scheduler{Store: st, Runner: manager, Tick: cfg.Cron.Tick.D(), Log: log}
			if err := job.Run(ctx); err != nil && ctx.Err() == nil {
				log.Error("scheduler failed", "error", err)
			}
		}()
	}
	shutdownHTTP := startHTTP(ctx, cfg, st, log)
	err = g.Run(ctx)
	cancel()
	deadline := cfg.Shutdown.D()
	if deadline <= 0 {
		deadline = 20 * time.Second
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), deadline)
	defer stopCancel()
	schedulerDone := make(chan struct{})
	go func() {
		schedulerWG.Wait()
		close(schedulerDone)
	}()
	select {
	case <-schedulerDone:
	case <-stopCtx.Done():
		log.Warn("scheduler shutdown timed out")
	}
	if shutdownHTTP != nil {
		shutdownHTTP(stopCtx)
	}
	if closeErr := manager.Close(stopCtx); closeErr != nil {
		log.Warn("manager shutdown", "error", closeErr)
	}
	if closeErr := toolReg.Close(); closeErr != nil {
		log.Warn("tool shutdown", "error", closeErr)
	}
	if closeErr := st.Close(stopCtx); closeErr != nil {
		log.Warn("database shutdown", "error", closeErr)
	}
	return err
}

func startHTTP(ctx context.Context, cfg *config.Config, st *store.Store, log *slog.Logger) func(context.Context) {
	var servers []*http.Server
	serve := func(addr string, handler http.Handler) {
		if addr == "" {
			return
		}
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			log.Error("HTTP listen failed", "addr", addr, "error", err)
			return
		}
		srv := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
		servers = append(servers, srv)
		go func() {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("HTTP server failed", "addr", addr, "error", err)
			}
		}()
		log.Info("HTTP listening", "addr", addr)
	}
	health := http.NewServeMux()
	health.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if ctx.Err() != nil {
			http.Error(w, "shutting down", http.StatusServiceUnavailable)
			return
		}
		probeCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := st.Ping(probeCtx); err != nil {
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "ok\n")
	})
	serve(cfg.HealthAddr, health)
	if cfg.Pprof {
		host, _, err := net.SplitHostPort(cfg.PprofAddr)
		if err != nil || (host != "localhost" && !net.ParseIP(host).IsLoopback()) {
			log.Error("pprof_addr must be localhost-only", "addr", cfg.PprofAddr)
		} else {
			mux := http.NewServeMux()
			mux.HandleFunc("/debug/pprof/", pprof.Index)
			mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
			mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
			mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
			mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
			serve(cfg.PprofAddr, mux)
		}
	}
	return func(stopCtx context.Context) {
		for _, srv := range servers {
			_ = srv.Shutdown(stopCtx)
		}
	}
}

// Go adjusts GOMEMLIMIT for constrained containers; recent Go versions tune
// GOMAXPROCS from the cgroup CPU quota automatically.
func setMemoryLimit() {
	if os.Getenv("GOMEMLIMIT") != "" {
		return
	}
	for _, path := range []string{"/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/memory/memory.limit_in_bytes"} {
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		limit, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
		if err == nil && limit >= 64<<20 && limit < 1<<50 {
			debug.SetMemoryLimit(limit * 85 / 100)
			return
		}
	}
}

type terminalChat struct{}

func (terminalChat) Send(_ context.Context, text string) error {
	fmt.Println("\n" + text)
	return nil
}
func (terminalChat) Update(context.Context, string) error { return nil }
func (terminalChat) Progress(_ context.Context, text string) error {
	if text != "" {
		fmt.Fprintln(os.Stderr, text)
	}
	return nil
}
func (terminalChat) Action(_ context.Context, text string) error {
	if text != "" {
		fmt.Fprintln(os.Stderr, "⚙️ "+text)
	}
	return nil
}
func (terminalChat) Think(_ context.Context, text string) error {
	if text != "" {
		fmt.Fprintln(os.Stderr, "💭 "+text)
	}
	return nil
}
func (terminalChat) Error(_ context.Context, text string) error {
	if text != "" {
		fmt.Fprintln(os.Stderr, "❌ "+text)
	}
	return nil
}
func (terminalChat) Status(_ context.Context, st agent.RunStatus) error {
	label := "Active"
	switch st.Phase {
	case "done":
		label = "Done"
	case "error":
		label = "Error"
	case "stopped":
		label = "Stopped"
	}
	if st.Running || st.Phase == "done" || st.Phase == "error" || st.Phase == "stopped" {
		detail := st.Phase
		if st.Action != "" {
			detail = st.Action
		}
		fmt.Fprintf(os.Stderr, "status: %s (%s)\n", label, detail)
	}
	return nil
}
func (terminalChat) Typing(context.Context) error { return nil }
func (terminalChat) SendFile(_ context.Context, path, _ string) error {
	fmt.Println("File:", path)
	return nil
}

func runChat(path string) error {
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	if len(cfg.Providers) == 0 {
		return errors.New("no providers configured (run agent init)")
	}
	if err := cfg.Prepare(); err != nil {
		return err
	}
	log := logx.New(cfg.LogLevel, cfg.LogFormat)
	st, err := openStore(cfg, log)
	if err != nil {
		return err
	}
	factory := newHTTPFactory(cfg)
	defer factory.CloseIdle()
	defer func() { _ = st.Close(context.Background()) }()
	reg, err := buildProviders(cfg, factory, log)
	if err != nil {
		return err
	}
	toolReg := tools.NewBuiltins(tools.Deps{Config: cfg, Store: st, HTTP: factory, ProxySpec: cfg.Proxy, Logger: log})
	defer func() { _ = toolReg.Close() }()
	engine, err := agent.NewEngine(cfg, st, toolReg, reg, log)
	if err != nil {
		return err
	}
	engine.LoadRuntimeOverride(context.Background())
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	const chatID int64 = -1
	session, err := st.EnsureSession(ctx, chatID, "Local chat")
	if err != nil {
		return err
	}
	items, err := st.Messages(ctx, session.ID, cfg.Agent.MaxHistoryMessages)
	if err != nil {
		return err
	}
	history := make([]provider.Message, 0, len(items))
	for _, m := range items {
		history = append(history, provider.Message{Role: provider.Role(m.Role), Content: m.Content, Name: m.Name, ToolCallID: m.ToolCallID})
	}
	fmt.Println("Kestrel local chat. Type /exit to quit, /new to reset.")
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for ctx.Err() == nil {
		fmt.Print("\n> ")
		if !scanner.Scan() {
			break
		}
		text := strings.TrimSpace(scanner.Text())
		switch text {
		case "":
			continue
		case "/exit", "/quit":
			return nil
		case "/new":
			session, err = st.CreateSession(ctx, chatID, "Local chat")
			if err != nil {
				return err
			}
			history = nil
			fmt.Println("Started a new conversation.")
			continue
		}
		out := engine.Run(ctx, agent.RunInput{
			Chat: terminalChat{}, SessionID: session.ID, ChatID: chatID, HomeDir: cfg.Workspace,
			History: history, User: provider.Message{Role: provider.RoleUser, Content: text},
		})
		history = out.History
		if len(out.Messages) > 0 {
			rows := make([]store.MessageRecord, 0, len(out.Messages))
			for _, m := range out.Messages {
				rows = append(rows, store.MessageRecord{SessionID: session.ID, Role: string(m.Role), Content: m.Content, Name: m.Name, ToolCallID: m.ToolCallID})
			}
			if err := st.AppendMessages(ctx, session.ID, rows); err != nil {
				log.Warn("persist local chat", "error", err)
			}
		}
		if out.Err != nil && !errors.Is(out.Err, agent.ErrCancelled) {
			fmt.Fprintln(os.Stderr, "Agent error:", out.Err)
		}
	}
	return scanner.Err()
}
