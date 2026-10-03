/*
 * transport.hpp — RPC 框架传输层（Layer 2）
 *
 * C++ 版本的 transport 层：封装一个 TCP 连接，提供
 * "带长度前缀的消息读写"。零依赖，用 POSIX socket。
 *
 * 设计与 Go 版一致：
 *   ┌─────────────┬──────────────────┐
 *   │ Length (4B) │ Body (Length 字节) │
 *   └─────────────┴──────────────────┘
 *
 * 注意：C++ 没有 io.ReadFull 这种自动循环读的便利函数，
 * 必须自己处理 partial read（这是写 socket 代码的基本功）。
 */
#pragma once

#include <cstdint>
#include <cstddef>
#include <string>
#include <vector>

namespace minirpc {

// Codec 封装一个 TCP socket，提供 read_msg / write_msg。
// 构造时传入一个已经 connect/accept 好的 socket fd。
class Codec {
public:
    explicit Codec(int fd);
    ~Codec();

    // 禁止拷贝（socket fd 不能被两个对象同时管理）
    Codec(const Codec&) = delete;
    Codec& operator=(const Codec&) = delete;

    // 读取一条完整消息。返回 true 表示成功，
    // 消息体存进 out。返回 false 表示连接关闭或出错。
    bool read_msg(std::vector<uint8_t>& out);

    // 写入一条消息（自动加 4 字节大端长度前缀）。
    // 返回 true 表示成功。
    bool write_msg(const uint8_t* data, size_t len);
    bool write_msg(const std::vector<uint8_t>& data);

    // 手动关闭。析构函数也会自动关闭。
    void close();

    int fd() const { return fd_; }

private:
    int fd_;
    bool closed_;

    // 循环读满 len 字节——C++ 版的 io.ReadFull
    // 返回实际读取字节数，< len 表示对端关闭或出错
    ssize_t read_full(uint8_t* buf, size_t len);

    // 循环写满 len 字节——C++ 版的 io.WriteFull
    bool write_all(const uint8_t* buf, size_t len);
};

} // namespace minirpc
