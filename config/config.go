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
			strings.Contains(strings.ToLower(c.Models[i].Name), strings.ToLower(selector)) {
			return &c.Models[i], nil
		}
	}

	return nil, fmt.Errorf("model %q not found in config", selector)
}
