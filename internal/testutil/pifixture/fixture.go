package pifixture

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"
)

const Version = "0.0.0-fixture"

// RetryBackoff is how long the retry scenario waits between its failed attempt and the retry.
const RetryBackoff = 300 * time.Millisecond

type Report struct {
	Args        []string `json:"args"`
	Environment []string `json:"environment"`
	Directory   string   `json:"directory"`
	Files       []string `json:"files"`
	PID         int      `json:"pid"`
	PromptID    string   `json:"prompt_id"`
	Message     string   `json:"message"`
	Images      []Image  `json:"images"`
	// BridgeFiles lists the directory of the --extension argument and Tools holds the tools.json
	// beside it, both captured while the turn is running because the adapter removes them after.
	BridgeFiles []string        `json:"bridge_files"`
	Tools       json.RawMessage `json:"tools"`
	// AbortReceived is rewritten by the retry scenarios once they stop waiting for abort_retry.
	AbortReceived bool `json:"abort_received"`
}

// Image is one picture the prompt carried, kept as Pi receives it.
type Image struct {
	Type     string `json:"type"`
	Data     string `json:"data"`
	MimeType string `json:"mimeType"`
}

// BridgeCall is what the tool scenario posts to the application, mirroring the real extension.
type BridgeCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type BridgeResult struct {
	Text    string `json:"text"`
	IsError bool   `json:"isError"`
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
	var extensions []string
	for i := 1; i < len(os.Args); i++ {
		key := os.Args[i]
		if i+1 < len(os.Args) && !strings.HasPrefix(os.Args[i+1], "-") {
			i++
			args[key] = os.Args[i]
			if key == "--extension" {
				extensions = append(extensions, os.Args[i])
			}
		}
	}
	var prompt struct {
		ID      string  `json:"id"`
		Type    string  `json:"type"`
		Message string  `json:"message"`
		Images  []Image `json:"images"`
	}
	// One decoder keeps commands sent after the prompt readable from its buffer.
	commands := json.NewDecoder(os.Stdin)
	if err := commands.Decode(&prompt); err != nil {
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
	report := Report{Args: os.Args[1:], Environment: os.Environ(), Directory: directory, PID: os.Getpid(), PromptID: prompt.ID, Message: prompt.Message, Images: prompt.Images}
	for _, entry := range entries {
		report.Files = append(report.Files, entry.Name())
	}
	// Only the bridge extension is named bridge.ts; the search extension is never loaded here.
	for _, extension := range extensions {
		if filepath.Base(extension) != "bridge.ts" {
			continue
		}
		bridgeEntries, err := os.ReadDir(filepath.Dir(extension))
		if err != nil {
			return err
		}
		for _, entry := range bridgeEntries {
			report.BridgeFiles = append(report.BridgeFiles, entry.Name())
		}
		report.Tools, err = os.ReadFile(filepath.Join(filepath.Dir(extension), "tools.json"))
		if err != nil {
			return err
		}
	}
	writeReport := func() error {
		data, err := json.Marshal(report)
		if err != nil {
			return err
		}
		return os.WriteFile(ReportPath(args["--session-dir"], args["--session-id"]), data, 0o600)
	}
	if err := writeReport(); err != nil {
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
	ack := func() {
		emit(map[string]any{"type": "response", "command": "prompt", "success": true, "data": map[string]string{"disposition": "started"}})
	}
	settle := func() { emit(map[string]string{"type": "agent_settled"}) }
	failMessage := func(text string) {
		emit(map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "stopReason": "error", "errorMessage": text}})
	}
	retryStart := func(text string) {
		emit(map[string]any{"type": "auto_retry_start", "attempt": 1, "maxAttempts": 3, "delayMs": 2000, "errorMessage": text})
	}
	const (
		quotaText      = "Codex error: The usage limit has been reached synthetic-private-text"
		authText       = "Unauthorized: token expired synthetic-private-text"
		missingKeyText = "No API key found for fixture-provider synthetic-private-text"
		otherText      = "network error synthetic-private-text"
	)
	if strings.HasPrefix(prompt.Message, "fixture:call ") {
		// Pi runs a tool call between two assistant messages; the events mirror its RPC stream.
		name, arguments, _ := strings.Cut(strings.TrimPrefix(prompt.Message, "fixture:call "), " ")
		ack()
		emit(map[string]any{"type": "message_start", "message": map[string]string{"role": "assistant"}})
		delta("Checking.")
		emit(map[string]any{"type": "message_update", "assistantMessageEvent": map[string]string{"type": "toolcall_start"}})
		emit(map[string]any{"type": "tool_execution_start", "toolCallId": "call-1", "toolName": name})
		text, isError, err := callBridge(name, arguments)
		if err != nil {
			return err
		}
		emit(map[string]any{"type": "tool_execution_end", "toolCallId": "call-1", "toolName": name, "isError": isError})
		emit(map[string]any{"type": "message_start", "message": map[string]string{"role": "assistant"}})
		if isError {
			text = "error: " + text
		}
		delta(text)
		settle()
		_, err = io.Copy(io.Discard, os.Stdin)
		return err
	}
	switch prompt.Message {
	case "fixture:usage", "fixture:usage-missing":
		// Two assistant messages around a web_search call, as Pi reports model usage per message.
		ack()
		// User and tool result messages carry usage-shaped fields the runtime must ignore.
		noise := map[string]int{"input": 1000, "output": 1000, "cacheRead": 1000, "cacheWrite": 1000}
		emit(map[string]any{"type": "message_start", "message": map[string]any{"role": "user"}})
		emit(map[string]any{"type": "message_end", "message": map[string]any{"role": "user", "usage": noise}})
		emit(map[string]any{"type": "message_start", "message": map[string]string{"role": "assistant", "provider": "fixture-provider", "model": "fixture-model"}})
		delta("Searching.")
		emit(map[string]any{"type": "message_end", "message": map[string]any{
			"role": "assistant", "provider": "fixture-provider", "model": "fixture-model", "stopReason": "toolUse",
			"usage": map[string]int{"input": 100, "output": 20, "cacheRead": 50, "cacheWrite": 10, "totalTokens": 180},
		}})
		emit(map[string]any{"type": "tool_execution_start", "toolCallId": "call-1", "toolName": "web_search"})
		emit(map[string]any{"type": "tool_execution_end", "toolCallId": "call-1", "toolName": "web_search", "isError": false})
		emit(map[string]any{"type": "message_start", "message": map[string]any{"role": "toolResult"}})
		emit(map[string]any{"type": "message_end", "message": map[string]any{"role": "toolResult", "usage": noise}})
		emit(map[string]any{"type": "message_start", "message": map[string]string{"role": "assistant", "provider": "fixture-provider", "model": "fixture-model"}})
		delta("found")
		final := map[string]any{"role": "assistant", "provider": "fixture-provider", "model": "fixture-model", "stopReason": "stop"}
		if prompt.Message == "fixture:usage" {
			final["usage"] = map[string]int{"input": 30, "output": 5, "cacheRead": 120, "cacheWrite": 0, "totalTokens": 155}
		}
		emit(map[string]any{"type": "message_end", "message": final})
		settle()
	case "fixture:pi-tools":
		// Pi 1.0.0's own order for a parallel tool batch: turn_start precedes every assistant
		// request, and tool ends arrive in completion order before the tool result messages.
		ack()
		emit(map[string]string{"type": "agent_start"})
		emit(map[string]string{"type": "turn_start"})
		emit(map[string]any{"type": "message_start", "message": map[string]any{"role": "system"}})
		emit(map[string]any{"type": "message_end", "message": map[string]any{"role": "system"}})
		emit(map[string]any{"type": "message_start", "message": map[string]any{"role": "user"}})
		emit(map[string]any{"type": "message_end", "message": map[string]any{"role": "user"}})
		emit(map[string]any{"type": "message_start", "message": map[string]string{"role": "assistant"}})
		emit(map[string]any{"type": "message_update", "assistantMessageEvent": map[string]string{"type": "toolcall_start"}})
		emit(map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "stopReason": "toolUse"}})
		emit(map[string]any{"type": "tool_execution_start", "toolCallId": "call-1", "toolName": "web_search"})
		emit(map[string]any{"type": "tool_execution_start", "toolCallId": "call-2", "toolName": "web_search"})
		emit(map[string]any{"type": "tool_execution_end", "toolCallId": "call-2", "toolName": "web_search", "isError": false})
		emit(map[string]any{"type": "tool_execution_end", "toolCallId": "call-1", "toolName": "web_search", "isError": false})
		for range 2 {
			emit(map[string]any{"type": "message_start", "message": map[string]any{"role": "toolResult"}})
			emit(map[string]any{"type": "message_end", "message": map[string]any{"role": "toolResult"}})
		}
		emit(map[string]string{"type": "turn_end"})
		emit(map[string]string{"type": "turn_start"})
		emit(map[string]any{"type": "message_start", "message": map[string]string{"role": "assistant"}})
		delta("found")
		emit(map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "stopReason": "stop"}})
		emit(map[string]string{"type": "turn_end"})
		emit(map[string]string{"type": "agent_end"})
		settle()
	case "fixture:pi-retry", "fixture:pi-retry-exhausted":
		// Pi 1.0.0 schedules a retry only after the failed attempt's turn and run end, then
		// sleeps through the backoff before starting a new run.
		ack()
		emit(map[string]string{"type": "agent_start"})
		emit(map[string]string{"type": "turn_start"})
		emit(map[string]any{"type": "message_start", "message": map[string]any{"role": "system"}})
		emit(map[string]any{"type": "message_end", "message": map[string]any{"role": "system"}})
		emit(map[string]any{"type": "message_start", "message": map[string]string{"role": "assistant"}})
		failMessage(otherText)
		emit(map[string]string{"type": "turn_end"})
		emit(map[string]any{"type": "agent_end", "willRetry": true})
		retryStart(otherText)
		emit(map[string]string{"type": "entry_appended"})
		time.Sleep(RetryBackoff)
		emit(map[string]string{"type": "agent_start"})
		emit(map[string]string{"type": "turn_start"})
		emit(map[string]any{"type": "message_start", "message": map[string]string{"role": "assistant"}})
		if prompt.Message == "fixture:pi-retry-exhausted" {
			failMessage(otherText)
			emit(map[string]string{"type": "turn_end"})
			emit(map[string]any{"type": "agent_end", "willRetry": false})
			emit(map[string]any{"type": "auto_retry_end", "success": false, "attempt": 1, "finalError": otherText})
			settle()
			break
		}
		delta("recovered")
		emit(map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "stopReason": "stop"}})
		emit(map[string]any{"type": "auto_retry_end", "success": true, "attempt": 1})
		emit(map[string]string{"type": "turn_end"})
		emit(map[string]string{"type": "agent_end"})
		settle()
	case "fixture:web-search":
		// web_search runs inside Pi through the search extension, so no bridge call is made.
		ack()
		emit(map[string]any{"type": "message_start", "message": map[string]string{"role": "assistant"}})
		delta("Searching.")
		emit(map[string]any{"type": "message_update", "assistantMessageEvent": map[string]string{"type": "toolcall_start"}})
		emit(map[string]any{"type": "tool_execution_start", "toolCallId": "call-1", "toolName": "web_search"})
		emit(map[string]any{"type": "tool_execution_end", "toolCallId": "call-1", "toolName": "web_search", "isError": false,
			"result": map[string]any{"content": []map[string]string{{"type": "text", "text": "Synthetic result: https://example.com/found?id=1"}}}})
		emit(map[string]any{"type": "message_start", "message": map[string]string{"role": "assistant"}})
		delta("found: synthetic search answer")
		settle()
	case "fixture:stall-once":
		// The first run records the user's message as Pi does and then goes silent; the run after
		// it answers.
		marker := filepath.Join(args["--session-dir"], args["--session-id"]+".stalled")
		if _, statErr := os.Stat(marker); statErr != nil {
			if err := os.WriteFile(marker, nil, 0o600); err != nil {
				return err
			}
			if err := recordExchange(args["--session-dir"], args["--session-id"], directory, prompt.Message, ""); err != nil {
				return err
			}
			ack()
			for {
				time.Sleep(time.Hour)
			}
		}
		if err := recordExchange(args["--session-dir"], args["--session-id"], directory, prompt.Message, "recovered"); err != nil {
			return err
		}
		ack()
		delta("recovered")
		settle()
	case "fixture:stall-after-tool":
		// Counts its starts in a file beside the session so a test can tell a retry happened.
		starts, err := os.OpenFile(filepath.Join(args["--session-dir"], args["--session-id"]+".starts"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
		if err != nil {
			return err
		}
		_, _ = starts.WriteString("x")
		_ = starts.Close()
		ack()
		emit(map[string]any{"type": "message_start", "message": map[string]string{"role": "assistant"}})
		emit(map[string]any{"type": "tool_execution_start", "toolCallId": "call-1", "toolName": "web_search"})
		for {
			time.Sleep(time.Hour)
		}
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
	case "fixture:quota", "fixture:auth", "fixture:missing-key":
		text := quotaText
		switch prompt.Message {
		case "fixture:auth":
			text = authText
		case "fixture:missing-key":
			text = missingKeyText
		}
		ack()
		failMessage(text)
		emit(map[string]string{"type": "agent_end"})
		settle()
	case "fixture:unrelated-error":
		ack()
		failMessage(otherText)
		settle()
	case "fixture:retry-quota", "fixture:retry-auth":
		text := quotaText
		if prompt.Message == "fixture:retry-auth" {
			text = authText
		}
		ack()
		retryStart(text)
		report.AbortReceived = awaitAbort(commands, 5*time.Second)
		return writeReport()
	case "fixture:retry-transient":
		ack()
		retryStart(otherText)
		emit(map[string]any{"type": "auto_retry_end", "success": true, "attempt": 1})
		delta("reco")
		delta("vered")
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
		if err := recordExchange(args["--session-dir"], args["--session-id"], directory, prompt.Message, answer); err != nil {
			return err
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

// recordExchange appends the turn to the session file the way Pi 1.0.0 does: a JSONL file named
// <timestamp>_<session id>.jsonl, resumed only when its header records the same working directory,
// with the prompt and the answer as message entries chained by parentId.
func recordExchange(sessionDir, sessionID, cwd, message, answer string) error {
	matches, err := filepath.Glob(filepath.Join(sessionDir, "*_"+sessionID+".jsonl"))
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	var path, parent string
	var lines []any
	for _, match := range matches {
		data, err := os.ReadFile(match)
		if err != nil {
			return err
		}
		entries := strings.Split(strings.TrimSpace(string(data)), "\n")
		var header struct {
			CWD string `json:"cwd"`
		}
		var last struct {
			ID string `json:"id"`
		}
		if json.Unmarshal([]byte(entries[0]), &header) != nil || header.CWD != cwd {
			continue
		}
		_ = json.Unmarshal([]byte(entries[len(entries)-1]), &last)
		path, parent = match, last.ID
		break
	}
	if path == "" {
		stamp := strings.NewReplacer(":", "-", ".", "-").Replace(now.Format("2006-01-02T15:04:05.000Z"))
		path = filepath.Join(sessionDir, stamp+"_"+sessionID+".jsonl")
		lines = append(lines, map[string]any{"type": "session", "version": 3, "id": sessionID, "timestamp": now.Format(time.RFC3339Nano), "cwd": cwd})
	}
	userID, assistantID := fmt.Sprintf("u%07d", now.UnixNano()%1e7), fmt.Sprintf("a%07d", now.UnixNano()%1e7)
	var parentID any
	if parent != "" {
		parentID = parent
	}
	lines = append(lines,
		map[string]any{"type": "message", "id": userID, "parentId": parentID, "timestamp": now.Format(time.RFC3339Nano),
			"message": map[string]any{"role": "user", "content": message, "timestamp": now.UnixMilli()}})
	if answer == "" {
		return appendLines(path, lines)
	}
	lines = append(lines,
		map[string]any{"type": "message", "id": assistantID, "parentId": userID, "timestamp": now.Format(time.RFC3339Nano),
			"message": map[string]any{"role": "assistant", "content": []map[string]string{{"type": "text", "text": answer}}, "stopReason": "stop", "timestamp": now.UnixMilli()}},
	)
	return appendLines(path, lines)
}

func appendLines(path string, lines []any) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(file)
	for _, line := range lines {
		if err := encoder.Encode(line); err != nil {
			_ = file.Close()
			return err
		}
	}
	return file.Close()
}

// callBridge posts the call the way the real extension does and returns the text the model would
// read; a rejected request is reported as an error result naming the HTTP status.
func callBridge(name, arguments string) (string, bool, error) {
	if arguments == "" {
		arguments = "{}"
	}
	body, err := json.Marshal(BridgeCall{Name: name, Arguments: json.RawMessage(arguments)})
	if err != nil {
		return "", false, err
	}
	request, err := http.NewRequest(http.MethodPost, os.Getenv("HOME_AGENT_TOOL_BRIDGE_URL"), bytes.NewReader(body))
	if err != nil {
		return "", false, err
	}
	request.Header.Set("Authorization", "Bearer "+os.Getenv("HOME_AGENT_TOOL_BRIDGE_TOKEN"))
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", false, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Sprintf("rejected: %d", response.StatusCode), true, nil
	}
	var result BridgeResult
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return "", false, err
	}
	return result.Text, result.IsError, nil
}

func awaitAbort(commands *json.Decoder, timeout time.Duration) bool {
	received := make(chan bool, 1)
	go func() {
		for {
			var command struct {
				Type string `json:"type"`
			}
			if err := commands.Decode(&command); err != nil {
				received <- false
				return
			}
			if command.Type == "abort_retry" {
				received <- true
				return
			}
		}
	}()
	select {
	case value := <-received:
		return value
	case <-time.After(timeout):
		return false
	}
}
