/*
Package protocol 负责 RPC 框架的协议层（Layer 4）。

═══════════════════════════════════════════════════════════════════════
为什么 wire struct 直接用 encoding/gob，user data 用 GobSerializer？
═══════════════════════════════════════════════════════════════════════

GobSerializer 有一个 AnyBox 包装（类型名 + 数据blob）来绕开 gob 对
interface{} concrete type 解码的限制。

但 wire struct（GobWireReq / GobWireResp）里全是具体类型：
  string, []byte, bool — 没有 interface{}！
所以 wire struct 可以直接用 encoding/gob 编码，不用 AnyBox 包装。

这样整套编码只有**两层**（不是三层）：
  外层：直接 gob 编码 GobWireReq/GobWireResp（无 interface{}）
  内层：GobSerializer.AnyBox 编码 args/body（有 interface{}，需要包装）

线上字节：
  [2B magic][2B version][直接 gob(GobWireReq)]
  GobWireReq.ArgsBlob 里 = GobSerializer.AnyBox 编码的 []interface{}

═══════════════════════════════════════════════════════════════════════
*/
package protocol

import (
	"bytes"
	"encoding/gob"
	"fmt"

	"github.com/minirpc/minirpc/pkg/serialize"
)

// Serializer 类型别名，保持 protocol 层面向接口。
type Serializer = serialize.Serializer

// 魔数，区分请求/响应，用于快速校验。
const (
	MagicRequest   uint16 = 0xA5A5
	MagicResponse  uint16 = 0xA5A6
	CurrentVersion uint16 = 1
)

// GobWireReq 是请求的外层 wire struct。
// 注意：所有字段都是 gob-friendly 的具体类型，不含 interface{}。
// ArgsBlob 是内层 GobSerializer 编码好的 bytes。
type GobWireReq struct {
	Method   string
	ArgsBlob []byte
}

// GobWireResp 是响应的外层 wire struct。
type GobWireResp struct {
	IsError  bool
	BodyBlob []byte
}

// Request / Response 是上层看到的完整对象。
type Request struct {
	Magic   uint16
	Version uint16
	Method  string
	Args    []interface{}
}

type Response struct {
	IsError bool
	Body    interface{}
}

/*
 * 编码
 */

// MarshalRequest 把 method + args 编成线上字节。
//
// 步骤：
//  1. 内层 GobSerializer 编码 args → argsBlob（AnyBox 包装，绕开 interface{} 限制）
//  2. 直接 gob 编码 GobWireReq{Method, argsBlob} → payload
//  3. 拼 magic + version + payload
func MarshalRequest(method string, args []interface{}, ser Serializer) ([]byte, error) {
	// Step 1: 内层 —— user args 需要 GobSerializer（interface{} 包装）
	argsBlob, err := ser.Encode(args)
	if err != nil {
		return nil, fmt.Errorf("protocol.req.args: %w", err)
	}

	// Step 2: wire struct 全是具体类型，直接 gob
	var payloadBuf bytes.Buffer
	enc := gob.NewEncoder(&payloadBuf)
	if err := enc.Encode(GobWireReq{Method: method, ArgsBlob: argsBlob}); err != nil {
		return nil, fmt.Errorf("protocol.req.wire: %w", err)
	}

	// Step 3: magic + version + payload
	payload := payloadBuf.Bytes()
	out := make([]byte, 0, 4+len(payload))
	out = appendUint16BE(out, MagicRequest)
	out = appendUint16BE(out, CurrentVersion)
	out = append(out, payload...)
	return out, nil
}

// MarshalResponse 把 isError + body 编成线上字节。
func MarshalResponse(isError bool, body interface{}, ser Serializer) ([]byte, error) {
	// Step 1: 内层 —— user body 需要 GobSerializer（interface{} 包装）
	bodyBlob, err := ser.Encode(body)
	if err != nil {
		return nil, fmt.Errorf("protocol.resp.body: %w", err)
	}

	// Step 2: wire struct 直接 gob
	var payloadBuf bytes.Buffer
	enc := gob.NewEncoder(&payloadBuf)
	if err := enc.Encode(GobWireResp{IsError: isError, BodyBlob: bodyBlob}); err != nil {
		return nil, fmt.Errorf("protocol.resp.wire: %w", err)
	}

	payload := payloadBuf.Bytes()
	out := make([]byte, 0, 2+len(payload))
	out = appendUint16BE(out, MagicResponse)
	out = append(out, payload...)
	return out, nil
}

/*
 * 解码 —— 与编码对称
 */

// UnmarshalRequest 把 transport 层 bytes 解成 Request。
func UnmarshalRequest(data []byte, ser Serializer) (*Request, error) {
	if len(data) < 4 {
		return nil, fmt.Errorf("protocol.req: too short (%d)", len(data))
	}
	magic := readUint16BE(data[0:2])
	if magic != MagicRequest {
		return nil, fmt.Errorf("protocol.req: bad magic 0x%x", magic)
	}
	ver := readUint16BE(data[2:4])
	if ver != CurrentVersion {
		return nil, fmt.Errorf("protocol.req: bad version %d", ver)
	}

	// 直接 gob 解 wire struct（全具体类型，安全）
	var wire GobWireReq
	if err := gob.NewDecoder(bytes.NewReader(data[4:])).Decode(&wire); err != nil {
		return nil, fmt.Errorf("protocol.req.wire: %w", err)
	}

	// 内层 GobSerializer 解 args
	var args []interface{}
	if wire.ArgsBlob != nil {
		if err := ser.Decode(wire.ArgsBlob, &args); err != nil {
			return nil, fmt.Errorf("protocol.req.args: %w", err)
		}
	}

	return &Request{Magic: magic, Version: ver, Method: wire.Method, Args: args}, nil
}

// UnmarshalResponse 把 transport 层 bytes 解成 Response。
func UnmarshalResponse(data []byte, ser Serializer) (*Response, error) {
	if len(data) < 2 {
		return nil, fmt.Errorf("protocol.resp: too short (%d)", len(data))
	}
	magic := readUint16BE(data[0:2])
	if magic != MagicResponse {
		return nil, fmt.Errorf("protocol.resp: bad magic 0x%x", magic)
	}

	// 直接 gob 解 wire struct
	var wire GobWireResp
	if err := gob.NewDecoder(bytes.NewReader(data[2:])).Decode(&wire); err != nil {
		return nil, fmt.Errorf("protocol.resp.wire: %w", err)
	}

	// 内层 GobSerializer 解 body
	var body interface{}
	if wire.BodyBlob != nil && len(wire.BodyBlob) > 0 {
		if err := ser.Decode(wire.BodyBlob, &body); err != nil {
			return nil, fmt.Errorf("protocol.resp.body: %w", err)
		}
	}

	return &Response{IsError: wire.IsError, Body: body}, nil
}

/*
 * 大端 uint16 编解码（手写两行，避免 import encoding/binary）
 */
func appendUint16BE(buf []byte, v uint16) []byte {
	buf = append(buf, byte(v>>8), byte(v))
	return buf
}

func readUint16BE(data []byte) uint16 {
	return uint16(data[0])<<8 | uint16(data[1])
}
