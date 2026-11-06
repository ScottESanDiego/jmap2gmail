package fastmail

import (
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"testing"
)

func TestConflictEmailsBatchOnlyHumanMetadata(t *testing.T) {
	calls := 0
	client := testClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		var req struct {
			Calls [][]json.RawMessage `json:"methodCalls"`
		}
		if err := json.Unmarshal(data, &req); err != nil {
			t.Fatal(err)
		}
		var args struct{ IDs, Properties []string }
		if len(req.Calls) != 1 {
			t.Fatal(string(data))
		}
		if err := json.Unmarshal(req.Calls[0][1], &args); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(args.IDs, []string{"e1", "missing"}) || !reflect.DeepEqual(args.Properties, []string{"id", "subject", "from", "to", "cc", "sentAt", "receivedAt", "mailboxIds"}) {
			t.Fatalf("unsafe diagnostic request: %s", data)
		}
		return response(200, "application/json", `{"methodResponses":[["Email/get",{"accountId":"account-1","state":"e1","list":[{"id":"e1","subject":"hello","from":[{"name":"Alice","email":"a@example.com"}],"to":[{"email":"b@example.com"}],"cc":[{"email":"c@example.com"}],"sentAt":"2026-01-01T00:00:00Z","receivedAt":"2026-01-02T00:00:00Z","mailboxIds":{"inbox":true}}],"notFound":["missing"]},"0"]]}`, r), nil
	}))
	result, err := client.ConflictEmails(t.Context(), []string{"e1", "missing"})
	if err != nil {
		t.Fatal(err)
	}
	s := result["e1"]
	if calls != 1 || len(result) != 1 || s.Subject != "hello" || s.From != "Alice <a@example.com>" || s.To != "b@example.com" || s.Cc != "c@example.com" || s.SentAt != "2026-01-01T00:00:00Z" || s.ReceivedAt != "2026-01-02T00:00:00Z" || s.LocationIDs[0] != "inbox" {
		t.Fatalf("summary=%#v", result)
	}
}
