package pifixture

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"
)

const Version = "0.0.0-fixture"

type Report struct {
	Args        []string `json:"args"`
	Environment []string `json:"environment"`
	Directory   string   `json:"directory"`
	Files       []string `json:"files"`
	PID         int      `json:"pid"`
	PromptID    string   `json:"prompt_id"`
	Message     string   `json:"message"`
}

func ReportPath(sessionDir, sessionID string) string {
	return filepath.Join(sessionDir, sessionID+".fixture.json")
}

// RunIfRequested dispatches before testing parses flags in the child test binary.
func RunIfRequested() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Println(Version)
		os.Exit(0)
	}
	if len(os.Args) < 3 || os.Args[1] != "--mode" || os.Args[2] != "rpc" {
		return
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	os.Exit(0)
}

func run() error {
	args := map[string]string{}
	for i := 1; i < len(os.Args); i++ {
		key := os.Args[i]
		if i+1 < len(os.Args) && !strings.HasPrefix(os.Args[i+1], "--") {
			i++
			args[key] = os.Args[i]
		}
	}
	var prompt struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(os.Stdin).Decode(&prompt); err != nil {
		return err
	}
	if prompt.Type != "prompt" {
		return fmt.Errorf("unexpected command %q", prompt.Type)
	}
	if prompt.Message == "fixture:stubborn" {
		signal.Ignore(os.Interrupt)
	}
	directory, err := os.Getwd()
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	report := Report{Args: os.Args[1:], Environment: os.Environ(), Directory: directory, PID: os.Getpid(), PromptID: prompt.ID, Message: prompt.Message}
	for _, entry := range entries {
		report.Files = append(report.Files, entry.Name())
	}
	data, err := json.Marshal(report)
	if err != nil {
		return err
	}
	if err := os.WriteFile(ReportPath(args["--session-dir"], args["--session-id"]), data, 0o600); err != nil {
		return err
	}
	output := bufio.NewWriter(os.Stdout)
	emit := func(value any) {
		_ = json.NewEncoder(output).Encode(value)
		_ = output.Flush()
	}
	delta := func(text string) {
		emit(map[string]any{"type": "message_update", "assistantMessageEvent": map[string]string{"type": "text_delta", "delta": text}})
	}
	ack := func() { emit(map[string]any{"type": "response", "command": "prompt", "success": true}) }
	settle := func() { emit(map[string]string{"type": "agent_settled"}) }
	switch prompt.Message {
	case "fixture:hold", "fixture:stubborn":
		for {
			time.Sleep(time.Hour)
		}
	case "fixture:exit":
		return nil
	case "fixture:malformed":
		fmt.Fprintln(os.Stdout, "not-json synthetic-private-text")
	case "fixture:oversized-event":
		fmt.Fprintln(os.Stdout, strings.Repeat("x", (4<<20)+1))
	case "fixture:oversized-response":
		ack()
		delta(strings.Repeat("x", (256<<10)+1))
	case "fixture:reject":
		emit(map[string]any{"type": "response", "command": "prompt", "success": false, "error": "synthetic-private-text"})
	case "fixture:tool":
		emit(map[string]string{"type": "tool_execution_start"})
	case "fixture:toolcall":
		emit(map[string]any{"type": "message_update", "assistantMessageEvent": map[string]string{"type": "toolcall_start"}})
	case "fixture:extension-error":
		emit(map[string]string{"type": "extension_error", "error": "synthetic-private-text"})
	case "fixture:no-ack":
		delta("unacknowledged")
		settle()
	case "fixture:empty":
		ack()
		delta("   ")
		settle()
	case "fixture:flood-after-tool":
		// One write keeps queued events available after the adapter rejects the first.
		var batch strings.Builder
		batch.WriteString("{\"type\":\"tool_execution_start\"}\n")
		for range 1000 {
			batch.WriteString("{\"type\":\"message_update\",\"assistantMessageEvent\":{\"type\":\"text_delta\",\"delta\":\"x\"}}\n")
		}
		fmt.Fprint(os.Stdout, batch.String())
	default:
		sessionFile := filepath.Join(args["--session-dir"], args["--session-id"]+".history")
		answer := "reply: " + prompt.Message
		if strings.HasPrefix(prompt.Message, "remember ") {
			value := strings.TrimPrefix(prompt.Message, "remember ")
			if err := os.WriteFile(sessionFile, []byte(value), 0o600); err != nil {
				return err
			}
			answer = "remembered " + value
		} else if prompt.Message == "recall" {
			value, err := os.ReadFile(sessionFile)
			switch {
			case err == nil:
				answer = string(value)
			case os.IsNotExist(err):
				answer = "nothing remembered"
			default:
				return err
			}
		}
		ack()
		middle := len(answer) / 2
		delta(answer[:middle])
		delta(answer[middle:])
		settle()
	}
	_, err = io.Copy(io.Discard, os.Stdin)
	return err
}
