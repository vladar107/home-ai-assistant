package collection

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/clip"
	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const (
	toolList   = "collection_list"
	toolQuery  = "collection_query"
	toolCreate = "collection_create"
	toolAlter  = "collection_alter"
	toolAdd    = "collection_add"
	toolUpdate = "collection_update"
	toolRemove = "collection_remove"

	alterAdd    = "add"
	alterRename = "rename"
	alterRemove = "remove"

	// maximumAddRecords is how many records one collection_add takes, so a shopping list fits one
	// call.
	maximumAddRecords = 20
	// queryResultBytes is the result cap of collection_query; renderQuery stops listing records
	// before it.
	queryResultBytes = 64 << 10
	queryTextBytes   = queryResultBytes - 2<<10
	// maximumGroupLabelRunes clips a group's value, which may be a long text.
	maximumGroupLabelRunes = 100
	// actTimeout lets a change made at once run as long as one made after Confirm.
	actTimeout = 45 * time.Second
)

const guidance = "Collections: typed lists the user keeps with you, such as things lent out, gift ideas, warranties, a shopping list, or meter readings, each with typed fields. " +
	"The turn's message names them with their record counts. collection_list shows each one's fields. " +
	"collection_query answers questions from the records in code: filters on fields, a sort, a limit, totals (count, sum, min, max, mean) of every number and money field over all matches, and an optional group_by on one field. " +
	"Answer counts, totals, and lists from its result instead of adding values up yourself, and query again with filters rather than reading every record. " +
	"When the user names a Notion table, use Notion instead; collections are for lists kept here."

func writeGuidance() string {
	return "collection_create proposes a new collection with its fields; it always waits for the user's Confirm, so never claim it exists before. Keep fields few and typed: text, number, money, date, datetime, bool, choice (with options), or person; mark a field required only when every record needs it. " +
		"collection_alter adds, renames, or removes fields; removing one deletes its values and always waits for Confirm. " +
		"collection_add adds records, collection_update changes one by its id from collection_query, and collection_remove deletes one; use a bool field to check items off. " +
		"When the user asked for it in this conversation and the conversation has read no outside content, each is done at once and the user can undo it; tell the user it is done. After outside content it is only proposed with Confirm and Reject buttons, so never claim it is saved. " +
		"Never invent a value the user did not give; leave the field empty or ask. Keep records in collections and not in memory. " +
		"A record's date can be the time of an ordinary one-time reminder when the user asks for one; nothing is scheduled by itself. " +
		fmt.Sprintf("Each user may have %d collections of up to %d fields and %d records each.", MaximumCollectionsPerPrincipal, MaximumFields, MaximumRecords)
}

const fieldSchema = `{"type":"object","properties":{` +
	`"name":{"type":"string","description":"The field's name, such as lent to."},` +
	`"type":{"type":"string","enum":["text","number","money","date","datetime","bool","choice","person"]},` +
	`"required":{"type":"boolean","description":"Every record must have a value."},` +
	`"options":{"type":"array","maxItems":20,"items":{"type":"string"},"description":"A choice field's options."}},` +
	`"required":["name","type"],"additionalProperties":false}`

const valuesSchema = `{"type":"object","description":"Field name to value. text, person, choice, date (YYYY-MM-DD), and datetime (local YYYY-MM-DDTHH:MM) are strings; number is a number; money is {\"amount\":12.5,\"currency\":\"EUR\"}; bool is true or false; null clears a field."}`

var listDefinition = tools.Definition{
	Name:        toolList,
	Description: "List the user's collections with their descriptions, typed fields, and record counts.",
	Risk:        tools.RiskRead,
	Private:     true,
	InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
}

var queryDefinition = tools.Definition{
	Name:           toolQuery,
	Description:    "Read the records of one collection: filter, sort, and limit them, with the totals of every number and money field over all matches and optional groups.",
	Risk:           tools.RiskRead,
	SourceLabel:    tools.ArgumentLabel("collection"),
	Private:        true,
	MaxResultBytes: queryResultBytes,
	InputSchema: json.RawMessage(`{"type":"object","properties":{` +
		`"collection":{"type":"string","description":"The collection's name."},` +
		`"filters":{"type":"array","maxItems":10,"items":{"type":"object","properties":{` +
		`"field":{"type":"string"},` +
		`"op":{"type":"string","enum":["=","!=","<",">","contains","empty","not_empty"]},` +
		`"value":{"description":"Compared as the field's type; a date compares a datetime field by its day; a bare number compares a money amount."}},` +
		`"required":["field","op"],"additionalProperties":false},"description":"All must hold."},` +
		`"sort":{"type":"object","properties":{"field":{"type":"string"},"descending":{"type":"boolean"}},"required":["field"],"additionalProperties":false},` +
		`"limit":{"type":"integer","minimum":0,"maximum":50,"description":"How many records to list, default 50; 0 lists none, for totals or groups only."},` +
		`"group_by":{"type":"string","description":"A field to group the matches by, with a count and totals per group."}},` +
		`"required":["collection"],"additionalProperties":false}`),
}

var writeDefinitions = []tools.Definition{
	{
		Name:        toolAlter,
		Description: "Add, rename, or remove fields of a collection. Adding and renaming are done at once and undoable, or proposed after outside content; removing a field always waits for Confirm.",
		Risk:        tools.RiskWrite,
		SourceLabel: tools.ArgumentLabel("collection"),
		Timeout:     actTimeout,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"collection":{"type":"string","description":"The collection's name."},` +
			`"changes":{"type":"array","minItems":1,"maxItems":20,"items":{"type":"object","properties":{` +
			`"action":{"type":"string","enum":["add","rename","remove"]},` +
			`"field":{"type":"string","description":"The field's name: the new one for add, the current one otherwise."},` +
			`"new_name":{"type":"string","description":"rename: the new name."},` +
			`"type":{"type":"string","enum":["text","number","money","date","datetime","bool","choice","person"],"description":"add: the type."},` +
			`"required":{"type":"boolean","description":"add: every record must have a value; only in a collection without records."},` +
			`"options":{"type":"array","maxItems":20,"items":{"type":"string"},"description":"add: a choice field's options."}},` +
			`"required":["action","field"],"additionalProperties":false}}},` +
			`"required":["collection","changes"],"additionalProperties":false}`),
	},
	{
		Name:        toolAdd,
		Description: "Add records to a collection. Done at once and undoable, or proposed for Confirm after outside content.",
		Risk:        tools.RiskWrite,
		SourceLabel: tools.ArgumentLabel("collection"),
		Timeout:     actTimeout,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"collection":{"type":"string","description":"The collection's name."},` +
			`"records":{"type":"array","minItems":1,"maxItems":20,"items":` + valuesSchema + `}},` +
			`"required":["collection","records"],"additionalProperties":false}`),
	},
	{
		Name:        toolUpdate,
		Description: "Change the values of one record; fields left out keep their values. Done at once and undoable, or proposed for Confirm after outside content.",
		Risk:        tools.RiskWrite,
		SourceLabel: tools.ArgumentLabel("collection", "record_id"),
		Timeout:     actTimeout,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"collection":{"type":"string","description":"The collection's name."},` +
			`"record_id":{"type":"string","description":"The record's id from collection_query."},` +
			`"values":` + valuesSchema + `},` +
			`"required":["collection","record_id","values"],"additionalProperties":false}`),
	},
	{
		Name:        toolRemove,
		Description: "Delete one record. Done at once and undoable, or proposed for Confirm after outside content.",
		Risk:        tools.RiskWrite,
		SourceLabel: tools.ArgumentLabel("collection", "record_id"),
		Timeout:     actTimeout,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"collection":{"type":"string","description":"The collection's name."},` +
			`"record_id":{"type":"string","description":"The record's id from collection_query."}},` +
			`"required":["collection","record_id"],"additionalProperties":false}`),
	},
}

var createDefinition = tools.Definition{
	Name:        toolCreate,
	Description: "Propose a new collection with its typed fields. It always waits for the user's Confirm.",
	Risk:        tools.RiskWrite,
	SourceLabel: tools.ArgumentLabel("name"),
	InputSchema: json.RawMessage(`{"type":"object","properties":{` +
		`"name":{"type":"string","description":"A short name, such as lent items."},` +
		`"description":{"type":"string","description":"What the collection is for, in one line."},` +
		`"fields":{"type":"array","minItems":1,"maxItems":20,"items":` + fieldSchema + `}},` +
		`"required":["name","fields"],"additionalProperties":false}`),
}

var (
	_ tools.Provider    = (*Service)(nil)
	_ approval.Executor = (*Service)(nil)
	_ approval.Undoer   = (*Service)(nil)
)

// Contribute offers collection_create whenever changes can be proposed, and the other tools only
// while the principal keeps a collection, so a user without any pays for one tool alone.
func (s *Service) Contribute(ctx context.Context, principalID string) (tools.Contribution, error) {
	collections, err := s.repository.ListCollections(ctx, principalID)
	if err != nil {
		return tools.Contribution{}, err
	}
	var contribution tools.Contribution
	if len(collections) > 0 {
		contribution.Guidance = guidance
		contribution.Definitions = []tools.Definition{listDefinition, queryDefinition}
	}
	if s.proposer != nil {
		contribution.WriteGuidance = writeGuidance()
		contribution.Definitions = append(contribution.Definitions, createDefinition)
		if len(collections) > 0 {
			contribution.Definitions = append(contribution.Definitions, writeDefinitions...)
		}
	}
	return contribution, nil
}

func (s *Service) Execute(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	switch call.Name {
	case toolList:
		collections, err := s.repository.ListCollections(ctx, principalID)
		if err != nil {
			return tools.Result{}, err
		}
		return tools.Result{Text: renderCollections(collections)}, nil
	case toolQuery:
		return s.query(ctx, principalID, call)
	case toolCreate:
		return s.proposeCreate(ctx, principalID, call)
	case toolAlter:
		return s.proposeAlter(ctx, principalID, call)
	case toolAdd:
		return s.proposeAdd(ctx, principalID, call)
	case toolUpdate:
		return s.proposeUpdate(ctx, principalID, call)
	case toolRemove:
		return s.proposeRemove(ctx, principalID, call)
	default:
		return errorResult("unknown collection tool"), nil
	}
}

type queryArguments struct {
	Collection string `json:"collection"`
	Query
}

type createArguments struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Fields      []Field `json:"fields"`
}

type fieldChange struct {
	Action   string   `json:"action"`
	Field    string   `json:"field"`
	NewName  string   `json:"new_name"`
	Type     string   `json:"type"`
	Required bool     `json:"required"`
	Options  []string `json:"options"`
}

type alterArguments struct {
	Collection string        `json:"collection"`
	Changes    []fieldChange `json:"changes"`
}

type addArguments struct {
	Collection string                       `json:"collection"`
	Records    []map[string]json.RawMessage `json:"records"`
}

type updateArguments struct {
	Collection string                     `json:"collection"`
	RecordID   string                     `json:"record_id"`
	Values     map[string]json.RawMessage `json:"values"`
}

type removeArguments struct {
	Collection string `json:"collection"`
	RecordID   string `json:"record_id"`
}

type confirmedCreate struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Fields      []Field `json:"fields"`
}

// confirmedAlter is the collection's fields as they will be, with its last change as proposed, so
// a collection changed before the press is not overwritten.
type confirmedAlter struct {
	CollectionID string    `json:"collection_id"`
	UpdatedAt    time.Time `json:"updated_at"`
	Fields       []Field   `json:"fields"`
}

type confirmedAdd struct {
	CollectionID string                       `json:"collection_id"`
	Records      []map[string]json.RawMessage `json:"records"`
}

type confirmedUpdate struct {
	CollectionID string                     `json:"collection_id"`
	RecordID     string                     `json:"record_id"`
	UpdatedAt    time.Time                  `json:"updated_at"`
	Values       map[string]json.RawMessage `json:"values"`
}

type confirmedRemove struct {
	CollectionID string `json:"collection_id"`
	RecordID     string `json:"record_id"`
}

// named finds the principal's collection by name; a refusal is text for the model.
func (s *Service) named(ctx context.Context, principalID, name string) (Collection, string, error) {
	collections, err := s.repository.ListCollections(ctx, principalID)
	if err != nil {
		return Collection{}, "", err
	}
	if collection, found := byName(collections, name); found {
		return collection, "", nil
	}
	if len(collections) == 0 {
		return Collection{}, "The user has no collections.", nil
	}
	return Collection{}, fmt.Sprintf("No collection is called %q. The user's collections: %s.", cleanLine(name), listNames(collections)), nil
}

// record finds a record of the collection by its id; a refusal is text for the model.
func (s *Service) record(ctx context.Context, principalID string, collection Collection, recordID string) (Record, string, error) {
	record, found, err := s.repository.LoadRecord(ctx, principalID, strings.TrimSpace(recordID))
	if err != nil {
		return Record{}, "", err
	}
	if !found || record.CollectionID != collection.ID {
		return Record{}, fmt.Sprintf("The collection %s has no record with id %q; find it with collection_query.", collection.Name, strings.TrimSpace(recordID)), nil
	}
	return record, "", nil
}

func (s *Service) query(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	var arguments queryArguments
	if err := tools.DecodeArguments(call.Arguments, &arguments); err != nil {
		return errorResult(err.Error()), nil
	}
	collection, refusal, err := s.named(ctx, principalID, arguments.Collection)
	if err != nil {
		return tools.Result{}, err
	}
	if refusal != "" {
		return errorResult(refusal), nil
	}
	records, err := s.repository.ListRecords(ctx, principalID, collection.ID)
	if err != nil {
		return tools.Result{}, err
	}
	result, err := Run(collection, records, arguments.Query, s.location)
	if err != nil {
		return errorResult(err.Error()), nil
	}
	return tools.Result{Text: s.renderQuery(result)}, nil
}

func (s *Service) proposeCreate(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	if s.proposer == nil {
		return errorResult("Collections cannot be created here."), nil
	}
	var arguments createArguments
	if err := tools.DecodeArguments(call.Arguments, &arguments); err != nil {
		return errorResult(err.Error()), nil
	}
	name, err := normalizeName(arguments.Name)
	if err != nil {
		return errorResult(err.Error()), nil
	}
	description, err := normalizeDescription(arguments.Description)
	if err != nil {
		return errorResult(err.Error()), nil
	}
	for index := range arguments.Fields {
		arguments.Fields[index].Key = ""
	}
	fields, _, err := normalizeFields(arguments.Fields, 1)
	if err != nil {
		return errorResult(err.Error()), nil
	}
	collections, err := s.repository.ListCollections(ctx, principalID)
	if err != nil {
		return tools.Result{}, err
	}
	if _, taken := byName(collections, name); taken {
		return errorResult(fmt.Sprintf("A collection called %q already exists; change its fields with collection_alter or choose another name.", name)), nil
	}
	if len(collections) >= MaximumCollectionsPerPrincipal {
		return errorResult(tooManyCollectionsText()), nil
	}
	canonical, err := json.Marshal(confirmedCreate{Name: name, Description: description, Fields: fields})
	if err != nil {
		return tools.Result{}, err
	}
	language := locale.From(ctx)
	summary := createSummary.Format(ctx, name, describeFieldsIn(language, fields))
	if description != "" {
		summary = createSummaryDescribed.Format(ctx, name, description, describeFieldsIn(language, fields))
	}
	proposal := approval.Proposal{Tool: toolCreate, Arguments: canonical, Summary: summary, Intent: approval.Intent{Verb: "create", TargetKind: "collection", TargetName: name}}
	if _, err := s.proposer.Propose(ctx, principalID, proposal); err != nil {
		if approval.Refused(err) {
			return errorResult(err.Error()), nil
		}
		s.logger.WarnContext(ctx, "collection.proposal.failed", "principal_id", principalID, "tool", call.Name, "error", err)
		return errorResult("the proposal could not be recorded"), nil
	}
	return tools.Result{Text: approval.ProposedText}, nil
}

func (s *Service) proposeAlter(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	if s.proposer == nil {
		return errorResult("Collections cannot be changed here."), nil
	}
	var arguments alterArguments
	if err := tools.DecodeArguments(call.Arguments, &arguments); err != nil {
		return errorResult(err.Error()), nil
	}
	collection, refusal, err := s.named(ctx, principalID, arguments.Collection)
	if err != nil {
		return tools.Result{}, err
	}
	if refusal != "" {
		return errorResult(refusal), nil
	}
	if len(arguments.Changes) == 0 {
		return errorResult("give at least one change"), nil
	}
	fields := slices.Clone(collection.Fields)
	nextNumber := collection.NextKey
	lines := make([]string, 0, len(arguments.Changes))
	removes := false
	for _, change := range arguments.Changes {
		working := Collection{Name: collection.Name, Fields: fields}
		switch strings.ToLower(strings.TrimSpace(change.Action)) {
		case alterAdd:
			if change.NewName != "" {
				return errorResult("adding a field takes no new_name"), nil
			}
			if change.Required && collection.Records > 0 {
				return errorResult(fmt.Sprintf("field %q cannot be required: the collection already has records without it", cleanLine(change.Field))), nil
			}
			added, err := normalizeField(Field{Name: change.Field, Type: change.Type, Required: change.Required, Options: change.Options})
			if err != nil {
				return errorResult(err.Error()), nil
			}
			added.Key = fieldKey(nextNumber)
			nextNumber++
			fields = append(fields, added)
			lines = append(lines, alterAddLine.Format(ctx, describeFieldIn(locale.From(ctx), added)))
		case alterRename:
			field, found := working.field(change.Field)
			if !found {
				return errorResult(working.unknownField(change.Field).Error()), nil
			}
			if change.Type != "" || change.Required || len(change.Options) > 0 {
				return errorResult("renaming a field takes only new_name; a field's type cannot change"), nil
			}
			name, err := plainText(change.NewName, maximumFieldNameRunes, "a field name")
			if err != nil {
				return errorResult(err.Error()), nil
			}
			if name == "" {
				return errorResult("give new_name"), nil
			}
			index := slices.IndexFunc(fields, func(candidate Field) bool { return candidate.Key == field.Key })
			fields[index].Name = name
			lines = append(lines, alterRenameLine.Format(ctx, field.Name, name))
		case alterRemove:
			field, found := working.field(change.Field)
			if !found {
				return errorResult(working.unknownField(change.Field).Error()), nil
			}
			if change.NewName != "" || change.Type != "" || change.Required || len(change.Options) > 0 {
				return errorResult("removing a field takes only its name"), nil
			}
			fields = slices.DeleteFunc(fields, func(candidate Field) bool { return candidate.Key == field.Key })
			lines = append(lines, alterRemoveLine.Format(ctx, field.Name))
			removes = true
		default:
			return errorResult("action must be add, rename, or remove"), nil
		}
	}
	fields, _, err = normalizeFields(fields, collection.NextKey)
	if err != nil {
		return errorResult(err.Error()), nil
	}
	canonical, err := json.Marshal(confirmedAlter{CollectionID: collection.ID, UpdatedAt: collection.UpdatedAt, Fields: fields})
	if err != nil {
		return tools.Result{}, err
	}
	summary := alterSummary.Format(ctx, collection.Name, strings.Join(lines, "\n- "))
	proposal := approval.Proposal{Tool: toolAlter, Arguments: canonical, Summary: summary, Intent: approval.Intent{Verb: "change the fields of", TargetKind: "collection", TargetName: collection.Name}}
	if removes {
		if _, err := s.proposer.Propose(ctx, principalID, proposal); err != nil {
			if approval.Refused(err) {
				return errorResult(err.Error()), nil
			}
			s.logger.WarnContext(ctx, "collection.proposal.failed", "principal_id", principalID, "tool", call.Name, "error", err)
			return errorResult("the proposal could not be recorded"), nil
		}
		return tools.Result{Text: approval.ProposedText}, nil
	}
	return s.act(ctx, principalID, call, proposal)
}

func (s *Service) proposeAdd(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	if s.proposer == nil {
		return errorResult("Records cannot be added here."), nil
	}
	var arguments addArguments
	if err := tools.DecodeArguments(call.Arguments, &arguments); err != nil {
		return errorResult(err.Error()), nil
	}
	collection, refusal, err := s.named(ctx, principalID, arguments.Collection)
	if err != nil {
		return tools.Result{}, err
	}
	if refusal != "" {
		return errorResult(refusal), nil
	}
	if len(arguments.Records) == 0 || len(arguments.Records) > maximumAddRecords {
		return errorResult(fmt.Sprintf("give 1 to %d records", maximumAddRecords)), nil
	}
	if collection.Records+len(arguments.Records) > MaximumRecords {
		return errorResult(tooManyRecordsText(collection)), nil
	}
	records := make([]map[string]json.RawMessage, 0, len(arguments.Records))
	lines := make([]string, 0, len(arguments.Records))
	for index, given := range arguments.Records {
		values, err := applyValues(collection, nil, given, s.location)
		if err != nil {
			if len(arguments.Records) > 1 {
				return errorResult(fmt.Sprintf("record %d: %s", index+1, err)), nil
			}
			return errorResult(err.Error()), nil
		}
		if len(values) == 0 {
			return errorResult("a record needs at least one value"), nil
		}
		records = append(records, values)
		lines = append(lines, "- "+describeValuesIn(locale.From(ctx), collection, values, s.location))
	}
	canonical, err := json.Marshal(confirmedAdd{CollectionID: collection.ID, Records: records})
	if err != nil {
		return tools.Result{}, err
	}
	summary := addSummary.Format(ctx, collection.Name, strings.Join(lines, "\n"))
	return s.act(ctx, principalID, call, approval.Proposal{Tool: toolAdd, Arguments: canonical, Summary: summary, Intent: approval.Intent{Verb: "add records to", TargetKind: "collection", TargetName: collection.Name}})
}

func (s *Service) proposeUpdate(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	if s.proposer == nil {
		return errorResult("Records cannot be changed here."), nil
	}
	var arguments updateArguments
	if err := tools.DecodeArguments(call.Arguments, &arguments); err != nil {
		return errorResult(err.Error()), nil
	}
	collection, refusal, err := s.named(ctx, principalID, arguments.Collection)
	if err != nil {
		return tools.Result{}, err
	}
	if refusal != "" {
		return errorResult(refusal), nil
	}
	record, refusal, err := s.record(ctx, principalID, collection, arguments.RecordID)
	if err != nil {
		return tools.Result{}, err
	}
	if refusal != "" {
		return errorResult(refusal), nil
	}
	values, err := applyValues(collection, record.Values, arguments.Values, s.location)
	if err != nil {
		return errorResult(err.Error()), nil
	}
	if len(values) == 0 {
		return errorResult("a record keeps at least one value; remove it with collection_remove instead"), nil
	}
	if sameValues(values, record.Values) {
		return errorResult("That changes nothing; the record already reads exactly like this."), nil
	}
	canonical, err := json.Marshal(confirmedUpdate{CollectionID: collection.ID, RecordID: record.ID, UpdatedAt: record.UpdatedAt, Values: values})
	if err != nil {
		return tools.Result{}, err
	}
	language := locale.From(ctx)
	summary := updateSummary.Format(ctx, collection.Name, describeValuesIn(language, collection, record.Values, s.location), describeValuesIn(language, collection, values, s.location))
	return s.act(ctx, principalID, call, approval.Proposal{Tool: toolUpdate, Arguments: canonical, Summary: summary, Intent: approval.Intent{Verb: "change a record of", TargetKind: "collection", TargetName: collection.Name}})
}

func (s *Service) proposeRemove(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	if s.proposer == nil {
		return errorResult("Records cannot be removed here."), nil
	}
	var arguments removeArguments
	if err := tools.DecodeArguments(call.Arguments, &arguments); err != nil {
		return errorResult(err.Error()), nil
	}
	collection, refusal, err := s.named(ctx, principalID, arguments.Collection)
	if err != nil {
		return tools.Result{}, err
	}
	if refusal != "" {
		return errorResult(refusal), nil
	}
	record, refusal, err := s.record(ctx, principalID, collection, arguments.RecordID)
	if err != nil {
		return tools.Result{}, err
	}
	if refusal != "" {
		return errorResult(refusal), nil
	}
	canonical, err := json.Marshal(confirmedRemove{CollectionID: collection.ID, RecordID: record.ID})
	if err != nil {
		return tools.Result{}, err
	}
	summary := removeSummary.Format(ctx, collection.Name, describeValuesIn(locale.From(ctx), collection, record.Values, s.location))
	return s.act(ctx, principalID, call, approval.Proposal{Tool: toolRemove, Arguments: canonical, Summary: summary, Intent: approval.Intent{Verb: "remove a record from", TargetKind: "collection", TargetName: collection.Name}})
}

// act makes the change at once in a clean turn and proposes it otherwise.
func (s *Service) act(ctx context.Context, principalID string, call tools.Call, proposal approval.Proposal) (tools.Result, error) {
	text, err := s.proposer.Act(ctx, principalID, proposal)
	if err != nil {
		if approval.Refused(err) {
			return errorResult(err.Error()), nil
		}
		s.logger.WarnContext(ctx, "collection.proposal.failed", "principal_id", principalID, "tool", call.Name, "error", err)
		return errorResult("the change could not be recorded"), nil
	}
	return tools.Result{Text: text}, nil
}

func (s *Service) ActionTools() []string {
	return []string{toolCreate, toolAlter, toolAdd, toolUpdate, toolRemove}
}

// ExecuteApproved makes the confirmed change. Everything is checked again against the collection
// as it is now, and the approval's own ID names what it creates, so a second execution cannot
// create anything twice.
func (s *Service) ExecuteApproved(ctx context.Context, record approval.Record) (approval.Outcome, error) {
	switch record.Tool {
	case toolCreate:
		return s.executeCreate(ctx, record)
	case toolAlter:
		return s.executeAlter(ctx, record)
	case toolAdd:
		return s.executeAdd(ctx, record)
	case toolUpdate:
		return s.executeUpdate(ctx, record)
	case toolRemove:
		return s.executeRemove(ctx, record)
	default:
		return approval.Outcome{}, actionFailed(ctx, "unavailable", execUnavailable)
	}
}

func malformed(ctx context.Context, message locale.Text) error {
	return actionFailed(ctx, "malformed", message)
}

func (s *Service) executeCreate(ctx context.Context, record approval.Record) (approval.Outcome, error) {
	var confirmed confirmedCreate
	if err := json.Unmarshal(record.Arguments, &confirmed); err != nil {
		return approval.Outcome{}, malformed(ctx, execUnreadableCreate)
	}
	name, err := normalizeName(confirmed.Name)
	if err == nil {
		confirmed.Description, err = normalizeDescription(confirmed.Description)
	}
	var fields []Field
	nextNumber := 0
	if err == nil {
		fields, nextNumber, err = normalizeFields(confirmed.Fields, 1)
	}
	if err != nil {
		return approval.Outcome{}, actionFailed(ctx, "invalid", execInvalidCollection, err.Error())
	}
	now := s.stamp()
	created, err := s.repository.CreateCollection(ctx, Collection{
		ID: record.ID, PrincipalID: record.PrincipalID, Name: name, Description: confirmed.Description, Fields: fields, NextKey: nextNumber, CreatedAt: now, UpdatedAt: now,
	}, MaximumCollectionsPerPrincipal)
	switch {
	case errors.Is(err, ErrTooManyCollections):
		return approval.Outcome{}, actionFailed(ctx, "too_many", execTooManyCollections, MaximumCollectionsPerPrincipal)
	case errors.Is(err, ErrNameTaken):
		return approval.Outcome{}, actionFailed(ctx, "name_taken", execNameTaken, name)
	case err != nil:
		return approval.Outcome{}, err
	}
	if created {
		s.logger.InfoContext(ctx, "collection.created", "principal_id", record.PrincipalID, "collection_id", record.ID, "fields", len(fields))
	}
	return doneOutcome(ctx, execCreated), nil
}

func (s *Service) executeAlter(ctx context.Context, record approval.Record) (approval.Outcome, error) {
	var confirmed confirmedAlter
	if err := json.Unmarshal(record.Arguments, &confirmed); err != nil {
		return approval.Outcome{}, malformed(ctx, execUnreadableChange)
	}
	current, found, err := s.repository.LoadCollection(ctx, record.PrincipalID, confirmed.CollectionID)
	if err != nil {
		return approval.Outcome{}, err
	}
	if !found {
		return approval.Outcome{}, actionFailed(ctx, "not_found", execCollectionGoneChange)
	}
	stale := actionFailed(ctx, "stale", execStaleCollection)
	if !current.UpdatedAt.Equal(confirmed.UpdatedAt) {
		return approval.Outcome{}, stale
	}
	fields, nextNumber, err := normalizeFields(confirmed.Fields, current.NextKey)
	if err == nil {
		err = checkFieldChanges(current, fields)
	}
	if err != nil {
		return approval.Outcome{}, actionFailed(ctx, "invalid", execInvalidFields, err.Error())
	}
	next := current
	next.Fields, next.NextKey, next.UpdatedAt = fields, nextNumber, s.stamp()
	updated, err := s.repository.UpdateCollectionFields(ctx, next, current.UpdatedAt, removedKeys(current.Fields, fields), false)
	if err != nil {
		return approval.Outcome{}, err
	}
	if !updated {
		return approval.Outcome{}, stale
	}
	s.logger.InfoContext(ctx, "collection.altered", "principal_id", record.PrincipalID, "collection_id", current.ID, "fields", len(fields))
	undo, err := json.Marshal(alterUndo{CollectionID: current.ID, UpdatedAt: next.UpdatedAt, Before: current.Fields})
	if err != nil {
		return approval.Outcome{}, err
	}
	return withUndo(doneOutcome(ctx, execChanged), undo), nil
}

// checkFieldChanges refuses fields that could not come from adding, renaming, and removing fields
// of current as it is now: a kept field that changed its type or became required, a new field
// under a key given out before, or a new required field in a collection that has records.
func checkFieldChanges(current Collection, fields []Field) error {
	for _, field := range fields {
		index := slices.IndexFunc(current.Fields, func(existing Field) bool { return existing.Key == field.Key })
		switch {
		case index >= 0 && (current.Fields[index].Type != field.Type || field.Required && !current.Fields[index].Required):
			return fmt.Errorf("field %q cannot change its type or become required", field.Name)
		case index < 0 && keyNumber(field.Key) < current.NextKey:
			return fmt.Errorf("field %q takes a key given out before", field.Name)
		case index < 0 && field.Required && current.Records > 0:
			return fmt.Errorf("field %q cannot be required: the collection already has records without it", field.Name)
		}
	}
	return nil
}

// removedKeys are the keys of before's fields that after no longer has.
func removedKeys(before, after []Field) []string {
	var keys []string
	for _, field := range before {
		if !slices.ContainsFunc(after, func(kept Field) bool { return kept.Key == field.Key }) {
			keys = append(keys, field.Key)
		}
	}
	return keys
}

// recordID names the index-th record an approval adds, the same on every execution.
func recordID(approvalID string, index int) string {
	digest := sha256.Sum256([]byte(approvalID + ":" + strconv.Itoa(index)))
	return hex.EncodeToString(digest[:8])
}

func (s *Service) executeAdd(ctx context.Context, record approval.Record) (approval.Outcome, error) {
	var confirmed confirmedAdd
	if err := json.Unmarshal(record.Arguments, &confirmed); err != nil || len(confirmed.Records) == 0 {
		return approval.Outcome{}, malformed(ctx, execUnreadableAdd)
	}
	collection, found, err := s.repository.LoadCollection(ctx, record.PrincipalID, confirmed.CollectionID)
	if err != nil {
		return approval.Outcome{}, err
	}
	if !found {
		return approval.Outcome{}, actionFailed(ctx, "not_found", execCollectionGoneAdd)
	}
	now := s.stamp()
	records := make([]Record, 0, len(confirmed.Records))
	for index, values := range confirmed.Records {
		if err := checkValues(collection, values, s.location); err != nil {
			return approval.Outcome{}, actionFailed(ctx, "invalid", execCollectionChangedAdd, err.Error())
		}
		// Records are listed by when they were made, so each of a batch is a millisecond after the
		// one before and the batch keeps the order it was given in.
		at := now.Add(time.Duration(index) * time.Millisecond)
		records = append(records, Record{ID: recordID(record.ID, index), PrincipalID: record.PrincipalID, CollectionID: collection.ID, Values: values, CreatedAt: at, UpdatedAt: at})
	}
	created, err := s.repository.CreateRecords(ctx, records, MaximumRecords)
	switch {
	case errors.Is(err, ErrTooManyRecords):
		return approval.Outcome{}, actionFailed(ctx, "too_many", execTooManyRecords, collection.Name, MaximumRecords)
	case errors.Is(err, ErrCollectionGone):
		return approval.Outcome{}, actionFailed(ctx, "not_found", execCollectionGoneAdd)
	case err != nil:
		return approval.Outcome{}, err
	}
	if created > 0 {
		s.logger.InfoContext(ctx, "collection.records.added", "principal_id", record.PrincipalID, "collection_id", collection.ID, "records", created)
	}
	undo := addUndo{Records: make(map[string]time.Time, len(records))}
	for _, added := range records {
		stored, found, err := s.repository.LoadRecord(ctx, record.PrincipalID, added.ID)
		if err != nil {
			return approval.Outcome{}, err
		}
		if found {
			undo.Records[added.ID] = stored.UpdatedAt
		}
	}
	payload, err := json.Marshal(undo)
	if err != nil {
		return approval.Outcome{}, err
	}
	return withUndo(doneCount(ctx, execAdded, len(records)), payload), nil
}

func (s *Service) executeUpdate(ctx context.Context, record approval.Record) (approval.Outcome, error) {
	var confirmed confirmedUpdate
	if err := json.Unmarshal(record.Arguments, &confirmed); err != nil {
		return approval.Outcome{}, malformed(ctx, execUnreadableChange)
	}
	current, found, err := s.repository.LoadRecord(ctx, record.PrincipalID, confirmed.RecordID)
	if err != nil {
		return approval.Outcome{}, err
	}
	if !found || current.CollectionID != confirmed.CollectionID {
		return approval.Outcome{}, actionFailed(ctx, "not_found", execRecordGoneChange)
	}
	stale := actionFailed(ctx, "stale", execStaleRecord)
	if !current.UpdatedAt.Equal(confirmed.UpdatedAt) {
		return approval.Outcome{}, stale
	}
	collection, found, err := s.repository.LoadCollection(ctx, record.PrincipalID, confirmed.CollectionID)
	if err != nil {
		return approval.Outcome{}, err
	}
	if !found {
		return approval.Outcome{}, actionFailed(ctx, "not_found", execCollectionGoneChange)
	}
	if err := checkValues(collection, confirmed.Values, s.location); err != nil {
		return approval.Outcome{}, actionFailed(ctx, "invalid", execCollectionChangedChange, err.Error())
	}
	next := current
	next.Values, next.UpdatedAt = confirmed.Values, s.stamp()
	updated, err := s.repository.UpdateRecord(ctx, next, current.UpdatedAt)
	if err != nil {
		return approval.Outcome{}, err
	}
	if !updated {
		return approval.Outcome{}, stale
	}
	s.logger.InfoContext(ctx, "collection.record.updated", "principal_id", record.PrincipalID, "collection_id", collection.ID, "record_id", current.ID)
	undo, err := json.Marshal(updateUndo{RecordID: current.ID, UpdatedAt: next.UpdatedAt, Before: current.Values})
	if err != nil {
		return approval.Outcome{}, err
	}
	return withUndo(doneOutcome(ctx, execChanged), undo), nil
}

func (s *Service) executeRemove(ctx context.Context, record approval.Record) (approval.Outcome, error) {
	var confirmed confirmedRemove
	if err := json.Unmarshal(record.Arguments, &confirmed); err != nil {
		return approval.Outcome{}, malformed(ctx, execUnreadableDelete)
	}
	// The whole record is read before it goes, so an Undo can put it back.
	current, found, err := s.repository.LoadRecord(ctx, record.PrincipalID, confirmed.RecordID)
	if err != nil {
		return approval.Outcome{}, err
	}
	deleted := false
	if found && current.CollectionID == confirmed.CollectionID {
		if deleted, err = s.repository.DeleteRecord(ctx, record.PrincipalID, current.ID); err != nil {
			return approval.Outcome{}, err
		}
	}
	if !deleted {
		return approval.Outcome{}, actionFailed(ctx, "not_found", execRecordGone)
	}
	s.logger.InfoContext(ctx, "collection.record.deleted", "principal_id", record.PrincipalID, "collection_id", current.CollectionID, "record_id", current.ID)
	undo, err := json.Marshal(removeUndo{Removed: &current})
	if err != nil {
		return approval.Outcome{}, err
	}
	return withUndo(doneOutcome(ctx, execDeleted), undo), nil
}

func tooManyCollectionsText() string {
	return fmt.Sprintf("You already have %d collections, which is the most allowed; delete one in /collections first.", MaximumCollectionsPerPrincipal)
}

func tooManyRecordsText(collection Collection) string {
	return fmt.Sprintf("The collection %s would hold more than %d records, which is the most allowed; remove some first.", collection.Name, MaximumRecords)
}

var (
	execUnavailable             = locale.T("That action is no longer available.", "Это действие больше недоступно.")
	execUnreadableCreate        = locale.T("The confirmed change could not be read; nothing was created.", "Не удалось прочитать подтверждённое изменение; ничего не создано.")
	execUnreadableChange        = locale.T("The confirmed change could not be read; nothing was changed.", "Не удалось прочитать подтверждённое изменение; ничего не изменено.")
	execUnreadableAdd           = locale.T("The confirmed change could not be read; nothing was added.", "Не удалось прочитать подтверждённое изменение; ничего не добавлено.")
	execUnreadableDelete        = locale.T("The confirmed change could not be read; nothing was deleted.", "Не удалось прочитать подтверждённое изменение; ничего не удалено.")
	execInvalidCollection       = locale.T("That collection is not valid (%s); nothing was created.", "Эта коллекция не подходит (%s); ничего не создано.")
	execTooManyCollections      = locale.T("You already have %d collections, which is the most allowed; delete one in /collections first. Nothing was created.", "У тебя уже %d коллекций, а это предел; сначала удали одну в /collections. Ничего не создано.")
	execNameTaken               = locale.T("Another collection is already called %s; nothing was created.", "Коллекция с названием %s уже есть; ничего не создано.")
	execCollectionGoneChange    = locale.T("That collection no longer exists; nothing was changed.", "Такой коллекции больше нет; ничего не изменено.")
	execStaleCollection         = locale.T("That collection changed after this was proposed; nothing was changed. Ask again to change it.", "Коллекция изменилась после предложения; ничего не изменено. Попроси изменить её ещё раз.")
	execInvalidFields           = locale.T("Those fields are not valid (%s); nothing was changed.", "Эти поля не подходят (%s); ничего не изменено.")
	execCollectionGoneAdd       = locale.T("That collection no longer exists; nothing was added.", "Такой коллекции больше нет; ничего не добавлено.")
	execCollectionChangedAdd    = locale.T("The collection changed since this was proposed (%s); nothing was added.", "Коллекция изменилась после предложения (%s); ничего не добавлено.")
	execTooManyRecords          = locale.T("The collection %s would hold more than %d records, which is the most allowed; remove some first. Nothing was added.", "В коллекции %s было бы больше %d записей, а это предел; сначала удали несколько. Ничего не добавлено.")
	execRecordGoneChange        = locale.T("That record no longer exists; nothing was changed.", "Такой записи больше нет; ничего не изменено.")
	execStaleRecord             = locale.T("That record changed after this was proposed; nothing was changed. Ask again to change it.", "Запись изменилась после предложения; ничего не изменено. Попроси изменить её ещё раз.")
	execCollectionChangedChange = locale.T("The collection changed since this was proposed (%s); nothing was changed.", "Коллекция изменилась после предложения (%s); ничего не изменено.")
	execRecordGone              = locale.T("That record no longer exists.", "Такой записи больше нет.")
	execCreated                 = locale.T("Created.", "Создано.")
	execChanged                 = locale.T("Changed.", "Изменено.")
	execDeleted                 = locale.T("Deleted.", "Удалено.")
	execAdded                   = locale.N("Added %d record.", "Added %d records.", "Добавлена %d запись.", "Добавлены %d записи.", "Добавлено %d записей.")
	createSummary               = locale.T("Create the collection %s\nFields:\n%s", "Создать коллекцию %s\nПоля:\n%s")
	createSummaryDescribed      = locale.T("Create the collection %s: %s\nFields:\n%s", "Создать коллекцию %s: %s\nПоля:\n%s")
	alterSummary                = locale.T("Change the fields of the collection %s:\n- %s", "Изменить поля коллекции %s:\n- %s")
	alterAddLine                = locale.T("add %s", "добавить поле %s")
	alterRenameLine             = locale.T("rename %s → %s", "переименовать %s → %s")
	alterRemoveLine             = locale.T("remove %s and its values", "удалить %s вместе со значениями")
	addSummary                  = locale.T("Add to %s:\n%s", "Добавить в %s:\n%s")
	updateSummary               = locale.T("Change a record of %s:\n%s\n→ %s", "Изменить запись в %s:\n%s\n→ %s")
	removeSummary               = locale.T("Remove from %s: %s", "Удалить из %s: %s")
	requiredWord                = locale.T("required", "обязательное")
	noValuesWord                = locale.T("(no values)", "(нет значений)")
	yesWord                     = locale.T("yes", "да")
	noWord                      = locale.T("no", "нет")
	typeWords                   = map[string]locale.Text{
		TypeText:     locale.T("text", "текст"),
		TypeNumber:   locale.T("number", "число"),
		TypeMoney:    locale.T("money", "сумма"),
		TypeDate:     locale.T("date", "дата"),
		TypeDateTime: locale.T("datetime", "дата и время"),
		TypeBool:     locale.T("bool", "да/нет"),
		TypeChoice:   locale.T("choice", "выбор"),
		TypePerson:   locale.T("person", "человек"),
	}
)

// actionFailed keeps the English sentence for the record and the model, and the user's language
// for the user.
func actionFailed(ctx context.Context, reason string, message locale.Text, arguments ...any) *approval.ActionError {
	return &approval.ActionError{Reason: reason, Text: message.FormatIn(locale.English, arguments...), UserText: message.Format(ctx, arguments...)}
}

func doneOutcome(ctx context.Context, message locale.Text, arguments ...any) approval.Outcome {
	return approval.Outcome{Text: message.FormatIn(locale.English, arguments...), UserText: message.Format(ctx, arguments...)}
}

func doneCount(ctx context.Context, message locale.Count, n int) approval.Outcome {
	return approval.Outcome{Text: message.FormatIn(locale.English, n), UserText: message.Format(ctx, n)}
}

func withUndo(outcome approval.Outcome, undo json.RawMessage) approval.Outcome {
	outcome.Undo = undo
	return outcome
}

// describeFieldIn is Field.Describe in the user's language; the model keeps reading Describe.
func describeFieldIn(language locale.Language, f Field) string {
	if language != locale.Russian {
		return f.Describe()
	}
	word, known := typeWords[f.Type]
	if !known {
		return f.Describe()
	}
	text := f.Name + ": " + word.In(language)
	if f.Type == TypeChoice {
		text += " (" + strings.Join(f.Options, ", ") + ")"
	}
	if f.Required {
		text += ", " + requiredWord.In(language)
	}
	return text
}

func describeFieldsIn(language locale.Language, fields []Field) string {
	lines := make([]string, 0, len(fields))
	for _, field := range fields {
		lines = append(lines, "- "+describeFieldIn(language, field))
	}
	return strings.Join(lines, "\n")
}

// describeValuesIn is describeValues in the user's language.
func describeValuesIn(language locale.Language, c Collection, values map[string]json.RawMessage, location *time.Location) string {
	if language != locale.Russian {
		return describeValues(c, values, location)
	}
	parts := make([]string, 0, len(c.Fields))
	for _, field := range c.Fields {
		value, ok := decodeValue(field, values[field.Key])
		if !ok {
			continue
		}
		shown := display(field, value, location)
		if field.Type == TypeBool {
			shown = noWord.In(language)
			if value.Bool {
				shown = yesWord.In(language)
			}
		}
		parts = append(parts, field.Name+": "+shown)
	}
	if len(parts) == 0 {
		return noValuesWord.In(language)
	}
	return strings.Join(parts, "; ")
}

func describeFields(fields []Field) string {
	lines := make([]string, 0, len(fields))
	for _, field := range fields {
		lines = append(lines, "- "+field.Describe())
	}
	return strings.Join(lines, "\n")
}

// renderCollections is the principal's collections as the model reads them.
func renderCollections(collections []Collection) string {
	if len(collections) == 0 {
		return "The user has no collections."
	}
	rendered := make([]string, 0, len(collections))
	for _, collection := range collections {
		heading := collection.Name + " (" + countText(collection.Records) + ")"
		if collection.Description != "" {
			heading += ": " + collection.Description
		}
		rendered = append(rendered, heading+"\n"+describeFields(collection.Fields))
	}
	return strings.Join(rendered, "\n\n")
}

// renderQuery is a query's result as the model reads it: the count, the totals, the groups, and
// the records with their ids. Each part stops where the next line would pass the result cap, saying
// how much it left out, so the broker never has to cut the result.
func (s *Service) renderQuery(result Result) string {
	var builder strings.Builder
	fits := func(text string) bool { return builder.Len()+len(text) <= queryTextBytes }
	collection := result.Collection
	fmt.Fprintf(&builder, "%s: %d of %d records match.\n", collection.Name, result.Matches, collection.Records)
	totalLines := func(prefix string, totals []Total) []string {
		lines := make([]string, 0, len(totals))
		for _, total := range totals {
			format := formatNumber
			if total.Field.Type == TypeMoney {
				format = func(value float64) string { return formatMoney(value, total.Currency) }
			}
			lines = append(lines, fmt.Sprintf("%s%s: %d with a value, sum %s, min %s, max %s, mean %s\n", prefix, total.Field.Name, total.Count,
				format(total.Sum), format(total.Min), format(total.Max), format(total.Mean())))
		}
		return lines
	}
	if len(result.Totals) > 0 {
		builder.WriteString("Totals over all matches:\n")
		lines := totalLines("- ", result.Totals)
		for index, line := range lines {
			if !fits(line) {
				fmt.Fprintf(&builder, "- and %d more totals; filter the records to see them.\n", len(lines)-index)
				break
			}
			builder.WriteString(line)
		}
	}
	if result.Grouped != nil {
		fmt.Fprintf(&builder, "Groups by %s:\n", result.Grouped.Name)
		more := result.MoreGroups
		for index, group := range result.Groups {
			block := "- " + clip.Runes(group.Label, maximumGroupLabelRunes) + ": " + countText(group.Count) + "\n" + strings.Join(totalLines("  ", group.Totals), "")
			if !fits(block) {
				more += len(result.Groups) - index
				break
			}
			builder.WriteString(block)
		}
		if more > 0 {
			fmt.Fprintf(&builder, "- and %d more groups\n", more)
		}
	}
	if len(result.Records) == 0 {
		return strings.TrimRight(builder.String(), "\n")
	}
	builder.WriteString("Records, each with its id:\n")
	listed := 0
	for _, record := range result.Records {
		line := "- [" + record.ID + "] " + describeValues(collection, record.Values, s.location) + "\n"
		if !fits(line) {
			builder.WriteString("- the rest do not fit; narrow the query or lower the limit.\n")
			break
		}
		builder.WriteString(line)
		listed++
	}
	if listed < result.Matches {
		fmt.Fprintf(&builder, "Listed %d of %d matches.", listed, result.Matches)
	}
	return strings.TrimRight(builder.String(), "\n")
}

func errorResult(text string) tools.Result {
	return tools.Result{Text: text, IsError: true}
}
