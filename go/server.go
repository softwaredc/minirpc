/*
Package minirpc — RPC Server（Layer 1 监听 + Layer 5 服务注册）。

RPC Server 做三件事：
  1. 监听一个端口 for 新连接（Layer 1）
  2. 维护一张 "method name → 函数" 的注册表（Layer 5）
  3. 收到请求 → 反序列化 → 查注册表 → 反射调用 → 序列化响应

为什么把 Server 写成单独一个 package 而不是和 pkg/* 平级？
  - pkg/ 下是分层组件（每层职责单一）
  - 根包 Server/Client 把它们组合起来
  - 这是 Go 的常见组织方式
*/
package minirpc

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"strings"
	"time"

	"github.com/minirpc/minirpc/pkg/protocol"
	"github.com/minirpc/minirpc/pkg/serialize"
	"github.com/minirpc/minirpc/pkg/transport"
)

// Server 是一个极简 RPC 服务端。
//
// 构造用 NewServer()，注册方法用 server.Register(new(YourService))，
// 启动用 server.ListenAndServe(":9999")。
type Server struct {
	// registry: "Calculator.Add" → reflect.Value of (int, int) int
	// 为什么用 reflect.Value？因为 reflect.Call 需要它。
	registry map[string]reflect.Value

	// ser 是当前用的序列化器。默认 gob。
	ser protocol.Serializer

	// connTimeout 给每个连接设置的读写超时。
	// 生产框架会细粒度到每个请求，这里先统一。
	connTimeout time.Duration
}

// NewServer 构造一个 Server，默认 gob 序列化器、无读写超时。
func NewServer() *Server {
	return &Server{
		registry:    make(map[string]reflect.Value),
		ser:         serialize.NewGobSerializer(),
		connTimeout: 10 * time.Second,
	}
}

// Register 注册一个服务对象。
//
// 传入的 svc 应该是一个 struct（或 *struct），它的每个可导出方法
// 都会被注册，名称格式为 "Type.Method"。
//
// 为什么这样设计？因为 Go net/rpc 就是这样做的，而且调用方只需要
// 写一个带方法的 struct 就完事，零配置。
//
// 示例：
//
//	type Calculator struct{}
//	func (Calculator) Add(a, b int) int { return a + b }
//	func (Calculator) Mul(a, b int) int { return a * b }
//	sv.Register(Calculator{})
//	// 注册了 "Calculator.Add" 和 "Calculator.Mul"
//
// 约束（检查规则）：
//  1. 方法必须可导出（首字母大写）
//  2. 方法参数和返回值必须是 gob 能序列化的类型
//  3. 方法返回值最多一个（简单版，进阶支持多返回 + error）
func (s *Server) Register(svc interface{}) error {
	v := reflect.ValueOf(svc)
	t := v.Type()

	// svc 可以是 struct 也可以是 *struct，统一解引用
	// 因为 reflect.Method 里的 Receiver 类型可能有差异
	if v.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if v.Kind() != reflect.Ptr && v.Kind() != reflect.Struct {
		return fmt.Errorf("server.Register: need struct or *struct, got %v", v.Kind())
	}

	typName := t.Name()
	if typName == "" {
		return fmt.Errorf("server.Register: anonymous type not allowed")
	}

	for i := 0; i < t.NumMethod(); i++ {
		method := t.Method(i)
		name := typName + "." + method.Name

		// 只注册可导出方法（虽然 t.NumMethod() 只返回可导出的，
		// 但保险起见再查一遍）
		if !method.IsExported() {
			continue
		}

		// 检查参数：除了 Receiver，其余全部可序列化？
		// 简化版不做 gob 预检查——反序列化时自然会发现问题。

		s.registry[name] = method.Func
	}
	return nil
}

// ListenAndServe 在 addr 上监听，循环 accept 连接并处理。
//
// 为什么用 for { accept } 循环？因为 TCP accept 是阻塞调用，
// 每来一个连接就返回一次。处理完循环回去继续 accept。
// 生产版会对每个连接起 goroutine（并发处理多个 client），
// 但这里先串行，进阶版再改。
func (s *Server) ListenAndServe(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("server.listen: %w", err)
	}
	defer ln.Close()
	fmt.Printf("[minirpc] listening on %s, registered %d methods\n", addr, len(s.registry))

	for {
		conn, err := ln.Accept()
		if err != nil {
			return fmt.Errorf("server.accept: %w", err)
		}
		// 每个连接起一个 goroutine——这是让 Server 能并发处理多 client 的关键。
		// 为什么用 goroutine 而不是线程？Go runtime 自动调度，量级轻。
		go s.handleConn(conn)
	}
}

// handleConn 处理一条连接的生命周期：循环读请求、调用、写响应，直到对端关闭。
//
// 为什么需要循环？因为同一个 TCP 连接上可以发多次 RPC（keep-alive 语义）。
// 进阶版会支持并发多路复用，但这里一个连接一条串行调用链。
func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()

	codec := transport.NewCodec(conn)
	if s.connTimeout > 0 {
		codec.SetDeadline(time.Now().Add(s.connTimeout))
	}

	for {
		// 1. 读完整消息
		data, err := codec.ReadMessage()
		if err != nil {
			// 对端关闭是正常结束，只打一行日志就退出
			if strings.Contains(err.Error(), "connection closed") {
				fmt.Println("[minirpc] peer closed")
			} else {
				fmt.Printf("[minirpc] read error: %v\n", err)
			}
			return
		}

		// 2. 反序列化成 Request
		req, err := protocol.UnmarshalRequest(data, s.ser)
		if err != nil {
			fmt.Printf("[minirpc] decode req error: %v\n", err)
			continue // 这个消息坏了，丢掉继续
		}

		// 3. 查注册表 → 反射调用
		resp := s.invoke(req)

		// 4. 序列化 Response
		respBytes, err := protocol.MarshalResponse(resp.IsError, resp.Body, s.ser)
		if err != nil {
			fmt.Printf("[minirpc] marshal resp error: %v\n", err)
			continue
		}

		// 5. 写回
		if err := codec.WriteMessage(respBytes); err != nil {
			fmt.Printf("[minirpc] write resp error: %v\n", err)
			return
		}

		// 重置 deadline（keep-alive 连接每次调用都刷新）
		if s.connTimeout > 0 {
			codec.SetDeadline(time.Now().Add(s.connTimeout))
		}
	}
}

// invoke 根据 req.Method 找注册表、执行反射调用、返回 Response。
//
// 这是整个 Server 最核心的函数——把一个字符串方法名 + 一组参数
// 变成一次真实的 Go 函数调用。
func (s *Server) invoke(req *protocol.Request) *protocol.Response {
	resp := &protocol.Response{}

	fn, ok := s.registry[req.Method]
	if !ok {
		resp.IsError = true
		resp.Body = fmt.Sprintf("method %q not found", req.Method)
		return resp
	}

	// 构造参数 slice：reflect.Call 需要 []reflect.Value
	numIn := fn.Type().NumIn() // 含 Receiver
	// 第一个参数必须是 Receiver（就是我们注册的那个对象实例）
	// 但我们注册时用的是 method.Func，它的第一个参数是 Receiver 值
	args := make([]reflect.Value, 0, numIn)

	// 第一个参数永远是 Receiver。reflect.Type.Method 返回的 Func
	// 签名里第一个参数是 Receiver。
	// 但我们注册的是 method.Func，不是 method.Value.Call，
	// 所以需要自己把 Receiver 加进去。
	//
	// 这是 Go reflect API 一个容易踩坑的点：
	// - method.Func 的类型签名包含 receiver
	// - method.Value.Call 会自动传 receiver（不把它算在 args 里）
	// 我们用的是 method.Func → 所以第一个参数就是 receiver。
	//
	// 解决方案：构造一个 receiver 值 + 用户参数列表
	recvType := fn.Type().In(0) // Receiver 的类型
	// 构造一个零值 receiver——因为我们只关心方法本身（纯函数语义）
	// 如果方法依赖 receiver 内部状态，这里会有问题，进阶版改进。
	recv := reflect.New(recvType).Elem()
	args = append(args, recv)

	// 把 gob 解码出来的 []interface{} 转成 reflect.Value 列表
	for i := 1; i < numIn; i++ {
		expectedType := fn.Type().In(i)

		// Args 可能不够（对端没发够参数）
		if req.Args == nil || i-1 >= len(req.Args) {
			resp.IsError = true
			resp.Body = fmt.Sprintf("too few args for %s (need %d)", req.Method, numIn-1)
			return resp
		}

		// gob 把数字编码成 int 或 float64，可能和预期类型不匹配，
		// 这里尝试做基本类型转换。
		v, err := coerceArg(req.Args[i-1], expectedType)
		if err != nil {
			resp.IsError = true
			resp.Body = fmt.Sprintf("arg %d: %v", i-1, err)
			return resp
		}
		args = append(args, v)
	}

	// 反射调用！一行代码把字符串方法名变成真实 Go 函数调用
	results := fn.Call(args)

	// 处理返回值
	// 规则：如果最后一个返回值是 error 且不为 nil → 当作 RPC 错误
	//       如果有多个非 error 返回值 → 合并成 []interface{} 返回
	numOut := fn.Type().NumOut()
	if numOut == 0 {
		resp.IsError = false
		resp.Body = nil
	} else {
		// 检查最后一个返回值是不是 error
		lastType := fn.Type().Out(numOut - 1)
		isErrorType := lastType.Implements(reflect.TypeOf((*error)(nil)).Elem())

		if isErrorType {
			// 有 error 返回值
			errVal := results[numOut-1]
			if !errVal.IsNil() {
				// error 非 nil → 当作 RPC 错误
				errObj := errVal.Interface().(error)
				resp.IsError = true
				resp.Body = errObj.Error()
				return resp
			}
			// error 为 nil → 正常返回前面的值
			numOut-- // 忽略 error 位
		}

		if numOut == 0 {
			resp.IsError = false
			resp.Body = nil
		} else if numOut == 1 {
			resp.IsError = false
			resp.Body = results[0].Interface()
		} else {
			// 多个返回值 → 合并成 slice
			out := make([]interface{}, numOut)
			for i := 0; i < numOut; i++ {
				out[i] = results[i].Interface()
			}
			resp.IsError = false
			resp.Body = out
		}
	}
	return resp
}

// coerceArg 把 gob 解码出来的 interface{} 转成目标类型。
//
// gob 把 int/uint 都编成 int，把浮点数编成 float64。
// 但目标函数可能要 int32 / int64 / float32。这里做一个
// 常见类型转换表。
func coerceArg(val interface{}, target reflect.Type) (reflect.Value, error) {
	if val == nil {
		// 目标允许 nil（interface{} / 指针）？
		if target.Kind() == reflect.Interface || target.Kind() == reflect.Ptr {
			return reflect.Zero(target), nil
		}
		return reflect.Value{}, fmt.Errorf("nil cannot convert to %s", target)
	}

	v := reflect.ValueOf(val)

	// 如果类型直接匹配，返回原值
	if v.Type() == target {
		return v, nil
	}

	// 数字类型互相转换（int → int32, int64, float64 等）
	switch target.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		// 源可能是 int 或 float64
		switch num := val.(type) {
		case int:
			return reflect.ValueOf(int64(num)).Convert(target), nil
		case float64:
			return reflect.ValueOf(int64(num)).Convert(target), nil
		default:
			return reflect.Value{}, fmt.Errorf("cannot convert %T to %s", val, target)
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		switch num := val.(type) {
		case int:
			return reflect.ValueOf(uint64(num)).Convert(target), nil
		case float64:
			return reflect.ValueOf(uint64(num)).Convert(target), nil
		default:
			return reflect.Value{}, fmt.Errorf("cannot convert %T to %s", val, target)
		}
	case reflect.Float32, reflect.Float64:
		switch num := val.(type) {
		case int:
			return reflect.ValueOf(float64(num)).Convert(target), nil
		case float64:
			return reflect.ValueOf(num).Convert(target), nil
		default:
			return reflect.Value{}, fmt.Errorf("cannot convert %T to %s", val, target)
		}
	case reflect.String:
		if s, ok := val.(string); ok {
			return reflect.ValueOf(s), nil
		}
	}

	// 最后尝试 reflect.Convert（Go 自带的类型转换规则）
	if v.Type().ConvertibleTo(target) {
		return v.Convert(target), nil
	}
	return reflect.Value{}, fmt.Errorf("cannot convert %T (%s) to %s", val, v.Type(), target)
}

// context 导入是为了未来扩展支持 context.Context 参数，
// 目前没用上，保留作为接口扩展点。
var _ = context.Background
