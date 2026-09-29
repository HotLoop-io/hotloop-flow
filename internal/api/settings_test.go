package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/HotLoop-io/hotloop-flow/internal/config"
)

func settingsServer(t *testing.T, authOn bool) *httptest.Server {
	t.Helper()
	cfg := config.Default()
	cfg.Auth.Enabled = authOn
	cfg.Auth.Insecure = !authOn
	s := New(Deps{Config: cfg, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Version: "test"})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv
}

// The editor asks /settings before deciding whether to show a login. With
// authentication off it has to answer without a token and say auth is off, or
// the editor lands on a login screen with no users behind it, which is what
// HOTLOOP_FLOW_INSECURE used to get you even once it stopped being refused.
func TestSettingsWithAuthOffAnswersWithoutAToken(t *testing.T) {
	srv := settingsServer(t, false)
	res, err := http.Get(srv.URL + "/settings")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	var body struct {
		Auth struct {
			Enabled *bool `json:"enabled"`
		} `json:"auth"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Auth.Enabled == nil || *body.Auth.Enabled {
		t.Errorf("auth.enabled = %v, want false", body.Auth.Enabled)
	}
}

func TestSettingsWithAuthOnWantsAToken(t *testing.T) {
	srv := settingsServer(t, true)
	res, err := http.Get(srv.URL + "/settings")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", res.StatusCode)
	}
}
