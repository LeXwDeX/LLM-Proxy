package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"
)

// Spare-lane retirement matches a credential exactly in the authentication
// header a provider sets, never as a substring of another key or header.
func TestHeadersCarryCredentialExact(t *testing.T) {
	const key = "sk-short-key-1"
	for _, tc := range []struct {
		name   string
		header http.Header
		want   bool
	}{
		{"bearer", http.Header{"Authorization": {"Bearer " + key}}, true},
		{"azure_api_key", http.Header{"Api-Key": {key}}, true},
		{"anthropic_x_api_key", http.Header{"X-Api-Key": {key}}, true},
		{"gemini_x_goog_api_key", http.Header{"X-Goog-Api-Key": {key}}, true},
		{"longer_key_containing_it", http.Header{"Authorization": {"Bearer " + key + "-and-more"}}, false},
		{"other_header_value", http.Header{"X-Request-Note": {"uses " + key}}, false},
		{"bare_authorization_value", http.Header{"Authorization": {key}}, false},
	} {
		if got := headersCarryCredential(tc.header, []string{key}); got != tc.want {
			t.Errorf("%s: carry=%v want %v", tc.name, got, tc.want)
		}
	}
	if headersCarryCredential(http.Header{"Authorization": {"Bearer "}}, []string{""}) {
		t.Error("an empty key matched")
	}
}

func spareLane(key string) *responsesWSConn {
	return &responsesWSConn{prewarmed: true, dial: &responsesWSDialSpec{headers: http.Header{"Authorization": {"Bearer " + key}}}}
}

// A key that is a prefix of another key retires only its own spare lanes.
func TestRetireSparesKeyIsSubstringOfAnother(t *testing.T) {
	short, long := spareLane("sk-team-key"), spareLane("sk-team-key-2")
	p := &responsesWSPool{entries: map[*responsesWSConn]struct{}{short: {}, long: {}}}
	p.retireSparesForCredentials([]string{"sk-team-key"})
	if _, ok := p.entries[short]; ok {
		t.Error("the unavailable key's spare lane was kept")
	}
	if _, ok := p.entries[long]; !ok {
		t.Error("the spare lane of a key containing it was retired")
	}
}

// A key that leaves rotation and recovers before the asynchronous
// notification runs keeps its spare lanes: the late callback re-reads the
// key's current state.
func TestLateUnavailableCallbackKeepsRecoveredKeySpare(t *testing.T) {
	pool := newKeyPool("target", []string{"sk-recovering-key", "sk-other-key-01"}, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	lane := spareLane("sk-recovering-key")
	ws := &responsesWSPool{entries: map[*responsesWSConn]struct{}{lane: {}}}
	var notified sync.WaitGroup
	notified.Add(1)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	pool.setOnKeyChange(func(active, total int) {
		first := false
		once.Do(func() { first = true })
		if first {
			close(entered)
			<-release // the exhaustion notification is delayed past the recovery
			defer notified.Done()
		}
	})
	pool.setOnKeysUnavailable(ws.retireSparesForCredentials)

	pool.markExhausted(0, "quota")
	<-entered
	if !pool.markRecovered(0) {
		t.Fatal("key did not recover")
	}
	close(release)
	notified.Wait()
	time.Sleep(50 * time.Millisecond) // let the late callback finish
	ws.mu.Lock()
	_, kept := ws.entries[lane]
	ws.mu.Unlock()
	if !kept {
		t.Fatal("a late unavailable callback retired the spare lane of a recovered key")
	}

	// Still unavailable at callback time: the spare lane is retired.
	pool.setOnKeyChange(nil)
	pool.markExhausted(0, "quota")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ws.mu.Lock()
		_, kept = ws.entries[lane]
		ws.mu.Unlock()
		if !kept {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the spare lane of an exhausted key was not retired")
}
