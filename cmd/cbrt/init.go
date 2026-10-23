package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/alex-danilin/cbrt/internal/autoconfig"
	"github.com/alex-danilin/cbrt/internal/config"
)

// runInit is `cbrt init`: detect providers and write a starting cbrt.json.
func runInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	cfgPath := fs.String("config", "cbrt.json", "configuration file to write")
	envFile := fs.String("env-file", ".env", "KEY=VALUE file loaded into the environment if present (process variables win)")
	force := fs.Bool("force", false, "overwrite an existing configuration file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("init: unexpected arguments %q", fs.Args())
	}
	return initConfig(context.Background(), *cfgPath, *envFile, *force, autoconfig.Options{}, os.Stdout)
}

// initConfig generates a configuration and writes it to cfgPath. An existing
// file is only replaced with force. Nothing is written when detection finds no
// usable provider.
func initConfig(ctx context.Context, cfgPath, envFile string, force bool, o autoconfig.Options, out io.Writer) error {
	if !force {
		if _, err := os.Stat(cfgPath); err == nil {
			return fmt.Errorf("%s already exists: edit it, or run `cbrt init -force` to replace it", cfgPath)
		}
	}
	if err := config.NewEnvFile(envFile).Load(); err != nil {
		return fmt.Errorf("env file: %w", err)
	}
	o.NVIDIA = os.Getenv("NVIDIA_API_KEY") != ""
	ctx, cancel := context.WithTimeout(ctx, detectTimeout)
	defer cancel()
	doc, rep, err := autoconfig.Generate(ctx, o)
	printReport(out, rep)
	if err != nil {
		return err
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if force {
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	f, err := os.OpenFile(cfgPath, flags, 0o600)
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("%s already exists: edit it, or run `cbrt init -force` to replace it", cfgPath)
	}
	if err != nil {
		return err
	}
	_, err = f.Write(doc)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "wrote %s: edit it, then run `cbrt serve`\n", cfgPath)
	return nil
}
