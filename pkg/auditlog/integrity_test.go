package auditlog

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var testKey = []byte("0123456789abcdef0123456789abcdef")

func TestHash(t *testing.T) {
	a, b := Hash(testKey, "42"), Hash(testKey, "43")
	if !strings.HasPrefix(a, "hmac-sha256:") || len(a) != len("hmac-sha256:")+64 || a == b {
		t.Fatalf("Hash = %q / %q", a, b)
	}
	if Hash(testKey, "42") != a || Hash([]byte("another-key-0123"), "42") == a {
		t.Fatal("Hash must be deterministic per key and differ across keys")
	}
}

// signedTrail sends n events through a keyed client and returns the stored records.
func signedTrail(t *testing.T, n int) []Record {
	t.Helper()
	r := &logsReceiver{}
	c := newTestClient(t, Config{Endpoint: startGRPCReceiver(t, r), Insecure: true, HMACKey: testKey})
	for i := 0; i < n; i++ {
		if err := c.Send(context.Background(), sampleEvent); err != nil {
			t.Fatal(err)
		}
	}
	var recs []Record
	for _, lr := range r.records() {
		recs = append(recs, RecordFromOTLP(lr))
	}
	return recs
}

func TestVerify(t *testing.T) {
	recs := signedTrail(t, 5)
	if rep := Verify(testKey, recs); !rep.OK() || rep.Checked != 5 {
		t.Fatalf("clean trail reported %+v", rep)
	}
	if recs[0].Sequence != 1 || recs[4].Sequence != 5 || recs[0].Stream == "" {
		t.Fatalf("sequence/stream not stamped: %+v", recs[0])
	}

	tampered := append([]Record(nil), recs...)
	tampered[2].Body = strings.Replace(tampered[2].Body, "refunded", "open", 1)
	if rep := Verify(testKey, tampered); len(rep.BadSignature) != 1 {
		t.Fatalf("modified body not detected: %+v", rep)
	}

	deleted := append(append([]Record(nil), recs[:2]...), recs[3:]...)
	if rep := Verify(testKey, deleted); len(rep.Gaps) != 1 || rep.Gaps[0].After != 2 || rep.Gaps[0].Before != 4 {
		t.Fatalf("deleted record not detected: %+v", rep)
	}

	// Delete record 3, then renumber 4→3 and 5→4 to hide the gap.
	renumbered := append(append([]Record(nil), recs[:2]...), recs[3:]...)
	renumbered[2].Sequence, renumbered[3].Sequence = 3, 4
	if rep := Verify(testKey, renumbered); len(rep.BadSignature) != 2 {
		t.Fatalf("renumbering to hide a deletion not detected: %+v", rep)
	}

	redelivered := append(append([]Record(nil), recs...), recs[1]) // a retried export
	if rep := Verify(testKey, redelivered); !rep.OK() {
		t.Fatalf("duplicate delivery of the same event must be benign: %+v", rep)
	}

	forged := recs[1]
	forged.EventID = "ffffffffffffffffffffffffffffffff"
	forged.Signature = sign([]byte("attacker-key-0123456"), forged)
	if rep := Verify(testKey, append(append([]Record(nil), recs...), forged)); len(rep.Conflicts) != 1 || len(rep.BadSignature) != 1 {
		t.Fatalf("forged record reusing a sequence not detected: %+v", rep)
	}

	if rep := Verify([]byte("wrong-key-0123456789"), recs); len(rep.BadSignature) != 5 {
		t.Fatal("verifying with the wrong key must fail every record")
	}
}

func TestEventJSON_SinglePassEnvelope(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)
	body, err := eventJSON(sampleEvent, at)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(body), `{"kind":"DataModification","schema_version":"v1","time":"2026-01-02T03:04:05.000000006Z",`) {
		t.Fatalf("envelope = %s", body)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil || m["object_id"] != "o-7" || strings.Count(string(body), `"time"`) != 1 {
		t.Fatalf("body invalid or time duplicated: %v\n%s", err, body)
	}
	if _, err := eventJSON(Custom{Common: Common{TenantID: "a"}, Name: "x", Payload: map[string]any{"c": make(chan int)}}, at); err == nil {
		t.Fatal("unserializable payload must error")
	}
}

func TestSign_FieldBoundariesMatter(t *testing.T) {
	a := Record{Stream: "s", Sequence: 1, EventID: "ab", EventName: "c", Body: "{}"}
	b := Record{Stream: "s", Sequence: 1, EventID: "a", EventName: "bc", Body: "{}"}
	if sign(testKey, a) == sign(testKey, b) {
		t.Fatal("shifting bytes between fields must change the signature")
	}
}
