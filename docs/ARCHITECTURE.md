# ARCHITECTURE — 6 Layers Deep Dive

This document walks through each of minirpc's 6 layers, explaining the *why* behind design choices and showing annotated code from the actual source. For the wire-level byte format, see [PROTOCOL.md](PROTOCOL.md).

---

## Layer 1: Server Listen & Accept

**Go**: `net.Listen` + `ln.Accept` in an infinite loop. Each connection spawns a goroutine via `go s.handleConn(conn)`.

**C++**: `socket()` → `bind()` → `listen()` → `accept()` POSIX calls. Each connection spawns a `std::thread` and detaches.

**The key insight**: TCP is a stream, not a message. `accept()` gives us one connected socket per client. We then hand each socket to Layer 2 for message framing.

### Go snippet — [server.go](../go/server.go)

```go
func (s *Server) ListenAndServe(addr string) error {
    ln, err := net.Listen("tcp", addr)
    defer ln.Close()

    for {
        conn, err := ln.Accept() // blocking — returns one connected socket
        if err != nil { return err }
        // Each connection gets its own goroutine — concurrent handling
        go s.handleConn(conn)
    }
}
```

### C++ snippet — [server.cpp](../cpp/src/server.cpp)

```cpp
int Server::listen_and_serve(const std::string& addr, int port) {
    int listen_fd = ::socket(AF_INET, SOCK_STREAM, 0);
    // bind, listen ...
    while (true) {
        int client_fd = ::accept(listen_fd, nullptr, nullptr);
        if (client_fd < 0) continue;
        std::thread t(handle_conn, client_fd, std::cref(registry_));
        t.detach(); // C++ equivalent of go handleConn(conn)
    }
}
```

---

## Layer 2: Transport Codec

**Purpose**: Turn a TCP stream into "one complete message at a time". This is the classic length-prefix framing — 4 bytes big-endian uint32 tells you how long the body is.

**Why 4 bytes?** Maximum message = 4 GB, which is more than enough.

**Why big-endian?** Network byte order convention (RFC 1700). Both Go and C++ Codecs use big-endian for the length prefix.

**Partial read handling**: `read()`/`io.Read` don't guarantee they return all requested bytes in one call. Both implementations loop until the full body is read. Go uses `io.ReadFull` (which does this internally); C++ implements `read_full` manually.

### Go snippet — [pkg/transport/tcp.go](../go/pkg/transport/tcp.go)

```go
// Read 4-byte big-endian length prefix first
_, err := io.ReadFull(c.conn, c.rBuf[:4])
length := binary.BigEndian.Uint32(c.rBuf[:4])

// Security cap — reject anything over 64MB
if length > 64*1024*1024 {
    return nil, fmt.Errorf("transport: message too large")
}

// io.ReadFull handles partial reads internally
buf := make([]byte, length)
_, err = io.ReadFull(c.conn, buf)
return buf, nil
```

### C++ snippet — [transport.cpp](../cpp/src/transport.cpp)

```cpp
ssize_t Codec::read_full(uint8_t* buf, size_t len) {
    size_t got = 0;
    while (got < len) {                          // loop until we have all bytes
        ssize_t n = ::read(fd_, buf + got, len - got);
        if (n > 0)      got += static_cast<size_t>(n);
        else if (n == 0) return static_cast<ssize_t>(got); // peer closed
        else if (errno == EINTR) continue;       // signal interrupted — retry
        else             return -1;              // real error
    }
    return static_cast<ssize_t>(got);
}
```

---

## Layer 3: Serializer

### Go — `GobSerializer` + `AnyBox` workaround

Go's `encoding/gob` has a well-known limitation: you **cannot** decode a concrete type (like `int(42)`) into a `*interface{}`. Gob refuses to decode "remote concrete types" into `interface{}` targets.

This is a problem for RPC: we want `Request.Args` and `Response.Body` to hold arbitrary types via `interface{}`.

**Our fix — `AnyBox`**:

1. Wrap every value in `AnyBox{TypeName string, Payload []byte}` — both fields are concrete types, no `interface{}`.
2. Gob-encode the original value into `Payload`, store its type name.
3. Gob-encode the `AnyBox` itself. Gob never sees an `interface{}`.

On decode:

1. Gob-decode the `AnyBox`.
2. Use `TypeName` to resolve back to a `reflect.Type`.
3. Allocate a zero value of that type, gob-decode `Payload` into it.
4. Assign to `*interface{}`.

### Go snippet — [pkg/serialize/gob.go](../go/pkg/serialize/gob.go)

```go
type AnyBox struct {
    TypeName string   // e.g. "int", "string", "main.Calculator"
    Payload  []byte   // gob-encoded concrete value
}

func (g *GobSerializer) Encode(v interface{}) ([]byte, error) {
    // 1. gob-encode the actual value into Payload
    var payloadBuf bytes.Buffer
    gob.NewEncoder(&payloadBuf).Encode(v)

    // 2. wrap in AnyBox (all concrete fields — gob-safe!)
    box := AnyBox{TypeName: g.getTypeName(reflect.TypeOf(v)),
                  Payload:  payloadBuf.Bytes()}

    // 3. gob-encode the AnyBox itself
    var buf bytes.Buffer
    gob.NewEncoder(&buf).Encode(box)
    return buf.Bytes(), nil
}
```

### C++ — Hand-rolled TypeTag binary format

C++ has no runtime reflection and no `interface{}` equivalent. We solve "any-type" with `std::variant` and a self-designed binary format:

```cpp
using Value = std::variant<
    std::monostate,   // Nil     (tag 0x00)
    int32_t,          // Int32   (tag 0x01)
    int64_t,          // Int64   (tag 0x02)
    double,           // Double  (tag 0x03)
    std::string,      // String  (tag 0x04)
    bool,             // Bool    (tag 0x05)
    std::vector<struct ValueHolder>  // Vector (tag 0x06)
>;
```

**Why little-endian?** x86/x64 is natively little-endian — direct `memcpy`, no byte-swap needed.

### C++ snippet — [serialize.cpp](../cpp/src/serialize.cpp)

```cpp
void Serializer::encode(const Value& v, std::vector<uint8_t>& out) {
    std::visit([&](auto&& arg) {
        using T = std::decay_t<decltype(arg)>;
        if constexpr (std::is_same_v<T, std::string>) {
            out.push_back(uint8_t(TypeTag::String));     // 1B tag
            put_u32_le(out, uint32_t(arg.size()));        // 4B length
            out.insert(out.end(), arg.begin(), arg.end()); // raw bytes
        }
        // ... similar branches for each type
    }, v);
}
```

---

## Layer 4: Protocol

**Go**: Wire struct + magic numbers + version, all encoded via gob on the wire struct (which has no `interface{}` fields, so plain gob works), while inner user data goes through `GobSerializer` → `AnyBox`.

**C++**: Same magic + version design, but wire struct (method string + args vector) is encoded directly via our `Serializer`. Only body goes through a second layer.

### Go wire struct — [protocol.go](../go/pkg/protocol/protocol.go)

```go
// GobWireReq fields are ALL concrete — safe for plain gob
type GobWireReq struct {
    Method   string   // concrete
    ArgsBlob []byte   // concrete — already AnyBox-wrapped
}

const (
    MagicRequest   uint16 = 0xA5A5
    MagicResponse  uint16 = 0xA5A6
    CurrentVersion uint16 = 1
)
```

**Why wire struct separate from user data?** The wire struct is part of the RPC *protocol* — it describes "how a call is framed on the wire". User data is whatever the app wants to pass through. This cleanly separates transport concerns from application concerns.

### C++ marshal — [protocol.cpp](../cpp/src/protocol.cpp)

```cpp
std::vector<uint8_t> marshal_request(const std::string& method,
                                      const std::vector<ValueHolder>& args) {
    std::vector<uint8_t> payload;
    Serializer::encode(Value{method}, payload);  // type-tag encoded string
    Serializer::encode(Value{args},   payload);  // type-tag encoded vector

    std::vector<uint8_t> out;
    put_u16_be(out, kMagicRequest);               // 0xA5A5 — network order
    out.insert(out.end(), payload.begin(), payload.end());
    return out;
}
```

---

## Layer 5: Service Registry

The registry maps a string method name → something callable.

**Go**: `map[string]reflect.Value` — values are `method.Func` obtained via reflection. Call-time `fn.Call(args)` does the actual invocation.

**C++**: `map[string]std::function<Value(const std::vector<ValueHolder>&)>` — every handler conforms to one unified signature. No reflection, no magic.

### Go snippet — [server.go](../go/server.go)

```go
// Registration — scan all exported methods of a struct
for i := 0; i < t.NumMethod(); i++ {
    method := t.Method(i)
    name := typName + "." + method.Name   // e.g. "Calculator.Add"
    s.registry[name] = method.Func        // reflect.Value — ready to Call()
}

// Invocation — lookup, build args, reflect.Call
fn, ok := s.registry[req.Method]
results := fn.Call(args)  // one line = one RPC dispatch
```

### C++ snippet — [server.cpp](../cpp/src/server.cpp)

```cpp
// Registration — just store a std::function
using Handler = std::function<Value(const std::vector<ValueHolder>&)>;
void Server::register_handler(const std::string& name, Handler h) {
    registry_[name] = std::move(h);
}

// Invocation — std::map lookup + direct call
auto it = registry.find(req.method);
Value result = it->second(req.args);   // plain C++ function call
```

**Key difference**: Go's `reflect.Call` supports heterogeneous argument types (int, string, struct...), but requires type coercion. C++ forces all handlers to accept `vector<ValueHolder>`, so handlers unpack types themselves. This is the direct consequence of language-level reflection support.

---

## Layer 6: Client Call

The client's job is simply: dial → marshal request → write → read → unmarshal response → check error flag → return body.

### Go snippet — [client.go](../go/client.go)

```go
func (c *Caller) Call(method string, args ...interface{}) (interface{}, error) {
    reqBytes, _ := protocol.MarshalRequest(method, args, c.ser)
    c.codec.WriteMessage(reqBytes)              // Layer 2 transport

    respBytes, _ := c.codec.ReadMessage()
    resp, _ := protocol.UnmarshalResponse(respBytes, c.ser)

    if resp.IsError {
        return nil, fmt.Errorf("rpc error: %s", resp.Body)
    }
    return resp.Body, nil
}
```

### C++ snippet — [client.cpp](../cpp/src/client.cpp)

```cpp
CallResult Client::call(const std::string& method,
                        const std::vector<ValueHolder>& args) {
    auto req_bytes = marshal_request(method, args);
    codec_->write_msg(req_bytes);

    std::vector<uint8_t> resp_bytes;
    codec_->read_msg(resp_bytes);

    WireResp resp = unmarshal_response(resp_bytes.data(), resp_bytes.size());

    if (resp.is_error) { /* build error result */ }
    // ...
    result.body = Serializer::decode_all(resp.body_blob);
    return result;
}
```

Notice that Go client and C++ client follow **exactly the same 5-step flow**. The frameworks share the same layered mental model even though their implementations differ.

---

## Summary — Data Flow

```
Client.Call("Calculator.Add", 3, 4)
  │
  ├─ Layer 4: MarshalRequest → [magic][version][gob(wire struct)]
  │                              └─ Layer 3: GobSerializer.Encode(3,4) → AnyBox-wrapped
  │
  ├─ Layer 2: Codec.WriteMessage → [4B big-endian len][protocol bytes]
  ├─ Layer 1: TCP write → socket
  │              ║ network ║
  │
  ├─ Layer 1: TCP read → socket → Server.handleConn
  ├─ Layer 2: Codec.ReadMessage → [protocol bytes]
  ├─ Layer 4: UnmarshalRequest → Request{Method: "Calculator.Add", Args: [3,4]}
  │              └─ Layer 3: GobSerializer.Decode → unwrap AnyBox
  ├─ Layer 5: registry["Calculator.Add"].Call([recv, 3, 4]) → 7
  └─ ... response path mirrors the request path in reverse
```
