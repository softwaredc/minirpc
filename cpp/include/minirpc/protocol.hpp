/*
 * protocol.hpp — RPC 框架协议层（Layer 4）
 *
 * C++ 版协议格式（与 Go 版一致的设计思路，只是用不同的序列化）：
 *
 * 请求：[4B length][2B magic(0xA5A5)][2B version(1)][序列化的Req]
 *   Req  = { string method, vector<ValueHolder> args }
 *
 * 响应：[4B length][2B magic(0xA5A6)][序列化的Resp]
 *   Resp = { bool is_error, Value body }
 *
 * 和 Go 版的差异：
 *   - Go 用 gob，C++ 用我们自己的 Serializer（小端+TypeTag）
 *   - Go 有两层 gob（wire struct + AnyBox），C++ 只有一层
 *     因为 C++ 的 variant 处理 concrete type 更自然，
 *     不像 Go gob 有 interface{} 解码限制
 */
#pragma once

#include <cstdint>
#include <cstddef>
#include <string>
#include <vector>

#include "minirpc/serialize.hpp"

namespace minirpc {

// 魔数
static constexpr uint16_t kMagicRequest  = 0xA5A5;
static constexpr uint16_t kMagicResponse = 0xA5A6;
static constexpr uint16_t kCurrentVersion = 1;

// C++ 版直接用 POD struct，自定义二进制序列化
struct WireReq {
    std::string method;
    std::vector<ValueHolder> args;
};

struct WireResp {
    bool is_error = false;
    // body 编码成 bytes 再外层 struct——类似 Go 的两层设计
    // 但 C++ 里直接编码 Value 就行
    std::vector<uint8_t> body_blob; // Serializer 编码后的 Value
};

// 请求/响应的编解码（与 Go 版对称）
std::vector<uint8_t> marshal_request(const std::string& method,
                                      const std::vector<ValueHolder>& args);
// 解码请求，抛出 std::runtime_error on error
WireReq unmarshal_request(const uint8_t* data, size_t len);

std::vector<uint8_t> marshal_response(bool is_error, const Value& body);
WireResp unmarshal_response(const uint8_t* data, size_t len);

} // namespace minirpc
