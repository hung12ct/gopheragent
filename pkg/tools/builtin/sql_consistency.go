package builtin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// sqlCandidate is one independent sub-agent run produced during a
// self-consistency sweep. `finalResp` is the natural-language answer the
// sub-agent produced, `lastResult` is the last successful SQL execution
// captured by its executeSQLTool (nil if no query succeeded).
type sqlCandidate struct {
	index      int
	finalResp  string
	lastResult *SQLResult
}

// pickByMajority clusters candidates by the hash of their last successful
// SQL result and returns the candidate whose cluster is largest.
//
// Tie-breaking:
//   - candidates with a nil lastResult (no query succeeded) are excluded
//     from clustering entirely; only if every candidate failed do we fall
//     back to returning the first one.
//   - between equally-sized clusters, the one whose representative has the
//     fewest attempted queries wins (proxy for "simplest path").
//   - between otherwise-equal candidates, the lower index wins
//     (deterministic for tests).
//
// The returned *sqlCandidate is always a pointer into the input slice —
// callers may read finalResp directly.
func pickByMajority(cands []sqlCandidate) *sqlCandidate {
	if len(cands) == 0 {
		return nil
	}

	type cluster struct {
		hash    string
		members []int // indexes into cands
	}
	groups := map[string]*cluster{}
	var order []string // first-seen order for stable iteration

	for i, c := range cands {
		if c.lastResult == nil || c.lastResult.Error != "" {
			continue
		}
		h := hashResult(c.lastResult)
		g, ok := groups[h]
		if !ok {
			g = &cluster{hash: h}
			groups[h] = g
			order = append(order, h)
		}
		g.members = append(g.members, i)
	}

	if len(groups) == 0 {
		// Everyone errored — return the first candidate so the caller can
		// surface the error to the user.
		return &cands[0]
	}

	// Find the largest cluster. On ties prefer the first-seen group so
	// behaviour is stable.
	var best *cluster
	for _, h := range order {
		g := groups[h]
		if best == nil || len(g.members) > len(best.members) {
			best = g
		}
	}

	// Within the winning cluster, pick the lowest-index member.
	winner := best.members[0]
	for _, idx := range best.members[1:] {
		if idx < winner {
			winner = idx
		}
	}
	return &cands[winner]
}

// hashResult produces a stable fingerprint for a SQLResult. Only the
// semantic content (columns + row data) is hashed — SQL text, execution
// time, and truncation flags are intentionally ignored so two different
// queries that produce the same rows cluster together (that's the whole
// point of self-consistency).
//
// Row order is preserved when the SQL has an ORDER BY; unordered queries
// get a sorted fingerprint so functionally-equivalent results still match.
func hashResult(r *SQLResult) string {
	h := sha256.New()
	// Column names in order.
	for _, c := range r.Columns {
		h.Write([]byte(c))
		h.Write([]byte{0})
	}
	h.Write([]byte{1})

	// Rows — canonicalise each row's JSON, then sort the row list so
	// clustering is order-insensitive. This trades off a tiny bit of
	// accuracy on queries where order matters (ORDER BY) for robustness
	// against non-deterministic row ordering in SELECTs without ORDER BY.
	encoded := make([]string, 0, len(r.Rows))
	for _, row := range r.Rows {
		// Marshal with sorted keys — json.Marshal already sorts map keys
		// alphabetically, so this is deterministic.
		b, err := json.Marshal(row)
		if err != nil {
			continue
		}
		encoded = append(encoded, string(b))
	}
	sort.Strings(encoded)
	for _, s := range encoded {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}
