package auditlog

import (
	"cmp"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strconv"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
)

// hmacPrefix tags keyed fingerprints and signatures with their algorithm, so
// a future algorithm change is visible in the data.
const hmacPrefix = "hmac-sha256:"

// minKeyLen is the shortest HMACKey accepted: below 128 bits a key is
// guessable.
const minKeyLen = 16

// Integrity attribute keys on every record.
const (
	attrStream    = "gopheragent.audit.stream"
	attrSequence  = "gopheragent.audit.sequence"
	attrSignature = "gopheragent.audit.signature"
)

// Hash returns the keyed fingerprint of value — what ToolInvocation.ArgsHMAC
// holds. An auditor with the key computes Hash(key, knownArgs) to find the
// records of a known request; without the key the fingerprint reveals
// nothing, even for low-entropy values a plain hash would leak.
func Hash(key []byte, value string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(value))
	return hmacPrefix + hex.EncodeToString(m.Sum(nil))
}

// Record is the integrity-relevant view of one stored audit record.
type Record struct {
	Stream    string
	Sequence  uint64
	EventID   string
	EventName string
	Body      string
	Signature string
}

// RecordFromOTLP extracts a Record from an exported OTLP log record, for
// verification tooling reading from the audit store.
func RecordFromOTLP(lr *logspb.LogRecord) Record {
	r := Record{Body: lr.GetBody().GetStringValue()}
	for _, kv := range lr.GetAttributes() {
		v := kv.GetValue()
		switch kv.GetKey() {
		case attrStream:
			r.Stream = v.GetStringValue()
		case attrSequence:
			r.Sequence = uint64(v.GetIntValue())
		case attrEventID:
			r.EventID = v.GetStringValue()
		case attrEventName:
			r.EventName = v.GetStringValue()
		case attrSignature:
			r.Signature = v.GetStringValue()
		}
	}
	return r
}

// sign computes a record's signature over every field that identifies it, so
// changing the body, renumbering, or moving it to another stream breaks it.
func sign(key []byte, r Record) string {
	m := hmac.New(sha256.New, key)
	for _, part := range []string{r.Stream, strconv.FormatUint(r.Sequence, 10), r.EventID, r.EventName, r.Body} {
		m.Write([]byte(part))
		m.Write([]byte{0}) // separator: fields can't run into each other
	}
	return hmacPrefix + hex.EncodeToString(m.Sum(nil))
}

// Gap is a run of missing sequence numbers in a stream: After and Before are
// the records on either side.
type Gap struct {
	Stream        string
	After, Before uint64
}

// VerifyReport is the result of Verify. OK reports a clean trail.
type VerifyReport struct {
	Checked      int
	BadSignature []Record // modified, forged, or signed with another key
	Gaps         []Gap    // deleted records — or a Send that returned an error
	Conflicts    []Record // a sequence number reused by a different event
}

// OK reports whether no tampering was detected.
func (r VerifyReport) OK() bool {
	return len(r.BadSignature) == 0 && len(r.Gaps) == 0 && len(r.Conflicts) == 0
}

// Verify checks a set of records from the audit store: every signature, and
// per stream, that sequence numbers are contiguous. Duplicate deliveries of
// the same event (a retried export) are benign and skipped. Detects modified,
// forged, and deleted records; cannot detect records removed from the end of a
// stream, so verify against the latest sequence your monitoring has seen.
// A gap also appears where Send returned an error — the caller saw that
// failure, so correlate gaps with export-error metrics.
func Verify(key []byte, records []Record) VerifyReport {
	rep := VerifyReport{Checked: len(records)}
	sorted := slices.Clone(records)
	slices.SortFunc(sorted, func(a, b Record) int {
		if a.Stream != b.Stream {
			return cmp.Compare(a.Stream, b.Stream)
		}
		return cmp.Compare(a.Sequence, b.Sequence)
	})
	for i, r := range sorted {
		if !hmac.Equal([]byte(r.Signature), []byte(sign(key, r))) {
			rep.BadSignature = append(rep.BadSignature, r)
		}
		if i == 0 || sorted[i-1].Stream != r.Stream {
			continue
		}
		prev := sorted[i-1]
		switch {
		case r.Sequence == prev.Sequence && r.EventID != prev.EventID:
			rep.Conflicts = append(rep.Conflicts, r)
		case r.Sequence > prev.Sequence+1:
			rep.Gaps = append(rep.Gaps, Gap{Stream: r.Stream, After: prev.Sequence, Before: r.Sequence})
		}
	}
	return rep
}
