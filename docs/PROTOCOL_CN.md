# PROTOCOL —— 线上字节格式规范

本文精确描述 minirpc 在 TCP 上到底发了什么字节。外层帧格式 Go 和 C++ 实现共享，内层 payload 格式因为各自的序列化器不同而有差异。

---

## 1. 外层帧（共享，Layer 2）

**每一条消息** —— 无论请求还是响应 —— 在真正上线路之前都会先套一层 4 字节大端长度前缀。**这是 transport 层的职责**，两种语言实现完全一致。

```
┌───────────────────────────────────┬───────────────────────────────┐
│   长度前缀（4 字节，大端）        │        Payload（N 字节）       │
│                                   │                               │
│   uint32，网络字节序              │   protocol.Marshal*()          │
│   告诉对端 Payload 有多大         │   产出的原始字节               │
└───────────────────────────────────┴───────────────────────────────┘
```

- **长度**: Payload 的字节数，**不**包括前面的 4 字节前缀本身。
- **上限**: 硬限制 64 MB，更大的消息直接拒绝（安全考虑，防 OOM）。
- **字节序**: 大端（RFC 1700 网络字节序）。

---

## 2. 请求 Payload（Layer 4）

### Go 实现

```
偏移    长度    字段
──────  ────    ────────────────────────────────────────────
  0      2      Magic = 0xA5A5  (uint16, 大端)
  2      2      Version = 1     (uint16, 大端)
  4      N      gob(GobWireReq{Method string, ArgsBlob []byte})

  GobWireReq.Method   — string，完整的 "TypeName.MethodName" 调用目标
  GobWireReq.ArgsBlob — GobSerializer.Encode([]interface{}{...}) 的输出
                         也就是：AnyBox 包装 + AnyBox 的 gob 编码
```

Payload 总长 = 4 字节头部 + gob 编码后的 `GobWireReq`。

**为什么两层 gob？**
- 外层: `GobWireReq` 里全是具体类型（`string`, `[]byte`），gob 直接编。
- 内层: `ArgsBlob` 里是 `GobSerializer.Encode(...)` 的产出，用 `AnyBox` 把 `[]interface{}` 值包了一层，绕开 gob 不能把 concrete type 解到 `*interface{}` 的限制。

### C++ 实现

```
偏移    长度    字段
──────  ────    ────────────────────────────────────────────
  0      2      Magic = 0xA5A5  (uint16, 大端)
  2      M      Serializer.encode(Value{method})    —— type-tag 编码的 string
  2+M    N      Serializer.encode(Value{args})      —— type-tag 编码的 vector<ValueHolder>

  （C++ 没有单独的 version 字段 —— 保持极简；magic 本身就是版本哨兵）
```

C++ 没有 gob 限制要绕，所以就是一层平铺。

### 魔数的作用

| 常量            | 值     | 用途 |
|-----------------|--------|------|
| `MagicRequest`  | 0xA5A5 | 每条请求 payload 的开头 |
| `MagicResponse` | 0xA5A6 | 每条响应 payload 的开头 |

**为什么要魔数？** 三个原因：

1. **快速拒绝** —— 前两个字节不对就直接丢消息，不触发任何 serializer（gob panic 或 C++ throw）。
2. **区分类型** —— 同一条 transport 上既有请求也有响应，魔数告诉我们该用哪个 decoder。
3. **未来扩展** —— 协议 v2 可以直接换魔数，干净利落。

---

## 3. 响应 Payload（Layer 4）

### Go 实现

```
偏移    长度    字段
──────  ────    ────────────────────────────────────────────
  0      2      Magic = 0xA5A6  (uint16, 大端)
  2      N      gob(GobWireResp{IsError bool, BodyBlob []byte})

  GobWireResp.IsError  — 调用失败则为 true（方法不存在、panic、返回 error）
  GobWireResp.BodyBlob — GobSerializer.Encode(body) 的输出
                          （无返回值或错误无消息时为 nil）
```

**注意**: Go 的响应没有 version 字段（和请求不对称）。故意保持响应稍微小一点。

### C++ 实现

```
偏移    长度    字段
──────  ────    ────────────────────────────────────────────
  0      2      Magic = 0xA5A6  (uint16, 大端)
  2      M      Serializer.encode(Value{is_error}) —— type-tag 编码的 bool
  2+M    N      Serializer.encode(Value{body_blob_str})
               —— body 字节当作 type-tag 编码的 string
                 （C++ 没有专门的 "Blob" 类型，用 string 存 bytes）
```

---

## 4. TypeTag 取值（C++ 自研 Serializer）

C++ Serializer 在每个值前面写一个 1 字节 `TypeTag`，decoder 靠它知道后面该怎么解析。

| TypeTag | 值     | 线上布局 |
|---------|--------|----------|
| `Nil`    | 0x00   | `[0x00]` —— 一个字节，后面没了 |
| `Int32`  | 0x01   | `[0x01][4B LE int32]` |
| `Int64`  | 0x02   | `[0x02][8B LE int64]` |
| `Double` | 0x03   | `[0x03][8B LE IEEE 754 double bits]` |
| `String` | 0x04   | `[0x04][4B LE uint32 长度][原始字节]` |
| `Bool`   | 0x05   | `[0x05][1B: 0x00 或 0x01]` |
| `Vector` | 0x06   | `[0x06][4B LE uint32 元素个数][N 个元素，每个都是完整 Serializer 编码]` |

**字节序**: C++ serializer 内部统一小端（x86/x64 原生），**唯独**开头的 2 字节 magic 是大端（网络字节序）。

**编码示例** —— 整数 `42`:

```
01 2A 00 00 00
│  └── 4 字节 LE：42 = 0x0000002A
└── tag = Int32
```

**编码示例** —— 字符串 `"hi"`:

```
04 02 00 00 00 68 69
│  │           └── "hi" 的 ASCII
│  └── 长度 = 2（4 字节 LE）
└── tag = String
```

---

## 5. 为什么自己造格式？

minirpc **没有**用 protobuf、thrift、FlatBuffers、HTTP、gRPC、JSON-RPC 或任何序列化库。故意从零造的。

| 替代品                    | 为什么不用 |
|--------------------------|-----------|
| **Go gob**（标准库）      | 零依赖，够教学。但 `*interface{}` 限制逼出了我们的 AnyBox wrapper —— 这反而是个好的教学点，展示 serializer 和语言特性如何相互作用。 |
| **C++ 自研 TypeTag**      | C++ 没有标准 serializer。160 行 `std::variant` + 手写编解码，线上每一个字节都透明。不用 proto 编译器，不用维护 `.proto` 文件。 |
| **不用 HTTP**             | RPC 根本不需要应用层传输协议。裸 TCP 帧更快、更简单，也让学习者直接理解 framing / serialization / protocol 这三层各自做什么。 |

**教学优先，不是生产**：追求速度该用 FlatBuffers，追求互通该用 protobuf。minirpc 要的是让你看见 *每一个字节* —— 自研格式才能做到。

---

## 6. Go vs C++ 格式差异一览

```
传输层帧（完全一致）:
[4B 大端长度] [protocol payload 字节]

Protocol payload（不同）:

Go 请求:
  [2B magic (大端)] [2B version (大端)]
  [gob(GobWireReq{Method, ArgsBlob})]
    └─ ArgsBlob = GobSerializer.Encode([]interface{}) 输出
       （即 gob(AnyBox{TypeName, Payload}) × N）

C++ 请求:
  [2B magic (大端)]
  [Serializer.encode(string method)]
  [Serializer.encode(vector<ValueHolder> args)]
    └─ 没有第二层 —— variant 处理 concrete type 天然合适

Go 响应:
  [2B magic (大端)]
  [gob(GobWireResp{IsError, BodyBlob})]
    └─ BodyBlob = GobSerializer.Encode(body) 输出

C++ 响应:
  [2B magic (大端)]
  [Serializer.encode(bool is_error)]
  [Serializer.encode(string body_blob)]  ← body 字节编码成 string
```

两种语言在魔数（0xA5A5 / 0xA5A6）和 transport 帧（4 字节大端长度）上达成一致，但内层 payload 是语言原生格式。要实现跨语言互通需要统一中间格式 —— 超出了本教学项目的范围。
