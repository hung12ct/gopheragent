package eval

import (
	"encoding/json"
	"fmt"
	"io"
)

// WriteJSON writes the report as indented JSON for programmatic consumption.
func WriteJSON(w io.Writer, rep *SuiteReport) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rep); err != nil {
		return fmt.Errorf("eval: write json: %w", err)
	}
	return nil
}
