package api

// Compatibility regression cases for the shared field rules on the HTTP
// surface: the repository/tag trim and the digest format are one
// implementation each behind ParseRegistration and ParseQuery, so the same
// probe must produce the same status code and the same normalized record
// whether the router is assembled over the bundled SQLite store (NewRouter)
// or over a caller-supplied service.Store (NewRouterWithStore). Each probe
// is also run through the direct input entry and the two outcomes are
// compared; on the custom-store assembly the captured store proves what the
// service was handed and that rejections never reach storage.

import (
	"errors"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/Bob-xisuke/orb-registry-catalog/internal/input"
	"github.com/Bob-xisuke/orb-registry-catalog/internal/service"
	"github.com/Bob-xisuke/orb-registry-catalog/internal/store"
)

// sharedRuleAssemblies builds one router per supported storage assembly. The
// returned captureStore is nil for the SQLite assembly, where dispatched
// inputs are observed through the query endpoints instead.
func sharedRuleAssemblies(t *testing.T) map[string]struct {
	router  *gin.Engine
	capture *captureStore
} {
	t.Helper()

	sqliteStore, err := store.Open(filepath.Join(t.TempDir(), "service.db"))
	if err != nil {
		t.Fatalf("open sqlite store: %v", err)
	}
	t.Cleanup(func() { sqliteStore.Close() })

	custom := newCaptureStore()
	return map[string]struct {
		router  *gin.Engine
		capture *captureStore
	}{
		"sqlite":       {NewRouter(sqliteStore), nil},
		"custom store": {NewRouterWithStore(custom, nil), custom},
	}
}

// TestSharedFieldRulesConsistentAcrossAssemblies registers through a body
// whose repository and tag carry Unicode edge whitespace, then resolves the
// record through a percent-encoded query carrying the same padding: both
// assemblies must normalize to the direct ParseRegistration/ParseQuery
// results.
func TestSharedFieldRulesConsistentAcrossAssemblies(t *testing.T) {
	for name, assembly := range sharedRuleAssemblies(t) {
		t.Run(name, func(t *testing.T) {
			router := assembly.router

			// NBSP before, em space after — JSON \u-escapes in the body.
			body := `{"repository":"\u00a0registry-demo/shared\u2003","digest":"` + testDigestA +
				`","tag":"\u2003release\u00a0","signature_verified":true,"retention_days":30,"size_bytes":1024}`
			direct, err := input.ParseRegistration(strings.NewReader(body))
			if err != nil {
				t.Fatalf("direct ParseRegistration: %v", err)
			}
			if direct.Repository != "registry-demo/shared" || direct.Tag != "release" {
				t.Fatalf("direct normalization = %+v", direct)
			}

			recorder := postArtifact(t, router, body)
			if recorder.Code != http.StatusCreated {
				t.Fatalf("register status = %d, want 201 (%s)", recorder.Code, recorder.Body)
			}
			response := decodeBody(t, recorder)
			if response["repository"] != direct.Repository || response["tag"] != direct.Tag {
				t.Fatalf("response names = %v/%v, want direct %q/%q",
					response["repository"], response["tag"], direct.Repository, direct.Tag)
			}
			if assembly.capture != nil {
				if assembly.capture.regCalls != 1 {
					t.Fatalf("store Register calls = %d, want 1", assembly.capture.regCalls)
				}
				recordMatchesInput(t, assembly.capture.lastRecord, direct)
			}

			// The same padding percent-encoded in the query string resolves
			// the very record the body registered.
			raw := "repository=%C2%A0registry-demo/shared%E2%80%83&tag=%E2%80%83release%C2%A0"
			directQuery, err := input.ParseQuery(raw)
			if err != nil {
				t.Fatalf("direct ParseQuery: %v", err)
			}
			if directQuery != (service.Query{Kind: service.QueryByTag, Repository: "registry-demo/shared", Tag: "release"}) {
				t.Fatalf("direct query = %+v", directQuery)
			}
			tagged := soleRecord(t, getArtifacts(t, router, raw))
			if tagged["digest"] != testDigestA {
				t.Fatalf("padded tag query resolved %v, want the registered record", tagged)
			}
			if assembly.capture != nil && !reflect.DeepEqual(assembly.capture.lastQuery, directQuery) {
				t.Fatalf("dispatched query = %+v, want direct result %+v",
					assembly.capture.lastQuery, directQuery)
			}

			// A padded repository-only query is the same list on both entries.
			list := allRecords(t, getArtifacts(t, router, "repository=+registry-demo/shared+"))
			if len(list) != 1 {
				t.Fatalf("padded list returned %d records, want 1", len(list))
			}
		})
	}
}

// TestSharedFieldRuleRejectionsAcrossAssemblies pins the invalid shared-rule
// rows on both assemblies: the direct entry rejects with ErrInvalidInput and
// a zero result, HTTP answers the published 400, and the custom store proves
// storage is never touched.
func TestSharedFieldRuleRejectionsAcrossAssemblies(t *testing.T) {
	invalidBodies := map[string]string{
		"repository blank unicode": `{"repository":"\u00a0\u2003","digest":"` + testDigestA + `","tag":"release","signature_verified":true,"retention_days":30,"size_bytes":1}`,
		"tag blank unicode":        `{"repository":"registry-demo/shared","digest":"` + testDigestA + `","tag":"\u00a0\u2003","signature_verified":true,"retention_days":30,"size_bytes":1}`,
		"digest edge space":        `{"repository":"registry-demo/shared","digest":" ` + testDigestA + `","tag":"release","signature_verified":true,"retention_days":30,"size_bytes":1}`,
	}
	invalidQueries := map[string]string{
		"repository blank unicode": "repository=%C2%A0%E2%80%83&tag=release",
		"tag blank unicode":        "repository=registry-demo/shared&tag=%C2%A0",
		"digest edge space":        "repository=registry-demo/shared&digest=+" + testDigestA,
	}

	for name, assembly := range sharedRuleAssemblies(t) {
		t.Run(name, func(t *testing.T) {
			for row, body := range invalidBodies {
				direct, err := input.ParseRegistration(strings.NewReader(body))
				if !errors.Is(err, input.ErrInvalidInput) || direct != (service.RegisterInput{}) {
					t.Fatalf("%s: direct = (%+v, %v), want zero + ErrInvalidInput", row, direct, err)
				}
				assertErrorResponse(t, postArtifact(t, assembly.router, body),
					http.StatusBadRequest, codeInvalidInput, msgInvalidReg)
			}
			for row, raw := range invalidQueries {
				direct, err := input.ParseQuery(raw)
				if !errors.Is(err, input.ErrInvalidInput) || direct != (service.Query{}) {
					t.Fatalf("%s: direct = (%+v, %v), want zero + ErrInvalidInput", row, direct, err)
				}
				assertErrorResponse(t, getArtifacts(t, assembly.router, raw),
					http.StatusBadRequest, codeInvalidInput, msgInvalidQuery)
			}
			if assembly.capture != nil &&
				(assembly.capture.regCalls != 0 || assembly.capture.readCalls != 0) {
				t.Fatalf("rejected shared-rule rows reached storage: %d register, %d read calls",
					assembly.capture.regCalls, assembly.capture.readCalls)
			}
		})
	}
}
