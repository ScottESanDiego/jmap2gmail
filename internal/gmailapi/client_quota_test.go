package gmailapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/googleapi"
)

func assertEmptySpoolDirectory(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("temporary spool files remain: %v, error=%v", entries, err)
	}
}

func TestEveryGmailMethodRetriesExplicitQuotaRejections(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	for _, tc := range []struct {
		name       string
		rejections int
		call       func(*Client) error
	}{
		{"labels list", 1, func(c *Client) error { _, e := c.Labels(t.Context()); return e }},
		{"labels create", 1, func(c *Client) error { _, e := c.CreateLabel(t.Context(), "name"); return e }},
		{"labels patch", 1, func(c *Client) error { _, e := c.RenameLabel(t.Context(), "l", "name"); return e }},
		{"message search and metadata", 2, func(c *Client) error { _, e := c.FindByMessageID(t.Context(), "id@example.com"); return e }},
		{"inventory and metadata", 2, func(c *Client) error { _, e := c.Inventory(t.Context()); return e }},
		{"message minimal", 1, func(c *Client) error { _, e := c.Message(t.Context(), "g1"); return e }},
		{"insert", 1, func(c *Client) error {
			_, e := c.Insert(t.Context(), strings.NewReader("Subject: test\r\n\r\nbody"), nil)
			return e
		}},
		{"modify", 1, func(c *Client) error { _, e := c.ModifyLabels(t.Context(), "g1", nil, nil); return e }},
		{"trash", 1, func(c *Client) error { return c.Trash(t.Context(), "g1") }},
		{"history cursor", 1, func(c *Client) error { _, e := c.CurrentHistoryID(t.Context()); return e }},
		{"identity profile", 1, func(c *Client) error { _, e := c.DestinationIdentity(t.Context()); return e }},
		{"history list", 1, func(c *Client) error { _, e := c.ChangesSince(t.Context(), 1); return e }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seen := make(map[string]int)
			calls := 0
			client := testGmailClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				key := r.Method + " " + r.URL.Path
				seen[key]++
				if seen[key] == 1 {
					return gmailResponse(r, 403, `{"error":{"code":403,"errors":[{"reason":"userRateLimitExceeded"}]}}`), nil
				}
				body := `{"id":"g1","payload":{},"historyId":"1","emailAddress":"user@example.com"}`
				if r.Method == http.MethodGet && r.URL.Path == "/gmail/v1/users/me/messages" {
					body = `{"messages":[{"id":"g1"}]}`
				}
				return gmailResponse(r, 200, body), nil
			}))
			p, _, waits := fakePolicy(t)
			client.policy = p
			if err := tc.call(client); err != nil {
				t.Fatal(err)
			}
			if calls != 2*tc.rejections || len(*waits) != tc.rejections {
				t.Fatalf("calls=%d waits=%d want rejections=%d", calls, len(*waits), tc.rejections)
			}
			assertEmptySpoolDirectory(t, dir)
		})
	}
}

func TestQuotaRetriesOnlyFailedInventoryPage(t *testing.T) {
	var tokens []string
	var gets []string
	client := testGmailClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/gmail/v1/users/me/messages" {
			token := r.URL.Query().Get("pageToken")
			tokens = append(tokens, token)
			if token == "" {
				return gmailResponse(r, 200, `{"messages":[{"id":"a"}],"nextPageToken":"next"}`), nil
			}
			if len(tokens) == 2 {
				return gmailResponse(r, 403, `{"error":{"code":403,"details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"RATE_LIMIT_EXCEEDED","metadata":{"quota_limit":"QueriesPerMinutePerUser"}}]}}`), nil
			}
			return gmailResponse(r, 200, `{"messages":[{"id":"b"}]}`), nil
		}
		id := filepath.Base(r.URL.Path)
		gets = append(gets, id)
		return gmailResponse(r, 200, `{"id":"`+id+`","payload":{}}`), nil
	}))
	p, _, _ := fakePolicy(t)
	client.policy = p
	messages, err := client.Inventory(t.Context())
	if err != nil || len(messages) != 2 || !reflect.DeepEqual(tokens, []string{"", "next", "next"}) || !reflect.DeepEqual(gets, []string{"a", "b"}) {
		t.Fatalf("messages=%v tokens=%v gets=%v error=%v", messages, tokens, gets, err)
	}
}

type patternedReader struct{ offset int }

func (r *patternedReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(r.offset % 251)
		r.offset++
	}
	return len(p), nil
}

func TestInsertQuotaReplayIsExactLargeStreamingMIME(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	const bodySize = googleapi.DefaultUploadChunkSize + 1024
	raw := func() io.Reader {
		return io.MultiReader(strings.NewReader("Subject: exact\r\n\r\n"), io.LimitReader(&patternedReader{}, bodySize))
	}
	wantHash := sha256.New()
	wantSize, err := io.Copy(wantHash, raw())
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	client := testGmailClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Query().Get("uploadType") != "multipart" || r.GetBody != nil {
			t.Fatalf("upload must be one streaming request, URL=%s getBody=%v", r.URL, r.GetBody != nil)
		}
		files, err := os.ReadDir(dir)
		if err != nil || len(files) != 1 {
			t.Fatalf("spool files=%v error=%v", files, err)
		}
		info, err := files[0].Info()
		if err != nil || info.Mode().Perm() != 0600 || info.Size() != wantSize {
			t.Fatalf("spool permissions/size=%v error=%v", info, err)
		}
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			t.Fatal(err)
		}
		reader := multipart.NewReader(r.Body, params["boundary"])
		metadata, err := reader.NextPart()
		if err != nil {
			t.Fatal(err)
		}
		var message gmail.Message
		if err := json.NewDecoder(metadata).Decode(&message); err != nil || !reflect.DeepEqual(message.LabelIds, []string{"INBOX", "managed"}) {
			t.Fatalf("metadata=%#v error=%v", message, err)
		}
		part, err := reader.NextPart()
		if err != nil {
			t.Fatal(err)
		}
		gotHash := sha256.New()
		gotSize, err := io.Copy(gotHash, part)
		if err != nil || gotSize != wantSize || !bytes.Equal(gotHash.Sum(nil), wantHash.Sum(nil)) {
			t.Fatalf("MIME replay size=%d want=%d hash=%x error=%v", gotSize, wantSize, gotHash.Sum(nil), err)
		}
		if calls == 1 {
			return gmailResponse(r, 429, `{"error":{"code":429,"message":"rate limited"}}`), nil
		}
		return gmailResponse(r, 200, `{"id":"inserted","labelIds":["INBOX","managed"]}`), nil
	}))
	p, _, _ := fakePolicy(t)
	client.policy = p
	message, err := client.Insert(t.Context(), raw(), []string{"INBOX", "managed"})
	if err != nil || calls != 2 || message.ID != "inserted" {
		t.Fatalf("message=%v calls=%d error=%v", message, calls, err)
	}
	assertEmptySpoolDirectory(t, dir)
}

type readFunc func([]byte) (int, error)

func (f readFunc) Read(p []byte) (int, error) { return f(p) }

func TestInsertSpoolCleanupAndNoAmbiguousRetry(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	for _, kind := range []string{"copy error", "copy cancellation", "network", "timeout", "server", "daily", "quota cancellation"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			client := testGmailClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				switch kind {
				case "network":
					return nil, io.ErrUnexpectedEOF
				case "timeout":
					return nil, context.DeadlineExceeded
				case "server":
					return gmailResponse(r, 503, `{"error":{"code":503}}`), nil
				case "daily":
					return gmailResponse(r, 403, `{"error":{"code":403,"errors":[{"reason":"dailyLimitExceeded"}]}}`), nil
				default:
					return gmailResponse(r, 403, `{"error":{"code":403,"errors":[{"reason":"userRateLimitExceeded"}]}}`), nil
				}
			}))
			p, _, _ := fakePolicy(t)
			p.wait = func(ctx context.Context, d time.Duration) error { cancel(); return ctx.Err() }
			client.policy = p
			var raw io.Reader = strings.NewReader("Subject: test\r\n\r\nbody")
			wantCalls := 1
			if kind == "copy error" {
				wantCalls = 0
				raw = readFunc(func([]byte) (int, error) { return 0, io.ErrUnexpectedEOF })
			}
			if kind == "copy cancellation" {
				wantCalls = 0
				raw = readFunc(func(p []byte) (int, error) { cancel(); p[0] = 'x'; return 1, nil })
			}
			_, err := client.Insert(ctx, raw, nil)
			if err == nil || calls != wantCalls {
				t.Fatalf("calls=%d want=%d error=%v", calls, wantCalls, err)
			}
			if strings.Contains(kind, "cancellation") && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation error=%v", err)
			}
			assertEmptySpoolDirectory(t, dir)
		})
	}
}

func TestQuotaPolicyConcurrentCalls(t *testing.T) {
	p := newRequestPolicy(ClientOptions{})
	c := &Client{policy: p}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if _, err := execute(t.Context(), c, "messages.get", func() (int, error) { return 1, nil }); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
}

func TestReadOnlyInventoryCreatesNoSpool(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	client := testGmailClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) { return gmailResponse(r, 200, `{}`), nil }))
	if _, err := client.Inventory(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertEmptySpoolDirectory(t, dir)
}

func TestInsertEarlyQuotaResponseReplaysIndependentMIMEReader(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	const size = 1 << 20
	want := sha256.New()
	io.Copy(want, io.LimitReader(&patternedReader{}, size))
	labels := []string{"original"}
	calls := 0
	client := testGmailClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			t.Fatal(err)
		}
		reader := multipart.NewReader(r.Body, params["boundary"])
		part, err := reader.NextPart()
		if err != nil {
			t.Fatal(err)
		}
		var message gmail.Message
		if err := json.NewDecoder(part).Decode(&message); err != nil || !reflect.DeepEqual(message.LabelIds, []string{"original"}) {
			t.Fatalf("immutable metadata=%v error=%v", message.LabelIds, err)
		}
		part, err = reader.NextPart()
		if err != nil {
			t.Fatal(err)
		}
		if calls == 1 {
			if _, err := io.CopyN(io.Discard, part, 512); err != nil {
				t.Fatal(err)
			}
			labels[0] = "mutated after first attempt"
			return gmailResponse(r, 403, `{"error":{"code":403,"errors":[{"reason":"rateLimitExceeded"}]}}`), nil
		}
		got := sha256.New()
		if n, err := io.Copy(got, part); err != nil || n != size || !bytes.Equal(got.Sum(nil), want.Sum(nil)) {
			t.Fatalf("early-response replay size=%d hash=%x error=%v", n, got.Sum(nil), err)
		}
		return gmailResponse(r, 200, `{"id":"inserted"}`), nil
	}))
	p, _, _ := fakePolicy(t)
	client.policy = p
	if _, err := client.Insert(t.Context(), io.LimitReader(&patternedReader{}, size), labels); err != nil || calls != 2 {
		t.Fatalf("calls=%d error=%v", calls, err)
	}
	assertEmptySpoolDirectory(t, dir)
}

func TestModifyQuotaRetryRecreatesImmutableBody(t *testing.T) {
	add, remove := []string{"add"}, []string{"remove"}
	calls := 0
	client := testGmailClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		var body gmail.ModifyMessageRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !reflect.DeepEqual(body.AddLabelIds, []string{"add"}) || !reflect.DeepEqual(body.RemoveLabelIds, []string{"remove"}) {
			t.Fatalf("immutable modify body=%v error=%v", body, err)
		}
		if calls == 1 {
			add[0], remove[0] = "mutated add", "mutated remove"
			return gmailResponse(r, 429, `{"error":{"code":429}}`), nil
		}
		return gmailResponse(r, 200, `{"id":"g1"}`), nil
	}))
	p, _, _ := fakePolicy(t)
	client.policy = p
	if _, err := client.ModifyLabels(t.Context(), "g1", add, remove); err != nil || calls != 2 {
		t.Fatalf("calls=%d error=%v", calls, err)
	}
}

func TestClientOptionsValidation(t *testing.T) {
	if _, err := New(t.Context(), &http.Client{}, "me", ClientOptions{}, ClientOptions{}); err == nil {
		t.Fatal("multiple options accepted")
	}

}
