package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Bob-xisuke/orb-registry-catalog/internal/store"
)

const (
	testDigestA = "sha256:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testDigestB = "sha256:" + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func newTestRouter(t *testing.T) (*gin.Engine, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "service.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return NewRouter(st), path
}

func postArtifact(t *testing.T, router *gin.Engine, body string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/artifacts", strings.NewReader(body))
	router.ServeHTTP(recorder, request)
	return recorder
}

func getArtifacts(t *testing.T, router *gin.Engine, query string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/artifacts?"+query, nil)
	router.ServeHTTP(recorder, request)
	return recorder
}

func decodeBody(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return body
}

func errorCode(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	body := decodeBody(t, recorder)
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("response has no error object: %v", body)
	}
	code, _ := errObj["code"].(string)
	if _, ok := errObj["message"].(string); !ok {
		t.Fatalf("error object has no message string: %v", errObj)
	}
	return code
}

func validRegistration(digest string) string {
	return `{"repository":"team/app","digest":"` + digest + `","tag":"latest",` +
		`"signature_verified":true,"retention_days":30,"size_bytes":1024}`
}

func TestRegisterArtifactCreated(t *testing.T) {
	router, _ := newTestRouter(t)

	recorder := postArtifact(t, router, validRegistration(testDigestA))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d (%s)", recorder.Code, http.StatusCreated, recorder.Body)
	}
	body := decodeBody(t, recorder)
	want := map[string]any{
		"repository":         "team/app",
		"digest":             testDigestA,
		"tag":                "latest",
		"signature_verified": true,
		"retention_days":     float64(30),
		"size_bytes":         float64(1024),
	}
	for key, value := range want {
		if body[key] != value {
			t.Fatalf("field %s = %v, want %v", key, body[key], value)
		}
	}
	pushedAt, ok := body["pushed_at"].(string)
	if !ok {
		t.Fatalf("pushed_at missing or not a string: %v", body)
	}
	parsed, err := time.Parse(time.RFC3339, pushedAt)
	if err != nil {
		t.Fatalf("pushed_at %q is not RFC3339: %v", pushedAt, err)
	}
	if parsed.Location() != time.UTC {
		t.Fatalf("pushed_at %q is not UTC", pushedAt)
	}
}

func TestRegisterArtifactNormalizesAndIgnoresUnknownFields(t *testing.T) {
	router, _ := newTestRouter(t)

	recorder := postArtifact(t, router, `{"repository":"  team/app  ","digest":"`+testDigestA+`",`+
		`"tag":"  latest ","signature_verified":false,"retention_days":1,"size_bytes":0,"extra":"ignored"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d (%s)", recorder.Code, http.StatusCreated, recorder.Body)
	}
	body := decodeBody(t, recorder)
	if body["repository"] != "team/app" || body["tag"] != "latest" {
		t.Fatalf("values were not trimmed: %v", body)
	}
}

func TestRegisterArtifactRejectsInvalidInput(t *testing.T) {
	cases := map[string]string{
		"empty body":            ``,
		"json array":            `[{"repository":"team/app"}]`,
		"json scalar":           `"text"`,
		"json null":             `null`,
		"trailing object":       validRegistration(testDigestA) + ` {}`,
		"malformed json":        `{"repository":`,
		"missing repository":    `{"digest":"` + testDigestA + `","tag":"latest","signature_verified":true,"retention_days":30,"size_bytes":1}`,
		"missing digest":        `{"repository":"team/app","tag":"latest","signature_verified":true,"retention_days":30,"size_bytes":1}`,
		"missing tag":           `{"repository":"team/app","digest":"` + testDigestA + `","signature_verified":true,"retention_days":30,"size_bytes":1}`,
		"missing signature":     `{"repository":"team/app","digest":"` + testDigestA + `","tag":"latest","retention_days":30,"size_bytes":1}`,
		"missing retention":     `{"repository":"team/app","digest":"` + testDigestA + `","tag":"latest","signature_verified":true,"size_bytes":1}`,
		"missing size":          `{"repository":"team/app","digest":"` + testDigestA + `","tag":"latest","signature_verified":true,"retention_days":30}`,
		"blank repository":      `{"repository":"   ","digest":"` + testDigestA + `","tag":"latest","signature_verified":true,"retention_days":30,"size_bytes":1}`,
		"blank tag":             `{"repository":"team/app","digest":"` + testDigestA + `","tag":"  ","signature_verified":true,"retention_days":30,"size_bytes":1}`,
		"repository wrong type": `{"repository":7,"digest":"` + testDigestA + `","tag":"latest","signature_verified":true,"retention_days":30,"size_bytes":1}`,
		"digest wrong prefix":   `{"repository":"team/app","digest":"sha512:` + strings.Repeat("a", 64) + `","tag":"latest","signature_verified":true,"retention_days":30,"size_bytes":1}`,
		"digest uppercase hex":  `{"repository":"team/app","digest":"sha256:` + strings.Repeat("A", 64) + `","tag":"latest","signature_verified":true,"retention_days":30,"size_bytes":1}`,
		"digest too short":      `{"repository":"team/app","digest":"sha256:` + strings.Repeat("a", 63) + `","tag":"latest","signature_verified":true,"retention_days":30,"size_bytes":1}`,
		"signature not bool":    `{"repository":"team/app","digest":"` + testDigestA + `","tag":"latest","signature_verified":"true","retention_days":30,"size_bytes":1}`,
		"retention zero":        `{"repository":"team/app","digest":"` + testDigestA + `","tag":"latest","signature_verified":true,"retention_days":0,"size_bytes":1}`,
		"retention too large":   `{"repository":"team/app","digest":"` + testDigestA + `","tag":"latest","signature_verified":true,"retention_days":3651,"size_bytes":1}`,
		"retention fractional":  `{"repository":"team/app","digest":"` + testDigestA + `","tag":"latest","signature_verified":true,"retention_days":1.5,"size_bytes":1}`,
		"retention as string":   `{"repository":"team/app","digest":"` + testDigestA + `","tag":"latest","signature_verified":true,"retention_days":"30","size_bytes":1}`,
		"size negative":         `{"repository":"team/app","digest":"` + testDigestA + `","tag":"latest","signature_verified":true,"retention_days":30,"size_bytes":-1}`,
		"size fractional":       `{"repository":"team/app","digest":"` + testDigestA + `","tag":"latest","signature_verified":true,"retention_days":30,"size_bytes":1.5}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			router, _ := newTestRouter(t)
			recorder := postArtifact(t, router, body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d (%s)", recorder.Code, http.StatusBadRequest, recorder.Body)
			}
			if code := errorCode(t, recorder); code != codeInvalidInput {
				t.Fatalf("error code = %q, want %q", code, codeInvalidInput)
			}
			// A rejected registration must leave no record behind.
			lookup := getArtifacts(t, router, "repository=team/app")
			if lookup.Code != http.StatusNotFound {
				t.Fatalf("after rejection, list status = %d, want %d", lookup.Code, http.StatusNotFound)
			}
		})
	}
}

func TestRegisterDuplicateReturnsOriginalRecord(t *testing.T) {
	router, _ := newTestRouter(t)

	first := postArtifact(t, router, validRegistration(testDigestA))
	if first.Code != http.StatusCreated {
		t.Fatalf("first status = %d", first.Code)
	}
	firstPushedAt := decodeBody(t, first)["pushed_at"]

	// Same content after normalization (extra whitespace, unknown field).
	second := postArtifact(t, router, `{"repository":" team/app ","digest":"`+testDigestA+`","tag":"latest",`+
		`"signature_verified":true,"retention_days":30,"size_bytes":1024,"unknown":1}`)
	if second.Code != http.StatusCreated {
		t.Fatalf("duplicate status = %d, want %d (%s)", second.Code, http.StatusCreated, second.Body)
	}
	if got := decodeBody(t, second)["pushed_at"]; got != firstPushedAt {
		t.Fatalf("pushed_at changed on duplicate: %v -> %v", firstPushedAt, got)
	}
}

func TestRegisterConflictOnDifferentContent(t *testing.T) {
	router, _ := newTestRouter(t)
	postArtifact(t, router, validRegistration(testDigestA))

	recorder := postArtifact(t, router, `{"repository":"team/app","digest":"`+testDigestA+`","tag":"other",`+
		`"signature_verified":true,"retention_days":30,"size_bytes":1024}`)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d (%s)", recorder.Code, http.StatusConflict, recorder.Body)
	}
	if code := errorCode(t, recorder); code != codeConflict {
		t.Fatalf("error code = %q, want %q", code, codeConflict)
	}
}

func TestTagPointerMovesToNewDigestAndOldRecordSurvives(t *testing.T) {
	router, _ := newTestRouter(t)
	postArtifact(t, router, validRegistration(testDigestA))
	postArtifact(t, router, validRegistration(testDigestB))

	// The tag now resolves to the new record.
	recorder := getArtifacts(t, router, "repository=team/app&tag=latest")
	if recorder.Code != http.StatusOK {
		t.Fatalf("tag query status = %d (%s)", recorder.Code, recorder.Body)
	}
	artifacts := decodeBody(t, recorder)["artifacts"].([]any)
	if len(artifacts) != 1 || artifacts[0].(map[string]any)["digest"] != testDigestB {
		t.Fatalf("tag does not point at new record: %v", artifacts)
	}

	// The old record is still queryable by digest.
	recorder = getArtifacts(t, router, "repository=team/app&digest="+testDigestA)
	if recorder.Code != http.StatusOK {
		t.Fatalf("digest query status = %d (%s)", recorder.Code, recorder.Body)
	}

	// Retrying the old registration succeeds but must not move the tag back.
	retry := postArtifact(t, router, validRegistration(testDigestA))
	if retry.Code != http.StatusCreated {
		t.Fatalf("retry status = %d (%s)", retry.Code, retry.Body)
	}
	recorder = getArtifacts(t, router, "repository=team/app&tag=latest")
	artifacts = decodeBody(t, recorder)["artifacts"].([]any)
	if artifacts[0].(map[string]any)["digest"] != testDigestB {
		t.Fatalf("retry moved the tag pointer: %v", artifacts)
	}
}

func TestListArtifactsInRegistrationOrder(t *testing.T) {
	router, _ := newTestRouter(t)
	postArtifact(t, router, validRegistration(testDigestB))
	postArtifact(t, router, validRegistration(testDigestA))

	recorder := getArtifacts(t, router, "repository=team/app")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body)
	}
	artifacts := decodeBody(t, recorder)["artifacts"].([]any)
	if len(artifacts) != 2 {
		t.Fatalf("got %d artifacts, want 2", len(artifacts))
	}
	if artifacts[0].(map[string]any)["digest"] != testDigestB ||
		artifacts[1].(map[string]any)["digest"] != testDigestA {
		t.Fatalf("records not in registration order: %v", artifacts)
	}
}

func TestQueryValidation(t *testing.T) {
	router, _ := newTestRouter(t)
	postArtifact(t, router, validRegistration(testDigestA))

	for name, query := range map[string]string{
		"missing repository":   "tag=latest",
		"blank repository":     "repository=%20%20",
		"tag and digest":       "repository=team/app&tag=latest&digest=" + testDigestA,
		"blank tag":            "repository=team/app&tag=%20",
		"malformed digest":     "repository=team/app&digest=sha256:xyz",
		"uppercase repository": "repository=TEAM/APP&tag=latest", // case-sensitive: valid but no match
	} {
		t.Run(name, func(t *testing.T) {
			recorder := getArtifacts(t, router, query)
			if name == "uppercase repository" {
				if recorder.Code != http.StatusNotFound {
					t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNotFound)
				}
				return
			}
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d (%s)", recorder.Code, http.StatusBadRequest, recorder.Body)
			}
			if code := errorCode(t, recorder); code != codeInvalidInput {
				t.Fatalf("error code = %q, want %q", code, codeInvalidInput)
			}
		})
	}
}

func TestQueryNotFound(t *testing.T) {
	router, _ := newTestRouter(t)
	postArtifact(t, router, validRegistration(testDigestA))

	for name, query := range map[string]string{
		"unknown repository": "repository=other/repo",
		"unknown tag":        "repository=team/app&tag=missing",
		"unknown digest":     "repository=team/app&digest=" + testDigestB,
	} {
		t.Run(name, func(t *testing.T) {
			recorder := getArtifacts(t, router, query)
			if recorder.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want %d (%s)", recorder.Code, http.StatusNotFound, recorder.Body)
			}
			if code := errorCode(t, recorder); code != codeNotFound {
				t.Fatalf("error code = %q, want %q", code, codeNotFound)
			}
		})
	}
}

func TestRecordsSurviveRestart(t *testing.T) {
	router, path := newTestRouter(t)
	first := postArtifact(t, router, validRegistration(testDigestA))
	pushedAt := decodeBody(t, first)["pushed_at"]

	// Reopen the same database file behind a fresh router.
	reopened, err := store.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	router = NewRouter(reopened)

	recorder := getArtifacts(t, router, "repository=team/app&tag=latest")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body)
	}
	artifacts := decodeBody(t, recorder)["artifacts"].([]any)
	record := artifacts[0].(map[string]any)
	if record["digest"] != testDigestA || record["pushed_at"] != pushedAt {
		t.Fatalf("record did not survive restart: %v", record)
	}
}
