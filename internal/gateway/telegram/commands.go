package telegram

import (
	"context"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/bitvibe/kestrel/internal/config"
	"github.com/bitvibe/kestrel/internal/tools"
)

var botCommands = []map[string]string{
	{"command": "start", "description": "Show a short introduction"},
	{"command": "help", "description": "List commands"},
	{"command": "new", "description": "Start a fresh conversation"},
	{"command": "stop", "description": "Stop the current run"},
	{"command": "status", "description": "Show session and model status"},
	{"command": "skills", "description": "List installed skills"},
	{"command": "model", "description": "Show or switch the active model"},
	{"command": "models", "description": "List available models"},
	{"command": "home", "description": "Show the current working directory"},
	{"command": "cd", "description": "Change the working directory"},
	{"command": "whoami", "description": "Show your Telegram IDs"},
	{"command": "pair", "description": "Redeem a pairing code"},
	{"command": "version", "description": "Show the Kestrel version"},
}

const helpText = `*Kestrel* — a self-hosted autonomous agent.

Send a message to run the agent. It can use shell, files, web, memory, schedules and more. The model's reasoning, every action, and any errors are posted here as they happen; runs have no iteration limit and stop when the task finishes, the provider fails, or you send /stop. Long answers stream into a single message.

Drop a SKILL.md, a markdown file, or a .zip/.tar.gz skill package into the chat and ask me to install it, or ask me to search a skill marketplace by name.

*Commands*
/start — introduction
/help — this help
/new — start a fresh conversation
/stop — stop the current run
/status — session, model and usage
/skills — list installed skills
/model [provider:model] — show or switch models
/models [provider] — list available models
/home — show the working directory
/cd <path> — change the working directory
/whoami — your Telegram IDs
/pair <code> — redeem a pairing code
/version — build information`

// handleCommand processes a slash command. It returns false when the text is
// not a recognized command, in which case it is forwarded to the agent.
func (g *Gateway) handleCommand(ctx context.Context, chatID, userID int64, text string) bool {
	cmd, args, ok := parseCommand(text)
	if !ok {
		return false
	}
	if at := strings.IndexByte(cmd, '@'); at >= 0 {
		mention := cmd[at+1:]
		cmd = cmd[:at]
		if g.botUser.Username != "" && !strings.EqualFold(mention, g.botUser.Username) {
			// The command is addressed to another bot in a group.
			return true
		}
	}
	reply := func(format string, a ...any) {
		_ = g.sendMarkdown(ctx, chatID, fmt.Sprintf(format, a...))
	}
	switch strings.ToLower(cmd) {
	case "start":
		reply("Hi! I'm *Kestrel*, your self-hosted agent. Send me a task, or use /help to see commands.")
	case "help":
		reply("%s", helpText)
	case "new", "reset":
		if g.manager == nil {
			reply("The agent is not initialized.")
			return true
		}
		if arg := strings.TrimSpace(args); arg != "" {
			reply("Tip: /new takes no arguments.")
		}
		if err := g.manager.Reset(ctx, chatID); err != nil {
			reply("Could not reset the conversation: %v", err)
		}
	case "stop":
		if g.manager == nil {
			return true
		}
		if err := g.manager.Stop(ctx, chatID); err != nil {
			reply("Could not stop the run: %v", err)
		}
	case "status":
		reply("%s", g.statusText(ctx, chatID))
	case "skills":
		reply("%s", g.skillsText())
	case "model":
		reply("%s", g.modelCommand(ctx, strings.TrimSpace(args)))
	case "models":
		reply("%s", g.modelsCommand(ctx, strings.TrimSpace(args)))
	case "home", "pwd":
		reply("Working directory: `%s`", g.managerHome(chatID))
	case "cd":
		arg := strings.TrimSpace(args)
		if arg == "" {
			reply("Usage: `/cd <path>` (relative paths are under the workspace).")
			return true
		}
		if g.manager == nil {
			reply("The agent is not initialized.")
			return true
		}
		path, err := g.manager.SetHome(ctx, chatID, arg)
		if err != nil {
			reply("Could not change directory: %v", err)
			return true
		}
		reply("Working directory: `%s`", path)
	case "whoami":
		info := ""
		if g.botUser.Username != "" {
			info = "\nBot: @" + g.botUser.Username
		}
		reply("User ID: `%d`\nChat ID: `%d`%s", userID, chatID, info)
	case "pair":
		if strings.TrimSpace(args) == "" {
			reply("Usage: `/pair <code>`")
			return true
		}
		if err := g.allow.Pair(ctx, userID, args); err != nil {
			switch err {
			case ErrPairingDisabled:
				reply("Pairing is disabled for this bot.")
			case ErrPairingInvalid:
				reply("That pairing code is not valid.")
			default:
				reply("Pairing failed: %v", err)
			}
			return true
		}
		reply("Paired successfully. You now have access.")
	case "version":
		reply("Kestrel %s\nGo %s", g.version, goVersion())
	default:
		reply("Unknown command `/%s`. Use /help to see what I can do.", cmd)
	}
	return true
}

// parseCommand recognizes Telegram command syntax without mistaking paths like
// /tmp/file for commands.
func parseCommand(text string) (cmd, args string, ok bool) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") || len(text) < 2 {
		return "", "", false
	}
	first := text[1:]
	if i := strings.IndexAny(first, " \t\n"); i >= 0 {
		args = strings.TrimSpace(first[i+1:])
		first = first[:i]
	}
	if first == "" {
		return "", "", false
	}
	for i := 0; i < len(first); i++ {
		c := first[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '@' {
			continue
		}
		return "", "", false
	}
	return first, args, true
}

func (g *Gateway) statusText(ctx context.Context, chatID int64) string {
	var b strings.Builder
	b.WriteString("*Status*\n")
	if g.engine != nil {
		name, model := g.engine.Active()
		fmt.Fprintf(&b, "Model: `%s:%s`\n", name, model)
	}
	uptime := time.Since(g.started).Round(time.Second)
	fmt.Fprintf(&b, "Uptime: %s\n", uptime)
	if g.manager != nil {
		st := g.manager.Status(chatID)
		running := "idle"
		if st.Running {
			running = "running"
		}
		fmt.Fprintf(&b, "Session: `%s`\nState: %s\nQueued: %d\nMessages: %d\nHome: `%s`\n",
			shortID(st.SessionID), running, st.Queued, st.History, st.Home)
		if g.engine != nil && st.SessionID != "" {
			if tasks := g.engine.Tasks(st.SessionID); len(tasks) > 0 {
				b.WriteString("\n*Plan*\n")
				for i, task := range tasks {
					if i >= 12 {
						fmt.Fprintf(&b, "… and %d more\n", len(tasks)-12)
						break
					}
					text := clipRunes(strings.TrimSpace(task.Text), 140)
					switch task.Status {
					case "completed":
						fmt.Fprintf(&b, "✅ ~~%s~~\n", text)
					case "in_progress":
						fmt.Fprintf(&b, "🔄 %s\n", text)
					default:
						fmt.Fprintf(&b, "⬜ %s\n", text)
					}
				}
			}
		}
	}
	if g.store != nil && g.manager != nil {
		st := g.manager.Status(chatID)
		if st.SessionID != "" {
			if prompt, completion, err := g.store.UsageTotals(ctx, st.SessionID); err == nil && prompt+completion > 0 {
				fmt.Fprintf(&b, "Tokens: %d in / %d out\n", prompt, completion)
			}
		}
	}
	return strings.TrimSpace(b.String())
}

func (g *Gateway) modelCommand(ctx context.Context, args string) string {
	if g.engine == nil {
		return "The agent is not initialized."
	}
	name, model := g.engine.Active()
	if args == "" {
		var b strings.Builder
		fmt.Fprintf(&b, "Active model: `%s:%s`\n\nConfigured providers:\n", name, model)
		for _, p := range g.cfg.Providers {
			fmt.Fprintf(&b, "- `%s` — %s\n", p.Name, p.DefaultModel)
		}
		b.WriteString("\nUse `/model provider:model` to switch, for example `/model " + name + ":" + model + "`.")
		return b.String()
	}
	fields := strings.Fields(args)
	providerName, modelName := "", ""
	switch {
	case len(fields) >= 2:
		providerName, modelName = fields[0], strings.Join(fields[1:], " ")
	case strings.Contains(fields[0], ":"):
		p, m, err := config.SplitModel(fields[0])
		if err != nil {
			return "Invalid model selector. Use `/model provider:model`."
		}
		providerName, modelName = p, m
	default:
		providerName = fields[0]
	}
	if _, ok := g.cfg.Provider(providerName); !ok {
		return fmt.Sprintf("Unknown provider `%s`. Configured: %s", providerName, strings.Join(g.providerNames(), ", "))
	}
	if err := g.engine.SetModel(providerName, modelName); err != nil {
		return fmt.Sprintf("Could not switch model: %v", err)
	}
	name, model = g.engine.Active()
	g.log.Info("model switched via Telegram", "provider", name, "model", model)
	return fmt.Sprintf("Active model is now `%s:%s`.", name, model)
}

func (g *Gateway) modelsCommand(ctx context.Context, args string) string {
	if g.engine == nil {
		return "The agent is not initialized."
	}
	providerName := strings.TrimSpace(args)
	if i := strings.IndexAny(providerName, " \t"); i >= 0 {
		providerName = providerName[:i]
	}
	if providerName == "" {
		providerName, _ = g.engine.Active()
	}
	p, ok := g.engine.Providers().Get(providerName)
	if !ok {
		return fmt.Sprintf("Unknown provider `%s`. Configured: %s", providerName, strings.Join(g.providerNames(), ", "))
	}
	actx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	models, err := p.ListModels(actx)
	if err != nil || len(models) == 0 {
		// Fall back to configured models without failing the command.
		if cfgProv, ok := g.cfg.Provider(providerName); ok {
			models = append([]string(nil), cfgProv.Models...)
			if len(models) == 0 && cfgProv.DefaultModel != "" {
				models = []string{cfgProv.DefaultModel}
			}
		}
	}
	if len(models) == 0 {
		if err != nil {
			return fmt.Sprintf("Could not list models for `%s`: %v", providerName, err)
		}
		return fmt.Sprintf("No models reported for `%s`.", providerName)
	}
	sort.Strings(models)
	if len(models) > 100 {
		models = models[:100]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "*Models — %s*\n", providerName)
	for _, m := range models {
		fmt.Fprintf(&b, "- `%s`\n", m)
	}
	b.WriteString("\nSwitch with `/model " + providerName + ":<model>`.")
	return b.String()
}

func (g *Gateway) skillsText() string {
	if g.cfg == nil || !g.cfg.Skills.Enabled {
		return "Skills are disabled in the configuration."
	}
	skills, err := tools.ListSkills(g.cfg.Skills.Dir)
	if err != nil {
		return fmt.Sprintf("Could not list skills: %v", err)
	}
	if len(skills) == 0 {
		return "No skills installed.\n\nDrop a SKILL.md, a markdown file, or a .zip/.tar.gz package into the chat and ask me to install it. You can also ask me to search a marketplace by name. Installed skills live in `" + g.cfg.Skills.Dir + "`."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "*Installed skills* (%d)\n", len(skills))
	for _, s := range skills {
		desc := clipRunes(strings.TrimSpace(s.Description), 200)
		if desc == "" {
			desc = "no description"
		}
		fmt.Fprintf(&b, "- `%s` — %s\n", s.Name, desc)
	}
	b.WriteString("\nInstall more by dropping a skill package into the chat or asking me to search a marketplace.")
	return b.String()
}

func (g *Gateway) providerNames() []string {
	names := make([]string, 0, len(g.cfg.Providers))
	for _, p := range g.cfg.Providers {
		names = append(names, p.Name)
	}
	sort.Strings(names)
	return names
}

func (g *Gateway) managerHome(chatID int64) string {
	if g.manager == nil {
		return g.cfg.Workspace
	}
	return g.manager.Home(chatID)
}

func shortID(id string) string {
	if len(id) <= 10 {
		return id
	}
	return id[:8] + "…"
}

func goVersion() string {
	return strings.TrimPrefix(runtime.Version(), "go")
}
