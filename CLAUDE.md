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

## CRITICAL: Handler Results + the SyncRPC Capability

**NEVER ignore the return value of handler() calls — ALWAYS `return handler(ctx, port, data)`.** Request-response subgraphs run blocking I/O: an upstream component (http_server holding its live socket) blocks until the response flows back through the handler chain; a dropped return loses the response and times out the caller. The rule applies in durable subgraphs too — the returned `Result` drives ack/retry.

If a component itself **blocks holding a live connection** (emits on a source port and waits for the chain to deliver a result back within the same request), it MUST declare the capability:

```go
func (c *Component) SyncRPC() module.SyncRPCInfo { return module.SyncRPCInfo{} }
```

`module build` auto-tags it `sync_rpc`; the platform keeps that component's subgraph on blocking request/reply while trigger-driven subgraphs run durable fire-and-forget — modes are derived, never configured. Forgetting the declaration is fatal: durable hops return nothing to their sender, so the awaited response never arrives. Canonical implementer: `http_server` (live socket). Components that merely sit in its subgraph — a Slack command handler, a router — declare nothing; they inherit classic delivery automatically.
