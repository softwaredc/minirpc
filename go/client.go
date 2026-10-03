/*
Package minirpc — RPC Client（Layer 6：Stub 调用）。

Client 做三件事：
  1. Dial 到服务端 TCP 端口
  2. 把一次 "调用方法 + 参数" 变成协议层的 Request
  3. 等待 Response 回来、解成 interface{} 返回

完整的 Stub 调用是：
  client.Call("Calculator.Add", 3, 4) → 7

进阶版可以让 Client 自己维护注册表（反射动态构造参数），
这里简单处理：Call() 直接接受方法名 + 参数列表。
*/
package minirpc

import (
	"fmt"
	"net"
	"time"

	"github.com/minirpc/minirpc/pkg/protocol"
	"github.com/minirpc/minirpc/pkg/serialize"
	"github.com/minirpc/minirpc/pkg/transport"
)

// Client 是一个极简 RPC 客户端。
type Client struct {
	addr string
	ser  protocol.Serializer

	// codecs 保存每条连接的 codec
	// 我们做连接池了吗？没做，简化版一次 Dial 一个连接，用完 Close。
	// 进阶版应该做连接池 + 长连接复用。
}

// NewClient 构造一个 Client。
// 可以传自定义 Serializer，不传默认 gob。
func NewClient(opts ...ClientOption) *Client {
	c := &Client{
		ser: serialize.NewGobSerializer(),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// ClientOption 是函数式选项，让 Client 可以灵活配置。
type ClientOption func(*Client)

// WithSerializer 允许自定义序列化器。
func WithSerializer(ser protocol.Serializer) ClientOption {
	return func(c *Client) { c.ser = ser }
}

// Dial 建立到 addr 的 TCP 连接并返回一个 Caller。
//
// 为什么不把 Dial 直接写进 Client 方法？因为 Caller 持有一条
// 已建立的连接（codec），Client 只是"配置容器"。
// 这样 Client 可以 Dial 多个 Caller，各管一条连接。
func (c *Client) Dial(addr string) (*Caller, error) {
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("client.dial: %w", err)
	}
	fmt.Printf("[minirpc] dialed %s\n", addr)
	return &Caller{
		codec: transport.NewCodec(conn),
		ser:   c.ser,
	}, nil
}

// Caller 持有一条已建立连接，负责实际 RPC 调用。
type Caller struct {
	codec *transport.Codec
	ser   protocol.Serializer
}

// Close 关闭底层连接。
func (c *Caller) Close() error {
	return c.codec.Close()
}

// Call 发起一次 RPC 调用，返回 Response 里的 Body。
//
// 参数：
//   method:  "Calculator.Add" — 完整的 "类型.方法" 名字
//   args:    可变参数，直接传给注册函数
//
// 返回值 interface{} 是 gob 解码后的结果。
// 进阶版可以改成泛型 Call[T](method, args...) T。
func (c *Caller) Call(method string, args ...interface{}) (interface{}, error) {
	// 1. 序列化（新版接口直接接 method + args）
	reqBytes, err := protocol.MarshalRequest(method, args, c.ser)
	if err != nil {
		return nil, fmt.Errorf("client.call.marshal: %w", err)
	}

	// 3. 发送
	if err := c.codec.WriteMessage(reqBytes); err != nil {
		return nil, fmt.Errorf("client.call.write: %w", err)
	}

	// 4. 等响应
	respBytes, err := c.codec.ReadMessage()
	if err != nil {
		return nil, fmt.Errorf("client.call.read: %w", err)
	}

	// 5. 反序列化
	resp, err := protocol.UnmarshalResponse(respBytes, c.ser)
	if err != nil {
		return nil, fmt.Errorf("client.call.unmarshal: %w", err)
	}

	// 6. 检查错误标志
	if resp.IsError {
		errStr, _ := resp.Body.(string)
		return nil, fmt.Errorf("rpc error: %s", errStr)
	}
	return resp.Body, nil
}
