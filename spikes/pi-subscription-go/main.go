package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

const expectedMarker = "PI_GO_OK"

type options struct {
	piBinary string
	provider string
	model    string
	prompt   string
	timeout  time.Duration
}

type rpcEvent struct {
	Type                  string                 `json:"type"`
	Command               string                 `json:"command"`
	Success               bool                   `json:"success"`
	Error                 string                 `json:"error"`
	Message               json.RawMessage        `json:"message"`
	AssistantMessageEvent *assistantMessageEvent `json:"assistantMessageEvent"`
}

type assistantMessageEvent struct {
	Type     string `json:"type"`
	Delta    string `json:"delta"`
	ToolName string `json:"toolName"`
}

type runResult struct {
	response           string
	promptAcknowledged bool
	agentSettled       bool
	toolEvents         int
	elapsed            time.Duration
}

func main() {
	opts := parseFlags()

	result, err := run(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pi_go_check=failed\nerror=%s\n", safeError(err))
		os.Exit(1)
	}

	fmt.Println("pi_go_check=passed")
	fmt.Printf("provider=%s\n", opts.provider)
	fmt.Printf("prompt_acknowledged=%t\n", result.promptAcknowledged)
	fmt.Printf("agent_settled=%t\n", result.agentSettled)
	fmt.Printf("tool_events=%d\n", result.toolEvents)
	fmt.Printf("elapsed=%s\n", result.elapsed.Round(time.Millisecond))
	fmt.Printf("response=%s\n", strings.TrimSpace(result.response))
}

func parseFlags() options {
	var opts options
	flag.StringVar(&opts.piBinary, "pi-binary", "pi", "path to the Pi executable")
	flag.StringVar(&opts.provider, "provider", "openai-codex", "Pi provider to verify")
	flag.StringVar(&opts.model, "model", "", "optional Pi model ID or pattern")
	flag.StringVar(&opts.prompt, "prompt", "Reply with exactly PI_GO_OK and nothing else.", "verification prompt")
	flag.DurationVar(&opts.timeout, "timeout", 2*time.Minute, "overall verification timeout")
	flag.Parse()
	return opts
}

func run(opts options) (runResult, error) {
	if opts.provider == "" {
		return runResult{}, errors.New("provider must not be empty")
	}
	if opts.timeout <= 0 {
		return runResult{}, errors.New("timeout must be positive")
	}

	workDir, err := os.MkdirTemp("", "pi-go-subscription-check-")
	if err != nil {
		return runResult{}, fmt.Errorf("create isolated working directory: %w", err)
	}
	defer os.RemoveAll(workDir)

	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout)
	defer cancel()

	args := []string{
		"--mode", "rpc",
		"--provider", opts.provider,
		"--no-session",
		"--no-tools",
		"--no-extensions",
		"--no-skills",
		"--no-prompt-templates",
		"--no-themes",
		"--no-context-files",
		"--no-approve",
	}
	if opts.model != "" {
		args = append(args, "--model", opts.model)
	}

	cmd := exec.CommandContext(ctx, opts.piBinary, args...)
	cmd.Dir = workDir
	cmd.Stderr = io.Discard

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return runResult{}, fmt.Errorf("open Pi stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return runResult{}, fmt.Errorf("open Pi stdout: %w", err)
	}

	startedAt := time.Now()
	if err := cmd.Start(); err != nil {
		return runResult{}, fmt.Errorf("start Pi: %w", err)
	}

	eventCh := make(chan rpcEvent)
	scanErrCh := make(chan error, 1)
	go scanEvents(stdout, eventCh, scanErrCh)

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()

	prompt := map[string]any{
		"id":      "pi-go-check-1",
		"type":    "prompt",
		"message": opts.prompt,
	}
	if err := json.NewEncoder(stdin).Encode(prompt); err != nil {
		stopProcess(cmd, stdin, waitCh)
		return runResult{}, fmt.Errorf("send RPC prompt: %w", err)
	}

	var result runResult
	var response strings.Builder

	for !result.agentSettled {
		select {
		case event, ok := <-eventCh:
			if !ok {
				stopProcess(cmd, stdin, waitCh)
				return runResult{}, errors.New("Pi event stream closed before agent_settled")
			}
			if err := consumeEvent(event, &result, &response); err != nil {
				stopProcess(cmd, stdin, waitCh)
				return runResult{}, err
			}
		case err := <-scanErrCh:
			stopProcess(cmd, stdin, waitCh)
			if err == nil {
				return runResult{}, errors.New("Pi output ended before agent_settled")
			}
			return runResult{}, fmt.Errorf("read Pi RPC output: %w", err)
		case err := <-waitCh:
			_ = stdin.Close()
			if err != nil {
				return runResult{}, errors.New("Pi exited before agent_settled")
			}
			return runResult{}, errors.New("Pi exited without agent_settled")
		case <-ctx.Done():
			stopProcess(cmd, stdin, waitCh)
			return runResult{}, fmt.Errorf("verification timeout: %w", ctx.Err())
		}
	}

	stopProcess(cmd, stdin, waitCh)
	result.response = response.String()
	result.elapsed = time.Since(startedAt)

	if !result.promptAcknowledged {
		return runResult{}, errors.New("Pi did not acknowledge the RPC prompt")
	}
	if result.toolEvents != 0 {
		return runResult{}, fmt.Errorf("Pi emitted %d tool events despite --no-tools", result.toolEvents)
	}
	if !strings.Contains(result.response, expectedMarker) {
		return runResult{}, errors.New("response did not contain the expected marker")
	}

	return result, nil
}

func scanEvents(stdout io.Reader, eventCh chan<- rpcEvent, scanErrCh chan<- error) {
	defer close(eventCh)

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var event rpcEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			scanErrCh <- errors.New("invalid JSONL event from Pi")
			return
		}
		eventCh <- event
	}
	scanErrCh <- scanner.Err()
}

func consumeEvent(event rpcEvent, result *runResult, response *strings.Builder) error {
	switch event.Type {
	case "response":
		if event.Command == "prompt" {
			if !event.Success {
				return errors.New("Pi rejected the RPC prompt")
			}
			result.promptAcknowledged = true
		}
	case "message_update":
		if event.AssistantMessageEvent == nil {
			return nil
		}
		switch event.AssistantMessageEvent.Type {
		case "text_delta":
			response.WriteString(event.AssistantMessageEvent.Delta)
		case "toolcall_start", "toolcall_delta", "toolcall_end":
			result.toolEvents++
		}
	case "tool_execution_start", "tool_execution_update", "tool_execution_end":
		result.toolEvents++
	case "agent_settled":
		result.agentSettled = true
	case "extension_error":
		return errors.New("Pi reported an extension error")
	}

	return nil
}

func stopProcess(cmd *exec.Cmd, stdin io.Closer, waitCh <-chan error) {
	_ = stdin.Close()

	select {
	case <-waitCh:
		return
	case <-time.After(2 * time.Second):
	}

	if cmd.Process != nil {
		_ = cmd.Process.Signal(os.Interrupt)
	}
	select {
	case <-waitCh:
		return
	case <-time.After(2 * time.Second):
	}

	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	select {
	case <-waitCh:
	case <-time.After(2 * time.Second):
	}
}

func safeError(err error) string {
	message := strings.ReplaceAll(err.Error(), "\n", " ")
	if len(message) > 300 {
		return message[:300]
	}
	return message
}
