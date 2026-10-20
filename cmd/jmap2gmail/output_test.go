package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"jmap2gmail/internal/gmailapi"
	"jmap2gmail/internal/store"
	"jmap2gmail/internal/syncer"
)

type outputSource struct{ syncer.Source }

func (outputSource) SourceIdentity(context.Context) (string, error) { return "source", nil }
func (outputSource) Mailboxes(context.Context) ([]syncer.Mailbox, error) {
	return []syncer.Mailbox{{ID: "inbox", Name: "Inbox", Role: "inbox"}}, nil
}
func (outputSource) State(context.Context) (string, error)         { return "s1", nil }
func (outputSource) AllEmailIDs(context.Context) ([]string, error) { return []string{}, nil }
func (outputSource) Changes(_ context.Context, since string) (syncer.Changes, error) {
	return syncer.Changes{NewState: since}, nil
}

type outputDestination struct {
	syncer.Destination
	created        int
	inventoryError error
}

func (*outputDestination) DestinationIdentity(context.Context) (string, error) {
	return "destination", nil
}
func (*outputDestination) Labels(context.Context) ([]syncer.Label, error) {
	return []syncer.Label{{ID: "INBOX", Name: "INBOX", Type: "system"}}, nil
}
func (d *outputDestination) Inventory(context.Context) ([]syncer.Message, error) {
	return []syncer.Message{}, d.inventoryError
}
func (d *outputDestination) CreateLabel(_ context.Context, name string) (syncer.Label, error) {
	d.created++
	return syncer.Label{ID: "marker", Name: name, Type: "user"}, nil
}

func TestSyncOutputSummaryAndVerboseActions(t *testing.T) {
	for _, verbose := range []bool{false, true} {
		for _, dryRun := range []bool{false, true} {
			t.Run(map[bool]string{false: "quiet", true: "verbose"}[verbose]+"/"+map[bool]string{false: "apply", true: "dry-run"}[dryRun], func(t *testing.T) {
				var stdout, stderr bytes.Buffer
				destination := &outputDestination{}
				engine := &syncer.Engine{Source: outputSource{}, Destination: destination, Store: store.NewMemory(), Options: syncer.Options{Apply: !dryRun, Output: syncDiagnosticWriter(verbose, &stderr)}}
				if err := runSyncEngine(t.Context(), engine, &stdout, dryRun); err != nil {
					t.Fatal(err)
				}
				prefix := "summary:"
				if dryRun {
					prefix = "DRY RUN summary:"
				}
				want := prefix + " labels-created=1 labels-renamed=0 inserted=0 adopted=0 modified=0 trashed=0 trash-disabled=0 spam-ignored=0 unchanged=0\n"
				if stdout.String() != want {
					t.Fatalf("stdout=%q want=%q", stdout.String(), want)
				}
				if !verbose && stderr.Len() != 0 {
					t.Fatalf("quiet stderr=%q", stderr.String())
				}
				if verbose {
					for _, message := range []string{"rebuild: inventorying Fastmail messages", "rebuild: inventorying Gmail metadata", "create Gmail label"} {
						if !strings.Contains(stderr.String(), message) {
							t.Fatalf("missing %q in stderr=%q", message, stderr.String())
						}
					}
				}
				wantCreated := 1
				if dryRun {
					wantCreated = 0
				}
				if destination.created != wantCreated {
					t.Fatalf("created=%d want=%d", destination.created, wantCreated)
				}
			})
		}
	}
}

func TestSyncFailureNeverPrintsSuccessSummary(t *testing.T) {
	expected := errors.New("inventory failed")
	for _, verbose := range []bool{false, true} {
		var stdout, stderr bytes.Buffer
		engine := &syncer.Engine{Source: outputSource{}, Destination: &outputDestination{inventoryError: expected}, Store: store.NewMemory(), Options: syncer.Options{Output: syncDiagnosticWriter(verbose, &stderr)}}
		if err := runSyncEngine(t.Context(), engine, &stdout, true); !errors.Is(err, expected) {
			t.Fatalf("verbose=%v error=%v", verbose, err)
		}
		if stdout.Len() != 0 {
			t.Fatalf("failure printed summary: %q", stdout.String())
		}
	}
}

type outputRoundTrip func(*http.Request) (*http.Response, error)

func (f outputRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSyncGmailRetryDiagnosticsFollowVerboseWriter(t *testing.T) {
	for _, verbose := range []bool{false, true} {
		var stdout, stderr bytes.Buffer
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		diagnostics := syncDiagnosticWriter(verbose, &stderr)
		client, err := gmailapi.New(ctx, &http.Client{Transport: outputRoundTrip(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 429, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"code":429}}`)), Request: r}, nil
		})}, "me", gmailapi.ClientOptions{Diagnostics: diagnostics})
		if err != nil {
			t.Fatal(err)
		}
		engine := &syncer.Engine{Source: outputSource{}, Destination: client, Store: store.NewMemory(), Options: syncer.Options{Output: diagnostics}}
		err = runSyncEngine(ctx, engine, &stdout, true)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("verbose=%v error=%v", verbose, err)
		}
		if stdout.Len() != 0 {
			t.Fatalf("failed retry printed stdout: %q", stdout.String())
		}
		if verbose && !strings.Contains(stderr.String(), "Gmail quota: rate limit; retrying getProfile") {
			t.Fatalf("retry diagnostics missing: %q", stderr.String())
		}
		if !verbose && stderr.Len() != 0 {
			t.Fatalf("quiet retry diagnostics=%q", stderr.String())
		}
	}
}

func TestSyncCallerDeadlineRemainsAnErrorWithoutSummary(t *testing.T) {
	for _, verbose := range []bool{false, true} {
		var stdout, stderr bytes.Buffer
		diagnostics := syncDiagnosticWriter(verbose, &stderr)
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
		client, err := gmailapi.New(ctx, &http.Client{Transport: outputRoundTrip(func(r *http.Request) (*http.Response, error) { <-r.Context().Done(); return nil, r.Context().Err() })}, "me", gmailapi.ClientOptions{Diagnostics: diagnostics})
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		engine := &syncer.Engine{Source: outputSource{}, Destination: client, Store: store.NewMemory(), Options: syncer.Options{Output: diagnostics}}
		err = runSyncEngine(ctx, engine, &stdout, true)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("verbose=%v caller deadline suppressed: %v", verbose, err)
		}
		if stdout.Len() != 0 {
			t.Fatalf("timeout printed summary: %q", stdout.String())
		}
		if !verbose && stderr.Len() != 0 {
			t.Fatalf("quiet timeout diagnostics=%q", stderr.String())
		}
	}
}
