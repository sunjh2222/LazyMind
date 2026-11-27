package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"testing/synctest"
	"time"
)

func TestMarketInstallConfirmationExpiresDuringSubmission(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newInstallConfirmFixture(t, true)
		token := fixture.preview(true)
		// The confirmation has one second left when the submission starts.
		time.Sleep(10*time.Minute - time.Second)
		transport := http.DefaultTransport
		t.Cleanup(func() { http.DefaultTransport = transport })
		readStarted := false
		http.DefaultTransport = installConfirmTransport(func(request *http.Request) (*http.Response, error) {
			if request.URL.Path == "/latest.json" {
				readStarted = true
				time.Sleep(2 * time.Second)
			}
			return transport.RoundTrip(request)
		})
		body, err := json.Marshal(map[string]string{"processing_level": "indexed", "confirmation_token": token})
		if err != nil {
			t.Fatal(err)
		}
		response := fixture.request(http.MethodPost, ":install", installConfirmReader, string(body))
		if !readStarted {
			t.Fatal("submission must reach release lookup before its confirmation expires")
		}
		if response.Code != http.StatusConflict {
			t.Errorf("confirmation expired during release lookup: got HTTP %d, want 409", response.Code)
		}
		fixture.assertCounts(0)
	})
}
