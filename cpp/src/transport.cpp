/*
 * transport.cpp — 传输层实现（POSIX socket）
 *
 * 这里的 read_full / write_all 是 socket 编程的基础：
 * - POSIX read() / write() 不保证一次处理完所有数据
 * - 必须循环处理 partial read/write
 * - 这也是 Go net.Conn.Read 在底层做的事
 */
#include "minirpc/transport.hpp"

#include <unistd.h>
#include <errno.h>
#include <string.h>
#include <sys/socket.h>

namespace minirpc {

// 单条消息最大 64MB——防 OOM
static constexpr size_t kMaxMsgLen = 64 * 1024 * 1024;

Codec::Codec(int fd) : fd_(fd), closed_(false) {}

Codec::~Codec() {
    close();
}

void Codec::close() {
    if (!closed_ && fd_ >= 0) {
        ::close(fd_);
        fd_ = -1;
        closed_ = true;
    }
}

ssize_t Codec::read_full(uint8_t* buf, size_t len) {
    size_t got = 0;
    while (got < len) {
        ssize_t n = ::read(fd_, buf + got, len - got);
        if (n > 0) {
            got += static_cast<size_t>(n);
        } else if (n == 0) {
            // 对端正常关闭（EOF）
            return static_cast<ssize_t>(got);
        } else {
            // -1: 出错。EINTR 可以重试，其他错误退出
            if (errno == EINTR) continue;
            return -1;
        }
    }
    return static_cast<ssize_t>(got);
}

bool Codec::write_all(const uint8_t* buf, size_t len) {
    size_t sent = 0;
    while (sent < len) {
        ssize_t n = ::write(fd_, buf + sent, len - sent);
        if (n > 0) {
            sent += static_cast<size_t>(n);
        } else if (n == 0) {
            return false; // 不应该发生
        } else {
            if (errno == EINTR) continue;
            return false;
        }
    }
    return true;
}

bool Codec::read_msg(std::vector<uint8_t>& out) {
    // 1. 读 4 字节大端长度前缀
    uint8_t len_buf[4];
    ssize_t n = read_full(len_buf, 4);
    if (n != 4) return false; // 对端关闭或出错

    // 手动把大端字节组装成 uint32
    uint32_t length = (uint32_t(len_buf[0]) << 24) |
                      (uint32_t(len_buf[1]) << 16) |
                      (uint32_t(len_buf[2]) << 8)  |
                      (uint32_t(len_buf[3]));

    if (length == 0 || length > kMaxMsgLen) return false;

    // 2. 读 body
    out.resize(length);
    ssize_t got = read_full(out.data(), length);
    if (got != static_cast<ssize_t>(length)) return false;

    return true;
}

bool Codec::write_msg(const uint8_t* data, size_t len) {
    if (len > kMaxMsgLen) return false;

    // 1. 写 4 字节大端长度前缀
    uint8_t len_buf[4];
    len_buf[0] = uint8_t(len >> 24);
    len_buf[1] = uint8_t(len >> 16);
    len_buf[2] = uint8_t(len >> 8);
    len_buf[3] = uint8_t(len);
    if (!write_all(len_buf, 4)) return false;

    // 2. 写 body
    if (len > 0) {
        return write_all(data, len);
    }
    return true;
}

bool Codec::write_msg(const std::vector<uint8_t>& data) {
    return write_msg(data.data(), data.size());
}

} // namespace minirpc
