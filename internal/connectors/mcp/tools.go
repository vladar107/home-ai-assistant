package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/vladar107/home-ai-assistant/internal/clip"
	"github.com/vladar107/home-ai-assistant/internal/config"
	"github.com/vladar107/home-ai-assistant/internal/integration"
	"github.com/vladar107/home-ai-assistant/internal/prompt"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const (
	// resultBytes is the cap a result is clipped to; the broker refuses anything longer.
	resultBytes = 16 << 10
	// noteBytes is kept free under resultBytes for the notes on what was clipped or dropped.
	noteBytes = 512
	// maximumSchemaBytes bounds a tool's input schema, which every turn carries in its prompt.
	maximumSchemaBytes  = 8 << 10
	maximumDescription  = 1024
	toolNamePrefix      = "mcp_"
	maximumToolNameSize = 64
)

// toolNamePattern is the broker's.
var toolNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// Texts the model reads when a call fails. They never carry what the server said.
const (
	textNeedsKey       = "This service needs the user's key; they can connect it in /settings."
	textNeedsReconnect = "The service moved since the user connected their key; they can reconnect it in /settings."
	textKeyRefused     = "The service refused the user's key; they can reconnect it in /settings."
	textRefused        = "The service refused the request."
	textTimeout        = "The service did not answer in time."
	textRejected       = "The service rejected the request; check the arguments against the tool's schema."
	textArguments      = "The arguments must be a JSON object."
	textTooLarge       = "The service's answer was too large to read."
	textUnavailable    = "The service is unavailable right now."
	textNoContent      = "The tool returned no content."
	textErrorNoDetail  = "The tool reported an error without details."
)

// knownKinds are the content kinds a dropped-content note may name; anything else is "other", so a
// server cannot put its own words in the note.
var knownKinds = map[string]bool{"image": true, "audio": true, "resource": true, "resource_link": true, "unknown": true}

type mappedTool struct {
	definition tools.Definition
	remote     string
}

// provider offers one catalog server's allowlisted tools.
type provider struct {
	service *Service
	server  config.MCPServer
	sw      integration.Switch
	// remote maps an offered name back to the allowlisted remote name. Two allowlisted names that
	// normalize to one offered name are both left out.
	remote map[string]string
}

var (
	_ tools.Provider       = (*provider)(nil)
	_ integration.Switched = (*provider)(nil)
)

func newProvider(service *Service, server config.MCPServer) *provider {
	counts := make(map[string]int, len(server.Tools))
	for _, tool := range server.Tools {
		if name, ok := offeredName(server.Name, tool.Name); ok {
			counts[name]++
		}
	}
	remote := make(map[string]string, len(server.Tools))
	for _, tool := range server.Tools {
		if name, ok := offeredName(server.Name, tool.Name); ok && counts[name] == 1 {
			remote[name] = tool.Name
		}
	}
	return &provider{
		service: service,
		server:  server,
		sw:      integration.Switch{Integration: IntegrationName(server.Name), Name: server.Title},
		remote:  remote,
	}
}

func (p *provider) Switch() integration.Switch {
	return p.sw
}

// offeredName is mcp_<server>_<tool> with the remote name lowered and every run of other
// characters made one underscore. A name over the broker's length is not shortened, since a
// shortened one could collide.
func offeredName(server, remote string) (string, bool) {
	var builder strings.Builder
	separated := true
	for _, r := range strings.ToLower(remote) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			builder.WriteRune(r)
			separated = false
		} else if !separated {
			builder.WriteByte('_')
			separated = true
		}
	}
	tail := strings.TrimRight(builder.String(), "_")
	if tail == "" {
		return "", false
	}
	name := toolNamePrefix + server + "_" + tail
	return name, len(name) <= maximumToolNameSize && toolNamePattern.MatchString(name)
}

// Contribute offers the server's allowlisted tools the principal may use. It never fails: a
// principal outside the entry's list, a keyed server without a usable key, and a server with no
// list to serve each offer nothing.
func (p *provider) Contribute(ctx context.Context, principalID string) (tools.Contribution, error) {
	s := p.service
	if !p.server.Allows(principalID) {
		return tools.Contribution{}, nil
	}
	key := listKey(principalID, p.server)
	// The generation is read before the key, so a key removed meanwhile cannot open a session.
	gen := s.generation(key)
	cred, state, err := s.credentialFor(ctx, principalID, p.server)
	if err != nil {
		s.logger.WarnContext(ctx, "mcp.connection.unreadable", "principal_id", principalID, "server", p.server.Name)
		return tools.Contribution{}, nil
	}
	if !usable(state) {
		return tools.Contribution{}, nil
	}
	mapped := s.toolList(ctx, principalID, key, p.server, cred, gen)
	if len(mapped) == 0 {
		return tools.Contribution{}, nil
	}
	definitions := make([]tools.Definition, 0, len(mapped))
	for _, tool := range mapped {
		definitions = append(definitions, tool.definition)
	}
	return tools.Contribution{Guidance: p.server.Guidance, Definitions: definitions}, nil
}

func usable(key KeyState) bool {
	return key == KeyNotNeeded || key == KeyConnected || key == KeyRefused
}

// Execute runs an allowlisted tool in the principal's own session. It rechecks the principal list
// and the key itself rather than trusting that Contribute ran, and refuses a tool the cached list
// dropped.
func (p *provider) Execute(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	s := p.service
	remote, allowlisted := p.remote[call.Name]
	if !p.server.Allows(principalID) || !allowlisted || !s.offers(listKey(principalID, p.server), call.Name) {
		return tools.Result{Text: prompt.ToolUnavailable, IsError: true}, nil
	}
	key := sessionKey{server: p.server.Name, principal: principalID}
	gen := s.generation(key)
	cred, state, err := s.credentialFor(ctx, principalID, p.server)
	switch {
	case err != nil:
		return tools.Result{Text: prompt.ToolUnavailableNow, IsError: true}, nil
	case state == KeyMissing:
		return tools.Result{Text: textNeedsKey, IsError: true}, nil
	case state == KeyStale:
		return tools.Result{Text: textNeedsReconnect, IsError: true}, nil
	}
	client, release, err := s.clientFor(key, p.server, cred, gen)
	if err != nil {
		return tools.Result{Text: prompt.ToolUnavailableNow, IsError: true}, nil
	}
	defer release()
	result, err := client.CallTool(ctx, remote, call.Arguments)
	if err != nil {
		if ctx.Err() != nil {
			return tools.Result{}, ctx.Err()
		}
		if errors.Is(err, ErrUnauthorized) {
			s.markProblem(ctx, principalID, p.server)
		}
		s.logger.WarnContext(ctx, "mcp.call.failed", "principal_id", principalID, "server", p.server.Name, "class", ClassOf(err))
		return tools.Result{Text: p.failureText(err), IsError: true}, nil
	}
	if p.server.Auth == config.MCPAuthKey {
		s.clearProblem(principalID, p.server)
	}
	return render(result), nil
}

func (p *provider) failureText(err error) string {
	switch {
	case errors.Is(err, ErrUnauthorized) && p.server.Auth == config.MCPAuthKey:
		return textKeyRefused
	case errors.Is(err, ErrUnauthorized):
		return textRefused
	case errors.Is(err, ErrTimeout):
		return textTimeout
	case errors.Is(err, ErrProtocol):
		return textRejected
	case errors.Is(err, ErrInvalidArguments):
		return textArguments
	case errors.Is(err, ErrTooLarge):
		return textTooLarge
	default:
		return textUnavailable
	}
}

// render keeps a result's text, clipped to resultBytes, and names the kinds of content it dropped.
func render(result Result) tools.Result {
	body := strings.Join(result.Texts(), "\n\n")
	var notes []string
	if len(body) > resultBytes-noteBytes {
		body = clip.Bytes(body, resultBytes-noteBytes)
		notes = append(notes, fmt.Sprintf("[The result was clipped to its first %d bytes.]", len(body)))
	}
	var kinds []string
	seen := map[string]bool{}
	for _, kind := range result.OtherKinds() {
		if !knownKinds[kind] {
			kind = "other"
		}
		if !seen[kind] {
			seen[kind] = true
			kinds = append(kinds, kind)
		}
	}
	if len(kinds) > 0 {
		notes = append(notes, "[Not shown: "+strings.Join(kinds, ", ")+" content.]")
	}
	switch {
	case body == "" && result.IsError && len(notes) == 0:
		body = textErrorNoDetail
	case body == "" && len(notes) == 0:
		body = textNoContent
	}
	text := body
	if len(notes) > 0 {
		text = strings.TrimSpace(body + "\n\n" + strings.Join(notes, "\n"))
	}
	return tools.Result{Text: text, IsError: result.IsError}
}

// mapTools turns the tools a server listed into definitions, in catalog order. Only allowlisted
// tools are kept, and only when their definition would pass the broker; dropped counts the
// allowlisted tools the server listed that were left out.
func mapTools(server config.MCPServer, listed []Tool) ([]mappedTool, int) {
	byRemote := make(map[string]Tool, len(listed))
	repeated := make(map[string]bool)
	for _, tool := range listed {
		if _, seen := byRemote[tool.Name]; seen {
			repeated[tool.Name] = true
		}
		byRemote[tool.Name] = tool
	}
	counts := make(map[string]int, len(server.Tools))
	for _, allowed := range server.Tools {
		if name, ok := offeredName(server.Name, allowed.Name); ok {
			counts[name]++
		}
	}
	var mapped []mappedTool
	dropped := 0
	for _, allowed := range server.Tools {
		tool, found := byRemote[allowed.Name]
		if !found {
			continue
		}
		definition, ok := define(server, allowed, tool)
		if !ok || repeated[allowed.Name] || counts[definition.Name] != 1 {
			dropped++
			continue
		}
		mapped = append(mapped, mappedTool{definition: definition, remote: allowed.Name})
	}
	return mapped, dropped
}

// define builds the definition of one allowlisted tool. The server's title and annotations are
// ignored; its description is used, clipped, only when the catalog pins none.
func define(server config.MCPServer, allowed config.MCPTool, tool Tool) (tools.Definition, bool) {
	name, ok := offeredName(server.Name, allowed.Name)
	if !ok {
		return tools.Definition{}, false
	}
	description := allowed.Description
	if description == "" {
		description = strings.TrimSpace(tool.Description)
	}
	if description == "" {
		return tools.Definition{}, false
	}
	schema, ok := cleanSchema(tool.InputSchema)
	if !ok {
		return tools.Definition{Name: name}, false
	}
	return tools.Definition{
		Name:           name,
		Description:    clip.Runes(server.Title+": "+description, maximumDescription),
		InputSchema:    schema,
		Risk:           tools.RiskRead,
		Timeout:        callTimeout,
		MaxResultBytes: resultBytes,
		ThirdParty:     true,
		Screen:         true,
		Activity:       tools.ActivityServices,
	}, true
}

// cleanSchema accepts a schema that is a JSON object of at most maximumSchemaBytes describing an
// object: a missing type becomes "object", and the top-level $schema is dropped because the
// runtime's validator may not know the draft it names.
func cleanSchema(raw json.RawMessage) (json.RawMessage, bool) {
	if len(raw) == 0 || len(raw) > maximumSchemaBytes {
		return nil, false
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, false
	}
	if kind, typed := object["type"]; typed {
		var name string
		if json.Unmarshal(kind, &name) != nil || name != "object" {
			return nil, false
		}
	} else {
		object["type"] = json.RawMessage(`"object"`)
	}
	if properties, present := object["properties"]; present {
		var fields map[string]json.RawMessage
		if json.Unmarshal(properties, &fields) != nil || fields == nil {
			return nil, false
		}
	}
	delete(object, "$schema")
	encoded, err := json.Marshal(object)
	if err != nil || len(encoded) > maximumSchemaBytes {
		return nil, false
	}
	return encoded, true
}
