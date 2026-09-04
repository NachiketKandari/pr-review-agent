// Command pr-review-agent is a CLI chat client for OpenAI-compatible models
// that can also review GitHub pull requests.
//
// Chat mode:  go run . "your message" [-flags]
// Review mode: go run . "https://github.com/owner/repo/pull/N" [-flags]
//
// In review mode the PR diff is fetched, split into chunks, reviewed with a
// map-reduce flow, and the merged review is streamed to stdout. All logs go
// to stderr (or the -log-file); stdout carries only the review/chat output.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
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
	repoDir := flag.String("repo", "", "path to a local clone of the repo; diff it with git instead of the API (default: try the current directory)")
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
	if len(args) == 0 && *diffPath == "" {
		fmt.Fprintln(os.Stderr, "usage: go run . [flags] \"PR URL\" | \"your message\"")
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
	}

	// A first argument that parses as a GitHub PR URL selects review mode;
	// -diff reviews a local diff file (the URL is then optional context).
	if ref, err := diff.ParseURL(args[0]); err == nil {
		runReview(flags, ref)
		return
	}
	if *diffPath != "" {
		runReview(flags, diff.Ref{})
		return
	}

	prompt := strings.Join(args, " ")
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

	cfg, err := config.Load(f.configPath)
	if err != nil {
		fatal(err)
	}
	xlog.Info("config loaded", "path", f.configPath, "name", cfg.Name, "version", cfg.Version, "models", len(cfg.Models))

	selector := f.modelSel
	if selector == "" && cfg.Review.Model != "" {
		selector = cfg.Review.Model
	}
	model, err := cfg.Model(selector)
	if err != nil {
		fatal(err)
	}
	if !supportedProviders[strings.ToLower(model.Provider)] {
		fatal(fmt.Errorf("provider %q is not supported yet", model.Provider))
	}
	xlog.Info("model selected", "name", model.Name, "provider", model.Provider, "model", model.Model)

	opts := modelOptions(model, f.insecure, f.caPath, f.timeout)
	hc, err := llm.NewHTTPClient(opts)
	if err != nil {
		fatal(err)
	}
	client, err := llm.New(opts)
	if err != nil {
		fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
			fatalAttr("diff.read", &ref, 0, err)
		}
		xlog.Info("loaded local diff", "path", f.diffPath, "bytes", len(body))
	case validRef:
		// Git route: no API token needed; git uses the credentials you
		// already have (SSH key / Git Credential Manager).
		dir := f.repoDir
		if dir == "" {
			dir = "."
		}
		if err := diff.RepoMatches(ctx, dir, ref); err == nil {
			body, gitMeta, err = diff.RepoDiff(ctx, dir, ref)
			if err != nil {
				if f.repoDir != "" {
					fatalAttr("git.diff", &ref, 0, err)
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
			fatalAttr("git.repo", &ref, 0, err)
		}
		if !usedGit {
			xlog.Info("no matching local clone; trying the .patch link / API routes",
				"pr", ref.String())
			linkToken := f.diffToken
			if linkToken == "" {
				linkToken = cfg.Github.DiffToken
			}
			if linkToken == "" {
				linkToken = os.Getenv("GITHUB_DIFF_TOKEN")
			}
			if linkToken != "" {
				xlog.Info("fetching PR patch via .patch link (github.diffToken)", "pr", ref.String())
				var subjects []string
				body, subjects, err = diff.FetchPatch(ctx, ref, linkToken, hc)
				if err != nil {
					fatalAttr("patch.fetch", &ref, 0, err)
				}
				usedPatch = true
				if len(subjects) > 0 {
					patchMeta.Title = subjects[0]
				}
				patchMeta.Commits = subjects
			} else {
				token = diff.ResolveToken(cfg.Github.Token, ref.Host)
				body, err = diff.Fetch(ctx, ref, token, hc)
				if err != nil {
					fatalAttr("diff.fetch", &ref, 0, err)
				}
			}
		}
	}
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
			token = diff.ResolveToken(cfg.Github.Token, ref.Host)
		}
		meta, err := diff.FetchMeta(ctx, ref, token, hc)
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

	// Wire the agent with config defaults + CLI overrides.
	maxChunk := cfg.Review.MaxChunkTokens
	if f.chunkTokens > 0 {
		maxChunk = f.chunkTokens
	}
	agent, err := review.New(review.Options{
		Model:             model.Model,
		Client:            client,
		SystemPrompt:      cfg.Review.SystemPrompt,
		ChunkPrompt:       cfg.Review.ChunkPrompt,
		MergePrompt:       cfg.Review.MergePrompt,
		MaxChunkTokens:    maxChunk,
		MaxResponseTokens: cfg.Review.MaxResponseTokens,
		Temperature:       cfg.Review.Temperature,
	})
	if err != nil {
		fatal(err)
	}

	text, err := agent.Review(ctx, ref, files, background, os.Stdout)
	if err != nil {
		fatalAttr("review", &ref, 0, err)
	}

	if strings.TrimSpace(text) == "" {
		xlog.Info("review finished with nothing to report", "pr", ref.String())
		return
	}
	if !strings.HasSuffix(text, "\n") {
		fmt.Println()
	}

	if f.outputPath != "" {
		if err := os.WriteFile(f.outputPath, []byte(text), 0o644); err != nil {
			fatalAttr("output.write", &ref, 0, err)
		}
		xlog.Info("wrote review output", "pr", ref.String(), "path", f.outputPath, "bytes", len(text))
	}
	if validRef {
		xlog.Info("review succeeded", "pr", ref.String(), "pr_url", ref.GitHubURL())
	} else {
		xlog.Info("review succeeded", "pr", ref.String(), "diff_file", f.diffPath)
	}
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
		fmt.Fprintf(&b, "Branch: %s\n", clip(m.HeadRef, 200))
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
func fatalAttr(pkg string, ref *diff.Ref, chunkIndex int, err error) {
	attrs := []any{"error", err, "package", pkg}
	if ref != nil {
		attrs = append(attrs, "pr", ref.String())
	}
	if chunkIndex > 0 {
		attrs = append(attrs, "chunk", chunkIndex)
	}
	xlog.Error("fatal", attrs...)
	os.Exit(1)
}

func fatal(err error) {
	xlog.Error("fatal", "error", err)
	os.Exit(1)
}
