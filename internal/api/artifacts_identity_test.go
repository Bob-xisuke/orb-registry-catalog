package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Bob-xisuke/orb-registry-catalog/internal/store"
)

// 本文件补充针对公开 HTTP 入口的回归用例，覆盖 docs/artifact-identity.md
// 中既有测试未覆盖的结论：完整 A/B/重试序列、四字段 409 矩阵及其不变量、
// 空白规范化判重、未知字段、跨仓库隔离、503、错误消息不泄露、
// 保留/签名字段惰性，以及换向后重开持久化。

func artifactRecords(t *testing.T, recorder *httptest.ResponseRecorder) []any {
	t.Helper()
	body := decodeBody(t, recorder)
	artifacts, ok := body["artifacts"].([]any)
	if !ok {
		t.Fatalf("response has no artifacts array: %v", body)
	}
	return artifacts
}

func recordField(record any, key string) any {
	return record.(map[string]any)[key]
}

// registrationBody builds a valid request body; %q is JSON-compatible for the
// ASCII repository/tag/digest values used here.
func registrationBody(repository, digest, tag string, verified bool, retention, size int64) string {
	return fmt.Sprintf(`{"repository":%q,"digest":%q,"tag":%q,`+
		`"signature_verified":%t,"retention_days":%d,"size_bytes":%d}`,
		repository, digest, tag, verified, retention, size)
}

func demoRegistration(digest, tag string, verified bool, retention, size int64) string {
	return registrationBody("team/demo", digest, tag, verified, retention, size)
}

func serveRaw(t *testing.T, router *gin.Engine, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	router.ServeHTTP(recorder, request)
	return recorder
}

// TestArtifactIdentityAndTagPointerSequence walks the full documented
// sequence in a repository with no prior records:
// register A, register B under the same tag, then retry A byte-for-byte.
func TestArtifactIdentityAndTagPointerSequence(t *testing.T) {
	router, _ := newTestRouter(t)
	bodyA := demoRegistration(testDigestA, "v1", true, 30, 1024)

	// Step 1: register A -> 201, server-generated pushed_at (T1).
	first := postArtifact(t, router, bodyA)
	if first.Code != http.StatusCreated {
		t.Fatalf("register A: status = %d, want %d (%s)", first.Code, http.StatusCreated, first.Body)
	}
	pushedA := decodeBody(t, first)["pushed_at"].(string)
	if _, err := time.Parse(time.RFC3339, pushedA); err != nil {
		t.Fatalf("A pushed_at %q is not RFC3339: %v", pushedA, err)
	}

	// Step 2: register B with the same tag -> 201; tag pointer moves.
	second := postArtifact(t, router, demoRegistration(testDigestB, "v1", true, 30, 1024))
	if second.Code != http.StatusCreated {
		t.Fatalf("register B: status = %d, want %d (%s)", second.Code, http.StatusCreated, second.Body)
	}
	pushedB := decodeBody(t, second)["pushed_at"].(string)

	// Step 3: retry A unchanged -> 201 and the ORIGINAL record (T1, not regenerated).
	retry := postArtifact(t, router, bodyA)
	if retry.Code != http.StatusCreated {
		t.Fatalf("retry A: status = %d, want %d (%s)", retry.Code, http.StatusCreated, retry.Body)
	}
	retryBody := decodeBody(t, retry)
	if retryBody["pushed_at"] != pushedA {
		t.Fatalf("retry A pushed_at = %v, want original %v", retryBody["pushed_at"], pushedA)
	}
	for key, want := range map[string]any{
		"digest": testDigestA, "tag": "v1", "signature_verified": true,
		"retention_days": float64(30), "size_bytes": float64(1024),
	} {
		if retryBody[key] != want {
			t.Fatalf("retry A field %s = %v, want %v", key, retryBody[key], want)
		}
	}

	// Repository list still holds exactly two records in first-registration order.
	list := getArtifacts(t, router, "repository=team/demo")
	if list.Code != http.StatusOK {
		t.Fatalf("list: status = %d (%s)", list.Code, list.Body)
	}
	records := artifactRecords(t, list)
	if len(records) != 2 {
		t.Fatalf("list has %d records, want 2: %v", len(records), records)
	}
	if recordField(records[0], "digest") != testDigestA ||
		recordField(records[1], "digest") != testDigestB {
		t.Fatalf("records not in first-registration order: %v", records)
	}

	// Tag query now hits B; the moved-off A remains fetchable by digest.
	byTag := getArtifacts(t, router, "repository=team/demo&tag=v1")
	tagged := artifactRecords(t, byTag)
	if len(tagged) != 1 || recordField(tagged[0], "digest") != testDigestB {
		t.Fatalf("tag v1 does not resolve to B: %v", tagged)
	}
	if recordField(tagged[0], "pushed_at") != pushedB {
		t.Fatalf("B pushed_at changed: got %v, want %v", recordField(tagged[0], "pushed_at"), pushedB)
	}

	byDigest := getArtifacts(t, router, "repository=team/demo&digest="+testDigestA)
	if byDigest.Code != http.StatusOK {
		t.Fatalf("digest query for A: status = %d (%s)", byDigest.Code, byDigest.Body)
	}
	byDigestRecords := artifactRecords(t, byDigest)
	if len(byDigestRecords) != 1 || recordField(byDigestRecords[0], "digest") != testDigestA ||
		recordField(byDigestRecords[0], "pushed_at") != pushedA {
		t.Fatalf("A no longer fetchable with its original pushed_at: %v", byDigestRecords)
	}
}

// TestConflictMatrixKeepsRecordAndTag changes exactly one content field at a
// time for an existing identity: every variation is 409 ArtifactConflictError,
// and neither the stored record nor the tag pointer may change.
func TestConflictMatrixKeepsRecordAndTag(t *testing.T) {
	cases := map[string]string{
		"tag changed":       demoRegistration(testDigestA, "v2", true, 30, 1024),
		"signature changed": demoRegistration(testDigestA, "v1", false, 30, 1024),
		"retention changed": demoRegistration(testDigestA, "v1", true, 7, 1024),
		"size changed":      demoRegistration(testDigestA, "v1", true, 30, 2048),
	}
	for name, conflictBody := range cases {
		t.Run(name, func(t *testing.T) {
			router, _ := newTestRouter(t)
			original := postArtifact(t, router, demoRegistration(testDigestA, "v1", true, 30, 1024))
			pushedA := decodeBody(t, original)["pushed_at"]

			recorder := postArtifact(t, router, conflictBody)
			if recorder.Code != http.StatusConflict {
				t.Fatalf("status = %d, want %d (%s)", recorder.Code, http.StatusConflict, recorder.Body)
			}
			if code := errorCode(t, recorder); code != codeConflict {
				t.Fatalf("error code = %q, want %q", code, codeConflict)
			}

			// The tag pointer must not move -- not even for the request that
			// named a different tag.
			tagged := artifactRecords(t, getArtifacts(t, router, "repository=team/demo&tag=v1"))
			if len(tagged) != 1 || recordField(tagged[0], "digest") != testDigestA {
				t.Fatalf("tag pointer changed after conflict: %v", tagged)
			}

			// The stored record keeps every original field and pushed_at.
			byDigest := artifactRecords(t, getArtifacts(t, router, "repository=team/demo&digest="+testDigestA))
			record := byDigest[0].(map[string]any)
			want := map[string]any{
				"tag": "v1", "signature_verified": true, "retention_days": float64(30),
				"size_bytes": float64(1024), "pushed_at": pushedA,
			}
			for key, value := range want {
				if record[key] != value {
					t.Fatalf("field %s = %v, want %v (record mutated by conflict)", key, record[key], value)
				}
			}

			// Conflict writes nothing: still exactly one record.
			records := artifactRecords(t, getArtifacts(t, router, "repository=team/demo"))
			if len(records) != 1 {
				t.Fatalf("conflict changed record count: got %d, want 1", len(records))
			}
		})
	}
}

// TestWhitespaceNormalizedDuplicate confirms identity/content comparison runs
// after TrimSpace on repository and tag, while digest is matched untrimmed.
func TestWhitespaceNormalizedDuplicate(t *testing.T) {
	router, _ := newTestRouter(t)
	original := postArtifact(t, router, demoRegistration(testDigestA, "v1", true, 30, 1024))
	pushedA := decodeBody(t, original)["pushed_at"]

	recorder := postArtifact(t, router, `{"repository":"  team/demo  ","digest":"`+testDigestA+`",`+
		`"tag":"  v1  ","signature_verified":true,"retention_days":30,`+
		`"size_bytes":1024,"unexpected_field":"ignored"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("normalized duplicate: status = %d, want %d (%s)", recorder.Code, http.StatusCreated, recorder.Body)
	}
	if got := decodeBody(t, recorder)["pushed_at"]; got != pushedA {
		t.Fatalf("normalized duplicate returned new pushed_at %v, want %v", got, pushedA)
	}
	records := artifactRecords(t, getArtifacts(t, router, "repository=team/demo"))
	if len(records) != 1 {
		t.Fatalf("duplicate inserted a record: got %d, want 1", len(records))
	}

	// digest is NOT trimmed: surrounding whitespace breaks the strict pattern.
	untidied := postArtifact(t, router, demoRegistration(" "+testDigestA+" ", "v1", true, 30, 1024))
	if untidied.Code != http.StatusBadRequest || errorCode(t, untidied) != codeInvalidInput {
		t.Fatalf("whitespace digest: status = %d, want 400 %s", untidied.Code, codeInvalidInput)
	}
}

// TestClientCannotOverridePushedAt verifies pushed_at is server-generated and a
// client-supplied value is just an ignored unknown field.
func TestClientCannotOverridePushedAt(t *testing.T) {
	router, _ := newTestRouter(t)
	body := `{"repository":"team/demo","digest":"` + testDigestA + `","tag":"v1",` +
		`"signature_verified":true,"retention_days":30,"size_bytes":1024,` +
		`"pushed_at":"1999-01-01T00:00:00Z"}`
	recorder := postArtifact(t, router, body)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d (%s)", recorder.Code, http.StatusCreated, recorder.Body)
	}
	pushedAt := decodeBody(t, recorder)["pushed_at"].(string)
	if pushedAt == "1999-01-01T00:00:00Z" {
		t.Fatalf("client-supplied pushed_at was honored: %s", pushedAt)
	}
	parsed, err := time.Parse(time.RFC3339, pushedAt)
	if err != nil || parsed.Location() != time.UTC {
		t.Fatalf("pushed_at %q is not a UTC RFC3339 timestamp: %v", pushedAt, err)
	}
}

// TestSameDigestAndTagInOtherRepositoryIndependent shows identity and tag
// pointers are both scoped by repository.
func TestSameDigestAndTagInOtherRepositoryIndependent(t *testing.T) {
	router, _ := newTestRouter(t)
	postArtifact(t, router, demoRegistration(testDigestA, "v1", true, 30, 1024))
	postArtifact(t, router, demoRegistration(testDigestB, "v1", true, 30, 1024))

	// Same digest and tag in a different repository is a fresh registration.
	other := postArtifact(t, router, registrationBody("team/other", testDigestA, "v1", true, 30, 1024))
	if other.Code != http.StatusCreated {
		t.Fatalf("other repository: status = %d, want %d (%s)", other.Code, http.StatusCreated, other.Body)
	}

	// Each repository's v1 resolves to its own record.
	demoTagged := artifactRecords(t, getArtifacts(t, router, "repository=team/demo&tag=v1"))
	if recordField(demoTagged[0], "digest") != testDigestB {
		t.Fatalf("demo tag changed: %v", demoTagged)
	}
	otherTagged := artifactRecords(t, getArtifacts(t, router, "repository=team/other&tag=v1"))
	if recordField(otherTagged[0], "digest") != testDigestA ||
		recordField(otherTagged[0], "repository") != "team/other" {
		t.Fatalf("other tag does not resolve independently: %v", otherTagged)
	}

	if records := artifactRecords(t, getArtifacts(t, router, "repository=team/demo")); len(records) != 2 {
		t.Fatalf("demo list = %d records, want 2", len(records))
	}
	if records := artifactRecords(t, getArtifacts(t, router, "repository=team/other")); len(records) != 1 {
		t.Fatalf("other list = %d records, want 1", len(records))
	}
}

// TestRetentionAndSignatureAreRecordedButNotActedOn records the observable
// surface of "saved only": both values round-trip, and no mutating HTTP method
// exists that the service could use to delete/expire a record.
func TestRetentionAndSignatureAreRecordedButNotActedOn(t *testing.T) {
	router, _ := newTestRouter(t)
	// signature_verified=false is accepted as-is; retention of 1 day is stored.
	recorder := postArtifact(t, router, demoRegistration(testDigestA, "v1", false, 1, 0))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d (%s)", recorder.Code, http.StatusCreated, recorder.Body)
	}
	created := decodeBody(t, recorder)
	if created["signature_verified"] != false || created["retention_days"] != float64(1) ||
		created["size_bytes"] != float64(0) {
		t.Fatalf("fields not recorded as submitted: %v", created)
	}

	// No delete/update route is exposed for artifacts (gin answers 404 unless
	// HandleMethodNotAllowed is enabled, which NewRouter does not do).
	for _, method := range []string{http.MethodDelete, http.MethodPut} {
		tried := serveRaw(t, router, method, "/v1/artifacts",
			demoRegistration(testDigestA, "v1", false, 1, 0))
		if tried.Code != http.StatusNotFound {
			t.Fatalf("%s /v1/artifacts: status = %d, want 404", method, tried.Code)
		}
	}

	// The record is still present and unchanged through the public query path.
	byDigest := artifactRecords(t, getArtifacts(t, router, "repository=team/demo&digest="+testDigestA))
	if len(byDigest) != 1 || recordField(byDigest[0], "retention_days") != float64(1) ||
		recordField(byDigest[0], "signature_verified") != false {
		t.Fatalf("record changed unexpectedly: %v", byDigest)
	}
}

// TestStorageUnavailableWhenDatabaseClosed exercises the 503 path on every
// read/write entry after the database handle becomes unusable.
func TestStorageUnavailableWhenDatabaseClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	router := NewRouter(st)
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	check := func(t *testing.T, recorder *httptest.ResponseRecorder) {
		t.Helper()
		if recorder.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want %d (%s)", recorder.Code, http.StatusServiceUnavailable, recorder.Body)
		}
		if code := errorCode(t, recorder); code != codeStorage {
			t.Fatalf("error code = %q, want %q", code, codeStorage)
		}
	}

	t.Run("post", func(t *testing.T) {
		check(t, postArtifact(t, router, demoRegistration(testDigestA, "v1", true, 30, 1024)))
	})
	t.Run("get", func(t *testing.T) {
		check(t, getArtifacts(t, router, "repository=team/demo"))
	})
	t.Run("healthz", func(t *testing.T) {
		check(t, serveRaw(t, router, http.MethodGet, "/healthz", ""))
	})
}

// TestErrorMessagesDoNotLeakInternals checks every error class returned by the
// artifact endpoints keeps the fixed {code,message} shape and never surfaces
// SQL, driver text, stack frames or file paths.
func TestErrorMessagesDoNotLeakInternals(t *testing.T) {
	router, _ := newTestRouter(t)
	postArtifact(t, router, demoRegistration(testDigestA, "v1", true, 30, 1024))

	responses := map[string]*httptest.ResponseRecorder{
		"bad request": postArtifact(t, router, `{"repository":`),
		"not found":   getArtifacts(t, router, "repository=team/missing"),
		"conflict":    postArtifact(t, router, demoRegistration(testDigestA, "v2", true, 30, 1024)),
	}
	path := filepath.Join(t.TempDir(), "closed.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	closedRouter := NewRouter(st)
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	responses["storage unavailable"] = postArtifact(t, closedRouter, demoRegistration(testDigestA, "v1", true, 30, 1024))

	leakMarkers := []string{"SELECT", "INSERT", "UPDATE", "DELETE", "sql:", "SQLITE",
		".go:", "goroutine", "/home/", `\`, "artifacts("}
	for name, recorder := range responses {
		t.Run(name, func(t *testing.T) {
			body := decodeBody(t, recorder)
			errObj, ok := body["error"].(map[string]any)
			if !ok {
				t.Fatalf("no error object: %v", body)
			}
			code, codeOK := errObj["code"].(string)
			message, msgOK := errObj["message"].(string)
			if !codeOK || code == "" || !msgOK || message == "" {
				t.Fatalf("error object lacks non-empty code/message strings: %v", errObj)
			}
			lowered := strings.ToLower(message)
			for _, marker := range leakMarkers {
				if strings.Contains(message, marker) || strings.Contains(lowered, strings.ToLower(marker)) {
					t.Fatalf("message %q leaks internal marker %q", message, marker)
				}
			}
		})
	}
}

// TestTagPointerAndRecordsSurviveRestart reopens the database file after a tag
// has moved and verifies records, pushed_at timestamps, list order and the tag
// pointer all survive.
func TestTagPointerAndRecordsSurviveRestart(t *testing.T) {
	router, path := newTestRouter(t)
	first := postArtifact(t, router, demoRegistration(testDigestA, "v1", true, 30, 1024))
	pushedA := decodeBody(t, first)["pushed_at"]
	second := postArtifact(t, router, demoRegistration(testDigestB, "v1", true, 30, 1024))
	pushedB := decodeBody(t, second)["pushed_at"]

	reopened, err := store.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	router = NewRouter(reopened)

	records := artifactRecords(t, getArtifacts(t, router, "repository=team/demo"))
	if len(records) != 2 ||
		recordField(records[0], "digest") != testDigestA ||
		recordField(records[1], "digest") != testDigestB ||
		recordField(records[0], "pushed_at") != pushedA ||
		recordField(records[1], "pushed_at") != pushedB {
		t.Fatalf("records/order/timestamps did not survive restart: %v", records)
	}
	tagged := artifactRecords(t, getArtifacts(t, router, "repository=team/demo&tag=v1"))
	if recordField(tagged[0], "digest") != testDigestB {
		t.Fatalf("tag pointer did not survive restart: %v", tagged)
	}
}
