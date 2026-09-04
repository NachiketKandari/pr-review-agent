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
	// Github configures GitHub API access. Token is optional; the
	// GITHUB_TOKEN environment variable is also honored.
	Github Github `yaml:"github"`
}

// Review holds review-mode settings. Zero values mean "use defaults".
type Review struct {
	Model             string  `yaml:"model"`             // optional override
	SystemPrompt      string  `yaml:"systemPrompt"`      // "" = built-in default
	ChunkPrompt       string  `yaml:"chunkPrompt"`       // "" = built-in default
	MergePrompt       string  `yaml:"mergePrompt"`       // "" = built-in default
	MaxChunkTokens    int     `yaml:"maxChunkTokens"`    // 0 = 8000
	MaxResponseTokens int     `yaml:"maxResponseTokens"` // 0 = 2048
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
