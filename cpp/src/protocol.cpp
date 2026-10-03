/*
 * protocol.cpp — 协议层实现
 *
 * 流程：验 magic → 验 version → 外层 POD struct（手工序列化）
 * 内层 user data 用 Serializer 编解码。
 */
#include "minirpc/protocol.hpp"
#include <cstring>
#include <stdexcept>

namespace minirpc {

// 大端写 uint16（网络字节序）
static void put_u16_be(std::vector<uint8_t>& out, uint16_t v) {
    out.push_back(uint8_t((v >> 8) & 0xFF));
    out.push_back(uint8_t(v & 0xFF));
}
static uint16_t get_u16_be(const uint8_t* p) {
    return (uint16_t(p[0]) << 8) | uint16_t(p[1]);
}

/* ---------- 编解码 ---------- */
/*
 * 简化策略：把 method/args/is_error/body 全部当作 Serializer 的 Value
 * 来编解码。外层只加 magic。
 *
 * 请求字节序列：
 *   [2B magic][method(string varint)][args(vector varint)]
 *
 * 响应字节序列：
 *   [2B magic][is_error(bool varint)][body_blob(string varint, 内含 Body 的 Serializer 编码)]
 */

std::vector<uint8_t> marshal_request(const std::string& method,
                                      const std::vector<ValueHolder>& args) {
    std::vector<uint8_t> payload;
    Serializer::encode(Value{method}, payload);
    Serializer::encode(Value{args}, payload);

    std::vector<uint8_t> out;
    out.reserve(4 + payload.size());
    put_u16_be(out, kMagicRequest);
    out.insert(out.end(), payload.begin(), payload.end());
    return out;
}

WireReq unmarshal_request(const uint8_t* data, size_t len) {
    if (len < 2) throw std::runtime_error("protocol.req: too short");
    uint16_t magic = get_u16_be(data);
    if (magic != kMagicRequest) throw std::runtime_error("protocol.req: bad magic");

    // decode 的 consumed 是"本次消耗"，需要手动维护 offset
    size_t offset = 2;
    size_t consumed_this = 0;
    Value method_v = Serializer::decode(data + offset, len - offset, consumed_this);
    offset += consumed_this;
    Value args_v = Serializer::decode(data + offset, len - offset, consumed_this);
    offset += consumed_this;

    WireReq r;
    if (auto* s = std::get_if<std::string>(&method_v)) r.method = *s;
    else throw std::runtime_error("protocol.req: method not string");
    if (auto* vec = std::get_if<std::vector<ValueHolder>>(&args_v)) r.args = *vec;
    else throw std::runtime_error("protocol.req: args not vector");
    return r;
}

std::vector<uint8_t> marshal_response(bool is_error, const Value& body) {
    // 内层：把 body 编码成 blob
    std::vector<uint8_t> body_blob;
    Serializer::encode(body, body_blob);

    // 外层：bool + vector<uint8_t>
    std::vector<uint8_t> payload;
    Serializer::encode(Value{is_error}, payload);
    // body_blob 当作一个 vector<ValueHolder> 来编码——
    // 或者直接把 bytes 塞进去（加一个 "Bytes" 类型太复杂，
    // 我们用 vector<ValueHolder> 里装多个 Int64 来表达 bytes）。
    // 简化：直接把 body_blob 的 bytes 追加到 payload
    // 但前面需要一个 Vector tag 来标记这是 blob...
    // 干脆把 body_blob 当成 string 编码！因为 string 就是 bytes。
    std::string blob_str(reinterpret_cast<const char*>(body_blob.data()), body_blob.size());
    Serializer::encode(Value{blob_str}, payload);

    std::vector<uint8_t> out;
    out.reserve(2 + payload.size());
    put_u16_be(out, kMagicResponse);
    out.insert(out.end(), payload.begin(), payload.end());
    return out;
}

WireResp unmarshal_response(const uint8_t* data, size_t len) {
    if (len < 2) throw std::runtime_error("protocol.resp: too short");
    uint16_t magic = get_u16_be(data);
    if (magic != kMagicResponse) throw std::runtime_error("protocol.resp: bad magic");

    size_t offset = 2;
    size_t consumed_this = 0;
    Value err_v = Serializer::decode(data + offset, len - offset, consumed_this);
    offset += consumed_this;
    Value blob_v = Serializer::decode(data + offset, len - offset, consumed_this);

    WireResp r;
    if (auto* b = std::get_if<bool>(&err_v)) r.is_error = *b;
    if (auto* s = std::get_if<std::string>(&blob_v)) {
        r.body_blob.assign(s->begin(), s->end());
    }
    return r;
}

} // namespace minirpc
