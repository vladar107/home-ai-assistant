// Command home-agent-eval is the operator's agent evaluation harness.
package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/vladar107/home-ai-assistant/internal/config"
	"github.com/vladar107/home-ai-assistant/internal/eval"
	"github.com/vladar107/home-ai-assistant/internal/judgment"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

const usageText = "usage: home-agent-eval <login|usage|scan|run|review|report> [flags]"

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
	case "run":
		return runSuite(ctx, args[1:])
	case "review":
		return runReview(args[1:])
	case "report":
		return runReport(args[1:])
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

type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }

func (l *listFlag) Set(value string) error {
	*l = append(*l, strings.Split(value, ",")...)
	return nil
}

func runSuite(ctx context.Context, args []string) error {
	flags, root, envPath := profilesFlags("run")
	casesDir := flags.String("cases", "evals/cases", "directory of case files")
	var caseIDs, tags listFlag
	flags.Var(&caseIDs, "case", "run only this case ID (repeatable or comma-separated)")
	flags.Var(&tags, "tag", "run only cases with this tag (repeatable or comma-separated)")
	budget := flags.Int("budget", eval.DefaultBudget, "most model calls the run may spend")
	maxUsed := flags.Float64("max-used", eval.DefaultMaxUsedPercent, "refuse when any quota window is above this percent used")
	results := flags.String("results", eval.DefaultResultsDir, "directory the run's result, report and cassettes are written under")
	trials := flags.Int("trials", 0, "trials per case, replacing each case's own count")
	if err := flags.Parse(args); err != nil {
		return err
	}
	profiles, err := prepareProfiles(*root, *envPath)
	if err != nil {
		return err
	}
	env, err := eval.ReadEnvFile(*envPath)
	if err != nil {
		return err
	}
	setting := func(name string) string {
		if value := os.Getenv(name); value != "" {
			return value
		}
		return env[name]
	}
	models := map[string]agentruntime.ModelEffort{}
	for _, name := range []string{"HOME_AGENT_PI_MODEL", "HOME_AGENT_PI_TASK_MODEL", "HOME_AGENT_PI_HEADSUP_MODEL"} {
		if value := setting(name); value != "" {
			if models[name], err = agentruntime.ParseModelEffort(value); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
	}
	typeSafeKey, err := typeSafeKey(setting)
	if err != nil {
		return err
	}
	webSearch, err := filepath.Abs(cmp.Or(setting("HOME_AGENT_PI_WEB_SEARCH_EXTENSION"), filepath.Join("third_party", "pi-web-search", "src", "index.ts")))
	if err != nil {
		return err
	}
	if info, statErr := os.Stat(webSearch); statErr != nil || !info.Mode().IsRegular() {
		fmt.Fprintln(os.Stderr, "home-agent-eval: web search extension not found; running without web search")
		webSearch = ""
	}
	piBinary, err := executablePath(setting("HOME_AGENT_PI_BINARY"))
	if err != nil {
		return err
	}
	nodeBinary, err := executablePath(setting("HOME_AGENT_PI_NODE_BINARY"))
	if err != nil {
		return err
	}
	result, err := eval.Run(ctx, eval.RunOptions{
		Profiles: profiles, CasesDir: *casesDir, CaseIDs: caseIDs, Tags: tags, Trials: *trials,
		Budget: *budget, MaxUsedPercent: *maxUsed, ResultsDir: *results,
		ScanEnvPaths: []string{*envPath, filepath.Join(filepath.Dir(*envPath), "deploy", ".env.nas")},
		ScanDirs:     []string{*casesDir, filepath.Join(filepath.Dir(*casesDir), "variants")},
		Pi: eval.PiOptions{
			Binary: piBinary, NodeBinary: nodeBinary,
			Provider: setting("HOME_AGENT_PI_PROVIDER"), WebSearchExtension: webSearch,
		},
		ChatModel: models["HOME_AGENT_PI_MODEL"], TaskModel: models["HOME_AGENT_PI_TASK_MODEL"], HeadsUpModel: models["HOME_AGENT_PI_HEADSUP_MODEL"],
		TypeSafe: judgment.Config{APIKey: typeSafeKey}, Commit: gitDescribe(), Out: os.Stdout,
	})
	if result != nil {
		fmt.Printf("pass rate %s, pass^k %.1f%% (k=%d)\nreport: %s\n", result.Summary.Overall, 100*result.Summary.PassK, result.Summary.K,
			filepath.Join(result.Dir, "report.md"))
	}
	if err != nil {
		return err
	}
	if result.Stopped != "" {
		return fmt.Errorf("run stopped early (%s): %s", result.Stopped, result.StoppedDetail)
	}
	return nil
}

// executablePath makes a binary setting that names a path independent of the working directory,
// because Pi runs in a per-principal directory; a bare command name is left for the PATH lookup.
func executablePath(setting string) (string, error) {
	if setting == "" || !strings.ContainsRune(setting, filepath.Separator) {
		return setting, nil
	}
	return filepath.Abs(setting)
}

func runDir(flags *flag.FlagSet, args []string) (string, error) {
	results := flags.String("results", eval.DefaultResultsDir, "directory the runs are written under")
	runID := flags.String("run", "", "run ID (default: the latest run)")
	if err := flags.Parse(args); err != nil {
		return "", err
	}
	if *runID == "" {
		latest, err := eval.LatestRun(*results)
		if err != nil {
			return "", err
		}
		*runID = latest
	}
	return filepath.Join(*results, *runID), nil
}

func runReview(args []string) error {
	flags := flag.NewFlagSet("review", flag.ContinueOnError)
	all := flags.Bool("all", false, "show trials that already have a verdict too")
	dir, err := runDir(flags, args)
	if err != nil {
		return err
	}
	return eval.Review(eval.ReviewOptions{Dir: dir, All: *all, In: os.Stdin, Out: os.Stdout})
}

func runReport(args []string) error {
	dir, err := runDir(flag.NewFlagSet("report", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	if err := eval.RewriteReport(dir); err != nil {
		return err
	}
	fmt.Println("report:", filepath.Join(dir, "report.md"))
	return nil
}

func typeSafeKey(setting func(string) string) (string, error) {
	if key := strings.TrimSpace(setting("HOME_AGENT_TYPESAFEAI_KEY")); key != "" {
		return key, nil
	}
	file := setting("HOME_AGENT_TYPESAFEAI_KEY_FILE")
	if file == "" {
		return "", nil
	}
	contents, err := os.ReadFile(file)
	if err != nil {
		return "", errors.New("HOME_AGENT_TYPESAFEAI_KEY_FILE: could not be read")
	}
	return strings.TrimSpace(string(contents)), nil
}

func gitDescribe() string {
	output, err := exec.Command("git", "describe", "--always", "--dirty").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(output))
}
