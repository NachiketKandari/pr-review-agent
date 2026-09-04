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
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: go run . [flags] \"PR URL\" | \"your message\"")
		flag.PrintDefaults()
		os.Exit(2)
	}

	// A first argument that parses as a GitHub PR URL selects review mode.
	if ref, err := diff.ParseURL(args[0]); err == nil {
		runReview(cfgFlags{
			configPath:  *configPath,
			modelSel:    *modelSel,
			insecure:    *insecure,
			caPath:      *caPath,
			timeout:     *timeout,
			outputPath:  *outputPath,
			chunkTokens: *chunkTokens,
		}, ref)
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
}

// runReview drives the map-reduce review of a single PR.
func runReview(f cfgFlags, ref diff.Ref) {
	xlog.Info("review mode", "pr", ref.GitHubURL())

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

	// Fetch the diff.
	token := diff.ResolveToken(cfg.Github.Token)
	body, err := diff.Fetch(ctx, ref, token, hc)
	if err != nil {
		fatalAttr("diff.fetch", &ref, 0, err)
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

	// Fetch PR intent context (title, description, commit messages) on a
	// best-effort basis; the review still runs when it is unavailable.
	background := ""
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
	xlog.Info("review succeeded", "pr", ref.String(), "pr_url", ref.GitHubURL())
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
