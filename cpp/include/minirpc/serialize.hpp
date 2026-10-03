/*
 * serialize.hpp — RPC 框架序列化层（Layer 3）
 *
 * C++ 版自己实现一个极简类型化二进制格式。
 * 不依赖 protobuf / msgpack / nlohmann/json——零依赖。
 *
 * 格式：每个值前面有一个 TypeTag（1 字节），标明类型。
 *
 * TypeTag:
 *   0x00 = nil
 *   0x01 = int32  （4 字节小端）
 *   0x02 = int64  （8 字节小端）
 *   0x03 = double （8 字节 IEEE 754）
 *   0x04 = string （4B 小端长度 + bytes）
 *   0x05 = bool   （1 字节 0/1）
 *   0x06 = vector （4B 小端长度 + 每个元素递归编码）
 *
 * 为什么小端？C++ 在 x86/x64 上是小端原生，直接 memcpy 就行。
 * 跨大端平台？教学项目不考虑，进阶版可以加字节序标记。
 *
 * 为什么和 Go 版不同？Go 版用 gob，C++ 没有现成的等价物，
 * 手写一个更能体现"编码"到底在做什么。
 */
#pragma once

#include <cstdint>
#include <cstddef>
#include <string>
#include <vector>
#include <variant>
#include <stdexcept>

namespace minirpc {

// 类型标签
enum class TypeTag : uint8_t {
    Nil    = 0x00,
    Int32  = 0x01,
    Int64  = 0x02,
    Double = 0x03,
    String = 0x04,
    Bool   = 0x05,
    Vector = 0x06,
};

// Value 是一个"任意类型"的值——类似 Go 的 interface{}
// C++17 std::variant 比 union 安全多了
using Value = std::variant<
    std::monostate,   // Nil
    int32_t,          // Int32
    int64_t,          // Int64
    double,           // Double
    std::string,      // String
    bool,             // Bool
    std::vector<struct ValueHolder>  // Vector — 元素是 ValueHolder
>;

// ValueHolder 包装 Value，让 vector<ValueHolder> 可以递归定义
struct ValueHolder {
    Value v;
};

// Serializer —— 把 Value 编成 bytes / 把 bytes 解回 Value
class Serializer {
public:
    // 把一个 Value 编成字节流，追加到 out
    static void encode(const Value& v, std::vector<uint8_t>& out);

    // 把字节流的前 n 字节解成一个 Value，返回该 Value
    // consumed 输出参数：实际消耗了多少字节
    static Value decode(const uint8_t* data, size_t len, size_t& consumed);

    // 便捷版本：把整个 vector 解成 Value（假设没有多余字节）
    static Value decode_all(const std::vector<uint8_t>& data);

private:
    // 小端编码工具
    static void put_u16_le(std::vector<uint8_t>& out, uint16_t v);
    static void put_u32_le(std::vector<uint8_t>& out, uint32_t v);
    static void put_u64_le(std::vector<uint8_t>& out, uint64_t v);

    static uint16_t get_u16_le(const uint8_t* p);
    static uint32_t get_u32_le(const uint8_t* p);
    static uint64_t get_u64_le(const uint8_t* p);
};

} // namespace minirpc
