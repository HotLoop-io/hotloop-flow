package api

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
)

func viBody(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile("../vi/testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]string{"name": name, "data": base64.StdEncoding.EncodeToString(raw)})
	return b
}

// A LabVIEW VI over the API: what it holds, the flow that carries it, and the
// list of what did not come across. Nothing is saved.
func TestImportVIAnswersWhatWasReadAndWhatWasNot(t *testing.T) {
	ts := newTestServer(t, map[string][]string{"admin": {"*"}, "reader": {"flows.read"}, "nobody": {}})
	tok := ts.mustLogin(t, "admin")
	res, out := ts.do(t, "POST", "/flows/import-vi", tok, viBody(t, "Get_Grayscale_Value.vi"))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("%d %s", res.StatusCode, out)
	}
	var got struct {
		Info     struct{ Description string }
		Flows    []map[string]any
		Unmapped []string
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Info.Description != "Determine the gray scale value of a color." || len(got.Flows) != 2 || len(got.Unmapped) < 5 {
		t.Errorf("answer = %s", out)
	}
	if !strings.Contains(strings.Join(got.Unmapped, "\n"), "wires") {
		t.Errorf("unmapped does not say the wires: %v", got.Unmapped)
	}
	if res, _ := ts.do(t, "GET", "/flows", tok, nil); res.StatusCode != http.StatusOK {
		t.Fatal(res.StatusCode)
	}
	// Not a VI: a reason, as 422.
	bad, _ := json.Marshal(map[string]string{"name": "x.vi", "data": base64.StdEncoding.EncodeToString([]byte(strings.Repeat("hello ", 30)))})
	if res, out := ts.do(t, "POST", "/flows/import-vi", tok, bad); res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(string(out), "RSRC") {
		t.Errorf("a text file: %d %s", res.StatusCode, out)
	}
	// Needs the read permission and a token.
	if res, _ := ts.do(t, "POST", "/flows/import-vi", "", viBody(t, "Get_Grayscale_Value.vi")); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("no token: %d", res.StatusCode)
	}
	rtok := ts.mustLogin(t, "nobody")
	if res, _ := ts.do(t, "POST", "/flows/import-vi", rtok, viBody(t, "Get_Grayscale_Value.vi")); res.StatusCode != http.StatusForbidden {
		t.Errorf("no permission: %d", res.StatusCode)
	}
}
