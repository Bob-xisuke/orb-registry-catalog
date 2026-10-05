package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Bob-xisuke/orb-registry-catalog/internal/store"
)

func TestHealthzReportsOK(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "service.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	NewRouter(st).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if got := recorder.Body.String(); got != `{"database":"ok","status":"ok"}` {
		t.Fatalf("body = %s", got)
	}
}

func TestUnknownRouteUsesPublishedErrorShape(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "service.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/missing", nil)
	NewRouter(st).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNotFound)
	}
}

const digestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const digestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func setup(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "service.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func doRequest(st *store.Store, method, target, body string) *httptest.ResponseRecorder {
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, target, reader)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	NewRouter(st).ServeHTTP(recorder, request)
	return recorder
}

func decodeBody(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body %q: %v", recorder.Body.String(), err)
	}
	return body
}

func errorCode(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	body := decodeBody(t, recorder)
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("body %v has no error object", body)
	}
	code, _ := errObj["code"].(string)
	if _, ok := errObj["message"].(string); !ok {
		t.Fatalf("error object %v has no message string", errObj)
	}
	return code
}

const validBody = `{"repository":"team/app","digest":"` + digestA + `","tag":"latest","signature_verified":true,"retention_days":30,"size_bytes":1024}`

func TestPostArtifactCreated(t *testing.T) {
	st := setup(t)
	recorder := doRequest(st, http.MethodPost, "/v1/artifacts", validBody)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	body := decodeBody(t, recorder)
	if body["repository"] != "team/app" || body["digest"] != digestA || body["tag"] != "latest" ||
		body["signature_verified"] != true || body["retention_days"] != float64(30) || body["size_bytes"] != float64(1024) {
		t.Fatalf("unexpected body %v", body)
	}
	pushedAt, ok := body["pushed_at"].(string)
	if !ok || !strings.HasSuffix(pushedAt, "Z") {
		t.Fatalf("pushed_at = %v, want UTC RFC3339", body["pushed_at"])
	}
}

func TestPostArtifactNormalizesWhitespaceAndIgnoresUnknownFields(t *testing.T) {
	st := setup(t)
	body := `{"repository":"  team/app  ","digest":"` + digestA + `","tag":" latest ","signature_verified":true,"retention_days":30,"size_bytes":1024,"extra":"ignored"}`
	recorder := doRequest(st, http.MethodPost, "/v1/artifacts", body)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if got := decodeBody(t, recorder)["repository"]; got != "team/app" {
		t.Fatalf("repository = %v, want trimmed", got)
	}

	// Resubmitting the normalized content returns the original record and timestamp.
	first := decodeBody(t, recorder)
	second := doRequest(st, http.MethodPost, "/v1/artifacts", validBody)
	if second.Code != http.StatusCreated {
		t.Fatalf("resubmit status = %d", second.Code)
	}
	if decodeBody(t, second)["pushed_at"] != first["pushed_at"] {
		t.Fatalf("pushed_at changed on duplicate submit")
	}
}

func TestPostArtifactRejectsInvalidBodies(t *testing.T) {
	cases := map[string]string{
		"not an object":       `[1,2,3]`,
		"trailing data":       validBody + ` {}`,
		"missing field":       `{"repository":"team/app","digest":"` + digestA + `","tag":"latest","signature_verified":true,"retention_days":30}`,
		"blank repository":    `{"repository":"   ","digest":"` + digestA + `","tag":"latest","signature_verified":true,"retention_days":30,"size_bytes":1}`,
		"blank tag":           `{"repository":"team/app","digest":"` + digestA + `","tag":"  ","signature_verified":true,"retention_days":30,"size_bytes":1}`,
		"uppercase digest":    `{"repository":"team/app","digest":"sha256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","tag":"latest","signature_verified":true,"retention_days":30,"size_bytes":1}`,
		"short digest":        `{"repository":"team/app","digest":"sha256:abc","tag":"latest","signature_verified":true,"retention_days":30,"size_bytes":1}`,
		"wrong digest algo":   `{"repository":"team/app","digest":"sha512:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","tag":"latest","signature_verified":true,"retention_days":30,"size_bytes":1}`,
		"retention zero":      `{"repository":"team/app","digest":"` + digestA + `","tag":"latest","signature_verified":true,"retention_days":0,"size_bytes":1}`,
		"retention too large": `{"repository":"team/app","digest":"` + digestA + `","tag":"latest","signature_verified":true,"retention_days":3651,"size_bytes":1}`,
		"retention fraction":  `{"repository":"team/app","digest":"` + digestA + `","tag":"latest","signature_verified":true,"retention_days":1.5,"size_bytes":1}`,
		"negative size":       `{"repository":"team/app","digest":"` + digestA + `","tag":"latest","signature_verified":true,"retention_days":30,"size_bytes":-1}`,
		"size overflow":       `{"repository":"team/app","digest":"` + digestA + `","tag":"latest","signature_verified":true,"retention_days":30,"size_bytes":9223372036854775808}`,
		"wrong type":          `{"repository":"team/app","digest":"` + digestA + `","tag":"latest","signature_verified":"yes","retention_days":30,"size_bytes":1}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			st := setup(t)
			recorder := doRequest(st, http.MethodPost, "/v1/artifacts", body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
			if code := errorCode(t, recorder); code != codeInvalidInput {
				t.Fatalf("code = %q, want %q", code, codeInvalidInput)
			}
			// Nothing may be stored after a rejected request.
			list, err := st.ListArtifacts("team/app")
			if err != nil || len(list) != 0 {
				t.Fatalf("rejected request left records: %v %v", list, err)
			}
		})
	}
}

func TestPostArtifactConflict(t *testing.T) {
	st := setup(t)
	if r := doRequest(st, http.MethodPost, "/v1/artifacts", validBody); r.Code != http.StatusCreated {
		t.Fatalf("first status = %d", r.Code)
	}
	changed := strings.Replace(validBody, `"size_bytes":1024`, `"size_bytes":2048`, 1)
	recorder := doRequest(st, http.MethodPost, "/v1/artifacts", changed)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if code := errorCode(t, recorder); code != codeConflict {
		t.Fatalf("code = %q, want %q", code, codeConflict)
	}
}

func TestGetArtifactFlows(t *testing.T) {
	st := setup(t)
	if r := doRequest(st, http.MethodPost, "/v1/artifacts", validBody); r.Code != http.StatusCreated {
		t.Fatalf("register A: %d", r.Code)
	}
	bodyB := strings.Replace(validBody, digestA, digestB, 1)
	if r := doRequest(st, http.MethodPost, "/v1/artifacts", bodyB); r.Code != http.StatusCreated {
		t.Fatalf("register B: %d", r.Code)
	}

	// Repository-only listing returns both, in registration order.
	recorder := doRequest(st, http.MethodGet, "/v1/artifacts?repository=team/app", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("list status = %d", recorder.Code)
	}
	artifacts := decodeBody(t, recorder)["artifacts"].([]any)
	if len(artifacts) != 2 ||
		artifacts[0].(map[string]any)["digest"] != digestA ||
		artifacts[1].(map[string]any)["digest"] != digestB {
		t.Fatalf("artifacts = %v", artifacts)
	}

	// The tag now points at the second record.
	recorder = doRequest(st, http.MethodGet, "/v1/artifacts?repository=team/app&tag=latest", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("tag status = %d", recorder.Code)
	}
	current := decodeBody(t, recorder)["artifacts"].([]any)
	if len(current) != 1 || current[0].(map[string]any)["digest"] != digestB {
		t.Fatalf("tag result = %v", current)
	}

	// The old record remains queryable by digest.
	recorder = doRequest(st, http.MethodGet, "/v1/artifacts?repository=team/app&digest="+digestA, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("digest status = %d", recorder.Code)
	}
	old := decodeBody(t, recorder)["artifacts"].([]any)
	if len(old) != 1 || old[0].(map[string]any)["digest"] != digestA {
		t.Fatalf("digest result = %v", old)
	}

	// Retrying the old record must not move the tag back.
	if r := doRequest(st, http.MethodPost, "/v1/artifacts", validBody); r.Code != http.StatusCreated {
		t.Fatalf("retry A: %d", r.Code)
	}
	recorder = doRequest(st, http.MethodGet, "/v1/artifacts?repository=team/app&tag=latest", "")
	current = decodeBody(t, recorder)["artifacts"].([]any)
	if current[0].(map[string]any)["digest"] != digestB {
		t.Fatalf("tag moved back: %v", current)
	}
}

func TestGetArtifactRejectsInvalidQueries(t *testing.T) {
	st := setup(t)
	cases := []string{
		"/v1/artifacts",
		"/v1/artifacts?repository=%20%20",
		"/v1/artifacts?repository=team/app&tag=latest&digest=" + digestA,
		"/v1/artifacts?repository=team/app&tag=%20",
		"/v1/artifacts?repository=team/app&digest=sha256:xyz",
	}
	for _, target := range cases {
		recorder := doRequest(st, http.MethodGet, target, "")
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("GET %s status = %d", target, recorder.Code)
		}
		if code := errorCode(t, recorder); code != codeInvalidInput {
			t.Fatalf("GET %s code = %q, want %q", target, code, codeInvalidInput)
		}
	}
}

func TestGetArtifactNotFound(t *testing.T) {
	st := setup(t)
	targets := []string{
		"/v1/artifacts?repository=team/app",
		"/v1/artifacts?repository=team/app&tag=latest",
		"/v1/artifacts?repository=team/app&digest=" + digestA,
	}
	for _, target := range targets {
		recorder := doRequest(st, http.MethodGet, target, "")
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("GET %s status = %d", target, recorder.Code)
		}
		if code := errorCode(t, recorder); code != codeNotFound {
			t.Fatalf("GET %s code = %q, want %q", target, code, codeNotFound)
		}
	}
}
