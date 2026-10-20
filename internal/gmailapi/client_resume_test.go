package gmailapi

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"google.golang.org/api/googleapi"

	"jmap2gmail/internal/syncer"
)

func TestInsertOnlyExplicitBadRequestMarksDefiniteRejection(t *testing.T) {
	for _, status := range []int{400, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			client := testGmailClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return gmailResponse(r, status, `{"error":{"message":"rejected"}}`), nil
			}))
			_, err := client.Insert(t.Context(), strings.NewReader("Subject: test\r\n\r\nbody"), nil)
			if errors.Is(err, syncer.ErrMutationRejected) != (status == 400) {
				t.Fatalf("status=%d error=%v", status, err)
			}
			if _, ok := errors.AsType[*googleapi.Error](err); !ok {
				t.Fatalf("HTTP cause lost: %v", err)
			}
		})
	}
}
