// Command home-agent-eval is the operator's agent evaluation harness.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"

	"github.com/vladar107/home-ai-assistant/internal/config"
	"github.com/vladar107/home-ai-assistant/internal/eval"
)

const usageText = "usage: home-agent-eval <login|usage|scan> [flags]"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "home-agent-eval:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New(usageText)
	}
	switch args[0] {
	case "login":
		return runLogin(ctx, args[1:])
	case "usage":
		return runUsage(ctx, args[1:])
	case "scan":
		return runScan(args[1:])
	default:
		return fmt.Errorf("unknown command %q\n%s", args[0], usageText)
	}
}

func profilesFlags(name string) (*flag.FlagSet, *string, *string) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	profiles := flags.String("profiles", "", "eval profiles root (required); the eval profile is <root>/eval")
	envPath := flags.String("env", ".env", "env file read for HOME_AGENT_DATA_DIR")
	return flags, profiles, envPath
}

func prepareProfiles(root, envPath string) (eval.Profiles, error) {
	dataDir := os.Getenv("HOME_AGENT_DATA_DIR")
	if dataDir == "" {
		values, err := eval.ReadEnvFile(envPath)
		if err != nil {
			return eval.Profiles{}, err
		}
		dataDir = values["HOME_AGENT_DATA_DIR"]
	}
	if dataDir == "" {
		dataDir = config.DefaultDataDir
	}
	return eval.PrepareProfiles(root, dataDir)
}

func runLogin(ctx context.Context, args []string) error {
	flags, root, envPath := profilesFlags("login")
	if err := flags.Parse(args); err != nil {
		return err
	}
	profiles, err := prepareProfiles(*root, *envPath)
	if err != nil {
		return err
	}
	return eval.Login(ctx, profiles, eval.Endpoints{}, os.Stdout)
}

func runUsage(ctx context.Context, args []string) error {
	flags, root, envPath := profilesFlags("usage")
	maxUsed := flags.Float64("max-used", eval.DefaultMaxUsedPercent, "refuse when any window is above this percent used")
	if err := flags.Parse(args); err != nil {
		return err
	}
	profiles, err := prepareProfiles(*root, *envPath)
	if err != nil {
		return err
	}
	windows, err := eval.ReadUsage(ctx, profiles, eval.Endpoints{})
	if err != nil {
		return err
	}
	eval.PrintUsage(os.Stdout, windows)
	return eval.CheckQuota(windows, *maxUsed)
}

func runScan(args []string) error {
	flags := flag.NewFlagSet("scan", flag.ContinueOnError)
	envPath := flags.String("env", ".env", "ignored env file holding the household values")
	evalsDir := flags.String("evals", "evals", "directory holding cases/ and variants/")
	if err := flags.Parse(args); err != nil {
		return err
	}
	envPaths := []string{*envPath, filepath.Join(filepath.Dir(*envPath), "deploy", ".env.nas")}
	findings, found, err := eval.Scan(envPaths, []string{filepath.Join(*evalsDir, "cases"), filepath.Join(*evalsDir, "variants")})
	if err != nil {
		return err
	}
	if !found {
		fmt.Println("no env file found: nothing to check")
		return nil
	}
	for _, finding := range findings {
		fmt.Printf("%s: %s contains a household value\n", finding.File, finding.Field)
	}
	if len(findings) > 0 {
		return fmt.Errorf("%d field(s) contain household values", len(findings))
	}
	fmt.Println("no household values found in cases or variants")
	return nil
}
