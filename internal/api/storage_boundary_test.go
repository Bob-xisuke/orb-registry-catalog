package api

// These regression cases pin the storage-integration boundary analyzed in
// docs/storage-boundary-analysis.md: how the service and the router treat a
// caller-supplied service.Store whose backend wraps the miss sentinel, returns
// a register status together with an error, or is paired with a nil health
// check. They run only against the public HTTP surface (POST/GET
// /v1/artifacts, GET /healthz) through NewRouterWithStore.

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/Bob-xisuke/orb-registry-catalog/internal/service"
)

// boundaryStore is a caller-supplied service.Store written only against the
// exported storage contract. It keeps records in memory like the SQLite
// adapter and adds three backend behaviors the contract leaves to the
// implementation: a wrapped miss sentinel, a register status returned
// together with an error, and a plain backend failure.
type boundaryStore struct {
	records []service.Record
	byID    map[string]int               // repository\x00digest -> records index
	tags    map[string]map[string]string // repository -> tag -> digest

	fault          error                  // non-nil: every method fails with it
	missWrapper    func(error) error      // non-nil: transforms the lookup miss sentinel
	registerStatus service.RegisterStatus // status Register returns together with fault
}

func newBoundaryStore() *boundaryStore {
	return &boundaryStore{
		byID: make(map[string]int),
		tags: make(map[string]map[string]string),
	}
}

func boundaryID(repository, digest string) string { return repository + "\x00" + digest }

func (b *boundaryStore) miss(err error) error {
	if b.missWrapper != nil {
		return b.missWrapper(err)
	}
	return err
}

func (b *boundaryStore) Register(in service.Record) (service.Record, service.RegisterStatus, error) {
	if b.fault != nil {
		return service.Record{}, b.registerStatus, b.fault
	}
	key := boundaryID(in.Repository, in.Digest)
	if idx, ok := b.byID[key]; ok {
		existing := b.records[idx]
		if existing.Tag == in.Tag &&
			existing.SignatureVerified == in.SignatureVerified &&
			existing.RetentionDays == in.RetentionDays &&
			existing.SizeBytes == in.SizeBytes {
			return existing, service.StatusDuplicate, nil
		}
		return service.Record{}, service.StatusConflict, nil
	}
	b.byID[key] = len(b.records)
	b.records = append(b.records, in)
	if b.tags[in.Repository] == nil {
		b.tags[in.Repository] = make(map[string]string)
	}
	b.tags[in.Repository][in.Tag] = in.Digest
	return in, service.StatusCreated, nil
}

func (b *boundaryStore) ListRecords(repository string) ([]service.Record, error) {
	if b.fault != nil {
		return nil, b.fault
	}
	var out []service.Record
	for _, record := range b.records {
		if record.Repository == repository {
			out = append(out, record)
		}
	}
	return out, nil
}

func (b *boundaryStore) RecordByDigest(repository, digest string) (service.Record, error) {
	if b.fault != nil {
		return service.Record{}, b.fault
	}
	if idx, ok := b.byID[boundaryID(repository, digest)]; ok {
		return b.records[idx], nil
	}
	return service.Record{}, b.miss(service.ErrRecordNotFound)
}

func (b *boundaryStore) RecordByTag(repository, tag string) (service.Record, error) {
	if b.fault != nil {
		return service.Record{}, b.fault
	}
	digest, ok := b.tags[repository][tag]
	if !ok {
		return service.Record{}, b.miss(service.ErrRecordNotFound)
	}
	return b.records[b.byID[boundaryID(repository, digest)]], nil
}

// TestWrappedRecordNotFoundStillMapsTo404 has the backend wrap the miss
// sentinel with its own message, as a real adapter would. The service must
// still classify it through errors.Is as a miss — 404 ArtifactNotFoundError,
// never 503 — while committed data keeps reading back normally.
func TestWrappedRecordNotFoundStillMapsTo404(t *testing.T) {
	st := newBoundaryStore()
	st.missWrapper = func(err error) error {
		return fmt.Errorf("boundary backend lookup: %w", err)
	}
	router := NewRouterWithStore(st, nil)

	// Seed one record so the lookups below are legal queries against committed
	// state whose tag/digest simply does not exist.
	if recorder := postArtifact(t, router,
		registration(sequenceRepo, testDigestA, sequenceTag, true, regRetention, regSizeA)); recorder.Code != http.StatusCreated {
		t.Fatalf("seed register status = %d (%s)", recorder.Code, recorder.Body)
	}

	// A miss wrapped by the backend is still a miss: 404, not 503.
	assertErrorResponse(t, queryByTag(t, router, sequenceRepo, "missing-tag"),
		http.StatusNotFound, codeNotFound, msgNotFound)
	assertErrorResponse(t, queryByDigest(t, router, sequenceRepo, testDigestB),
		http.StatusNotFound, codeNotFound, msgNotFound)

	// The committed record is unaffected by the wrapped misses.
	if got := soleRecord(t, queryByTag(t, router, sequenceRepo, sequenceTag)); got["digest"] != testDigestA {
		t.Fatalf("tag query after wrapped misses = %v, want the seeded record", got)
	}
}

// TestRegisterStatusPlusErrorMapsTo503 returns a register status together with
// a backend error for every status variant. The service checks the error
// before the status, so the response is always 503 storage_unavailable —
// never 201 and never 409 — and the failed register commits nothing.
func TestRegisterStatusPlusErrorMapsTo503(t *testing.T) {
	statuses := map[string]service.RegisterStatus{
		"created with error":   service.StatusCreated,
		"duplicate with error": service.StatusDuplicate,
		"conflict with error":  service.StatusConflict,
	}
	for name, status := range statuses {
		t.Run(name, func(t *testing.T) {
			st := newBoundaryStore()
			st.fault = errors.New("boundary backend failed mid-register")
			st.registerStatus = status
			router := NewRouterWithStore(st, nil)

			// The error wins over any status the backend pairs it with.
			assertErrorResponse(t, postArtifact(t, router,
				registration(sequenceRepo, testDigestA, sequenceTag, true, regRetention, regSizeA)),
				http.StatusServiceUnavailable, codeStorage, msgStorage)

			// The failed register left no record behind: once the backend
			// recovers, the repository is still empty.
			st.fault = nil
			assertErrorResponse(t, queryList(t, router, sequenceRepo),
				http.StatusNotFound, codeNotFound, msgNotFound)
		})
	}
}

// TestNilHealthCheckWithFailingStore pairs a failing store with a nil health
// check through the assembly entry. The health check answers for itself
// alone: the /healthz response is the published 200 body while legal artifact
// requests degrade to 503 storage_unavailable.
func TestNilHealthCheckWithFailingStore(t *testing.T) {
	st := newBoundaryStore()
	router := NewRouterWithStore(st, nil)

	// The nil health check reports the service as healthy.
	if recorder := getHealth(t, router); recorder.Code != http.StatusOK ||
		recorder.Body.String() != `{"database":"ok","status":"ok"}` {
		t.Fatalf("nil health check = %d %s", recorder.Code, recorder.Body)
	}

	// A failing store degrades only the artifact entries: the health check
	// stays 200 because no check was provided, and it never gates them.
	st.fault = errors.New("boundary backend down")
	if recorder := getHealth(t, router); recorder.Code != http.StatusOK ||
		recorder.Body.String() != `{"database":"ok","status":"ok"}` {
		t.Fatalf("nil health check with failing store = %d %s", recorder.Code, recorder.Body)
	}
	assertErrorResponse(t, postArtifact(t, router,
		registration(sequenceRepo, testDigestA, sequenceTag, true, regRetention, regSizeA)),
		http.StatusServiceUnavailable, codeStorage, msgStorage)
	assertErrorResponse(t, queryList(t, router, sequenceRepo),
		http.StatusServiceUnavailable, codeStorage, msgStorage)
	assertErrorResponse(t, queryByTag(t, router, sequenceRepo, sequenceTag),
		http.StatusServiceUnavailable, codeStorage, msgStorage)
	assertErrorResponse(t, queryByDigest(t, router, sequenceRepo, testDigestA),
		http.StatusServiceUnavailable, codeStorage, msgStorage)
}
