package telegram

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/collection"
	"github.com/vladar107/home-ai-assistant/internal/locale/localetest"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const lentItems = `{"name":"Lent items","description":"things I lend out","fields":[` +
	`{"name":"item","type":"text","required":true},{"name":"lent to","type":"person"},{"name":"lent on","type":"date"}]}`

// A collection is created only on Confirm. A record added in the user's own turn is done at once
// with an Undo that removes it, while one added after outside content is only proposed. The turn
// names the collection with its record count, and another user sees none of it.
func TestCollectionIsCreatedOnConfirmAndRecordsActWithUndo(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{providers: []tools.Provider{outsideProvider{}}})
	h.provision("alice", "account-alice")
	h.provision("bob", "account-bob")
	h.start()

	proposal := h.proposeTurn(runtime, 1, "collection_create", lentItems)
	if !strings.Contains(proposal.text, "Create the collection Lent items: things I lend out\nFields:\n- item: text, required\n- lent to: person\n- lent on: date") {
		t.Fatalf("proposal = %q", proposal.text)
	}
	if count := h.countRows(`SELECT count(*) FROM collections`); count != 0 {
		t.Fatalf("a proposal created %d collections", count)
	}
	accept, _ := approvalButtons(t, proposal.keyboard)
	h.push(callbackUpdate(2, 101, proposal.messageID, accept))
	h.wantAnswer(2)
	h.nextEdit()
	h.continueTurn(runtime, 2)
	if count := h.countRows(`SELECT count(*) FROM collections WHERE principal_id = 'alice' AND name = 'Lent items'`); count != 1 {
		t.Fatalf("collections after Confirm = %d", count)
	}

	added := h.ownDataTurn(runtime, 3, false, "collection_add", `{"collection":"lent items","records":[{"item":"drill","lent to":"Sam","lent on":"2026-10-01"},{"item":"ladder","lent to":"Kim"}]}`)
	id := h.wantDirect(added, "collection_add")
	if count := h.countRows(`SELECT count(*) FROM collection_records WHERE principal_id = 'alice'`); count != 2 {
		t.Fatalf("records after the add = %d", count)
	}
	request, results := h.memoryTurn(runtime, 4, 101, "ok", memoryCall("collection_query", `{"collection":"Lent items","filters":[{"field":"lent to","op":"=","value":"sam"}]}`))
	if !strings.Contains(request.Message, `The user's collections, read with collection_query: "Lent items" (2 records).`) {
		t.Fatalf("turn message = %q", request.Message)
	}
	if len(results) != 1 || results[0].IsError || !strings.Contains(results[0].Text, "1 of 2 records match.") || !strings.Contains(results[0].Text, "item: drill; lent to: Sam; lent on: 2026-10-01") {
		t.Fatalf("query = %+v", results)
	}
	if strings.Contains(request.Tools.Guidance, "lent to") {
		t.Fatalf("the prompt carries the collection's fields: %q", request.Tools.Guidance)
	}

	h.undoDirect(id)
	if count := h.countRows(`SELECT count(*) FROM collection_records`); count != 0 {
		t.Fatalf("records after Undo = %d", count)
	}

	exposed := h.ownDataTurn(runtime, 5, true, "collection_add", `{"collection":"Lent items","records":[{"item":"tent"}]}`)
	h.wantProposedOnly(exposed, "collection_add")
	if count := h.countRows(`SELECT count(*) FROM collection_records`); count != 0 {
		t.Fatalf("an add after outside content stored %d records", count)
	}

	invalid := h.ownDataTurn(runtime, 6, false, "collection_add", `{"collection":"Lent items","records":[{"lent to":"Lee"}]}`)
	if !invalid.IsError || !strings.Contains(invalid.Text, `field "item" is required`) {
		t.Fatalf("an invalid record = %+v", invalid)
	}

	request, _ = h.memoryTurn(runtime, 7, 202, "ok")
	names := toolNames(request.Tools)
	if strings.Contains(request.Message, "collections") || !slices.Contains(names, "collection_create") || slices.Contains(names, "collection_query") {
		t.Fatalf("bob's turn = %q with tools %v", request.Message, names)
	}
}

func (h *conversationHarness) seedCollection(principalID, id, name string, records int) []string {
	h.t.Helper()
	return h.seedCollectionOf(collection.Collection{ID: id, PrincipalID: principalID, Name: name}, records, func(index int) string { return fmt.Sprintf("title %d", index+1) })
}

// seedCollectionOf stores the collection with one text field, title, and records with the titles
// given.
func (h *conversationHarness) seedCollectionOf(c collection.Collection, records int, title func(int) string) []string {
	h.t.Helper()
	now := time.Now().UTC().Truncate(time.Millisecond)
	c.Fields, c.NextKey, c.CreatedAt, c.UpdatedAt = []collection.Field{{Key: "f1", Name: "title", Type: collection.TypeText}}, 2, now, now
	if _, err := h.store.CreateCollection(h.ctx, c, collection.MaximumCollectionsPerPrincipal); err != nil {
		h.t.Fatal(err)
	}
	principalID, id := c.PrincipalID, c.ID
	var ids []string
	var list []collection.Record
	for index := range records {
		at := now.Add(time.Duration(index) * time.Millisecond)
		recordID := fmt.Sprintf("%s%02d", id[:14], index+1)
		ids = append(ids, recordID)
		value, err := json.Marshal(title(index))
		if err != nil {
			h.t.Fatal(err)
		}
		list = append(list, collection.Record{ID: recordID, PrincipalID: principalID, CollectionID: id, Values: map[string]json.RawMessage{"f1": value}, CreatedAt: at, UpdatedAt: at})
	}
	if _, err := h.store.CreateRecords(h.ctx, list, collection.MaximumRecords); err != nil {
		h.t.Fatal(err)
	}
	return ids
}

func TestCollectionsCommandPagesAndDeletes(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	h := newMemoryHarness(t, runtime)

	h.push(textUpdate(1, 101, "/collections"))
	if empty := h.nextMessage(); empty.text != localetest.English(NoCollectionsReply) || len(empty.keyboard) != 0 {
		t.Fatalf("empty /collections = %+v", empty)
	}

	const books, others = "aaaaaaaaaaaaaa00", "bbbbbbbbbbbbbb00"
	records := h.seedCollection("alice", books, "Books", 10)
	bobs := h.seedCollection("bob", others, "Secrets", 1)

	h.push(textUpdate(2, 101, "/collections"))
	list := h.nextMessage()
	if list.text != "Collections\n1. Books: 10 records" || !slices.Equal(buttonData(list.keyboard), []string{"col:open:" + books + ":1"}) {
		t.Fatalf("/collections = %+v", list)
	}

	press := func(updateID int64, data string) editedMessage {
		t.Helper()
		h.push(callbackUpdate(updateID, 101, list.messageID, data))
		h.wantAnswer(updateID)
		return h.nextEdit()
	}
	page := press(3, "col:open:"+books+":1")
	if !strings.HasPrefix(page.text, "Books (page 1 of 2)\n\n1. title: title 1\n") || !strings.Contains(page.text, "8. title: title 8") || strings.Contains(page.text, "title 9") {
		t.Fatalf("page 1 = %q", page.text)
	}
	data := buttonData(page.keyboard)
	for _, want := range []string{"col:rdel:" + records[0] + ":1", "col:rdel:" + records[7] + ":1", "col:open:" + books + ":2", "col:ask:" + books + ":1", "col:list:1"} {
		if !slices.Contains(data, want) {
			t.Fatalf("buttons %v lack %s", data, want)
		}
	}
	if second := press(4, "col:open:"+books+":2"); !strings.Contains(second.text, "page 2 of 2") || !strings.Contains(second.text, "10. title: title 10") || strings.Contains(second.text, "title 8") {
		t.Fatalf("page 2 = %q", second.text)
	}

	if deleted := press(5, "col:rdel:"+records[1]+":1"); strings.Contains(deleted.text, "title 2\n") || !strings.Contains(deleted.text, "2. title: title 3") || !strings.Contains(deleted.text, "page 1 of 2") {
		t.Fatalf("after a delete = %q", deleted.text)
	}
	if count := h.countRows(`SELECT count(*) FROM collection_records WHERE principal_id = 'alice'`); count != 9 {
		t.Fatalf("alice's records after a delete = %d", count)
	}

	press(6, "col:rdel:"+bobs[0]+":1")
	h.wantMessage(101, localetest.English(CollectionRecordGoneLine))
	if forged := press(7, "col:open:"+others+":1"); forged.text != "Collections\n1. Books: 9 records" {
		t.Fatalf("alice opening bob's collection = %q", forged.text)
	}
	h.wantMessage(101, localetest.English(CollectionGoneReply))
	press(8, "col:cdel:"+others+":1")
	h.wantMessage(101, localetest.English(CollectionGoneReply))
	if count := h.countRows(`SELECT count(*) FROM collection_records WHERE principal_id = 'bob'`); count != 1 {
		t.Fatalf("bob's records after alice's forged presses = %d", count)
	}

	if parsed, ok := parseCollectionCallback("col:rdel:" + records[0] + ":1:extra"); ok {
		t.Fatalf("a malformed callback parsed as %+v", parsed)
	}
	if ask := press(9, "col:ask:"+books+":1"); ask.text != "Delete the collection Books and its 9 records? This cannot be undone." {
		t.Fatalf("confirmation = %q", ask.text)
	}
	if back := press(10, "col:open:"+books+":1"); !strings.HasPrefix(back.text, "Books (page 1 of 2)") {
		t.Fatalf("after Cancel = %q", back.text)
	}
	press(11, "col:ask:"+books+":1")
	if gone := press(12, "col:cdel:"+books+":1"); gone.text != localetest.English(NoCollectionsReply) {
		t.Fatalf("after deleting the collection = %q", gone.text)
	}
	h.wantMessage(101, localetest.English(CollectionDeletedNote))
	if count := h.countRows(`SELECT count(*) FROM collection_records WHERE principal_id = 'alice'`); count != 0 {
		t.Fatalf("alice's records after deleting the collection = %d", count)
	}
}

// A page of the longest records, all of characters Telegram counts twice, still fits one message.
func TestCollectionPageFitsOneTelegramMessage(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	h := newMemoryHarness(t, runtime)
	const id = "cccccccccccccc00"
	wide := func(n int) string { return strings.Repeat("🎁", n) }
	h.seedCollectionOf(collection.Collection{ID: id, PrincipalID: "alice", Name: wide(60), Description: wide(300)}, 9, func(int) string { return wide(500) })

	h.push(textUpdate(1, 101, "/collections"))
	list := h.nextMessage()
	h.push(callbackUpdate(2, 101, list.messageID, "col:open:"+id+":1"))
	h.wantAnswer(2)
	page := h.nextEdit()
	if units := utf16Length(page.text); units > 4096 || !strings.Contains(page.text, "8. title: 🎁") || !strings.Contains(page.text, "…") {
		t.Fatalf("a page of %d UTF-16 units: %.200s", units, page.text)
	}
}
