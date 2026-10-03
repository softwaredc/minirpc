/*
 * serialize.cpp — 自定义类型化二进制编解码实现
 *
 * 核心思路：每个值前面先写 TypeTag（1B），然后按类型写 payload。
 * 解码时先读 TypeTag，switch 到对应解码分支。
 *
 * 这个格式故意做得很简单，教学用。
 * 真实项目可以用 msgpack / protobuf / flatbuffers。
 */
#include "minirpc/serialize.hpp"
#include <cstring>

namespace minirpc {

/* ---------- 小端工具 ---------- */

void Serializer::put_u16_le(std::vector<uint8_t>& out, uint16_t v) {
    out.push_back(uint8_t(v & 0xFF));
    out.push_back(uint8_t((v >> 8) & 0xFF));
}
void Serializer::put_u32_le(std::vector<uint8_t>& out, uint32_t v) {
    out.push_back(uint8_t(v & 0xFF));
    out.push_back(uint8_t((v >> 8) & 0xFF));
    out.push_back(uint8_t((v >> 16) & 0xFF));
    out.push_back(uint8_t((v >> 24) & 0xFF));
}
void Serializer::put_u64_le(std::vector<uint8_t>& out, uint64_t v) {
    for (int i = 0; i < 8; i++)
        out.push_back(uint8_t((v >> (i * 8)) & 0xFF));
}
uint16_t Serializer::get_u16_le(const uint8_t* p) {
    return uint16_t(p[0]) | (uint16_t(p[1]) << 8);
}
uint32_t Serializer::get_u32_le(const uint8_t* p) {
    return uint32_t(p[0]) | (uint32_t(p[1]) << 8) |
           (uint32_t(p[2]) << 16) | (uint32_t(p[3]) << 24);
}
uint64_t Serializer::get_u64_le(const uint8_t* p) {
    uint64_t r = 0;
    for (int i = 0; i < 8; i++) r |= uint64_t(p[i]) << (i * 8);
    return r;
}

/* ---------- 编码 ---------- */

void Serializer::encode(const Value& v, std::vector<uint8_t>& out) {
    // std::visit 根据 variant 里实际存的类型走不同分支
    std::visit([&](auto&& arg) {
        using T = std::decay_t<decltype(arg)>;

        if constexpr (std::is_same_v<T, std::monostate>) {
            out.push_back(uint8_t(TypeTag::Nil));

        } else if constexpr (std::is_same_v<T, int32_t>) {
            out.push_back(uint8_t(TypeTag::Int32));
            put_u32_le(out, uint32_t(arg));

        } else if constexpr (std::is_same_v<T, int64_t>) {
            out.push_back(uint8_t(TypeTag::Int64));
            put_u64_le(out, uint64_t(arg));

        } else if constexpr (std::is_same_v<T, double>) {
            out.push_back(uint8_t(TypeTag::Double));
            // IEEE 754 double 直接 memcpy 成 8 字节
            uint64_t bits;
            std::memcpy(&bits, &arg, 8);
            put_u64_le(out, bits);

        } else if constexpr (std::is_same_v<T, std::string>) {
            out.push_back(uint8_t(TypeTag::String));
            put_u32_le(out, uint32_t(arg.size()));
            out.insert(out.end(), arg.begin(), arg.end());

        } else if constexpr (std::is_same_v<T, bool>) {
            out.push_back(uint8_t(TypeTag::Bool));
            out.push_back(arg ? 1 : 0);

        } else if constexpr (std::is_same_v<T, std::vector<ValueHolder>>) {
            out.push_back(uint8_t(TypeTag::Vector));
            put_u32_le(out, uint32_t(arg.size()));
            for (const auto& h : arg) {
                encode(h.v, out); // 递归编码每个元素
            }
        }
    }, v);
}

/* ---------- 解码 ---------- */

Value Serializer::decode(const uint8_t* data, size_t len, size_t& consumed) {
    if (len < 1) throw std::runtime_error("serialize: no data");

    TypeTag tag = TypeTag(data[0]);
    consumed = 1;
    const uint8_t* p = data + 1;
    size_t remaining = len - 1;

    switch (tag) {
    case TypeTag::Nil:
        return Value{std::monostate{}};

    case TypeTag::Int32:
        if (remaining < 4) throw std::runtime_error("serialize: short int32");
        consumed += 4;
        return Value{int32_t(get_u32_le(p))};

    case TypeTag::Int64:
        if (remaining < 8) throw std::runtime_error("serialize: short int64");
        consumed += 8;
        return Value{int64_t(get_u64_le(p))};

    case TypeTag::Double: {
        if (remaining < 8) throw std::runtime_error("serialize: short double");
        consumed += 8;
        uint64_t bits = get_u64_le(p);
        double d;
        std::memcpy(&d, &bits, 8);
        return Value{d};
    }

    case TypeTag::String: {
        if (remaining < 4) throw std::runtime_error("serialize: short string len");
        uint32_t slen = get_u32_le(p);
        p += 4; remaining -= 4; consumed += 4;
        if (remaining < slen) throw std::runtime_error("serialize: short string data");
        std::string s(reinterpret_cast<const char*>(p), slen);
        consumed += slen;
        return Value{std::move(s)};
    }

    case TypeTag::Bool:
        if (remaining < 1) throw std::runtime_error("serialize: short bool");
        consumed += 1;
        return Value{bool(p[0] != 0)};

    case TypeTag::Vector: {
        if (remaining < 4) throw std::runtime_error("serialize: short vector len");
        uint32_t vlen = get_u32_le(p);
        p += 4; remaining -= 4; consumed += 4;
        std::vector<ValueHolder> vec;
        vec.reserve(vlen);
        for (uint32_t i = 0; i < vlen; i++) {
            size_t elem_consumed = 0;
            Value elem = decode(p, remaining, elem_consumed);
            vec.push_back(ValueHolder{std::move(elem)});
            p += elem_consumed;
            remaining -= elem_consumed;
            consumed += elem_consumed;
        }
        return Value{std::move(vec)};
    }
    default:
        throw std::runtime_error(std::string{"serialize: unknown tag 0x"} +
                                 std::to_string(int(p[0])) +
                                 " at offset " + std::to_string(int(consumed)));
    }
}

Value Serializer::decode_all(const std::vector<uint8_t>& data) {
    size_t consumed = 0;
    Value v = decode(data.data(), data.size(), consumed);
    if (consumed != data.size()) {
        throw std::runtime_error("serialize: trailing data");
    }
    return v;
}

} // namespace minirpc
