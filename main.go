// Command pr-review-agent is a CLI chat client for OpenAI-compatible models
// that can also review GitHub pull requests.
//
// Chat mode:  go run . "your message" [-flags]
// Branch review: go run . "source_branch" "target_branch" [-flags]
// PR review: go run . "https://github.com/owner/repo/pull/N" [-flags]
//
// -prompt-file reads the prompt text from a file instead of the command
// line: it is the chat message in chat mode, and overrides
// review.systemPrompt (the review instructions) in review mode.
//
// Branch review diffs merge-base(target)...source inside a local git clone
// (the current directory or -repo), so no PR link and no API token are
// needed. In review mode the diff is split into chunks, reviewed with a
// map-reduce flow, and the merged review is streamed to stdout. All logs go
// to stderr (or the -log-file); stdout carries only the review/chat output.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/NachiketKandari/pr-review-agent/config"
	"github.com/NachiketKandari/pr-review-agent/diff"
	"github.com/NachiketKandari/pr-review-agent/llm"
	"github.com/NachiketKandari/pr-review-agent/review"
	"github.com/NachiketKandari/pr-review-agent/xlog"
)

var supportedProviders = map[string]bool{
	"":                  true,
	"openai":            true,
	"openai-compatible": true,
	"deepseek":          true,
}

func main() {
	configPath := flag.String("config", "local.yaml", "path to config file")
	modelSel := flag.String("model", "", "model to use (name, substring, or index)")
	noStream := flag.Bool("no-stream", false, "wait for the full response instead of streaming")
	insecure := flag.Bool("insecure", false, "skip TLS certificate verification")
	caPath := flag.String("ca", "", "path to a CA bundle file")
	timeout := flag.Duration("timeout", 0, "request timeout (overrides requestOptions.timeout)")
	outputPath := flag.String("output", "", "write the review to this file (review mode)")
	diffPath := flag.String("diff", "", "review a local unified diff file instead of fetching from GitHub")
	diffToken := flag.String("diff-token", "", "the ?token= value GitHub puts on private .patch/.diff links (overrides github.diffToken)")
	promptFile := flag.String("prompt-file", "", "read the prompt from this text file (chat mode: the message; review mode: overrides review.systemPrompt)")
	repoDir := flag.String("repo", "", "path to a local clone of the repo; required for branch review, and used to diff PRs with git instead of the API (default: try the current directory)")
	chunkTokens := flag.Int("chunk-tokens", 0, "override review.maxChunkTokens (review mode)")
	logFile := flag.String("log-file", "", "also append structured JSON logs to this file")
	debug := flag.Bool("debug", false, "log HTTP-level detail (request URLs, statuses, durations)")
	quiet := flag.Bool("quiet", false, "log errors and warnings only")
	flag.Parse()

	level := slog.LevelInfo
	switch {
	case *debug:
		level = slog.LevelDebug
	case *quiet:
		level = slog.LevelWarn
	}
	cleanupLog, err := xlog.Setup(level, *logFile)
	if err != nil {
		fatal(fmt.Errorf("open log file %q: %w", *logFile, err))
	}
	defer cleanupLog()

	args := flag.Args()
	if len(args) == 0 && *diffPath == "" && *promptFile == "" {
		fmt.Fprintln(os.Stderr, `usage:
  go run . [flags] "source_branch" "target_branch"        review a branch merge
  go run . [flags] "https://github.com/owner/repo/pull/N" review a pull request
  go run . [flags] "your message"                         chat
  go run . [flags] -prompt-file prompt.txt                chat with a prompt from a file`)
		flag.PrintDefaults()
		os.Exit(2)
	}

	flags := cfgFlags{
		configPath:  *configPath,
		modelSel:    *modelSel,
		insecure:    *insecure,
		caPath:      *caPath,
		timeout:     *timeout,
		outputPath:  *outputPath,
		chunkTokens: *chunkTokens,
		diffPath:    *diffPath,
		diffToken:   *diffToken,
		repoDir:     *repoDir,
		promptFile:  *promptFile,
	}

	// A first argument that parses as a GitHub PR URL selects PR review
	// mode (kept as a fallback); -diff reviews a local diff file (the URL
	// is then optional context); two branch names select branch review.
	// -prompt-file supplies the prompt text: the review instructions in
	// review mode, the chat message otherwise.
	if len(args) > 0 {
		if ref, err := diff.ParseURL(args[0]); err == nil {
			runReview(flags, ref)
			return
		}
		if len(args) >= 2 && !strings.ContainsAny(args[0], " \t") && !strings.ContainsAny(args[1], " \t") {
			runBranchReview(flags, args[0], args[1])
			return
		}
	} else if *diffPath != "" {
		runReview(flags, diff.Ref{})
		return
	}

	prompt := strings.Join(args, " ")
	if flags.promptFile != "" {
		content, err := readPromptFile(flags.promptFile)
		if err != nil {
			fatal(err)
		}
		if prompt != "" {
			xlog.Warn("prompt file takes precedence over the positional message",
				"prompt_file", flags.promptFile)
		}
		prompt = content
	}
	runChat(*configPath, *modelSel, *noStream, *insecure, *caPath, *timeout, prompt)
}

type cfgFlags struct {
	configPath  string
	modelSel    string
	insecure    bool
	caPath      string
	timeout     time.Duration
	outputPath  string
	chunkTokens int
	diffPath    string
	diffToken   string
	repoDir     string
	promptFile  string
}

// readPromptFile reads the prompt text from path. Surrounding whitespace is
// trimmed so a trailing newline in the file does not become part of the
// prompt, and an empty file is rejected early with a clear error.
func readPromptFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read prompt file %q: %w", path, err)
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		return "", fmt.Errorf("prompt file %q is empty", path)
	}
	xlog.Info("prompt file loaded", "path", path, "chars", len(text))
	return text, nil
}

// reviewKit bundles everything a review run needs: config (for GitHub
// tokens), the model options (for lazily building the GitHub HTTP client),
// the review agent, and the signal-aware context.
type reviewKit struct {
	cfg   *config.Config
	opts  llm.Options
	hc    *http.Client // built on first use; branch reviews never need it
	agent *review.Agent
	ctx   context.Context
	stop  context.CancelFunc
}

// githubClient returns the HTTP client for GitHub web/API fetches, building
// it from the model options on first use. Branch reviews and local-diff
// reviews never contact GitHub, so they never pay for it.
func (k *reviewKit) githubClient() (*http.Client, error) {
	if k.hc == nil {
		hc, err := llm.NewHTTPClient(k.opts)
		if err != nil {
			return nil, err
		}
		k.hc = hc
	}
	return k.hc, nil
}

// startReview loads the config, selects the model, builds the clients and
// the review agent, and installs signal handling. The caller must invoke
// kit.stop when done.
func startReview(f cfgFlags) (*reviewKit, error) {
	cfg, err := config.Load(f.configPath)
	if err != nil {
		return nil, err
	}
	xlog.Info("config loaded", "path", f.configPath, "name", cfg.Name, "version", cfg.Version, "models", len(cfg.Models))

	selector := f.modelSel
	if selector == "" && cfg.Review.Model != "" {
		selector = cfg.Review.Model
	}
	model, err := cfg.Model(selector)
	if err != nil {
		return nil, err
	}
	if !supportedProviders[strings.ToLower(model.Provider)] {
		return nil, fmt.Errorf("provider %q is not supported yet", model.Provider)
	}
	xlog.Info("model selected", "name", model.Name, "provider", model.Provider, "model", model.Model)

	opts := modelOptions(model, f.insecure, f.caPath, f.timeout)
	client, err := llm.New(opts)
	if err != nil {
		return nil, err
	}

	// Wire the agent with config defaults + CLI overrides.
	maxChunk := cfg.Review.MaxChunkTokens
	if f.chunkTokens > 0 {
		maxChunk = f.chunkTokens
	}
	// -prompt-file overrides the configured review instructions.
	systemPrompt := cfg.Review.SystemPrompt
	if f.promptFile != "" {
		text, err := readPromptFile(f.promptFile)
		if err != nil {
			return nil, err
		}
		systemPrompt = text
	}
	agent, err := review.New(review.Options{
		Model:             model.Model,
		Client:            client,
		SystemPrompt:      systemPrompt,
		ChunkPrompt:       cfg.Review.ChunkPrompt,
		MergePrompt:       cfg.Review.MergePrompt,
		MaxChunkTokens:    maxChunk,
		MaxResponseTokens: cfg.Review.MaxResponseTokens,
		Temperature:       cfg.Review.Temperature,
	})
	if err != nil {
		return nil, err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	return &reviewKit{cfg: cfg, opts: opts, agent: agent, ctx: ctx, stop: stop}, nil
}

// runReview drives the map-reduce review of a single PR. When f.diffPath
// is set, the diff is read from a local file produced by git (the GitHub
// API is not contacted), which works when the org blocks API tokens but
// git over SSH/credentials is available.
func runReview(f cfgFlags, ref diff.Ref) {
	localFile := f.diffPath != ""
	validRef := ref.Number > 0
	if localFile {
		xlog.Info("review mode (local diff)", "diff_file", f.diffPath, "pr", ref.String())
	} else {
		xlog.Info("review mode", "pr", ref.GitHubURL())
	}

	kit, err := startReview(f)
	if err != nil {
		fatal(err)
	}
	defer kit.stop()

	// Fetch the diff. Preferred order: -diff local file, then the local git
	// clone (run from inside the repo, or point -repo at it) using git's own
	// credentials, then the PR's .patch web link (github.diffToken), then
	// the GitHub REST API.
	var token string
	var body []byte
	gitMeta := diff.Meta{}
	patchMeta := diff.Meta{}
	usedGit := false
	usedPatch := false
	switch {
	case localFile:
		body, err = os.ReadFile(f.diffPath)
		if err != nil {
			fatalAttr("diff.read", &ref, err)
		}
		xlog.Info("loaded local diff", "path", f.diffPath, "bytes", len(body))
	case validRef:
		// Git route: no API token needed; git uses the credentials you
		// already have (SSH key / Git Credential Manager).
		dir := f.repoDir
		if dir == "" {
			dir = "."
		}
		if err := diff.RepoMatches(kit.ctx, dir, ref); err == nil {
			body, gitMeta, err = diff.RepoDiff(kit.ctx, dir, ref)
			if err != nil {
				if f.repoDir != "" {
					fatalAttr("git.diff", &ref, err)
				}
				xlog.Warn("local git diff failed; trying the next fetch route",
					"pr", ref.String(), "error", err)
			} else if len(body) == 0 {
				// Merged/closed PRs have their commits in the base branch
				// already, so merge-base diffing yields nothing.
				xlog.Warn("git diff is empty (the PR may be merged or closed); trying the next fetch route",
					"pr", ref.String())
			} else {
				usedGit = true
				xlog.Info("using local git clone for the diff (no API token needed)",
					"pr", ref.String(), "dir", dir, "bytes", len(body))
			}
		} else if f.repoDir != "" {
			fatalAttr("git.repo", &ref, err)
		}
		if !usedGit {
			xlog.Info("no matching local clone; trying the .patch link / API routes",
				"pr", ref.String())
			linkToken := f.diffToken
			if linkToken == "" {
				linkToken = kit.cfg.Github.DiffToken
			}
			if linkToken == "" {
				linkToken = os.Getenv("GITHUB_DIFF_TOKEN")
			}
			if linkToken != "" {
				xlog.Info("fetching PR patch via .patch link (github.diffToken)", "pr", ref.String())
				hc, err := kit.githubClient()
				if err != nil {
					fatalAttr("http.client", &ref, err)
				}
				var subjects []string
				body, subjects, err = diff.FetchPatch(kit.ctx, ref, linkToken, hc)
				if err != nil {
					fatalAttr("patch.fetch", &ref, err)
				}
				usedPatch = true
				if len(subjects) > 0 {
					patchMeta.Title = subjects[0]
				}
				patchMeta.Commits = subjects
			} else {
				token = diff.ResolveToken(kit.cfg.Github.Token, ref.Host)
				hc, err := kit.githubClient()
				if err != nil {
					fatalAttr("http.client", &ref, err)
				}
				body, err = diff.Fetch(kit.ctx, ref, token, hc)
				if err != nil {
					fatalAttr("diff.fetch", &ref, err)
				}
			}
		}
	}
	files := parseAndLogDiff(ref, body)

	// PR intent context (title, description, commit messages) comes from
	// git when the clone route was used, from the patch headers when the
	// .patch route was used, otherwise best-effort from the GitHub API;
	// the review runs either way.
	background := ""
	switch {
	case usedGit:
		background = prBackground(gitMeta)
		xlog.Info("gathered PR context from git", "pr", ref.String(),
			"commits", len(gitMeta.Commits), "background_chars", len(background))
	case usedPatch:
		background = prBackground(patchMeta)
		xlog.Info("gathered PR context from patch", "pr", ref.String(),
			"commits", len(patchMeta.Commits), "background_chars", len(background))
	case validRef:
		if token == "" {
			token = diff.ResolveToken(kit.cfg.Github.Token, ref.Host)
		}
		hc, err := kit.githubClient()
		if err != nil {
			fatalAttr("http.client", &ref, err)
		}
		meta, err := diff.FetchMeta(kit.ctx, ref, token, hc)
		if err != nil {
			xlog.Warn("PR metadata unavailable; reviewing the diff only",
				"pr", ref.String(), "error", err)
		} else {
			background = prBackground(meta)
			xlog.Info("fetched PR metadata", "pr", ref.String(), "host", ref.Host,
				"title", clip(meta.Title, 120), "commits", len(meta.Commits),
				"background_chars", len(background))
		}
	}

	text, err := kit.agent.Review(kit.ctx, ref, files, background, os.Stdout)
	if err != nil {
		fatalAttr("review", &ref, err)
	}

	if validRef {
		finishReview(f, ref, text, "pr_url", ref.GitHubURL())
	} else {
		finishReview(f, ref, text, "diff_file", f.diffPath)
	}
}

// runBranchReview reviews the merge-base diff of merging source into
// target inside a local git clone (the current directory or -repo). No PR
// number and no GitHub API call are involved; auth is whatever git itself
// uses (SSH key / Git Credential Manager).
func runBranchReview(f cfgFlags, source, target string) {
	kit, err := startReview(f)
	if err != nil {
		fatal(err)
	}
	defer kit.stop()

	dir := f.repoDir
	if dir == "" {
		dir = "."
	}
	xlog.Info("review mode (branches)", "source", source, "target", target, "dir", dir)

	orepo, err := diff.OriginRepo(kit.ctx, dir)
	if err != nil {
		fatal(fmt.Errorf("branch review needs a local clone of the repository: %w", err))
	}
	body, meta, err := diff.BranchDiff(kit.ctx, dir, source, target)
	if err != nil {
		fatalAttr("git.branchdiff", &orepo, err)
	}
	files := parseAndLogDiff(orepo, body)

	background := prBackground(meta)
	xlog.Info("gathered context from git", "pr", orepo.String(),
		"commits", len(meta.Commits), "background_chars", len(background))

	text, err := kit.agent.Review(kit.ctx, orepo, files, background, os.Stdout)
	if err != nil {
		fatalAttr("review", &orepo, err)
	}

	finishReview(f, orepo, text, "source", source, "target", target)
}

// parseAndLogDiff splits a unified diff into per-file sections and logs the
// summary statistics.
func parseAndLogDiff(ref diff.Ref, body []byte) []diff.File {
	files := diff.ParseDiff(body)
	added, deleted, binary := 0, 0, 0
	for _, f := range files {
		added += f.Additions
		deleted += f.Deletions
		if f.Binary {
			binary++
		}
	}
	xlog.Info("diff parsed", "pr", ref.String(), "files", len(files),
		"added", added, "deleted", deleted, "binary_skipped", binary)
	return files
}

// finishReview handles the tail of a review run: trailing newline, the
// optional -output file, and the success log. detail, when given, is
// appended to the success log line.
func finishReview(f cfgFlags, ref diff.Ref, text string, detail ...any) {
	if strings.TrimSpace(text) == "" {
		xlog.Info("review finished with nothing to report", "pr", ref.String())
		return
	}
	if !strings.HasSuffix(text, "\n") {
		fmt.Println()
	}

	if f.outputPath != "" {
		if err := os.WriteFile(f.outputPath, []byte(text), 0o644); err != nil {
			fatalAttr("output.write", &ref, err)
		}
		xlog.Info("wrote review output", "pr", ref.String(), "path", f.outputPath, "bytes", len(text))
	}
	xlog.Info("review succeeded", append([]any{"pr", ref.String()}, detail...)...)
}

// runChat is the original single-message chat flow.
func runChat(configPath, modelSel string, noStream, insecure bool, caPath string, timeout time.Duration, prompt string) {
	cfg, err := config.Load(configPath)
	if err != nil {
		fatal(err)
	}
	xlog.Info("config loaded", "path", configPath, "name", cfg.Name, "version", cfg.Version, "models", len(cfg.Models))
	model, err := cfg.Model(modelSel)
	if err != nil {
		fatal(err)
	}
	if !supportedProviders[strings.ToLower(model.Provider)] {
		fatal(fmt.Errorf("provider %q is not supported yet", model.Provider))
	}
	xlog.Info("model selected", "name", model.Name, "provider", model.Provider, "model", model.Model)

	client, err := llm.New(modelOptions(model, insecure, caPath, timeout))
	if err != nil {
		fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	temp := 0.5
	req := llm.ChatRequest{
		Model:       model.Model,
		Messages:    []llm.Message{{Role: "user", Content: prompt}},
		MaxTokens:   2048,
		Temperature: &temp,
	}

	start := time.Now()
	if noStream {
		xlog.Info("chat request", "model", model.Model, "stream", false)
		out, err := client.Chat(ctx, req)
		if err != nil {
			fatal(err)
		}
		fmt.Println(out)
		xlog.Info("chat complete", "model", model.Model, "stream", false,
			"duration_ms", time.Since(start).Milliseconds(), "chars", len(out))
		return
	}

	xlog.Info("chat request", "model", model.Model, "stream", true)
	if err := client.StreamChat(ctx, req, func(delta string) {
		fmt.Print(delta)
	}); err != nil {
		fatal(err)
	}
	fmt.Println()
	xlog.Info("chat complete", "model", model.Model, "stream", true,
		"duration_ms", time.Since(start).Milliseconds())
}

// modelOptions builds the shared llm.Options from config + CLI flags.
func modelOptions(model *config.Model, insecure bool, caPath string, timeout time.Duration) llm.Options {
	opts := llm.Options{
		APIBase: model.APIBase,
		APIKey:  model.APIKey,
	}
	if ro := model.RequestOptions; ro != nil {
		opts.Headers = ro.Headers
		opts.CABundlePath = ro.CABundlePath
		opts.Proxy = ro.Proxy
		if ro.VerifySSL != nil {
			opts.InsecureSkipVerify = !*ro.VerifySSL
		}
		if ro.TimeoutSeconds > 0 {
			opts.Timeout = time.Duration(ro.TimeoutSeconds) * time.Second
		}
	}
	if insecure {
		opts.InsecureSkipVerify = true
	}
	if caPath != "" {
		opts.CABundlePath = caPath
	}
	if timeout > 0 {
		opts.Timeout = timeout
	}
	return opts
}

// prBackground composes the intent context handed to the review: PR title,
// description, head branch, and commit messages, all length-capped so a
// huge PR body cannot blow up every prompt.
func prBackground(m diff.Meta) string {
	var b strings.Builder

	if m.Title != "" {
		fmt.Fprintf(&b, "Title: %s\n", clip(m.Title, 300))
	}
	if m.HeadRef != "" {
		fmt.Fprintf(&b, "Source branch: %s\n", clip(m.HeadRef, 200))
	}
	if m.BaseRef != "" {
		fmt.Fprintf(&b, "Target branch: %s\n", clip(m.BaseRef, 200))
	}
	if m.Body != "" {
		fmt.Fprintf(&b, "Description:\n%s\n", clip(m.Body, bgBodyCap))
	}
	if len(m.Commits) > 0 {
		b.WriteString("Commit messages:\n")
		for _, c := range m.Commits {
			first := c
			if i := strings.IndexByte(c, '\n'); i >= 0 {
				first = c[:i]
			}
			fmt.Fprintf(&b, "- %s\n", clip(strings.TrimSpace(first), 200))
		}
	}
	return strings.TrimSpace(b.String())
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Context caps for prBackground.
const (
	bgBodyCap = 4000
)

// fatal logs a structured error with package/PR context, then exits.
func fatalAttr(pkg string, ref *diff.Ref, err error) {
	attrs := []any{"error", err, "package", pkg}
	if ref != nil {
		attrs = append(attrs, "pr", ref.String())
	}
	xlog.Error("fatal", attrs...)
	os.Exit(1)
}

func fatal(err error) {
	xlog.Error("fatal", "error", err)
	os.Exit(1)
}
