package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

// MemoryStore is the persistence interface behind the memory_* tools. It is
// keyed by (sessionKey, key) so each agent session gets an isolated
// namespace.
//
// Implementations must be safe for concurrent use. The default in-memory
// implementation (InMemoryStore) uses a RWMutex; swap in a Redis / SQL
// backend by providing your own type that satisfies this interface.
type MemoryStore interface {
	Set(ctx context.Context, sessionKey, key, value string) error
	Get(ctx context.Context, sessionKey, key string) (string, bool, error)
	Delete(ctx context.Context, sessionKey, key string) error
	List(ctx context.Context, sessionKey string) ([]string, error)
}

// InMemoryStore is a process-local MemoryStore. Lost on restart — fine for
// short-lived sessions or when the agent is embedded in a longer-running
// process whose lifetime bounds the memory's usefulness.
type InMemoryStore struct {
	mu   sync.RWMutex
	data map[string]map[string]string
}

// NewInMemoryStore returns a ready-to-use in-memory MemoryStore.
func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{data: make(map[string]map[string]string)}
}

// Set stores value under (sessionKey, key), replacing any previous value.
func (s *InMemoryStore) Set(_ context.Context, sessionKey, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ns, ok := s.data[sessionKey]
	if !ok {
		ns = make(map[string]string)
		s.data[sessionKey] = ns
	}
	ns[key] = value
	return nil
}

// Get returns the value for (sessionKey, key) and whether it was present.
func (s *InMemoryStore) Get(_ context.Context, sessionKey, key string) (string, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ns, ok := s.data[sessionKey]
	if !ok {
		return "", false, nil
	}
	v, ok := ns[key]
	return v, ok, nil
}

// Delete removes (sessionKey, key). No-op when the key is absent.
func (s *InMemoryStore) Delete(_ context.Context, sessionKey, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ns, ok := s.data[sessionKey]; ok {
		delete(ns, key)
	}
	return nil
}

// List returns the sorted keys under sessionKey. Empty when the session has
// no entries.
func (s *InMemoryStore) List(_ context.Context, sessionKey string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ns, ok := s.data[sessionKey]
	if !ok {
		return nil, nil
	}
	keys := make([]string, 0, len(ns))
	for k := range ns {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys, nil
}

// memorySessionKey pulls the sessionKey that AgentLoop pushes into ctx. All
// memory tools fail closed when the key is absent — without it, one session
// could read or clobber another's memory.
func memorySessionKey(ctx context.Context) (string, error) {
	sk, ok := ctx.Value(agent.SessionKeyCtx("sessionKey")).(string)
	if !ok || sk == "" {
		return "", fmt.Errorf("tools: sessionKey not found in context")
	}
	return sk, nil
}

// MemorySetTool writes a key/value pair into the current session's memory.
type MemorySetTool struct {
	Store MemoryStore
}

// NewMemorySetTool wires a MemorySetTool to the given store.
func NewMemorySetTool(store MemoryStore) *MemorySetTool {
	return &MemorySetTool{Store: store}
}

func (t *MemorySetTool) Name() string { return "memory_set" }
func (t *MemorySetTool) Description() string {
	return "Save a string value under a named key for the current session. Overwrites any previous value."
}
type memorySetArgs struct {
	Key   string `json:"key"   description:"Memory key (stable identifier, e.g. 'user_preferences')."`
	Value string `json:"value" description:"Value to store. Serialise structured data yourself (e.g. JSON)."`
}

func (t *MemorySetTool) ParametersSchema() tools.ToolSchema {
	return tools.SchemaFor[memorySetArgs]()
}
func (t *MemorySetTool) RequiresConfirmation() bool { return false }
func (t *MemorySetTool) Display() tools.ToolDisplay { return tools.DefaultDisplay(t.Name(), t.Description()) }
func (t *MemorySetTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	sessionKey, err := memorySessionKey(ctx)
	if err != nil {
		return "", err
	}
	var args memorySetArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("tools: invalid arguments: %w", err)
	}
	args.Key = strings.TrimSpace(args.Key)
	if args.Key == "" {
		return "", fmt.Errorf("tools: key is required")
	}
	if t.Store == nil {
		return "", fmt.Errorf("tools: memory_set has no store configured")
	}
	if err := t.Store.Set(ctx, sessionKey, args.Key, args.Value); err != nil {
		return "", fmt.Errorf("tools: memory_set: %w", err)
	}
	return fmt.Sprintf(`{"ok":true,"key":%q}`, args.Key), nil
}

// MemoryGetTool reads a previously saved value from the current session.
type MemoryGetTool struct {
	Store MemoryStore
}

// NewMemoryGetTool wires a MemoryGetTool to the given store.
func NewMemoryGetTool(store MemoryStore) *MemoryGetTool {
	return &MemoryGetTool{Store: store}
}

func (t *MemoryGetTool) Name() string { return "memory_get" }
func (t *MemoryGetTool) Description() string {
	return "Read a value previously saved with memory_set for the current session."
}
type memoryGetArgs struct {
	Key string `json:"key" description:"Memory key to read."`
}

func (t *MemoryGetTool) ParametersSchema() tools.ToolSchema {
	return tools.SchemaFor[memoryGetArgs]()
}
func (t *MemoryGetTool) RequiresConfirmation() bool { return false }
func (t *MemoryGetTool) Display() tools.ToolDisplay { return tools.DefaultDisplay(t.Name(), t.Description()) }
func (t *MemoryGetTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	sessionKey, err := memorySessionKey(ctx)
	if err != nil {
		return "", err
	}
	var args memoryGetArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("tools: invalid arguments: %w", err)
	}
	args.Key = strings.TrimSpace(args.Key)
	if args.Key == "" {
		return "", fmt.Errorf("tools: key is required")
	}
	if t.Store == nil {
		return "", fmt.Errorf("tools: memory_get has no store configured")
	}
	v, ok, err := t.Store.Get(ctx, sessionKey, args.Key)
	if err != nil {
		return "", fmt.Errorf("tools: memory_get: %w", err)
	}
	envelope := map[string]any{
		"key":   args.Key,
		"found": ok,
		"value": v,
	}
	out, err := json.Marshal(envelope)
	if err != nil {
		return "", fmt.Errorf("tools: marshal: %w", err)
	}
	return string(out), nil
}

// MemoryDeleteTool removes a key from the current session's memory.
type MemoryDeleteTool struct {
	Store MemoryStore
}

// NewMemoryDeleteTool wires a MemoryDeleteTool to the given store.
func NewMemoryDeleteTool(store MemoryStore) *MemoryDeleteTool {
	return &MemoryDeleteTool{Store: store}
}

func (t *MemoryDeleteTool) Name() string { return "memory_delete" }
func (t *MemoryDeleteTool) Description() string {
	return "Delete a key from the current session's memory. No error when the key does not exist."
}
type memoryDeleteArgs struct {
	Key string `json:"key" description:"Memory key to delete."`
}

func (t *MemoryDeleteTool) ParametersSchema() tools.ToolSchema {
	return tools.SchemaFor[memoryDeleteArgs]()
}
func (t *MemoryDeleteTool) RequiresConfirmation() bool { return false }
func (t *MemoryDeleteTool) Display() tools.ToolDisplay { return tools.DefaultDisplay(t.Name(), t.Description()) }
func (t *MemoryDeleteTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	sessionKey, err := memorySessionKey(ctx)
	if err != nil {
		return "", err
	}
	var args memoryDeleteArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("tools: invalid arguments: %w", err)
	}
	args.Key = strings.TrimSpace(args.Key)
	if args.Key == "" {
		return "", fmt.Errorf("tools: key is required")
	}
	if t.Store == nil {
		return "", fmt.Errorf("tools: memory_delete has no store configured")
	}
	if err := t.Store.Delete(ctx, sessionKey, args.Key); err != nil {
		return "", fmt.Errorf("tools: memory_delete: %w", err)
	}
	return fmt.Sprintf(`{"ok":true,"key":%q}`, args.Key), nil
}

// MemoryListTool returns all keys stored in the current session's memory.
type MemoryListTool struct {
	Store MemoryStore
}

// NewMemoryListTool wires a MemoryListTool to the given store.
func NewMemoryListTool(store MemoryStore) *MemoryListTool {
	return &MemoryListTool{Store: store}
}

func (t *MemoryListTool) Name() string { return "memory_list" }
func (t *MemoryListTool) Description() string {
	return "List all keys currently stored in the session's memory."
}
func (t *MemoryListTool) ParametersSchema() tools.ToolSchema {
	return tools.SchemaFor[struct{}]()
}
func (t *MemoryListTool) RequiresConfirmation() bool { return false }
func (t *MemoryListTool) Display() tools.ToolDisplay { return tools.DefaultDisplay(t.Name(), t.Description()) }
func (t *MemoryListTool) Execute(ctx context.Context, _ string) (string, error) {
	sessionKey, err := memorySessionKey(ctx)
	if err != nil {
		return "", err
	}
	if t.Store == nil {
		return "", fmt.Errorf("tools: memory_list has no store configured")
	}
	keys, err := t.Store.List(ctx, sessionKey)
	if err != nil {
		return "", fmt.Errorf("tools: memory_list: %w", err)
	}
	if keys == nil {
		keys = []string{}
	}
	envelope := map[string]any{
		"keys":  keys,
		"count": len(keys),
	}
	out, err := json.Marshal(envelope)
	if err != nil {
		return "", fmt.Errorf("tools: marshal: %w", err)
	}
	return string(out), nil
}
