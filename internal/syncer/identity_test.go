package syncer

import (
	"io"
	"strings"
	"testing"

	"jmap2gmail/internal/store"
)

func TestToolIdentityNamesAndSyntheticMessageID(t *testing.T) {
	if HeaderAccount != "X-Jmap2gmail-Account" || HeaderEmailID != "X-Jmap2gmail-Email-ID" {
		t.Fatalf("identity header names: account=%q email=%q", HeaderAccount, HeaderEmailID)
	}
	engine := &Engine{Source: oneMessageSource(), Destination: &fakeDestination{}, state: store.New()}
	if err := engine.bindAccounts(t.Context()); err != nil {
		t.Fatal(err)
	}
	const hash = "a04cb99c00a7d85edcb8f0dd6b1466c526c155bd2224b8908c07b7d78fa4bf16"
	if engine.accountHash != hash || engine.managedLabel != "jmap2gmail-managed/"+hash {
		t.Fatalf("account=%q marker=%q", engine.accountHash, engine.managedLabel)
	}
	const messageID = "jmap2gmail.8b5cc4df7eec7d32a7814eca4af047ae@invalid"
	if got := effectiveMessageID(Email{ID: "e1"}); got != messageID {
		t.Fatalf("synthetic Message-ID=%q", got)
	}
	reader, err := rewriteIdentityHeaders(strings.NewReader("Subject: test\r\n\r\nbody"), hash, "e1", messageID, true)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"X-Jmap2gmail-Account: " + hash, "X-Jmap2gmail-Email-ID: e1", "Message-ID: <" + messageID + ">"} {
		if !strings.Contains(string(data), want+"\r\n") {
			t.Fatalf("rewritten MIME lacks %q: %s", want, data)
		}
	}
}
