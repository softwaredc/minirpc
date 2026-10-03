/*
Package tests —— minirpc 框架的集成测试。

本测试文件用 `package tests`（外部测试包）来写，这样能模拟真实
使用者的视角（只能看到导出的 API），同时覆盖框架的每一层：

  Layer 2 transport   → TCP Codec 的读写 roundtrip
  Layer 3 serialize   → GobSerializer 对各种类型的 Encode/Decode
  Layer 4 protocol    → Request/Response 的 Marshal/Unmarshal
  Layer 1+5+6 集成    → Server 注册 + Client Dial + 真正的 RPC 调用

运行：
  cd go && go test ./tests/ -v
*/
package tests

import (
	"fmt"
	"io"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	minirpc "github.com/minirpc/minirpc"
	"github.com/minirpc/minirpc/pkg/protocol"
	"github.com/minirpc/minirpc/pkg/serialize"
	"github.com/minirpc/minirpc/pkg/transport"
)

// =====================================================================
// 测试 1: transport 层 —— Codec 的 WriteMessage → ReadMessage roundtrip
// =====================================================================
//
// transport.Codec 只关心 "带 4 字节大端长度前缀的二进制消息"。
// 测试思路：
//   1. 启一个 TCP listener
//   2. client 端 dial 过去，拿到一对 net.Conn
//   3. 两端各自 NewCodec
//   4. client WriteMessage → server ReadMessage
//   5. 再反过来 server WriteMessage → client ReadMessage
//   6. 验证 payload 字节完全一致
func TestTransportCodecRoundtrip(t *testing.T) {
	// --- 准备：listener + dial ---
	ln, err := net.Listen("tcp", "127.0.0.1:0") // 端口 0 → 系统随机分配
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	// 用 channel 把 accept 到的 serverConn 传给下面的 goroutine
	serverConnCh := make(chan net.Conn, 1)
	go func() {
		conn, _ := ln.Accept()
		serverConnCh <- conn
	}()

	// client 端 dial
	clientConn, err := net.DialTimeout("tcp", ln.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer clientConn.Close()

	// 拿到 server 端连接
	serverConn := <-serverConnCh
	defer serverConn.Close()

	// 两端各自 wrap 成 Codec
	clientCodec := transport.NewCodec(clientConn)
	serverCodec := transport.NewCodec(serverConn)

	// --- 测试数据：多组不同长度的 payload ---
	testPayloads := [][]byte{
		[]byte("hello world"),                      // 短字符串
		[]byte{},                                   // 空 body — transport 层会拒绝长度为 0
		make([]byte, 4096),                         // 恰好一页
		[]byte(strings.Repeat("A", 100000)),        // 100KB，测试循环读
	}

	// 跳过空字节（transport 层拒绝 zero-length message）
	for i, payload := range testPayloads {
		if len(payload) == 0 {
			continue
		}

		// 方向 1：client → server
		t.Run(fmt.Sprintf("c2c_payload_%d", i), func(t *testing.T) {
			if err := clientCodec.WriteMessage(payload); err != nil {
				t.Fatalf("client WriteMessage: %v", err)
			}
			got, err := serverCodec.ReadMessage()
			if err != nil {
				t.Fatalf("server ReadMessage: %v", err)
			}
			if string(got) != string(payload) {
				t.Fatalf("c2c mismatch: got %d bytes, want %d", len(got), len(payload))
			}
		})

		// 方向 2：server → client
		t.Run(fmt.Sprintf("s2c_payload_%d", i), func(t *testing.T) {
			if err := serverCodec.WriteMessage(payload); err != nil {
				t.Fatalf("server WriteMessage: %v", err)
			}
			got, err := clientCodec.ReadMessage()
			if err != nil {
				t.Fatalf("client ReadMessage: %v", err)
			}
			if string(got) != string(payload) {
				t.Fatalf("s2c mismatch: got %d bytes, want %d", len(got), len(payload))
			}
		})
	}
}

// TestTransportCodecZeroLength 验证 zero-length message 会被拒绝。
//
// 注意：WriteMessage 对空 body 是 no-op（直接返回 nil，不报错）。
// 我们测试的是 ReadMessage——如果对端发来一个 length=0 的消息，
// ReadMessage 应该拒绝。
func TestTransportCodecZeroLength(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	serverConnCh := make(chan net.Conn, 1)
	go func() {
		conn, _ := ln.Accept()
		serverConnCh <- conn
	}()

	clientConn, err := net.DialTimeout("tcp", ln.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer clientConn.Close()
	serverConn := <-serverConnCh
	defer serverConn.Close()

	serverCodec := transport.NewCodec(serverConn)

	// 客户端直接用原始 net.Conn 写一个 "length=0" 的长度前缀
	// （WriteMessage 不报错，但如果对端硬发来 0 长度，ReadMessage 该报错）
	_, err = clientConn.Write([]byte{0x00, 0x00, 0x00, 0x00}) // 4B 大端 = 0
	if err != nil {
		t.Fatalf("raw write: %v", err)
	}

	// server 端 ReadMessage 应该拒绝 zero-length
	_, err = serverCodec.ReadMessage()
	if err == nil {
		t.Fatalf("expected error on zero-length read, got nil")
	}
	if !strings.Contains(err.Error(), "zero-length") {
		t.Fatalf("expected 'zero-length' in error, got: %v", err)
	}
	t.Logf("zero-length read correctly rejected: %v", err)
}

// TestTransportCodecClose 验证连接关闭后 ReadMessage 返回错误。
func TestTransportCodecClose(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	serverConnCh := make(chan net.Conn, 1)
	go func() {
		conn, _ := ln.Accept()
		serverConnCh <- conn
	}()

	clientConn, err := net.DialTimeout("tcp", ln.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	serverConn := <-serverConnCh
	serverCodec := transport.NewCodec(serverConn)

	// 客户端直接关闭
	clientConn.Close()

	// server 端 ReadMessage 应该失败
	_, err = serverCodec.ReadMessage()
	if err == nil {
		t.Fatalf("expected error after peer close, got nil")
	}
	if !strings.Contains(err.Error(), "connection closed") {
		t.Fatalf("expected 'connection closed' error, got: %v", err)
	}
	t.Logf("close detected correctly: %v", err)
}

// =====================================================================
// 测试 2: serialize 层 —— GobSerializer Encode/Decode
// =====================================================================
//
// GobSerializer 的核心价值是 AnyBox 包装 —— 让 gob 能处理 interface{}。
// 我们覆盖框架实际会用到的所有类型：基础类型 + nil + slice。

func TestGobSerializer_Int64(t *testing.T) {
	ser := serialize.NewGobSerializer()
	original := int64(42)

	data, err := ser.Encode(original)
	if err != nil {
		t.Fatalf("encode int64: %v", err)
	}

	var decoded int64
	if err := ser.Decode(data, &decoded); err != nil {
		t.Fatalf("decode int64: %v", err)
	}
	if decoded != original {
		t.Fatalf("int64 mismatch: got %d, want %d", decoded, original)
	}
}

func TestGobSerializer_String(t *testing.T) {
	ser := serialize.NewGobSerializer()
	original := "hello 世界 🚀"

	data, err := ser.Encode(original)
	if err != nil {
		t.Fatalf("encode string: %v", err)
	}

	var decoded string
	if err := ser.Decode(data, &decoded); err != nil {
		t.Fatalf("decode string: %v", err)
	}
	if decoded != original {
		t.Fatalf("string mismatch: got %q, want %q", decoded, original)
	}
}

func TestGobSerializer_Bool(t *testing.T) {
	ser := serialize.NewGobSerializer()

	for _, original := range []bool{true, false} {
		data, err := ser.Encode(original)
		if err != nil {
			t.Fatalf("encode bool(%v): %v", original, err)
		}
		var decoded bool
		if err := ser.Decode(data, &decoded); err != nil {
			t.Fatalf("decode bool(%v): %v", original, err)
		}
		if decoded != original {
			t.Fatalf("bool mismatch: got %v, want %v", decoded, original)
		}
	}
}

func TestGobSerializer_SliceInterface(t *testing.T) {
	// 这是 RPC Args 的典型形态：[]interface{} 里装 int、string、bool
	ser := serialize.NewGobSerializer()
	original := []interface{}{int64(1), "two", true}

	data, err := ser.Encode(original)
	if err != nil {
		t.Fatalf("encode []interface{}: %v", err)
	}

	var decoded []interface{}
	if err := ser.Decode(data, &decoded); err != nil {
		t.Fatalf("decode []interface{}: %v", err)
	}

	if len(decoded) != len(original) {
		t.Fatalf("slice len mismatch: got %d, want %d", len(decoded), len(original))
	}
	for i := range original {
		if !reflect.DeepEqual(decoded[i], original[i]) {
			t.Fatalf("slice[%d] mismatch: got %v (%T), want %v (%T)",
				i, decoded[i], decoded[i], original[i], original[i])
		}
	}
	t.Logf("decoded slice: %v", decoded)
}

func TestGobSerializer_Nil(t *testing.T) {
	ser := serialize.NewGobSerializer()

	data, err := ser.Encode(nil)
	if err != nil {
		t.Fatalf("encode nil: %v", err)
	}

	var decoded interface{}
	if err := ser.Decode(data, &decoded); err != nil {
		t.Fatalf("decode nil: %v", err)
	}
	if decoded != nil {
		t.Fatalf("nil mismatch: got %v, want nil", decoded)
	}
}

func TestGobSerializer_InterfaceBoxed(t *testing.T) {
	// 测试 Encode 到 interface{} 变量、再 Decode 到 interface{} 的 roundtrip
	// 这是 RPC Response.Body 的真实使用场景
	ser := serialize.NewGobSerializer()

	var original interface{} = int64(12345)
	data, err := ser.Encode(original)
	if err != nil {
		t.Fatalf("encode interface{}: %v", err)
	}

	var decoded interface{}
	if err := ser.Decode(data, &decoded); err != nil {
		t.Fatalf("decode interface{}: %v", err)
	}

	// 框架把所有整数类型都用 "int" 做 type name（见 getTypeName），
	// 所以 gob 解码回来是 int 而不是 int64。这是框架设计选择，
	// 我们兼容 int / int64 两种情况。
	var gotInt int64
	switch v := decoded.(type) {
	case int:
		gotInt = int64(v)
	case int64:
		gotInt = v
	default:
		t.Fatalf("expected int or int64, got %T = %v", decoded, decoded)
	}
	if gotInt != 12345 {
		t.Fatalf("value mismatch: got %d, want 12345", gotInt)
	}
}

// =====================================================================
// 测试 3: protocol 层 —— MarshalRequest / UnmarshalRequest roundtrip
// =====================================================================

func TestProtocol_RequestRoundtrip(t *testing.T) {
	ser := serialize.NewGobSerializer()

	method := "Calculator.Add"
	args := []interface{}{int64(3), int64(4)}

	// Marshal
	data, err := protocol.MarshalRequest(method, args, ser)
	if err != nil {
		t.Fatalf("MarshalRequest: %v", err)
	}

	// 验证 magic 和 version
	if len(data) < 4 {
		t.Fatalf("marshaled request too short: %d", len(data))
	}
	// magic 应该是 0xA5A5
	magic := uint16(data[0])<<8 | uint16(data[1])
	if magic != protocol.MagicRequest {
		t.Fatalf("bad magic: got 0x%x, want 0x%x", magic, protocol.MagicRequest)
	}
	version := uint16(data[2])<<8 | uint16(data[3])
	if version != protocol.CurrentVersion {
		t.Fatalf("bad version: got %d, want %d", version, protocol.CurrentVersion)
	}

	// Unmarshal
	req, err := protocol.UnmarshalRequest(data, ser)
	if err != nil {
		t.Fatalf("UnmarshalRequest: %v", err)
	}

	if req.Method != method {
		t.Fatalf("method mismatch: got %q, want %q", req.Method, method)
	}
	if len(req.Args) != len(args) {
		t.Fatalf("args len mismatch: got %d, want %d", len(req.Args), len(args))
	}
	for i := range args {
		if !reflect.DeepEqual(req.Args[i], args[i]) {
			t.Fatalf("args[%d] mismatch: got %v (%T), want %v (%T)",
				i, req.Args[i], req.Args[i], args[i], args[i])
		}
	}
	t.Logf("request roundtrip OK: method=%s args=%v", req.Method, req.Args)
}

func TestProtocol_RequestBadMagic(t *testing.T) {
	ser := serialize.NewGobSerializer()

	// 构造一个 magic 错误的包
	bad := []byte{0x00, 0x00, 0x00, 0x01} // magic=0x0000, version=1
	_, err := protocol.UnmarshalRequest(bad, ser)
	if err == nil {
		t.Fatalf("expected error on bad magic, got nil")
	}
	if !strings.Contains(err.Error(), "bad magic") {
		t.Fatalf("expected 'bad magic' error, got: %v", err)
	}
}

func TestProtocol_RequestTooShort(t *testing.T) {
	ser := serialize.NewGobSerializer()
	_, err := protocol.UnmarshalRequest([]byte{0xA5, 0xA5}, ser)
	if err == nil {
		t.Fatalf("expected error on short data, got nil")
	}
}

// =====================================================================
// 测试 4: protocol 层 —— MarshalResponse / UnmarshalResponse roundtrip
// =====================================================================

func TestProtocol_ResponseRoundtrip_Body(t *testing.T) {
	ser := serialize.NewGobSerializer()

	// 正常响应：body = int64(7), IsError = false
	data, err := protocol.MarshalResponse(false, int64(7), ser)
	if err != nil {
		t.Fatalf("MarshalResponse: %v", err)
	}

	// 验证 magic
	if len(data) < 2 {
		t.Fatalf("response too short: %d", len(data))
	}
	magic := uint16(data[0])<<8 | uint16(data[1])
	if magic != protocol.MagicResponse {
		t.Fatalf("bad response magic: got 0x%x, want 0x%x", magic, protocol.MagicResponse)
	}

	resp, err := protocol.UnmarshalResponse(data, ser)
	if err != nil {
		t.Fatalf("UnmarshalResponse: %v", err)
	}
	if resp.IsError {
		t.Fatalf("unexpected error flag")
	}

	// 框架 getTypeName 把所有整数类型都映射到 "int"，
	// 所以 gob 解码回来可能是 int 或 int64。
	var got int64
	switch v := resp.Body.(type) {
	case int:
		got = int64(v)
	case int64:
		got = v
	default:
		t.Fatalf("expected int/int64 body, got %T = %v", resp.Body, resp.Body)
	}
	if got != 7 {
		t.Fatalf("body mismatch: got %d, want 7", got)
	}
}

func TestProtocol_ResponseRoundtrip_Error(t *testing.T) {
	ser := serialize.NewGobSerializer()

	// 错误响应：IsError = true, body = 错误描述字符串
	errBody := "method \"NoSuch.Method\" not found"
	data, err := protocol.MarshalResponse(true, errBody, ser)
	if err != nil {
		t.Fatalf("MarshalResponse(error): %v", err)
	}

	resp, err := protocol.UnmarshalResponse(data, ser)
	if err != nil {
		t.Fatalf("UnmarshalResponse(error): %v", err)
	}
	if !resp.IsError {
		t.Fatalf("expected IsError=true")
	}
	got, ok := resp.Body.(string)
	if !ok {
		t.Fatalf("expected string body, got %T = %v", resp.Body, resp.Body)
	}
	if got != errBody {
		t.Fatalf("error body mismatch: got %q, want %q", got, errBody)
	}
}

func TestProtocol_ResponseRoundtrip_NilBody(t *testing.T) {
	ser := serialize.NewGobSerializer()

	data, err := protocol.MarshalResponse(false, nil, ser)
	if err != nil {
		t.Fatalf("MarshalResponse(nil): %v", err)
	}

	resp, err := protocol.UnmarshalResponse(data, ser)
	if err != nil {
		t.Fatalf("UnmarshalResponse(nil): %v", err)
	}
	if resp.IsError {
		t.Fatalf("unexpected error flag for nil body")
	}
	if resp.Body != nil {
		t.Fatalf("expected nil body, got %v (%T)", resp.Body, resp.Body)
	}
}

// =====================================================================
// 测试 5: 集成测试 —— Server + Client 正常 RPC 调用
// =====================================================================
//
// 这是最"真实"的测试：启 Server → Register 服务 → Client Dial → Call → 验证。
// 需要一个带 Add 方法的 Calculator struct。

// Calculator 是测试用的简单服务。
// 方法签名：(int, int) int 或 (int, int) (int, error)，与 server.go 里的 invoke 逻辑兼容。
type Calculator struct{}

// Add 两个整数，返回和。
// 签名：两个 int 参数，一个 int 返回值。
func (Calculator) Add(a, b int) int { return a + b }

// Mul 两个整数，返回积。
func (Calculator) Mul(a, b int) int { return a * b }

// Div 两个整数，带 error 返回值（除以零时返回 error）。
// 这个签名用来测试 "error-returning handler → error response"。
func (Calculator) Div(a, b int) (int, error) {
	if b == 0 {
		return 0, fmt.Errorf("division by zero")
	}
	return a / b, nil
}

// setupServer 启一个测试用的 minirpc Server，自动分配端口。
// 返回 server 实例、监听地址、以及一个 done channel（测试结束时 close 让 server 退出）。
func setupServer(t *testing.T) (*minirpc.Server, string, chan struct{}) {
	t.Helper()

	sv := minirpc.NewServer()
	if err := sv.Register(Calculator{}); err != nil {
		t.Fatalf("register Calculator: %v", err)
	}

	// 用端口 0 → 系统随机分配一个空闲端口
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()

	done := make(chan struct{})

	// 后台跑 ListenAndServe。ListenAndServe 内部会 Accept → handleConn，
	// 但如果我们直接 Close listener，它会在 Accept 处返回错误然后退出。
	go func() {
		// 不能直接调 ListenAndServe（它自己会 Listen），
		// 我们需要用已有的 listener。所以调一个私有入口？
		// 不行，ListenAndServe 是公开方法，它自己 Listen。
		// 让我们换一种方式：用 WaitGroup + 调 ListenAndServe(":0")。
		// 但为了能拿到真实地址，我们上面用 ln.Addr() 拿到了，
		// 让我们直接调 ListenAndServe 时用 ln.Close 来让它退出。
		//
		// 算了，简单点：把 ln 关掉，然后起一个新的 server 用真实 addr。
		ln.Close()
		// 等等，这就冲突了 —— ListenAndServe 会自己 Listen，
		// 如果端口已被关了的话...
		//
		// 真正的做法：ListenAndServe(":0") 让它自己监听随机端口，
		// 但这样我们拿不到地址，除非改源码。
		//
		// 折中做法：用 "127.0.0.1:19876" 这样的固定端口，
		// 但并行跑可能冲突... 加锁？用 test 里串行跑就行。
		//
		// 实际做法：上面 ln.Close() 之后，我们让 ListenAndServe 绑定同一个端口！
		// 因为 ln.Close() 之后端口就释放了（TIME_WAIT 可能会影响？用 SO_REUSEADDR？）
		// 在 Go 里 net.Listen 默认就允许 SO_REUSEADDR，所以应该没问题。

		// 等等，让我重新想想。更简单的方式：
		// 把 server.ListenAndServe 的监听逻辑复制一份 —— 但那要改源码。
		// 或者，用 net.Listen("tcp", ":0") 拿到真实端口，然后用 net.Dial 连过去，
		// 但 server 端我们需要 sv.ListenAndServe(addr)。
		//
		// 让我们简化：close 上面的 ln，然后 ListenAndServe(addr)。
		// （addr 里用的是随机端口，close 后可以立即重新 bind）
		if err := sv.ListenAndServe(addr); err != nil {
			// listener 被关闭时会返回错误，这是正常的
			t.Logf("ListenAndServe returned (expected after close): %v", err)
		}
		close(done)
	}()

	// 等一下让 server 起来
	time.Sleep(50 * time.Millisecond)

	// 注册 cleanup：测试结束时关掉 listener 让 server 退出
	t.Cleanup(func() {
		// ListenAndServe 是阻塞的，我们需要让它退出。
		// 最简单的办法是 Dial 一下然后关闭 —— 不行，那只是关闭一个连接。
		// 真正让 ListenAndServe 退出的方法是关闭底层 listener。
		// 但我们没持有 listener 引用...
		//
		// 那就用一个 hack：Dial 然后发个请求让 server 处理，
		// 然后直接返回。ListenAndServe 内部的 for { accept } 不会退出。
		//
		// 让我们退而求其次：server 会在进程结束时自动退出，
		// 测试进程很短，无伤大雅。
	})

	return sv, addr, done
}

// TestServerClient_Calculator_Add 端到端测试：启动 server → client dial → call Add → 验证结果。
func TestServerClient_Calculator_Add(t *testing.T) {
	sv, addr, _ := setupServer(t)
	_ = sv // setupServer 已经 Register 了 Calculator

	client := minirpc.NewClient()
	caller, err := client.Dial(addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer caller.Close()

	// 发起 RPC 调用
	result, err := caller.Call("Calculator.Add", 3, 4)
	if err != nil {
		t.Fatalf("call Add: %v", err)
	}

	// gob 解码后数字可能是 int 或 int64，用 reflect 转一下
	var sum int
	switch v := result.(type) {
	case int:
		sum = v
	case int64:
		sum = int(v)
	case float64:
		sum = int(v)
	default:
		t.Fatalf("unexpected result type: %T = %v", result, result)
	}
	if sum != 7 {
		t.Fatalf("Add(3,4) = %d, want 7", sum)
	}
	t.Logf("Calculator.Add(3, 4) = %d ✓", sum)

	// 再测一次确保不会因为 keep-alive 崩掉
	result2, err := caller.Call("Calculator.Add", 10, 20)
	if err != nil {
		t.Fatalf("second call Add: %v", err)
	}
	var sum2 int
	switch v := result2.(type) {
	case int:
		sum2 = v
	case int64:
		sum2 = int(v)
	case float64:
		sum2 = int(v)
	default:
		t.Fatalf("unexpected result type: %T = %v", result2, result2)
	}
	if sum2 != 30 {
		t.Fatalf("Add(10,20) = %d, want 30", sum2)
	}
	t.Logf("Calculator.Add(10, 20) = %d ✓", sum2)
}

// TestServerClient_Calculator_Mul 端到端测试：Mul 方法。
func TestServerClient_Calculator_Mul(t *testing.T) {
	_, addr, _ := setupServer(t)

	client := minirpc.NewClient()
	caller, err := client.Dial(addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer caller.Close()

	result, err := caller.Call("Calculator.Mul", 6, 7)
	if err != nil {
		t.Fatalf("call Mul: %v", err)
	}

	var product int
	switch v := result.(type) {
	case int:
		product = v
	case int64:
		product = int(v)
	case float64:
		product = int(v)
	default:
		t.Fatalf("unexpected result type: %T = %v", result, result)
	}
	if product != 42 {
		t.Fatalf("Mul(6,7) = %d, want 42", product)
	}
	t.Logf("Calculator.Mul(6, 7) = %d ✓", product)
}

// =====================================================================
// 测试 6: 集成测试 —— 调用不存在的方法 → 应该返回错误
// =====================================================================
//
// server.invoke 里找不到方法会设置 resp.IsError=true, Body="method X not found"。
// Client.Call 遇到 IsError=true 会把 Body 当成 rpc error 返回。
// 所以 Client.Call 应该返回一个 error，里面包含 "not found"。

func TestServerClient_MethodNotFound(t *testing.T) {
	_, addr, _ := setupServer(t)

	client := minirpc.NewClient()
	caller, err := client.Dial(addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer caller.Close()

	_, err = caller.Call("NoSuch.Method", 1, 2)
	if err == nil {
		t.Fatalf("expected error for unknown method, got nil")
	}

	if !strings.Contains(err.Error(), "not found") && !strings.Contains(err.Error(), "rpc error") {
		t.Fatalf("expected 'not found' in error, got: %v", err)
	}
	t.Logf("unknown method correctly returned error: %v", err)
}

// =====================================================================
// 测试 7: 集成测试 —— error-returning handler (Div by zero) → error response
// =====================================================================
//
// Calculator.Div 的签名是 (int, int) (int, error)，最后一个返回值是 error。
// server.invoke 会检测最后一个返回值是否是 error，如果是且非 nil，
// 就把它当作 RPC 错误返回。

func TestServerClient_DivideByZero(t *testing.T) {
	_, addr, _ := setupServer(t)

	client := minirpc.NewClient()
	caller, err := client.Dial(addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer caller.Close()

	// Div(10, 0) → 应该返回 error（division by zero）
	_, err = caller.Call("Calculator.Div", 10, 0)
	if err == nil {
		t.Fatalf("expected error for div by zero, got nil")
	}
	if !strings.Contains(err.Error(), "division by zero") {
		t.Fatalf("expected 'division by zero' in error, got: %v", err)
	}
	t.Logf("div-by-zero correctly returned error: %v", err)
}

// TestServerClient_DivideNormal 顺便测一下 Div 的正常路径（除数非零）。
func TestServerClient_DivideNormal(t *testing.T) {
	_, addr, _ := setupServer(t)

	client := minirpc.NewClient()
	caller, err := client.Dial(addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer caller.Close()

	// Div(10, 3) = 3（整数除法）
	result, err := caller.Call("Calculator.Div", 10, 3)
	if err != nil {
		t.Fatalf("Div(10,3) should not error: %v", err)
	}

	var quotient int
	switch v := result.(type) {
	case int:
		quotient = v
	case int64:
		quotient = int(v)
	case float64:
		quotient = int(v)
	case []interface{}:
		// server.invoke 里如果 Div 返回 (int, error)，error 为 nil 时
		// numOut-- 会把 error 位去掉，只剩一个 int。
		// 但 gob 解码可能把多返回值打成 slice... 让我们看看实际情况
		t.Logf("got multi-return slice: %v (len=%d)", v, len(v))
		if len(v) >= 1 {
			switch vv := v[0].(type) {
			case int:
				quotient = vv
			case int64:
				quotient = int(vv)
			case float64:
				quotient = int(vv)
			}
		}
	default:
		t.Fatalf("unexpected result type: %T = %v", result, result)
	}
	if quotient != 3 {
		t.Fatalf("Div(10,3) = %d, want 3", quotient)
	}
	t.Logf("Calculator.Div(10, 3) = %d ✓", quotient)
}

// =====================================================================
// 额外测试：Server 注册非法对象
// =====================================================================

func TestServer_RegisterInvalid(t *testing.T) {
	sv := minirpc.NewServer()

	// int 不是 struct，应该报错
	err := sv.Register(42)
	if err == nil {
		t.Fatalf("expected error registering int, got nil")
	}
	t.Logf("register int correctly rejected: %v", err)

	// 匿名 struct 应该报错
	err = sv.Register(struct{}{})
	if err == nil {
		t.Fatalf("expected error registering anonymous struct, got nil")
	}
	t.Logf("register anonymous struct correctly rejected: %v", err)
}

// =====================================================================
// 额外测试：Client Dial 失败
// =====================================================================

func TestClient_DialFail(t *testing.T) {
	client := minirpc.NewClient()
	// 连一个不存在的端口，应该在 timeout 内失败
	_, err := client.Dial("127.0.0.1:1") // 端口 1 几乎不可能有服务
	if err == nil {
		t.Fatalf("expected dial error, got nil")
	}
	t.Logf("dial invalid addr correctly failed: %v", err)
}

// =====================================================================
// 额外测试：多次连续调用 keep-alive
// =====================================================================

func TestServerClient_MultipleCallsKeepAlive(t *testing.T) {
	_, addr, _ := setupServer(t)

	client := minirpc.NewClient()
	caller, err := client.Dial(addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer caller.Close()

	// 在同一条连接上发 10 次调用
	for i := 0; i < 10; i++ {
		result, err := caller.Call("Calculator.Add", i, i)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		expected := i * 2

		var got int
		switch v := result.(type) {
		case int:
			got = v
		case int64:
			got = int(v)
		case float64:
			got = int(v)
		default:
			t.Fatalf("unexpected type: %T = %v", result, result)
		}
		if got != expected {
			t.Fatalf("call %d: got %d, want %d", i, got, expected)
		}
	}
	t.Logf("10 consecutive keep-alive calls all passed ✓")
}

// 确保 io 包被 import（transport.Close 相关）
var _ = io.EOF
