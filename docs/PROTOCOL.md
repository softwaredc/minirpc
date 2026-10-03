# PROTOCOL — Wire Specification

This document describes exactly what bytes minirpc sends over TCP. The framing is shared between Go and C++ implementations, but the *inner payload* format differs because each language uses its own serializer.

---

## 1. Outer Framing (shared, Layer 2)

Every single message — request or response — is wrapped in a 4-byte big-endian length prefix before hitting the wire. **This is the transport layer's job** and is identical for both languages.

```
┌───────────────────────────────────┬───────────────────────────────┐
│   Length Prefix (4 bytes, BE)     │        Payload (N bytes)      │
│                                   │                               │
│   uint32, big-endian network order │   the bytes produced by        │
│   tells peer how big Payload is    │   protocol.Marshal*()          │
└───────────────────────────────────┴───────────────────────────────┘
```

- **Length**: number of Payload bytes, not including the 4 prefix bytes themselves.
- **Max**: hard limit 64 MB; anything larger is rejected as a safety measure.
- **Endianness**: big-endian (RFC 1700 network order).

---

## 2. Request Payload (Layer 4)

### Go implementation

```
Offset  Size  Field
──────  ────  ────────────────────────────────────────────
  0      2    Magic = 0xA5A5  (big-endian uint16)
  2      2    Version = 1     (big-endian uint16)
  4      N    gob(GobWireReq{Method string, ArgsBlob []byte})

  GobWireReq.Method   — string, the full "TypeName.MethodName" call target
  GobWireReq.ArgsBlob — bytes produced by GobSerializer.Encode([]interface{}{...})
                         which means: GobSerializer's AnyBox wrapper + gob of that box
```

Total payload = 4 header bytes + gob-encoded `GobWireReq`.

**Why two gob layers?**
- Outer: `GobWireReq` has only concrete types (`string`, `[]byte`) — gob handles this directly.
- Inner: `ArgsBlob` contains `GobSerializer.Encode(...)` output, which wraps `[]interface{}` values in `AnyBox` to circumvent gob's `*interface{}` decoding limitation.

### C++ implementation

```
Offset  Size  Field
──────  ────  ────────────────────────────────────────────
  0      2    Magic = 0xA5A5  (big-endian uint16)
  2      M    Serializer.encode(Value{method})   — type-tag encoded string
  2+M    N    Serializer.encode(Value{args})     — type-tag encoded vector<ValueHolder>

  (No separate version field in C++ — kept minimal; magic IS the version sentinel.)
```

C++ has no gob limitation to work around, so it's one flat layer.

### Magic number purpose

| Constant    | Value  | Used for |
|-------------|--------|----------|
| `MagicRequest`  | 0xA5A5 | Every request payload starts with this |
| `MagicResponse` | 0xA5A6 | Every response payload starts with this |

**Why magic?** Three reasons:

1. **Fast rejection** — if the first 2 bytes aren't what we expect, we drop the message immediately without invoking any serializer (gob panic, C++ throw).
2. **Disambiguation** — the same transport carries both requests and responses; magic tells us which decoder to use.
3. **Future-proofing** — changing magic is a clean way to introduce protocol v2.

---

## 3. Response Payload (Layer 4)

### Go implementation

```
Offset  Size  Field
──────  ────  ────────────────────────────────────────────
  0      2    Magic = 0xA5A6  (big-endian uint16)
  2      N    gob(GobWireResp{IsError bool, BodyBlob []byte})

  GobWireResp.IsError  — true if the call failed (method not found, panic, error return)
  GobWireResp.BodyBlob — GobSerializer.Encode(body) output
                          (nil for void returns or errors without a message)
```

**Note**: Go response does NOT have a version field (unlike request). This is asymmetric by design — keeping response slightly smaller.

### C++ implementation

```
Offset  Size  Field
──────  ────  ────────────────────────────────────────────
  0      2    Magic = 0xA5A6  (big-endian uint16)
  2      M    Serializer.encode(Value{is_error}) — type-tag encoded bool
  2+M    N    Serializer.encode(Value{body_blob_str})
               — body bytes encoded as a type-tagged string
                 (C++ uses string-as-bytes trick to avoid a dedicated "Blob" type)
```

---

## 4. TypeTag Values (C++ self-built Serializer)

The C++ Serializer writes a 1-byte `TypeTag` before every value, so the decoder knows how to parse what follows.

| TypeTag | Value | On-wire layout |
|---------|-------|----------------|
| `Nil`    | 0x00  | `[0x00]` — one byte, nothing after |
| `Int32`  | 0x01  | `[0x01][4B LE int32]` |
| `Int64`  | 0x02  | `[0x02][8B LE int64]` |
| `Double` | 0x03  | `[0x03][8B LE IEEE 754 double bits]` |
| `String` | 0x04  | `[0x04][4B LE uint32 length][raw bytes]` |
| `Bool`   | 0x05  | `[0x05][1B: 0x00 or 0x01]` |
| `Vector` | 0x06  | `[0x06][4B LE uint32 count][N items, each a full Serializer-encoded Value]` |

**Endianness**: little-endian throughout the C++ serializer (x86/x64 native), **except** the 2-byte magic number prefix which is big-endian (network order).

**Encoding example** — the integer `42`:

```
01 2A 00 00 00
│  └── 4 bytes LE: 42 = 0x0000002A
└── tag = Int32
```

**Encoding example** — the string `"hi"`:

```
04 02 00 00 00 68 69
│  │           └── "hi" in ASCII
│  └── length = 2 (4 bytes LE)
└── tag = String
```

---

## 5. Why a Custom Format?

minirpc does **not** use protobuf, thrift, FlatBuffers, HTTP, gRPC, JSON-RPC, or any wire format library. We deliberately built one from scratch.

| What we used instead | Reason |
|---------------------|--------|
| **Go gob** (standard lib) | Built-in, zero dep, good enough for teaching. But the `*interface{}` limitation forced our AnyBox wrapper — which is itself a great teaching point about how serializers interact with language features. |
| **C++ self-built TypeTag** | C++ has no standard serializer. Writing one with `std::variant` is ~160 LOC and makes every on-wire byte transparent. No proto compiler step, no `.proto` files to maintain. |
| **No HTTP** | RPC doesn't need an application-level transport layer. Raw TCP frames are faster, simpler, and force the learner to understand framing/serialization/protocol directly. |

**Teaching priority over production**: if we wanted speed we'd use FlatBuffers. If we wanted interop we'd use protobuf. minirpc wants you to see *every byte* — a custom format makes that possible.

---

## 6. Go vs C++ Format Difference — At a Glance

```
On the wire (transport frame — identical):
[4B BE length] [protocol payload bytes]

Protocol payload (DIFFERS):

Go Request:
  [2B magic (BE)] [2B version (BE)]
  [gob(GobWireReq{Method, ArgsBlob})]
    └─ ArgsBlob = GobSerializer.Encode([]interface{}) output
       (which is gob(AnyBox{TypeName, Payload}) × N)

C++ Request:
  [2B magic (BE)]
  [Serializer.encode(string method)]
  [Serializer.encode(vector<ValueHolder> args)]
    └─ no second layer — variant handles concrete types naturally

Go Response:
  [2B magic (BE)]
  [gob(GobWireResp{IsError, BodyBlob})]
    └─ BodyBlob = GobSerializer.Encode(body) output

C++ Response:
  [2B magic (BE)]
  [Serializer.encode(bool is_error)]
  [Serializer.encode(string body_blob)]  ← body bytes encoded as a string
```

Both languages agree on magic numbers (0xA5A5 / 0xA5A6) and transport framing (4-byte big-endian length), but the inner payload is language-native. Cross-language interoperability would require a unified intermediate format — intentionally out of scope for this teaching project.
