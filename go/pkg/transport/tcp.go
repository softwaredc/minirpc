/*
Package transport 负责 RPC 框架的传输层（Layer 2）。

这个包只做一件事：把一个 TCP 连接封装成"带长度前缀的消息读写器"。
它不关心 RPC 协议是什么、不关心序列化格式——它只负责
"从 net.Conn 里读出一个完整的二进制消息" 和 "把一个二进制消息写出去"。

为什么需要独立出来？因为如果不加长度前缀，TCP 是流，你不知道
什么时候一条消息结束。RPC 协议的二进制格式里有 Length 字段，
但那是更上层的事情。这里的长度前缀只负责"一个 Codec 一次读写一个消息"。

和 Go 标准库 net/rpc 的 Codec 接口概念一样，但我们自己实现一遍。
*/
package transport

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// Codec 是消息编解码器。它封装一个 net.Conn，
// 在收发两端都用 4 字节大端无符号整数做消息长度前缀。
//
// ┌─────────────┬──────────────────┐
// │ Length (4B) │ Body (Length 字节) │
// └─────────────┴──────────────────┘
//
// 为什么用大端？跨平台网络传输的通用约定（RFC 1700）。
// 为什么用 uint32？单条消息理论上不会超过 4GB，足够。
type Codec struct {
	conn net.Conn

	// 读的时候要并发安全吗？我们的 RPC 场景是
	// "一个请求一个响应，串行发送/接收"，所以读写各自加锁就行。
	// 如果做全双工，可以让 Reader 和 Writer 各自独立 goroutine，
	// 但那是进阶话题，本简化版先串行。
	rMu sync.Mutex // 保护 ReadMessage 并发
	wMu sync.Mutex // 保护 WriteMessage 并发

	// 读写缓冲区。预分配好复用，减少 GC 压力。
	// 这是一个"优化"，但放在这里解释一下：
	// 每条消息一个新的 byte slice 会让 GC 频繁工作。
	// 复用缓冲区是高性能网络服务的常见做法。
	rBuf [4]byte // 读长度前缀用
	wBuf [4]byte // 写长度前缀用

	// Read 出来的消息会放到这里。调用方拿到后可以复用。
	// 我们提供 ResetReadBuffer 让上层归还。
	readPool sync.Pool
}

// NewCodec 用一个现成的 net.Conn 构造 Codec。
// 为什么需要传入现成的？因为"谁来 accept 连接"是更上层 Server 的事，
// transport 层只负责"对一个已建立的连接做编解码"。
func NewCodec(conn net.Conn) *Codec {
	c := &Codec{conn: conn}
	// sync.Pool 是 Go 官方提供的对象池，GC 压力小。
	c.readPool = sync.Pool{
		New: func() interface{} {
			// 默认给 4KB 起始，不够上层可以扩容
			b := make([]byte, 0, 4096)
			return &b
		},
	}
	return c
}

// ReadMessage 从连接读出一条完整消息，返回消息体。
//
// 流程：
//  1. 读 4 字节长度前缀（大端 uint32）
//  2. 分配/复用 body buffer
//  3. 循环读 body，直到读满 length 字节
//
// 为什么不一次性 Read 够？因为 TCP recv 不保证一次读完，
// 需要 for 循环处理 partial read。这是写网络代码的基本常识。
func (c *Codec) ReadMessage() ([]byte, error) {
	c.rMu.Lock()
	defer c.rMu.Unlock()

	// 1. 读长度前缀（恰好 4 字节）
	_, err := io.ReadFull(c.conn, c.rBuf[:4])
	if err != nil {
		// io.EOF 表示对端正常关闭。其他错误透传。
		if err == io.EOF {
			return nil, fmt.Errorf("transport: connection closed by peer")
		}
		return nil, fmt.Errorf("transport: read length: %w", err)
	}
	length := binary.BigEndian.Uint32(c.rBuf[:4])

	// 安全检查：长度为 0 或过大都拒绝
	// 为什么有长度上限？防止对端发一个超大 Length 让我们 OOM。
	// 64MB 足够任何教学场景，生产环境可以调大。
	if length == 0 {
		return nil, fmt.Errorf("transport: zero-length message")
	}
	const maxMsgLen = 64 * 1024 * 1024
	if length > maxMsgLen {
		return nil, fmt.Errorf("transport: message too large (%d > %d)", length, maxMsgLen)
	}

	// 2. 分配 buffer：优先用 pool 的，容量不够就新 make
	pooled := c.readPool.Get().(*[]byte)
	var buf []byte
	if cap(*pooled) >= int(length) {
		buf = (*pooled)[:length]
	} else {
		// pool buffer 太小，放回 pool，新 make 一个
		c.readPool.Put(pooled)
		buf = make([]byte, length)
		pooled = nil // 标记为非 pooled，后面不归还
	}

	// 3. 读 body（io.ReadFull 内部循环直到读满 length 或出错）
	_, err = io.ReadFull(c.conn, buf)
	if err != nil {
		if pooled != nil {
			c.readPool.Put(pooled) // 归还，避免浪费
		}
		return nil, fmt.Errorf("transport: read body: %w", err)
	}

	// 简化起见不做归还——真正的优化版会提供 Release(buf) 方法。
	return buf, nil
}

// WriteMessage 把 body 写进连接，前面带 4 字节大端长度前缀。
//
// 为什么要先写前缀再写 body？因为对端需要先知道消息长度，
// 才能正确地 ReadFull。这是 RPC 协议的基础约定。
func (c *Codec) WriteMessage(body []byte) error {
	c.wMu.Lock()
	defer c.wMu.Unlock()

	length := uint32(len(body))
	// 安全检查：长度超上限拒绝
	if length > 64*1024*1024 {
		return fmt.Errorf("transport: message too large (%d)", length)
	}

	// 1. 写长度前缀
	binary.BigEndian.PutUint32(c.wBuf[:4], length)
	if _, err := c.conn.Write(c.wBuf[:4]); err != nil {
		return fmt.Errorf("transport: write length: %w", err)
	}

	// 2. 写 body
	if len(body) > 0 {
		if _, err := c.conn.Write(body); err != nil {
			return fmt.Errorf("transport: write body: %w", err)
		}
	}
	return nil
}

// SetDeadline 给底层连接设置读写超时。
// 为什么需要这个？如果对端不发也不收，我们会永久阻塞。
// 生产级 RPC 框架会有心跳和 deadline，这里暴露接口让上层调。
func (c *Codec) SetDeadline(t time.Time) error {
	return c.conn.SetDeadline(t)
}

// Close 关闭底层连接。
func (c *Codec) Close() error {
	return c.conn.Close()
}
