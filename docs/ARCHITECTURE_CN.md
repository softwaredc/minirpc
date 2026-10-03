# ARCHITECTURE —— 六层架构深潜

本文逐层拆解 minirpc 的六层架构，解释每个设计选择背后的"为什么"，并附上源码中的真实注释代码。线上字节格式细节见 [PROTOCOL_CN.md](PROTOCOL_CN.md)。

---

## Layer 1: Server 监听与接受

**Go**: `net.Listen` + `ln.Accept` 无限循环。每个新连接起一个 goroutine —— `go s.handleConn(conn)`。

**C++**: `socket()` → `bind()` → `listen()` → `accept()` POSIX 调用。每个新连接起一个 `std::thread` 并 detach。

**核心认知**: TCP 是字节流，不是消息。`accept()` 每调用一次返回一个已连接 socket。拿到 socket 后交给 Layer 2 做消息帧封装。

### Go 代码片段 —— [server.go](../go/server.go)

```go
func (s *Server) ListenAndServe(addr string) error {
    ln, err := net.Listen("tcp", addr)
    defer ln.Close()

    for {
        conn, err := ln.Accept() // 阻塞调用 —— 每来一个新连接返回一次
        if err != nil { return err }
        // 每个连接一个 goroutine —— 并发处理多 client
        go s.handleConn(conn)
    }
}
```

### C++ 代码片段 —— [server.cpp](../cpp/src/server.cpp)

```cpp
int Server::listen_and_serve(const std::string& addr, int port) {
    int listen_fd = ::socket(AF_INET, SOCK_STREAM, 0);
    // bind, listen ...
    while (true) {
        int client_fd = ::accept(listen_fd, nullptr, nullptr);
        if (client_fd < 0) continue;
        std::thread t(handle_conn, client_fd, std::cref(registry_));
        t.detach(); // C++ 版的 go handleConn(conn)
    }
}
```

---

## Layer 2: Transport 编解码器

**职责**: 把 TCP 字节流变成"每次一个完整消息"。经典的长度前缀帧格式 —— 4 字节大端 uint32 告诉对端 body 有多长。

**为什么 4 字节？** 单条消息理论上限 4GB，够用。

**为什么大端？** 网络字节序约定（RFC 1700）。Go 和 C++ 两个 Codec 都用大端写长度前缀。

**partial read 处理**: `read()` / `io.Read` 不保证一次返回所有请求的字节。两端实现都用循环直到读满。Go 用 `io.ReadFull`（内部已实现循环），C++ 自己写 `read_full`。

### Go 代码片段 —— [pkg/transport/tcp.go](../go/pkg/transport/tcp.go)

```go
// 先读 4 字节大端长度前缀
_, err := io.ReadFull(c.conn, c.rBuf[:4])
length := binary.BigEndian.Uint32(c.rBuf[:4])

// 安全上限 —— 超过 64MB 拒绝，防 OOM
if length > 64*1024*1024 {
    return nil, fmt.Errorf("transport: message too large")
}

// io.ReadFull 内部已处理 partial read
buf := make([]byte, length)
_, err = io.ReadFull(c.conn, buf)
return buf, nil
```

### C++ 代码片段 —— [transport.cpp](../cpp/src/transport.cpp)

```cpp
ssize_t Codec::read_full(uint8_t* buf, size_t len) {
    size_t got = 0;
    while (got < len) {                          // 循环直到读满
        ssize_t n = ::read(fd_, buf + got, len - got);
        if (n > 0)      got += static_cast<size_t>(n);
        else if (n == 0) return static_cast<ssize_t>(got); // 对端正常关闭
        else if (errno == EINTR) continue;       // 信号中断 —— 重试
        else             return -1;              // 真正错误
    }
    return static_cast<ssize_t>(got);
}
```

---

## Layer 3: 序列化器

### Go —— `GobSerializer` + `AnyBox` 绕弯

Go 标准库 `encoding/gob` 有一个知名限制：**不能**把 concrete type（比如 `int(42)`）解码到 `*interface{}`。Gob 设计上只把 "远端 interface type" 解到 `interface{}`，concrete type 必须用具体类型指针接。

这在 RPC 里是个大问题 —— 我们希望 `Request.Args` / `Response.Body` 用 `interface{}` 传任意类型。

**我们的方案 —— `AnyBox`**:

1. 每个值塞进 `AnyBox{TypeName string, Payload []byte}` —— 两个字段全是具体类型，没有 `interface{}`。
2. 原始值 gob 编码进 `Payload`，类型名存 `TypeName`。
3. 把 `AnyBox` 整体再 gob 一次。Gob 全程没碰 `interface{}`。

解码时反序：

1. Gob 解出 `AnyBox`。
2. `TypeName` 反查 `reflect.Type`。
3. 分配该类型零值，`Payload` gob 解进去。
4. 赋值给 `*interface{}`。

### Go 代码片段 —— [pkg/serialize/gob.go](../go/pkg/serialize/gob.go)

```go
type AnyBox struct {
    TypeName string   // e.g. "int", "string", "main.Calculator"
    Payload  []byte   // gob 编码后的 concrete value
}

func (g *GobSerializer) Encode(v interface{}) ([]byte, error) {
    // 1. 原始值 gob 编码进 Payload
    var payloadBuf bytes.Buffer
    gob.NewEncoder(&payloadBuf).Encode(v)

    // 2. 包装成 AnyBox（全具体字段 —— gob 安全！）
    box := AnyBox{TypeName: g.getTypeName(reflect.TypeOf(v)),
                  Payload:  payloadBuf.Bytes()}

    // 3. AnyBox 整体再 gob 一次
    var buf bytes.Buffer
    gob.NewEncoder(&buf).Encode(box)
    return buf.Bytes(), nil
}
```

### C++ —— 自研 TypeTag 二进制格式

C++ 没有运行时反射，也没有 `interface{}`。"任意类型值"用 `std::variant` 表达，再自己设计一套带 TypeTag 的二进制格式：

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

**为什么小端？** x86/x64 原生就是小端 —— 直接 `memcpy`，不用字节翻转。

### C++ 代码片段 —— [serialize.cpp](../cpp/src/serialize.cpp)

```cpp
void Serializer::encode(const Value& v, std::vector<uint8_t>& out) {
    std::visit([&](auto&& arg) {
        using T = std::decay_t<decltype(arg)>;
        if constexpr (std::is_same_v<T, std::string>) {
            out.push_back(uint8_t(TypeTag::String));     // 1 字节 tag
            put_u32_le(out, uint32_t(arg.size()));        // 4 字节长度
            out.insert(out.end(), arg.begin(), arg.end()); // 原始字节
        }
        // ... 每种类型一个分支
    }, v);
}
```

---

## Layer 4: 协议层

**Go**: wire struct + 魔数 + 版本号。wire struct 全是具体类型（没有 `interface{}`），直接 gob 编码；内部用户数据走 `GobSerializer` → `AnyBox`。

**C++**: 同样的魔数 + 版本号设计，但 wire struct（method string + args vector）直接用我们的 `Serializer` 编码。只有 body 走第二层。

### Go wire struct —— [protocol.go](../go/pkg/protocol/protocol.go)

```go
// GobWireReq 所有字段都是具体类型 —— plain gob 安全
type GobWireReq struct {
    Method   string   // concrete
    ArgsBlob []byte   // concrete —— 已 AnyBox 包装
}

const (
    MagicRequest   uint16 = 0xA5A5
    MagicResponse  uint16 = 0xA5A6
    CurrentVersion uint16 = 1
)
```

**为什么 wire struct 和 user data 分开？** wire struct 属于 RPC *协议本身* —— 描述"一次调用如何在网络上框定"。user data 是应用层想传什么就传什么。这让传输层关注点和应用层关注点干净分离。

### C++ marshal —— [protocol.cpp](../cpp/src/protocol.cpp)

```cpp
std::vector<uint8_t> marshal_request(const std::string& method,
                                      const std::vector<ValueHolder>& args) {
    std::vector<uint8_t> payload;
    Serializer::encode(Value{method}, payload);  // type-tag 编码的 string
    Serializer::encode(Value{args},   payload);  // type-tag 编码的 vector

    std::vector<uint8_t> out;
    put_u16_be(out, kMagicRequest);               // 0xA5A5 —— 网络字节序
    out.insert(out.end(), payload.begin(), payload.end());
    return out;
}
```

---

## Layer 5: 服务注册表

注册表把字符串方法名映射到可调用对象。

**Go**: `map[string]reflect.Value` —— 值是反射拿到的 `method.Func`，调用时 `fn.Call(args)`。

**C++**: `map[string]std::function<Value(const std::vector<ValueHolder>&)>` —— 所有 handler 强制统一签名，没有反射，没有魔法。

### Go 代码片段 —— [server.go](../go/server.go)

```go
// 注册时 —— 扫描 struct 所有可导出方法
for i := 0; i < t.NumMethod(); i++ {
    method := t.Method(i)
    name := typName + "." + method.Name   // e.g. "Calculator.Add"
    s.registry[name] = method.Func        // reflect.Value —— 随时可 Call()
}

// 调用时 —— 查表、组装参数、反射调用
fn, ok := s.registry[req.Method]
results := fn.Call(args)  // 一行代码 = 一次 RPC dispatch
```

### C++ 代码片段 —— [server.cpp](../cpp/src/server.cpp)

```cpp
// 注册 —— 就是存一个 std::function
using Handler = std::function<Value(const std::vector<ValueHolder>&)>;
void Server::register_handler(const std::string& name, Handler h) {
    registry_[name] = std::move(h);
}

// 调用 —— std::map 查找 + 普通 C++ 函数调用
auto it = registry.find(req.method);
Value result = it->second(req.args);   // 就是个 std::function 的 operator()
```

**关键差异**: Go 的 `reflect.Call` 天然支持异构参数类型（int、string、struct 混合传），但需要做类型 coercion。C++ 强制所有 handler 统一收 `vector<ValueHolder>`，handler 自己拆类型。这正是语言反射能力差异导致的框架设计差异。

---

## Layer 6: 客户端调用

客户端的事很简单：dial → marshal 请求 → 发送 → 等响应 → unmarshal → 查错误标志 → 返回 body。

### Go 代码片段 —— [client.go](../go/client.go)

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

### C++ 代码片段 —— [client.cpp](../cpp/src/client.cpp)

```cpp
CallResult Client::call(const std::string& method,
                        const std::vector<ValueHolder>& args) {
    auto req_bytes = marshal_request(method, args);
    codec_->write_msg(req_bytes);

    std::vector<uint8_t> resp_bytes;
    codec_->read_msg(resp_bytes);

    WireResp resp = unmarshal_response(resp_bytes.data(), resp_bytes.size());

    if (resp.is_error) { /* 构造错误返回 */ }
    result.body = Serializer::decode_all(resp.body_blob);
    return result;
}
```

注意 Go client 和 C++ client 完全遵循**相同的五步流程**。即使实现细节不同，两个框架共享同一个分层心智模型。

---

## 总结 —— 数据全流程

```
Client.Call("Calculator.Add", 3, 4)
  │
  ├─ Layer 4: MarshalRequest → [magic][version][gob(wire struct)]
  │                              └─ Layer 3: GobSerializer.Encode(3,4) → AnyBox 包装
  │
  ├─ Layer 2: Codec.WriteMessage → [4B big-endian len][protocol bytes]
  ├─ Layer 1: TCP write → socket
  │              ║ 网络 ║
  │
  ├─ Layer 1: TCP read → socket → Server.handleConn
  ├─ Layer 2: Codec.ReadMessage → [protocol bytes]
  ├─ Layer 4: UnmarshalRequest → Request{Method: "Calculator.Add", Args: [3,4]}
  │              └─ Layer 3: GobSerializer.Decode → 拆 AnyBox
  ├─ Layer 5: registry["Calculator.Add"].Call([recv, 3, 4]) → 7
  └─ ... 响应路径镜像请求路径反向走一遍
```
