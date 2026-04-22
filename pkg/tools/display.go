package tools

// ToolDisplay carries UI-facing metadata for a tool — friendly label,
// user-facing description, logical category, and an icon hint. Clients
// rendering tool calls in a chat UI read Display() on every Tool to drive
// consistent labeling without having to hard-code per-tool friendly names.
//
// Zero-value fields let the client fall back to whatever default it
// prefers (typically Name() and Description() from the Tool itself).
// DefaultDisplay is a one-liner for tools that don't want to invent
// separate UI copy.
type ToolDisplay struct {
	// Label is a short human-readable name shown above/beside the tool
	// invocation in the UI — e.g. "Looking up game" for a find_game tool.
	// When empty, clients typically fall back to Tool.Name().
	Label string

	// Description is a longer user-facing description, may differ from
	// Tool.Description (which is model-facing). When empty, clients may
	// fall back to Tool.Description() or omit entirely.
	Description string

	// Category is a logical grouping key — "search", "io", "control",
	// "memory", etc. Clients use this to group tool calls visually or
	// to apply category-level styling. Free-form; no enum enforced.
	Category string

	// IconHint is a string key clients map to their own icon registry
	// — e.g. "magnifier" or "database". Not an icon URL; just a hint
	// that lets the library stay render-agnostic.
	IconHint string
}

// DefaultDisplay returns a ToolDisplay with Label and Description populated
// from the given name and description. Tools that don't want to invent
// separate UI copy can return this directly from their Display() method:
//
//	func (t *MyTool) Display() tools.ToolDisplay {
//	    return tools.DefaultDisplay(t.Name(), t.Description())
//	}
func DefaultDisplay(name, description string) ToolDisplay {
	return ToolDisplay{Label: name, Description: description}
}
