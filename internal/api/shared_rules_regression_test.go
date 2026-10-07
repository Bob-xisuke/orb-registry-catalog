package api

// These regression cases pin the shared repository/tag/digest rules on the
// public HTTP surface across both supported assemblies — the bundled SQLite
// store behind NewRouter and a caller-supplied store behind
// NewRouterWithStore. A name normalized one way at registration must be
// reachable by the identically normalized query, blank and edge-padded
// values must be the same 400 on POST and GET, and /healthz stays untouched.

import (
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
)

// sharedRuleAssemblies builds one router per supported storage assembly.
func sharedRuleAssemblies(t *testing.T) map[string]*gin.Engine {
	t.Helper()
	sqlite, _ := newTestRouter(t)
	return map[string]*gin.Engine{
		"sqlite":       sqlite,
		"custom store": newAssemblyRouter(newAssemblyStore(), &assemblyProbe{}),
	}
}

// TestSharedNameRuleConsistentAcrossEntriesAndAssemblies registers a record
// whose repository and tag carry Unicode whitespace padding, then queries it
// back with the same padding percent-encoded: both entries run the one
// shared decode-then-trim rule, so the registration is reachable and the
// stored values are the trimmed ones.
func TestSharedNameRuleConsistentAcrossEntriesAndAssemblies(t *testing.T) {
	for name, router := range sharedRuleAssemblies(t) {
		t.Run(name, func(t *testing.T) {
			// \u00a0 and \u2003 are the JSON escapes for NBSP and em space; \t is a tab.
			body := `{"repository":"\u00a0registry-demo/shared\u2003","digest":"` + testDigestA +
				`","tag":"\trelease\t","signature_verified":true,"retention_days":30,"size_bytes":1024}`
			recorder := postArtifact(t, router, body)
			if recorder.Code != http.StatusCreated {
				t.Fatalf("register status = %d, want %d (%s)", recorder.Code, http.StatusCreated, recorder.Body)
			}
			record := decodeBody(t, recorder)
			if record["repository"] != "registry-demo/shared" || record["tag"] != "release" {
				t.Fatalf("stored values not trimmed: %v", record)
			}
			pushedAt := record["pushed_at"]

			// The same padding percent-encoded (%C2%A0 = NBSP, %E2%80%83 = em
			// space, %09 = tab) selects the registered record.
			padded := getArtifacts(t, router,
				"repository=%C2%A0registry-demo/shared%E2%80%83&tag=%09release%09")
			if padded.Code != http.StatusOK {
				t.Fatalf("padded query status = %d, want %d (%s)", padded.Code, http.StatusOK, padded.Body)
			}
			got := soleRecord(t, padded)
			if got["digest"] != testDigestA || got["pushed_at"] != pushedAt {
				t.Fatalf("padded query record = %v, want the registered one", got)
			}

			// Interior characters and case are preserved by both entries: the
			// mixed-case, interior-spaced identity registers and queries back.
			interior := postArtifact(t, router, `{"repository":" Team/App  Name ","digest":"`+testDigestB+
				`","tag":" Rel+A  B ","signature_verified":false,"retention_days":1,"size_bytes":0}`)
			if interior.Code != http.StatusCreated {
				t.Fatalf("interior register status = %d (%s)", interior.Code, interior.Body)
			}
			interiorRecord := decodeBody(t, interior)
			if interiorRecord["repository"] != "Team/App  Name" || interiorRecord["tag"] != "Rel+A  B" {
				t.Fatalf("interior characters or case lost: %v", interiorRecord)
			}
			hit := getArtifacts(t, router, "repository=+Team/App++Name+&tag=+Rel%2BA++B+")
			if hit.Code != http.StatusOK {
				t.Fatalf("interior query status = %d, want %d (%s)", hit.Code, http.StatusOK, hit.Body)
			}
			if got := soleRecord(t, hit)["digest"]; got != testDigestB {
				t.Fatalf("interior query digest = %v, want %s", got, testDigestB)
			}

			// The health entry is untouched by the shared input rules.
			if recorder := getHealth(t, router); recorder.Code != http.StatusOK {
				t.Fatalf("/healthz = %d, want %d", recorder.Code, http.StatusOK)
			}
		})
	}
}

// TestSharedRuleRejectionsIdenticalAcrossEntriesAndAssemblies pins the
// rejection parity the shared helpers guarantee: an all-whitespace
// repository or tag and an edge-padded digest are the same published 400 on
// POST and GET alike, on both assemblies, and never reach storage.
func TestSharedRuleRejectionsIdenticalAcrossEntriesAndAssemblies(t *testing.T) {
	for name, router := range sharedRuleAssemblies(t) {
		t.Run(name, func(t *testing.T) {
			// POST rejections: Unicode-blank names and a padded digest.
			postBodies := map[string]string{
				"repository unicode blank": `{"repository":"\u00a0\u2003","digest":"` + testDigestA +
					`","tag":"release","signature_verified":true,"retention_days":30,"size_bytes":1}`,
				"tag unicode blank": `{"repository":"registry-demo/shared","digest":"` + testDigestA +
					`","tag":"\u00a0","signature_verified":true,"retention_days":30,"size_bytes":1}`,
				"digest unicode padded": `{"repository":"registry-demo/shared","digest":"\u00a0` + testDigestA +
					`","tag":"release","signature_verified":true,"retention_days":30,"size_bytes":1}`,
			}
			for caseName, body := range postBodies {
				t.Run("post/"+caseName, func(t *testing.T) {
					assertErrorResponse(t, postArtifact(t, router, body),
						http.StatusBadRequest, codeInvalidInput, msgInvalidReg)
				})
			}

			// GET rejections: the same classes percent-encoded.
			getQueries := map[string]string{
				"repository unicode blank": "repository=%C2%A0%E2%80%83&tag=release",
				"tag unicode blank":        "repository=registry-demo/shared&tag=%C2%A0",
				"digest unicode padded":    "repository=registry-demo/shared&digest=%C2%A0" + testDigestA,
				"digest trailing padded":   "repository=registry-demo/shared&digest=" + testDigestA + "%E2%80%83",
			}
			for caseName, query := range getQueries {
				t.Run("get/"+caseName, func(t *testing.T) {
					assertErrorResponse(t, getArtifacts(t, router, query),
						http.StatusBadRequest, codeInvalidInput, msgInvalidQuery)
				})
			}

			// None of the rejections left a record behind.
			assertErrorResponse(t, queryList(t, router, "registry-demo/shared"),
				http.StatusNotFound, codeNotFound, msgNotFound)
		})
	}
}
