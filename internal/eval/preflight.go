package eval

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/chatgpt"
)

// DefaultMaxUsedPercent is the share of any quota window an eval may find already used.
const DefaultMaxUsedPercent = 50.0

// Window is one quota window of the eval profile's subscription.
type Window struct {
	Name        string
	UsedPercent float64
	ResetAt     time.Time
}

// ReadUsage refreshes the eval credential when it is due and reads the subscription's usage. The
// usage reader never refreshes, and a profile used weekly almost always holds an expired access
// token, so the refresh goes first, through the connection service and under the profile's lock.
func ReadUsage(ctx context.Context, profiles Profiles, endpoints Endpoints) ([]Window, error) {
	store, err := profiles.OpenStore(ctx)
	if err != nil {
		return nil, err
	}
	defer store.Close()

	service, err := newService(profiles, store, discardDeliverer{}, endpoints)
	if err != nil {
		return nil, err
	}
	if _, err := service.Credential(ctx, PrincipalID); err != nil {
		return nil, fmt.Errorf("eval profile credential: %w", err)
	}
	client, err := newChatGPT(endpoints, service)
	if err != nil {
		return nil, err
	}
	usage, err := client.Usage(ctx, PrincipalID)
	if err != nil {
		return nil, fmt.Errorf("read eval usage: %w", err)
	}
	return windows(usage), nil
}

func windows(usage chatgpt.Usage) []Window {
	var result []Window
	for _, window := range []struct {
		name  string
		value *chatgpt.UsageWindow
	}{{"five-hour", usage.Primary}, {"weekly", usage.Secondary}} {
		if window.value != nil {
			result = append(result, Window{Name: window.name, UsedPercent: window.value.UsedPercent, ResetAt: window.value.ResetAt})
		}
	}
	return result
}

// CheckQuota refuses when any window is above the threshold, naming the window.
func CheckQuota(windows []Window, maxUsedPercent float64) error {
	var over []string
	for _, window := range windows {
		if window.UsedPercent > maxUsedPercent {
			over = append(over, fmt.Sprintf("the %s window is %.0f%% used (resets %s)", window.Name, window.UsedPercent, window.ResetAt.Local().Format(time.DateTime)))
		}
	}
	if len(over) > 0 {
		return fmt.Errorf("quota too high to run (limit %.0f%%): %s", maxUsedPercent, strings.Join(over, "; "))
	}
	return nil
}

// PrintUsage writes the windows, one per line.
func PrintUsage(out io.Writer, windows []Window) {
	if len(windows) == 0 {
		_, _ = fmt.Fprintln(out, "no usage windows reported")
	}
	for _, window := range windows {
		_, _ = fmt.Fprintf(out, "%s window: %.0f%% used, resets %s\n", window.Name, window.UsedPercent, window.ResetAt.Local().Format(time.DateTime))
	}
}

// Preflight reads the usage and refuses to proceed when a window is above the threshold.
func Preflight(ctx context.Context, profiles Profiles, endpoints Endpoints, maxUsedPercent float64) ([]Window, error) {
	windows, err := ReadUsage(ctx, profiles, endpoints)
	if err != nil {
		return nil, err
	}
	return windows, CheckQuota(windows, maxUsedPercent)
}
