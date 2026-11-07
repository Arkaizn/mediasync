//go:build integration

package integration

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/arkaizn/mediasync/internal/model"
	"github.com/arkaizn/mediasync/internal/provider/jellyfin"
	"github.com/arkaizn/mediasync/internal/redact"
)

// TestRealJellyfinPlayURLLimit checks the request-URL limit of play commands
// on a real server: 249 ids (the playlist size from the bug report) are
// rejected by Kestrel with HTTP 414 and mapped to a clear error, and the
// number of ids mediasync's budget allows is accepted and delivered.
func TestRealJellyfinPlayURLLimit(t *testing.T) {
	env := jellyfinEnv(t)
	ids := audioItems(t, env)
	phone := newSimClient(t, env, "Finamp", "IT Phone URL", ids, true)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	jc, err := jellyfin.NewClient(jellyfin.ClientOptions{BaseURL: env.url, DeviceID: "mediasync-it-url", DeviceName: "it", Version: "it"})
	if err != nil {
		t.Fatal(err)
	}
	_, tok, err := jc.AuthenticateByName(ctx, env.user, redact.Secret(env.password))
	if err != nil {
		t.Fatal(err)
	}
	jc.SetToken(tok)
	t.Cleanup(func() { _ = jc.Logout(context.Background()) })

	var jfID string
	deadline := time.Now().Add(10 * time.Second)
	for jfID == "" && time.Now().Before(deadline) {
		sessions, err := jc.Sessions(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range sessions {
			if s.DeviceID == phone.deviceID && s.SupportsRemoteControl {
				jfID = s.ID
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if jfID == "" {
		t.Fatal("phone session not found")
	}

	repeat := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = ids[i%len(ids)]
		}
		return out
	}

	// 249 ids: over Kestrel's 8 KiB request line.
	tooMany := repeat(249)
	t.Logf("249 ids: request target %d bytes", jc.PlayTargetLen(jfID, "PlayLast", tooMany, nil))
	err = jc.PlayCommand(ctx, jfID, "PlayLast", tooMany, nil)
	me := model.AsError(err)
	if err == nil || !strings.Contains(me.Message, "too large for one command") || me.Data["http_status"] != http.StatusRequestURITooLong {
		t.Fatalf("249 ids: want mapped HTTP 414, got %v (%v)", err, me.Data)
	}

	// As many ids as the budget allows: accepted and delivered.
	budget, sep := jc.PlayIDBudget(jfID, "PlayLast")
	n := 1 + (budget-len(ids[0]))/(len(ids[0])+sep)
	fits := repeat(n)
	t.Logf("budget allows %d ids: request target %d bytes", n, jc.PlayTargetLen(jfID, "PlayLast", fits, nil))
	if n < 150 {
		t.Errorf("budget allows only %d ids", n)
	}
	before := len(phone.Received())
	if err := jc.PlayCommand(ctx, jfID, "PlayLast", fits, nil); err != nil {
		t.Fatalf("%d ids within the budget: %v", n, err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for len(phone.Received()) == before && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	got := phone.Received()
	if len(got) == before || got[len(got)-1] != "Play:PlayLast:"+strings.Join(fits, ",") {
		t.Errorf("phone did not receive the %d ids", n)
	}
}
