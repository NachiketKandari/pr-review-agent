// Command pr-review-agent is a CLI chat client for OpenAI-compatible models
// that can also review GitHub pull requests.
//
// Chat mode:  go run . "your message" [-flags]
// Branch review: go run . "source_branch" "target_branch" [-flags]
// PR review: go run . "https://github.com/owner/repo/pull/N" [-flags]
// Explain: go run . -explain "source_branch" "target_branch" [-flags]
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
//
// -explain is a separate mode, not a review style: it fetches the change the
// same way and then writes a self-contained interactive HTML page explaining
// it — background, intuition, a code walkthrough, and a quiz on the substance
// — instead of a list of findings. It has its own prompts and its own
// per-call budgets (explain.* in the config), tuned so nothing exceeds a
// 16K context window, and it never touches the review path.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/NachiketKandari/pr-review-agent/config"
	"github.com/NachiketKandari/pr-review-agent/diff"
	"github.com/NachiketKandari/pr-review-agent/explain"
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
	promptFile := flag.String("prompt-file", "", "read the prompt from this text file (chat mode: the message; review/explain mode: overrides the system prompt)")
	explainMode := flag.Bool("explain", false, "explain the change as a rich interactive HTML page instead of reviewing it")
	openPage := flag.Bool("open", false, "open the generated page in a browser (explain mode)")
	repoDir := flag.String("repo", "", "path to a local clone of the repo; required for branch review, and used to diff PRs with git instead of the API (default: try the current directory)")
	chunkTokens := flag.Int("chunk-tokens", 0, "override the per-call context budget (review.maxChunkTokens / explain.maxChunkTokens)")
	renderSpec := flag.String("render", "", "re-render a saved .spec.json to HTML without calling the model")
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
	if *renderSpec != "" {
		// Re-rendering is a pure function of a saved spec: no config, no
		// model, no network. It runs before the usage check so it works
		// with no arguments at all.
		runRender(*renderSpec, *outputPath, *openPage)
		return
	}
	if len(args) == 0 && *diffPath == "" && *promptFile == "" {
		fmt.Fprintln(os.Stderr, `usage:
  go run . [flags] "source_branch" "target_branch"        review a branch merge
  go run . [flags] "target_branch"                        review the current branch into target_branch (inside a git clone)
  go run . [flags] "https://github.com/owner/repo/pull/N" review a pull request
  go run . [flags] "your message"                         chat
  go run . [flags] -prompt-file prompt.txt                chat with a prompt from a file

  -explain writes a rich interactive HTML page explaining the same change
  instead of reviewing it; it accepts every source above. Add -open to open
  the page in a browser.

  Branch review uses the git auth already in your terminal (SSH key /
  credential manager); it never reads a token from the YAML or env.`)
		flag.PrintDefaults()
		usageExamples()
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
		explainMode: *explainMode,
		openPage:    *openPage,
	}

	// A first argument that parses as a GitHub PR URL selects PR review
	// mode (kept as a fallback); -diff reviews a local diff file (the URL
	// is then optional context); two branch names select branch review; a
	// single branch name inside a git clone reviews the current branch
	// into that target branch (auth is whatever git itself uses — your
	// SSH key / credential manager — never a token from the YAML).
	// -explain swaps the agent, not the input: every one of these sources
	// feeds the explainer too, so the flag is orthogonal to how the diff
	// is obtained.
	// -prompt-file supplies the prompt text: the instructions in
	// review/explain mode, the chat message otherwise.
	run := runReview
	if *explainMode {
		run = runExplain
	}
	if len(args) > 0 {
		if ref, err := diff.ParseURL(args[0]); err == nil {
			run(flags, ref)
			return
		}
		if len(args) >= 2 && !strings.ContainsAny(args[0], " \t") && !strings.ContainsAny(args[1], " \t") {
			runBranchReview(flags, run, args[0], args[1])
			return
		}
		if len(args) == 1 && !strings.ContainsAny(args[0], " \t") {
			if source, ok := currentBranchSource(flags.repoDir, args[0]); ok {
				runBranchReview(flags, run, source, args[0])
				return
			}
		}
	} else if *diffPath != "" {
		run(flags, diff.Ref{})
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
	explainMode bool
	openPage    bool
}

// usageExamples prints worked examples of the three review sources and of
// explain mode, after the flag list.
func usageExamples() {
	fmt.Fprint(os.Stderr, `
examples:
  go run . "feature/auth" "main"                    review a branch merge
  go run . -explain -open "feature/auth" "main"     explain it and open the page
  go run . "octocat/Hello-World/pull/123"           review a pull request
  go run . -output review.md "my-branch" "main"     also save the review to a file
  go run . -diff pr-123.patch "octocat/Hello-World/pull/123"
`)
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
		FilePrompt:        cfg.Review.FilePrompt,
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
	kit, err := startReview(f)
	if err != nil {
		fatal(err)
	}
	defer kit.stop()

	files, background, repo, detail, _ := fetchChange(kit, f, ref)
	text, err := kit.agent.Review(kit.ctx, repo, files, background, os.Stdout)
	if err != nil {
		fatalAttr("review", &repo, err)
	}
	finishReview(f, repo, text, detail...)
}

// runExplain produces the rich HTML explanation of the same change instead
// of a review. The diff is fetched by exactly the same route a review uses,
// so -explain works with every source (branch, PR link, .patch, -diff file)
// and changes only what is done with it afterwards.
func runExplain(f cfgFlags, ref diff.Ref) {
	kit, err := startReview(f)
	if err != nil {
		fatal(err)
	}
	defer kit.stop()

	files, background, repo, _, src := fetchChange(kit, f, ref)
	runExplainWith(kit, f, repo, files, background, src)
}

// runExplainWith renders the explanation for an already-fetched change.
// src may be nil, in which case the explainer works from the diff alone.
func runExplainWith(kit *reviewKit, f cfgFlags, repo diff.Ref, files []diff.File, background string, src *explain.Source) {
	agent, err := explainAgent(kit, f)
	if err != nil {
		fatalAttr("explain.agent", &repo, err)
	}
	spec, err := agent.Explain(kit.ctx, repo, files, background, src)
	if err != nil {
		fatalAttr("explain", &repo, err)
	}
	page, err := explain.Render(spec)
	if err != nil {
		fatalAttr("explain.render", &repo, err)
	}
	path, err := writeExplanation(f, repo, spec, page)
	if err != nil {
		fatalAttr("explain.write", &repo, err)
	}
	xlog.Info("explanation written", "pr", repo.String(), "path", path,
		"sections", len(spec.Sections), "quiz_questions", len(spec.Quiz), "bytes", len(page))
	// stdout carries the artifact path and nothing else; progress went to
	// stderr as it happened.
	fmt.Printf("Explained %s -> %s\n", repo.String(), path)
}

// explainAgent builds the explainer from the same loaded config as the
// review agent, falling back to review's model choice when explain has none
// of its own. Only the explain.* keys are read, so a review run is unaffected.
func explainAgent(kit *reviewKit, f cfgFlags) (*explain.Agent, error) {
	selector := f.modelSel
	if selector == "" {
		selector = kit.cfg.Explain.Model
	}
	if selector == "" {
		selector = kit.cfg.Review.Model
	}
	model, err := kit.cfg.Model(selector)
	if err != nil {
		return nil, err
	}
	xlog.Info("explain model selected", "name", model.Name, "provider", model.Provider, "model", model.Model)

	client, err := llm.New(modelOptions(model, f.insecure, f.caPath, f.timeout))
	if err != nil {
		return nil, err
	}

	systemPrompt := kit.cfg.Explain.SystemPrompt
	if f.promptFile != "" {
		text, err := readPromptFile(f.promptFile)
		if err != nil {
			return nil, err
		}
		systemPrompt = text
	}
	maxChunk := kit.cfg.Explain.MaxChunkTokens
	if f.chunkTokens > 0 {
		maxChunk = f.chunkTokens
	}
	return explain.New(explain.Options{
		Model:             model.Model,
		Client:            client,
		SystemPrompt:      systemPrompt,
		ChunkPrompt:       kit.cfg.Explain.ChunkPrompt,
		MergePrompt:       kit.cfg.Explain.MergePrompt,
		QuizPrompt:        kit.cfg.Explain.QuizPrompt,
		TitlePrompt:       kit.cfg.Explain.TitlePrompt,
		MaxChunkTokens:    maxChunk,
		MaxResponseTokens: kit.cfg.Explain.MaxResponseTokens,
		MaxQuizTokens:     kit.cfg.Explain.MaxQuizTokens,
		Temperature:       kit.cfg.Explain.Temperature,
	})
}

// writeExplanation saves the page and, next to it, the content spec it was
// rendered from, so the page can be regenerated or edited without spending
// another model call. -output overrides the HTML path; the spec always sits
// beside it.
func writeExplanation(f cfgFlags, ref diff.Ref, spec explain.Spec, page string) (string, error) {
	// Normalize before deriving the filename: Render works on its own copy,
	// so without this the spec saved to disk would keep an empty slug and a
	// later -render run would not reproduce the same page.
	spec.Normalize()
	name := time.Now().Format("2006-01-02") + "-" + spec.Slug

	htmlPath := f.outputPath
	if htmlPath == "" {
		htmlPath = filepath.Join(".", name+".html")
	}
	if err := os.WriteFile(htmlPath, []byte(page), 0o644); err != nil {
		return "", fmt.Errorf("write page: %w", err)
	}

	specJSON, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		xlog.Warn("could not serialize the content spec", "error", err)
		return htmlPath, nil
	}
	specPath := strings.TrimSuffix(htmlPath, filepath.Ext(htmlPath)) + ".spec.json"
	if err := os.WriteFile(specPath, specJSON, 0o644); err != nil {
		xlog.Warn("wrote the page but not its content spec", "path", specPath, "error", err)
	} else {
		xlog.Info("wrote content spec", "pr", ref.String(), "path", specPath)
	}

	if f.openPage {
		openInBrowser(htmlPath)
	}
	return htmlPath, nil
}

// runRender regenerates the HTML page from a saved content spec. This is the
// equivalent of the skill's render.py, and it is why the spec is written
// beside every page: the prose can be edited by hand and re-rendered, and a
// page can be regenerated later without spending a single model call.
func runRender(specPath, outPath string, open bool) {
	data, err := os.ReadFile(specPath)
	if err != nil {
		fatal(fmt.Errorf("read spec %q: %w", specPath, err))
	}
	var spec explain.Spec
	if err := json.Unmarshal(data, &spec); err != nil {
		fatal(fmt.Errorf("parse spec %q: %w", specPath, err))
	}
	page, err := explain.Render(spec)
	if err != nil {
		fatal(fmt.Errorf("render spec %q: %w", specPath, err))
	}

	if outPath == "" {
		base := strings.TrimSuffix(specPath, filepath.Ext(specPath))
		if base == specPath {
			base = specPath + ".html"
		}
		outPath = base + ".html"
	}
	if err := os.WriteFile(outPath, []byte(page), 0o644); err != nil {
		fatal(fmt.Errorf("write page: %w", err))
	}
	xlog.Info("rendered page from spec", "spec", specPath, "path", outPath, "bytes", len(page))
	fmt.Printf("Rendered %s -> %s\n", specPath, outPath)
	if open {
		openInBrowser(outPath)
	}
}

// openInBrowser opens the generated page with the platform's opener. It
// failing is not worth an error: the path was printed either way.
func openInBrowser(path string) {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		cmd, args = "open", []string{path}
	case "windows":
		cmd, args = "rundll32", []string{"url.dll,FileProtocolHandler", path}
	default:
		cmd, args = "xdg-open", []string{path}
	}
	if err := exec.Command(cmd, args...).Start(); err != nil {
		xlog.Warn("could not open a browser", "error", err, "path", path)
		return
	}
	xlog.Info("opened the page in a browser", "path", path)
}

// fetchChange obtains the diff and its intent context, trying the same routes
// in the same order a review does: -diff local file, the local git clone, the
// PR's .patch link, then the GitHub REST API. It is shared by review and
// explain mode so the two can never disagree about what is being analysed.
// It returns the parsed files, the intent context, the resolved ref to log
// under, and log detail about which route was used.
func fetchChange(kit *reviewKit, f cfgFlags, ref diff.Ref) (files []diff.File, background string, repo diff.Ref, detail []any, src *explain.Source) {
	localFile := f.diffPath != ""
	validRef := ref.Number > 0
	if localFile {
		xlog.Info("local diff", "diff_file", f.diffPath, "pr", ref.String())
	} else {
		xlog.Info("pull request", "pr", ref.GitHubURL())
	}

	// Fetch the diff. Preferred order: -diff local file, then the local git
	// clone (run from inside the repo, or point -repo at it) using git's own
	// credentials, then the PR's .patch web link (github.diffToken), then
	// the GitHub REST API.
	var token string
	var body []byte
	var err error
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
				// The clone is right here and holds the code the PR
				// modifies; remember where, so explain mode can read it.
				src = clonePreImage(dir, ref, kit.cfg.Explain, kit.ctx)
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
	files = parseAndLogDiff(ref, body)

	// PR intent context (title, description, commit messages) comes from
	// git when the clone route was used, from the patch headers when the
	// .patch route was used, otherwise best-effort from the GitHub API;
	// the review runs either way.
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
			xlog.Warn("PR metadata unavailable; analysing the diff only",
				"pr", ref.String(), "error", err)
		} else {
			background = prBackground(meta)
			xlog.Info("fetched PR metadata", "pr", ref.String(), "host", ref.Host,
				"title", clip(meta.Title, 120), "commits", len(meta.Commits),
				"background_chars", len(background))
		}
	}

	detail = fetchDetail(ref, f, validRef)
	return files, background, ref, detail, src
}

// fetchDetail describes which route produced the diff, for the success log.
func fetchDetail(ref diff.Ref, f cfgFlags, validRef bool) []any {
	if validRef {
		return []any{"pr_url", ref.GitHubURL()}
	}
	return []any{"diff_file", f.diffPath}
}

// runBranch reviews (or, with -explain, explains) the merge-base diff of
// merging source into target inside a local git clone (the current directory
// or -repo). No PR number and no GitHub API call are involved; auth is
// whatever git itself uses (SSH key / Git Credential Manager).
//
// run is runReview or runExplain: the diff is fetched identically either way.
func runBranchReview(f cfgFlags, run func(cfgFlags, diff.Ref), source, target string) {
	kit, err := startReview(f)
	if err != nil {
		fatal(err)
	}
	defer kit.stop()

	dir := f.repoDir
	if dir == "" {
		dir = "."
	}
	xlog.Info("branch change", "source", source, "target", target, "dir", dir)

	repo, err := diff.OriginRepo(kit.ctx, dir)
	if err != nil {
		fatal(fmt.Errorf("branch analysis needs a local clone of the repository: %w", err))
	}
	body, meta, err := diff.BranchDiff(kit.ctx, dir, source, target)
	if err != nil {
		fatalAttr("git.branchdiff", &repo, err)
	}
	files := parseAndLogDiff(repo, body)

	background := prBackground(meta)
	xlog.Info("gathered context from git", "pr", repo.String(),
		"commits", len(meta.Commits), "background_chars", len(background))

	if f.explainMode {
		// The clone is right here: the merge base of the two branches is
		// the tree the source branch was written against.
		rev, err := diff.MergeBase(kit.ctx, dir, source, target)
		src := &explain.Source{Dir: dir, Rev: rev}
		if err != nil {
			xlog.Debug("no merge base for the pre-image; explaining from the diff alone", "error", err)
			src = nil
		} else {
			src.MaxFiles = kit.cfg.Explain.SourceMaxFiles
			src.MaxTokens = kit.cfg.Explain.SourceMaxTokens
		}
		runExplainWith(kit, f, repo, files, background, src)
		return
	}

	text, err := kit.agent.Review(kit.ctx, repo, files, background, os.Stdout)
	if err != nil {
		fatalAttr("review", &repo, err)
	}
	finishReview(f, repo, text, "source", source, "target", target)
}

// clonePreImage locates the commit whose tree holds the code a pull request
// modifies, so explain mode can read the surrounding code before it reads the
// diff. It is best effort by design: any failure yields a nil Source, and the
// explainer then works from the diff alone.
func clonePreImage(dir string, ref diff.Ref, cfg config.Explain, ctx context.Context) *explain.Source {
	rev, err := diff.RepoMergeBase(ctx, dir, ref)
	if err != nil {
		xlog.Debug("no pre-image for this pull request; explaining from the diff alone",
			"pr", ref.String(), "error", err)
		return nil
	}
	return &explain.Source{
		Dir:       dir,
		Rev:       rev,
		MaxFiles:  cfg.SourceMaxFiles,
		MaxTokens: cfg.SourceMaxTokens,
	}
}

// currentBranchSource resolves the checked-out branch of the repo at
// repoDir for the single-argument branch-review form ("target_branch").
// ok is false when the argument is not a real ref or repoDir is not a git
// clone, so a lone chat message is never misread as a branch review.
func currentBranchSource(repoDir, target string) (string, bool) {
	dir := repoDir
	if dir == "" {
		dir = "."
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if !diff.IsGitRepo(ctx, dir) || !diff.RefExists(ctx, dir, target) {
		return "", false
	}
	src, err := diff.CurrentBranch(ctx, dir)
	if err != nil {
		xlog.Warn("single-argument branch review fell back to chat", "err", err)
		return "", false
	}
	return src, true
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
