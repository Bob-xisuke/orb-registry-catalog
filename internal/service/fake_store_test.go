package service

import (
	"errors"
)

// errFakeStorage is a backend-native failure: it is deliberately not
// ErrRecordNotFound, so tests can prove the service reports a fault as
// ErrStorage rather than mistaking it for a miss.
var errFakeStorage = errors.New("fake: storage is unavailable")

// fakeStore is an in-memory service.Store used by the business tests. It models
// the same rules as the SQLite adapter — (repository, digest) identity,
// insertion-ordered records and a mutable per-repository tag pointer — without
// opening any database. Each method can be forced to fail independently.
type fakeStore struct {
	records []Record                     // global first-registration order
	byID    map[string]int               // repository\x00digest -> records index
	tags    map[string]map[string]string // repository -> tag -> digest
	fail    map[string]bool              // method name -> force a backend fault
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		byID: make(map[string]int),
		tags: make(map[string]map[string]string),
		fail: make(map[string]bool),
	}
}

func identityKey(repository, digest string) string { return repository + "\x00" + digest }

func (f *fakeStore) Register(a Record) (Record, RegisterStatus, error) {
	if f.fail["register"] {
		return Record{}, 0, errFakeStorage
	}
	key := identityKey(a.Repository, a.Digest)
	if idx, ok := f.byID[key]; ok {
		existing := f.records[idx]
		if existing.Tag == a.Tag &&
			existing.SignatureVerified == a.SignatureVerified &&
			existing.RetentionDays == a.RetentionDays &&
			existing.SizeBytes == a.SizeBytes {
			return existing, StatusDuplicate, nil
		}
		return Record{}, StatusConflict, nil
	}

	f.byID[key] = len(f.records)
	f.records = append(f.records, a)
	pointers := f.tags[a.Repository]
	if pointers == nil {
		pointers = make(map[string]string)
		f.tags[a.Repository] = pointers
	}
	pointers[a.Tag] = a.Digest
	return a, StatusCreated, nil
}

func (f *fakeStore) ListRecords(repository string) ([]Record, error) {
	if f.fail["list"] {
		return nil, errFakeStorage
	}
	var out []Record
	for _, r := range f.records {
		if r.Repository == repository {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeStore) RecordByDigest(repository, digest string) (Record, error) {
	if f.fail["digest"] {
		return Record{}, errFakeStorage
	}
	if idx, ok := f.byID[identityKey(repository, digest)]; ok {
		return f.records[idx], nil
	}
	return Record{}, ErrRecordNotFound
}

func (f *fakeStore) RecordByTag(repository, tag string) (Record, error) {
	if f.fail["tag"] {
		return Record{}, errFakeStorage
	}
	digest, ok := f.tags[repository][tag]
	if !ok {
		return Record{}, ErrRecordNotFound
	}
	idx, ok := f.byID[identityKey(repository, digest)]
	if !ok {
		return Record{}, ErrRecordNotFound
	}
	return f.records[idx], nil
}
