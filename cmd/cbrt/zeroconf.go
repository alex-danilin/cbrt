package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/alex-danilin/cbrt/internal/autoconfig"
	"github.com/alex-danilin/cbrt/internal/config"
)

// loader builds the gateway configuration. serve calls it at startup and on every
// /api/reload, so it must re-read everything that may have changed.
type loader func() (*config.Config, error)

// detectTimeout bounds one provider detection (Ollama probes: 3s per call by default).
const detectTimeout = 30 * time.Second

// fileLoader re-reads the env file and then the configuration file on every call.
// Variables set in the real process environment always win over the env file.
func fileLoader(env *config.EnvFile, cfgPath string) loader {
	return func() (*config.Config, error) {
		if err := env.Load(); err != nil {
			return nil, fmt.Errorf("env file: %w", err)
		}
		return config.Load(cfgPath)
	}
}

// zeroLoader builds the configuration from what is available right now: it
// re-reads the env file, then runs autoconfig.Generate (NVIDIA is enabled when
// NVIDIA_API_KEY is non-empty). Every call re-runs detection, so a reload picks
// up an Ollama that was started later. report, if non-nil, receives each
// detection's report, also when detection fails.
func zeroLoader(env *config.EnvFile, o autoconfig.Options, report func(autoconfig.Report)) loader {
	return func() (*config.Config, error) {
		if err := env.Load(); err != nil {
			return nil, fmt.Errorf("env file: %w", err)
		}
		opts := o
		opts.NVIDIA = os.Getenv("NVIDIA_API_KEY") != ""
		ctx, cancel := context.WithTimeout(context.Background(), detectTimeout)
		defer cancel()
		doc, rep, err := autoconfig.Generate(ctx, opts)
		if report != nil {
			report(rep)
		}
		if err != nil {
			return nil, err
		}
		cfg, err := config.Parse(doc)
		if err != nil {
			return nil, fmt.Errorf("generated configuration: %w", err)
		}
		return cfg, nil
	}
}

// serveLoader picks the configuration source for `cbrt serve`: the file when
// -config was given (a missing file is then an error) or when it exists, and
// provider detection otherwise. The first detection report goes to out; later
// ones (from /api/reload) go to the log.
func serveLoader(cfgPath string, explicit bool, envFile string, o autoconfig.Options, out io.Writer) loader {
	env := config.NewEnvFile(envFile)
	if _, err := os.Stat(cfgPath); explicit || !errors.Is(err, os.ErrNotExist) {
		return fileLoader(env, cfgPath)
	}
	first := true
	return zeroLoader(env, o, func(rep autoconfig.Report) {
		if first {
			first = false
			fmt.Fprintf(out, "cbrt: %s not found; detecting providers (run `cbrt init` to write a config you can edit)\n", cfgPath)
			printReport(out, rep)
			return
		}
		logReport(slog.Default(), rep)
	})
}

// printReport prints what detection found, then its warnings.
func printReport(w io.Writer, rep autoconfig.Report) {
	for _, f := range rep.Found {
		fmt.Fprintln(w, "  "+f)
	}
	for _, x := range rep.Warnings {
		fmt.Fprintln(w, "warning: "+x)
	}
}

func logReport(log *slog.Logger, rep autoconfig.Report) {
	for _, f := range rep.Found {
		log.Info("detected", "what", f)
	}
	for _, x := range rep.Warnings {
		log.Warn("detection", "warning", x)
	}
}
