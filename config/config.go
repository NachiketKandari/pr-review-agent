package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type RequestOptions struct {
	VerifySSL      *bool             `yaml:"verifySsl"`
	CABundlePath   string            `yaml:"caBundlePath"`
	TimeoutSeconds int               `yaml:"timeout"`
	Proxy          string            `yaml:"proxy"`
	Headers        map[string]string `yaml:"headers"`
}

type Model struct {
	Name           string          `yaml:"name"`
	Provider       string          `yaml:"provider"`
	Model          string          `yaml:"model"`
	APIBase        string          `yaml:"apiBase"`
	APIKey         string          `yaml:"apiKey"`
	RequestOptions *RequestOptions `yaml:"requestOptions"`
}

type Config struct {
	Name    string  `yaml:"name"`
	Version string  `yaml:"version"`
	Schema  string  `yaml:"schema"`
	Models  []Model `yaml:"models"`

	// Review configures pull-request review mode. All fields are optional
	// and backward compatible; defaults live in the review package.
	Review Review `yaml:"review"`
	// Explain configures explain mode (-explain), which renders a rich
	// HTML walkthrough of a change instead of reviewing it. Defaults live
	// in the explain package; it has its own prompts and budgets and
	// shares nothing with Review.
	Explain Explain `yaml:"explain"`
	// Github configures GitHub API access. Token is optional; the
	// GITHUB_TOKEN environment variable is also honored.
	Github Github `yaml:"github"`
}

// Review holds review-mode settings. Zero values mean "use defaults".
type Review struct {
	Model             string  `yaml:"model"`             // optional override
	SystemPrompt      string  `yaml:"systemPrompt"`      // "" = built-in default
	ChunkPrompt       string  `yaml:"chunkPrompt"`       // "" = built-in default
	FilePrompt        string  `yaml:"filePrompt"`        // "" = built-in default; weaves one file's chunk findings
	MergePrompt       string  `yaml:"mergePrompt"`       // "" = built-in default
	MaxChunkTokens    int     `yaml:"maxChunkTokens"`    // 0 = 10000 (16K-context tuning)
	MaxResponseTokens int     `yaml:"maxResponseTokens"` // 0 = 4000
	Temperature       float64 `yaml:"temperature"`       // 0 = 0.2
}

// Github holds GitHub API settings.
type Github struct {
	Token string `yaml:"token"` // optional; GITHUB_TOKEN env honored
	// DiffToken is the ?token= query value GitHub puts on shareable
	// .diff/.patch links of private pull requests. When set, the diff is
	// downloaded from the web .patch endpoint with it instead of the REST
	// API, which works when the org blocks API tokens for private repos.
	DiffToken string `yaml:"diffToken"`
}

// Explain holds explain-mode settings, for producing a rich interactive HTML
// explanation of a change instead of a review. Entirely separate from
// Review: enabling -explain must never change how a plain review behaves, so
// these prompts and budgets are not shared.
type Explain struct {
	Model             string  `yaml:"model"`             // "" = -model flag, then review.model, then first model
	SystemPrompt      string  `yaml:"systemPrompt"`      // "" = built-in default
	ChunkPrompt       string  `yaml:"chunkPrompt"`       // "" = built-in default; one chunk of the diff
	MergePrompt       string  `yaml:"mergePrompt"`       // "" = built-in default; weaves one section
	QuizPrompt        string  `yaml:"quizPrompt"`        // "" = built-in default; the closing questions
	TitlePrompt       string  `yaml:"titlePrompt"`       // "" = built-in default; the page headline
	MaxChunkTokens    int     `yaml:"maxChunkTokens"`    // 0 = 7000 (16K-context tuning)
	MaxResponseTokens int     `yaml:"maxResponseTokens"` // 0 = 3000
	MaxQuizTokens     int     `yaml:"maxQuizTokens"`     // 0 = 2000
	Temperature       float64 `yaml:"temperature"`       // 0 = 0.4
	SourceMaxFiles    int     `yaml:"sourceMaxFiles"`    // 0 = 6 pre-image files
	SourceMaxTokens   int     `yaml:"sourceMaxTokens"`   // 0 = 3000 for the pre-image context
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}

	if len(cfg.Models) == 0 {
		return nil, fmt.Errorf("no models defined in %q", path)
	}

	return &cfg, nil
}

func (c *Config) Model(selector string) (*Model, error) {
	if selector == "" {
		return &c.Models[0], nil
	}

	if idx, err := strconv.Atoi(selector); err == nil {
		if idx < 0 || idx >= len(c.Models) {
			return nil, fmt.Errorf("model index %d out of range (found %d models)", idx, len(c.Models))
		}
		return &c.Models[idx], nil
	}

	for i := range c.Models {
		if strings.EqualFold(c.Models[i].Name, selector) ||
			strings.EqualFold(c.Models[i].Model, selector) ||
			strings.Contains(strings.ToLower(c.Models[i].Name), strings.ToLower(selector)) {
			return &c.Models[i], nil
		}
	}

	return nil, fmt.Errorf("model %q not found in config", selector)
}
