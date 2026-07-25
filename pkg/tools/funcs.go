package tools

import (
	"context"
	"encoding/json"
	"fmt"
)

// FuncToolOpts tunes the ToolDescriptor that RegisterFunc builds. The
// zero value gives a sensible default — non-confirmation, non-cacheable,
// non-inline, default Display. Override individual fields when a tool
// needs caching, HITL approval, or custom UI hints.
type FuncToolOpts struct {
	RequiresConfirmation bool
	Cacheable            bool
	Inline               bool
	Display              ToolDisplay

	// Schema, when non-nil, replaces the reflected SchemaFor[T] output.
	// Reflection reads enum values from struct tags, which are
	// compile-time constants — so a tool whose valid parameter values are
	// only known at runtime (names discovered from disk, IDs read from a
	// database) cannot express them any other way.
	//
	// T still decodes argsJSON, so the override must stay structurally
	// compatible with T: a property the override declares but T lacks is
	// dropped silently on unmarshal.
	Schema *ToolSchema
}

// RegisterFunc registers a tool built from a strongly-typed function,
// dropping the per-tool struct/Descriptor/Execute boilerplate. The
// parameter schema is generated via SchemaFor[T] unless FuncToolOpts.Schema
// overrides it; argsJSON arriving from the LLM is unmarshalled into T
// before fn runs.
//
// Example:
//
//	type LookupArgs struct {
//	    UserID string `json:"user_id" description:"the user to look up"`
//	}
//
//	tools.RegisterFunc(reg, "lookup_user", "Look up a user by ID",
//	    func(ctx context.Context, a LookupArgs) (tools.Result, error) {
//	        return tools.Text("found " + a.UserID), nil
//	    })
//
// Compared with the struct-and-methods form (~40 LOC), the typed-fn
// form is one declaration + the function body. The trade-off is that
// adopters who need to customize Descriptor flags pass FuncToolOpts.
//
// The reg parameter accepts anything implementing Registerer — that's
// the in-tree *Registry as well as adopter wrappers like
// builder.GlobalCatalog. The interface keeps the call site readable
// when the same tools register into both the agent registry and the
// catalog: pass either pointer directly.
func RegisterFunc[T any](reg Registerer, name, description string, fn func(ctx context.Context, args T) (Result, error), opts ...FuncToolOpts) {
	var o FuncToolOpts
	if len(opts) > 0 {
		o = opts[0]
	}
	if o.Display == (ToolDisplay{}) {
		o.Display = DefaultDisplay(name, description)
	}
	params := SchemaFor[T]()
	if o.Schema != nil {
		params = *o.Schema
	}
	reg.Register(&funcTool[T]{
		desc: ToolDescriptor{
			Name:                 name,
			Description:          description,
			Parameters:           params,
			RequiresConfirmation: o.RequiresConfirmation,
			Cacheable:            o.Cacheable,
			Inline:               o.Inline,
			Display:              o.Display,
		},
		fn: fn,
	})
}

// funcTool is the generic Tool implementation backing RegisterFunc.
// Unexported so adopters can't accidentally hand-construct one with a
// mismatched descriptor/fn pair.
type funcTool[T any] struct {
	desc ToolDescriptor
	fn   func(ctx context.Context, args T) (Result, error)
}

func (t *funcTool[T]) Descriptor() ToolDescriptor { return t.desc }

// Execute unmarshals argsJSON into T then delegates to the typed
// function. Empty argsJSON is treated as the zero value of T — common
// for argument-less tools where the LLM emits "{}" or "".
func (t *funcTool[T]) Execute(ctx context.Context, argsJSON string) (Result, error) {
	var args T
	if argsJSON != "" {
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return Result{}, fmt.Errorf("tools: %s: decode args: %w", t.desc.Name, err)
		}
	}
	return t.fn(ctx, args)
}
