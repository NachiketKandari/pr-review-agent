package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/NachiketKandari/pr-review-agent/config"
	"github.com/NachiketKandari/pr-review-agent/llm"
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
	flag.Parse()

	prompt := strings.Join(flag.Args(), " ")
	if prompt == "" {
		fmt.Fprintln(os.Stderr, "usage: go run . [flags] \"your message\"")
		flag.PrintDefaults()
		os.Exit(2)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fatal(err)
	}
	model, err := cfg.Model(*modelSel)
	if err != nil {
		fatal(err)
	}
	if !supportedProviders[strings.ToLower(model.Provider)] {
		fatal(fmt.Errorf("provider %q is not supported yet", model.Provider))
	}

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
	if *insecure {
		opts.InsecureSkipVerify = true
	}
	if *caPath != "" {
		opts.CABundlePath = *caPath
	}
	if *timeout > 0 {
		opts.Timeout = *timeout
	}

	client, err := llm.New(opts)
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

	if *noStream {
		out, err := client.Chat(ctx, req)
		if err != nil {
			fatal(err)
		}
		fmt.Println(out)
		return
	}

	if err := client.StreamChat(ctx, req, func(delta string) {
		fmt.Print(delta)
	}); err != nil {
		fatal(err)
	}
	fmt.Println()
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
