package service_test

import (
	"errors"
	"testing"

	"github.com/Bob-xisuke/orb-registry-catalog/internal/service"
)

// customStore is a caller-supplied storage implementation written entirely
// against the exported service contract: it never imports internal/store and
// uses none of its types. It proves a third party can back the business layer
// with its own backend by satisfying service.Store.
type customStore struct {
	records []service.Record
	index   map[string]int // repository + "\x00" + digest
	tags    map[string]map[string]string
	fault   error
}

func newCustomStore() *customStore {
	return &customStore{
		index: make(map[string]int),
		tags:  make(map[string]map[string]string),
	}
}

func customID(repo, digest string) string { return repo + "\x00" + digest }

func (c *customStore) Register(in service.Record) (service.Record, service.RegisterStatus, error) {
	if c.fault != nil {
		return service.Record{}, 0, c.fault
	}
	if i, ok := c.index[customID(in.Repository, in.Digest)]; ok {
		ex := c.records[i]
		if ex.Tag == in.Tag && ex.SignatureVerified == in.SignatureVerified &&
			ex.RetentionDays == in.RetentionDays && ex.SizeBytes == in.SizeBytes {
			return ex, service.StatusDuplicate, nil
		}
		return service.Record{}, service.StatusConflict, nil
	}
	c.index[customID(in.Repository, in.Digest)] = len(c.records)
	c.records = append(c.records, in)
	if c.tags[in.Repository] == nil {
		c.tags[in.Repository] = make(map[string]string)
	}
	c.tags[in.Repository][in.Tag] = in.Digest
	return in, service.StatusCreated, nil
}

func (c *customStore) ListRecords(repo string) ([]service.Record, error) {
	if c.fault != nil {
		return nil, c.fault
	}
	var out []service.Record
	for _, r := range c.records {
		if r.Repository == repo {
			out = append(out, r)
		}
	}
	return out, nil
}

func (c *customStore) RecordByDigest(repo, digest string) (service.Record, error) {
	if c.fault != nil {
		return service.Record{}, c.fault
	}
	if i, ok := c.index[customID(repo, digest)]; ok {
		return c.records[i], nil
	}
	return service.Record{}, service.ErrRecordNotFound
}

func (c *customStore) RecordByTag(repo, tag string) (service.Record, error) {
	if c.fault != nil {
		return service.Record{}, c.fault
	}
	digest, ok := c.tags[repo][tag]
	if !ok {
		return service.Record{}, service.ErrRecordNotFound
	}
	return c.records[c.index[customID(repo, digest)]], nil
}

// TestAlternativeStorePlugsIn wires a fully custom service.Store into the
// business layer and exercises the same created / duplicate / conflict /
// not-found / storage behavior the SQLite adapter provides, confirming the
// business outcomes depend only on the port.
func TestAlternativeStorePlugsIn(t *testing.T) {
	st := newCustomStore()
	var _ service.Store = st // the custom backend satisfies the exported port
	svc := service.New(st)

	reg := service.RegisterInput{
		Repository:        "team/app",
		Digest:            sqliteDigestA,
		Tag:               "latest",
		SignatureVerified: true,
		RetentionDays:     30,
		SizeBytes:         1024,
	}

	first, err := svc.Register(reg)
	if err != nil || first.Outcome != service.OutcomeCreated {
		t.Fatalf("first register = %+v, %v; want created", first, err)
	}

	dup, err := svc.Register(reg)
	if err != nil || dup.Outcome != service.OutcomeDuplicate ||
		dup.Artifact.PushedAt != first.Artifact.PushedAt {
		t.Fatalf("duplicate = %+v, %v; want duplicate with original pushed_at", dup, err)
	}

	// A different digest under the same tag moves the pointer; the old record
	// survives by digest.
	regB := reg
	regB.Digest = sqliteDigestB
	regB.SizeBytes = 2048
	if _, err := svc.Register(regB); err != nil {
		t.Fatalf("register B: %v", err)
	}
	byTag, err := svc.Query(service.Query{Kind: service.QueryByTag, Repository: "team/app", Tag: "latest"})
	if err != nil || len(byTag) != 1 || byTag[0].Digest != sqliteDigestB {
		t.Fatalf("tag = %+v, %v; want B", byTag, err)
	}
	byDigest, err := svc.Query(service.Query{Kind: service.QueryByDigest, Repository: "team/app", Digest: sqliteDigestA})
	if err != nil || len(byDigest) != 1 || byDigest[0] != first.Artifact {
		t.Fatalf("digest lookup = %+v, %v; want original A", byDigest, err)
	}

	// Repository list stays in first-registration order.
	list, err := svc.Query(service.Query{Kind: service.QueryByRepository, Repository: "team/app"})
	if err != nil || len(list) != 2 || list[0].Digest != sqliteDigestA || list[1].Digest != sqliteDigestB {
		t.Fatalf("list = %+v, %v; want [A B]", list, err)
	}

	// Changed content for an existing identity is a conflict and changes nothing.
	conflict := reg
	conflict.RetentionDays = 31
	if _, err := svc.Register(conflict); !errors.Is(err, service.ErrConflict) {
		t.Fatalf("conflict error = %v, want ErrConflict", err)
	}
	again, _ := svc.Query(service.Query{Kind: service.QueryByDigest, Repository: "team/app", Digest: sqliteDigestA})
	if again[0].RetentionDays != 30 {
		t.Fatalf("record mutated by conflict: %+v", again)
	}

	// A legal miss and an empty repository are ErrNotFound.
	if _, err := svc.Query(service.Query{Kind: service.QueryByTag, Repository: "team/app", Tag: "nope"}); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("missing tag error = %v, want ErrNotFound", err)
	}
	if _, err := svc.Query(service.Query{Kind: service.QueryByRepository, Repository: "other/x"}); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("empty repo error = %v, want ErrNotFound", err)
	}

	// A backend-native fault surfaces as ErrStorage, never ErrNotFound.
	st.fault = errors.New("custom backend down")
	if _, err := svc.Register(regB); !errors.Is(err, service.ErrStorage) {
		t.Fatalf("faulty register error = %v, want ErrStorage", err)
	}
	if _, err := svc.Query(service.Query{Kind: service.QueryByRepository, Repository: "team/app"}); !errors.Is(err, service.ErrStorage) {
		t.Fatalf("faulty list error = %v, want ErrStorage", err)
	}
}
