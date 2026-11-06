package gmailapi

import (
	"net/http"
	"reflect"
	"testing"
)

func TestConflictMessageOnlyHumanMetadata(t *testing.T) {
	client := testGmailClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		q := r.URL.Query()
		if r.Method != "GET" || q.Get("format") != "metadata" || q.Get("fields") != "id,threadId,labelIds,internalDate,payload(headers)" || !reflect.DeepEqual(q["metadataHeaders"], []string{"Subject", "From", "To", "Cc", "Date"}) {
			t.Fatalf("unsafe diagnostic request: %s %v", r.Method, q)
		}
		return gmailResponse(r, 200, `{"id":"g1","threadId":"thread","labelIds":["L1"],"internalDate":"1000","payload":{"headers":[{"name":"Subject","value":"hello"},{"name":"From","value":"Alice"},{"name":"To","value":"Bob"},{"name":"Cc","value":"Carol"},{"name":"Date","value":"Fri, 6 Nov 2026 10:00:00 -0800"}]}}`), nil
	}))
	s, err := client.ConflictMessage(t.Context(), "g1")
	if err != nil {
		t.Fatal(err)
	}
	if s.Subject != "hello" || s.From != "Alice" || s.To != "Bob" || s.Cc != "Carol" || s.ThreadID != "thread" || s.ReceivedAt != "1970-01-01T00:00:01Z" || s.LocationIDs[0] != "L1" {
		t.Fatalf("summary=%#v", s)
	}
}
