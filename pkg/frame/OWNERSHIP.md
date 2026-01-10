# Buffer Ownership Contract — VerbooRTC frame package

This document defines the ownership contract for buffers used by `EncodePooled`,
`ReleaseEncoded`, `DecodeInto`, `ZeroCopyDecode` and related helpers in the
VerbooRTC codebase. The goal is to make buffer ownership explicit, avoid
double-free and use-after-free bugs, and provide safe patterns for asynchronous
paths (channels, NATS callbacks, transports).

## Ownership semantics (short)
- `EncodePooled(f)` returns a pooled `[]byte` buffer. The caller becomes the owner
  and **MUST** call `ReleaseEncoded(buf)` exactly once unless ownership is
  explicitly transferred.
    - `ReleaseEncoded(buf)` is **noop‑safe** for heap buffers; calling it
      on a heap buffer is allowed and must not panic.
    - `DecodeInto` / `ZeroCopyDecode` produce zero‑copy views into the input buffer.
      If the payload will be used asynchronously or stored beyond the lifetime of
      the input buffer, the consumer **MUST** call `CopyPayload` or
      `CloneFrameForStorage` to obtain an independent owned copy.
      +- Ownership transfer conventions:
    - **Boolean return**: callbacks like `TryLocalDeliver(target, buf) bool` return
      `true` to indicate they accepted ownership and will call `ReleaseEncoded`
      later; `false` means the caller retains ownership and must call
      `ReleaseEncoded`.
    - **Channel transfer**: when transferring a pooled buffer via channel, prefer
      non‑blocking send; on failure call `ReleaseEncoded` to avoid leaks.
    - **Publish/JetStream**: a published message must either complete ownership
      transfer (and call ReleaseEncoded) or the caller retains ownership.

## Rules (short)
1. Immediately adopt the `owned := true` + `defer` pattern after obtaining a pooled buffer:
   ```go
       buf, _ := frame.EncodePooled(f)
       owned := true
       defer func(){ if owned { frame.ReleaseEncoded(buf) } }()
   ```
   When transferring ownership (e.g., to TryLocalDeliver or a connection writeCh)
   set owned = false.
2. When sending buffers via channels: use non‑blocking enqueue; if enqueue fails,
   call ReleaseEncoded to avoid leaks.
3. Document in every callback whether it accepts ownership (return semantics).
4. Publish/JetStream helpers must clearly state ownership semantics: either they
   take ownership and call ReleaseEncoded, or the caller must call ReleaseEncoded.
5. Protect calls to external callbacks with recover to avoid losing ownership on panic.
## Recommended patterns (copy-paste)

### Safe TryLocalDeliver
```go
buf, _ := frame.EncodePooled(f)
owned := true
defer func(){ if owned { frame.ReleaseEncoded(buf) } }()
accepted := false
func(){
    defer func(){ if r := recover(); r != nil { accepted = false } }()
    if dm.localDeliver != nil { accepted = dm.localDeliver(target, buf) }
}())
if accepted { owned = false } // ownership transferred
```

### Plain NATS publish (caller releases)
```go
buf, _ := frame.EncodePooled(f)
defer frame.ReleaseEncoded(buf) // plain NATS publish does not accept ownership
nc.Publish(subject, buf[frame.HeaderLen:])
```

### Transfer ownership via connection.writeCh
```go
buf, _ := frame.EncodePooled(f)
select {
case conn.writeCh <- buf:
    // ownership transferred; writePump must call ReleaseEncoded(buf)
default:
    frame.ReleaseEncoded(buf) // backpressure: caller releases
}
```

### NATS subscription callback (always CopyPayload before async use)
```go
func(m *nats.Msg) {
  owned := frame.CopyPayload(m.Data)
  select {
  case c.writeCh <- owned:
  default:
    frame.ReleaseEncoded(owned)
  }
}
```

## Examples: writePump / readPump
Provide canonical examples so reviewers and contributors follow the same patterns.

### Example: connection writePump (receiver must ReleaseEncoded)
```go
func (c *Connection) writePump() {
    for {
        select {
        case buf, ok := <-c.writeCh:
            if !ok { return }
                // Send to network (write to connection)
            if err := ws.WriteMessage(websocket.BinaryMessage, buf); err != nil {
                frame.ReleaseEncoded(buf) // Release since we're returning
                return
            }
            frame.ReleaseEncoded(buf) // Release after successful send (not needed if WriteMessage owns it)
        }
    }
 }
 ```

### Example: transport OnFrame callback (transport transfers ownership)
```go
// transport calls cb(f) and transfers ownership to the callback.
tr.OnFrame(func(f *frame.Frame) {
    // transfer ownership into transportCh; receiver must PutFrame when done.
    select {
    case transportCh <- f:
        // ownership moved to receiver
    default:
        frame.PutFrame(f) // return to pool on backpressure
    }
})
 ```

## Docstrings (suggested text)
Add these docstrings above the corresponding functions in the `frame` package.

**EncodePooled**
```go
// EncodePooled encodes f into a pooled []byte and returns it.
// Ownership: caller becomes owner of the returned buffer and MUST call
// ReleaseEncoded(buf) exactly once unless ownership is explicitly transferred.
```

**ReleaseEncoded**
```go
// ReleaseEncoded returns a pooled buffer to the pool.
// It is noop‑safe for heap buffers (calling ReleaseEncoded on a non‑pooled buffer is allowed).
```

**TryLocalDeliver**
```go
// TryLocalDeliver attempts fast local delivery. It receives ownership of the
// provided pooled buffer only if it returns true. If it returns false, the
// caller retains ownership and must call ReleaseEncoded.
```

**DecodeInto / ZeroCopyDecode**
```go
// DecodeInto decodes data into the provided *Frame using zero‑copy where possible.
// The decoded Frame may reference the input buffer. If the payload will be used
// asynchronously or stored beyond the lifetime of the input buffer, call CopyPayload
// or CloneFrameForStorage to obtain an independent owned copy.
```

## Checklist for reviewers
- Every `EncodePooled` is matched by either `ReleaseEncoded` or an explicit ownership transfer.
- Any zero‑copy payload moved to an async path uses `CopyPayload` or `CloneFrameForStorage`.
- Publish/JetStream helpers document ownership semantics.
- Unit tests validate local accept/reject and panic‑safety paths.

## Testing recommendations
- Tests for `TryLocalDeliver`:
  - **accept**: localDeliver returns true; ensure caller does not call `ReleaseEncoded`.
  - **reject**: localDeliver returns false; ensure caller releases buffer.
  - **panic safety**: callback panics; ensure buffer is released and no leak.
     - Tests ensuring subscription callbacks use `CopyPayload` and drop/Release on backpressure.
     - Run `go test -race ./...` for modules that manipulate pooled buffers.

## Common pitfalls
- **Double `PutFrame` / double `ReleaseEncoded`** — avoid calling release both in transport and in consumer.
- **Freeing before transfer** — do not `PutFrame` or `ReleaseEncoded` before transferring ownership to a channel or callback.
- **Zero‑copy into async** — never move zero‑copy views into async paths without copying.

## Docstrings and code updates
Add the suggested docstrings above the corresponding functions in the frame package.
