# Claude Code Rules for HTTP Module

## Code Style

- Early returns, no nested ifs
- Extract logic into small, focused functions
- Flat structure over deep nesting
- Idiomatic Go - if err != nil { return } pattern

## Component Design

- Handle() switch cases should be minimal - delegate to functions
- No JSON parsing in components - SDK handles deserialization
- No knowledge of other modules' metadata keys

## CRITICAL: System Port Delivery Order

System ports (`_settings`, `_control`, `_reconcile`) have NO guaranteed delivery order. On pod restart, `_reconcile` may fire before `_settings`. Components that persist state to metadata must use a guard flag to prevent reconcile from overwriting fresh values with stale metadata. See SDK CLAUDE.md for the full pattern.

## Context Pattern for Schema Generation

Define a type alias for Context and use it in structs:

```go
// Context type alias for schema generation
type Context any

// Request input
type Request struct {
    Context Context `json:"context,omitempty" configurable:"true" title:"Context"`
    // ... other fields
}

// Output struct
type Output struct {
    Context Context `json:"context,omitempty" configurable:"true" title:"Context"`
    // ... other fields
}

// Error output - only Context and Error, no Request duplication
type Error struct {
    Context Context `json:"context,omitempty" configurable:"true" title:"Context"`
    Error   string  `json:"error" title:"Error"`
}
```

Key points:
- Use `type Context any` not just `any` directly - this enables proper schema generation
- Add `configurable:"true"` to Context fields on both input AND output ports
- Error structs should only have Context and Error message, not duplicate the entire Request
