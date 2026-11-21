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
	"slices"
	"strings"

	"github.com/vladar107/home-ai-assistant/internal/config"
	"github.com/vladar107/home-ai-assistant/internal/eval"
	"github.com/vladar107/home-ai-assistant/internal/judgment"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

const usageText = "usage: home-agent-eval <login|usage|scan|probe|run|review|report|import|trend|compare|replay> [flags]"

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
	case "probe":
		return runProbe(ctx, args[1:])
	case "run":
		return runSuite(ctx, args[1:])
	case "review":
		return runReview(args[1:])
	case "report":
		return runReport(args[1:])
	case "replay":
		return runReplay(ctx, args[1:])
	case "import":
		return runImport(ctx, args[1:])
	case "trend":
		return runTrend(ctx, args[1:])
	case "compare":
		return runCompare(ctx, args[1:])
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

func runProbe(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("probe", flag.ContinueOnError)
	casesDir := flags.String("cases", "evals/cases", "directory of case files")
	var caseIDs listFlag
	flags.Var(&caseIDs, "case", "probe only this case ID (repeatable or comma-separated)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cases, err := eval.LoadCases(*casesDir)
	if err != nil {
		return err
	}
	if len(caseIDs) > 0 {
		for _, id := range caseIDs {
			if !slices.ContainsFunc(cases, func(c eval.Case) bool { return c.ID == id }) {
				return fmt.Errorf("no case %q in %s", id, *casesDir)
			}
		}
		cases = slices.DeleteFunc(cases, func(c eval.Case) bool { return !slices.Contains(caseIDs, c.ID) })
	}
	report, err := eval.RunProbes(ctx, cases)
	if err != nil {
		return err
	}
	fmt.Print(report.Markdown())
	if n := report.Flagged(); n > 0 {
		return fmt.Errorf("%d case(s) pass an unexpected trivial-answer probe", n)
	}
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
	variantName := flags.String("variant", "", "run under this prompt variant, evals/variants/<name>.json (default: production's prompt)")
	variantsDir := flags.String("variants", "", "directory of variant files (default: variants/ beside the cases directory)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	profiles, err := prepareProfiles(*root, *envPath)
	if err != nil {
		return err
	}
	if *variantsDir == "" {
		*variantsDir = filepath.Join(filepath.Dir(*casesDir), "variants")
	}
	var variant *eval.Variant
	if *variantName != "" {
		if variant, err = eval.LoadVariant(*variantsDir, *variantName); err != nil {
			return err
		}
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
		ScanDirs:     []string{*casesDir, *variantsDir}, Variant: variant,
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
		if importErr := importRun(context.WithoutCancel(ctx), result.Dir); importErr != nil {
			fmt.Fprintln(os.Stderr, "home-agent-eval:", importErr)
		}
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
	cases := flags.String("case", "", "comma-separated case IDs to review (default: every case)")
	dir, err := runDir(flags, args)
	if err != nil {
		return err
	}
	var only []string
	if *cases != "" {
		only = strings.Split(*cases, ",")
	}
	reviewErr := eval.Review(eval.ReviewOptions{Dir: dir, All: *all, Cases: only, In: os.Stdin, Out: os.Stdout})
	return errors.Join(reviewErr, importRun(context.Background(), dir))
}

// importRun records a run directory in the results store beside it.
func importRun(ctx context.Context, dir string) error {
	if err := eval.ImportRun(ctx, filepath.Dir(dir), filepath.Base(dir)); err != nil {
		return fmt.Errorf("import the run into the results store: %w", err)
	}
	return nil
}

func openStore(flags *flag.FlagSet, args []string) (*eval.ResultsStore, string, error) {
	results := flags.String("results", eval.DefaultResultsDir, "directory the runs and the results store are in")
	if err := flags.Parse(args); err != nil {
		return nil, "", err
	}
	store, err := eval.OpenResultsStore(*results)
	return store, *results, err
}

func runReplay(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("replay", flag.ContinueOnError)
	results := flags.String("results", eval.DefaultResultsDir, "directory the runs are written under")
	runID := flags.String("run", "", "run ID to replay (default: the latest run)")
	casesDir := flags.String("cases", "evals/cases", "directory of case files holding the checks to score with")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *runID == "" {
		latest, err := eval.LatestRun(*results)
		if err != nil {
			return err
		}
		*runID = latest
	}
	replayed, err := eval.Replay(ctx, eval.ReplayOptions{ResultsDir: *results, SourceRunID: *runID, CasesDir: *casesDir})
	if replayed != nil {
		fmt.Printf("replayed %s as %s: pass rate %s, pass^k %.1f%% (k=%d); %d verdict(s) changed\nreport: %s\n", *runID, replayed.RunID,
			replayed.Summary.Overall, 100*replayed.Summary.PassK, replayed.Summary.K, len(replayed.ReplayChanges), filepath.Join(replayed.Dir, "report.md"))
	}
	if err != nil {
		return err
	}
	return importRun(ctx, replayed.Dir)
}

func runImport(ctx context.Context, args []string) error {
	store, results, err := openStore(flag.NewFlagSet("import", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	defer store.Close()
	ids, err := store.ImportAll(ctx, results)
	if err != nil {
		return err
	}
	fmt.Printf("imported %d run(s) into %s\n", len(ids), filepath.Join(results, eval.ResultsStoreFile))
	return nil
}

func runTrend(ctx context.Context, args []string) error {
	store, _, err := openStore(flag.NewFlagSet("trend", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	defer store.Close()
	runs, err := store.Runs(ctx)
	if err != nil {
		return err
	}
	eval.WriteTrend(os.Stdout, runs)
	return nil
}

func runCompare(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("compare", flag.ContinueOnError)
	store, results, err := openStore(flags, args)
	if err != nil {
		return err
	}
	defer store.Close()
	if flags.NArg() != 2 {
		return errors.New("usage: home-agent-eval compare [--results dir] <first-run-id> <second-run-id>  (an ID may be \"latest\")")
	}
	var ids [2]string
	for i := range ids {
		if ids[i], err = store.ResolveRun(ctx, flags.Arg(i)); err != nil {
			return err
		}
	}
	comparison, err := eval.Compare(ctx, store, ids[0], ids[1])
	if err != nil {
		return err
	}
	path, err := eval.WriteComparison(results, comparison)
	if err != nil {
		return err
	}
	fmt.Println(comparison.Summary())
	fmt.Println("comparison:", path)
	return nil
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
