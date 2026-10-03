/*
 * client.hpp — RPC Client（Layer 6：Stub 调用）
 *
 * 和 Go 版 Caller 对应：持有一条 TCP 连接，负责发起 RPC 调用。
 *
 * 关键设计：Client 持有一个 transport::Codec 成员，
 * Call() 之间复用同一条连接，避免每次创建临时 Codec 导致关闭 socket。
 */
#pragma once

#include <cstdint>
#include <cstddef>
#include <memory>
#include <string>
#include <vector>

#include "minirpc/serialize.hpp"
#include "minirpc/transport.hpp"

namespace minirpc {

// CallResult 封装"成功 Value"或"错误字符串"
struct CallResult {
    bool ok = false;
    Value body;
    std::string err;
};

// Client —— 极简 RPC 客户端。
class Client {
public:
    Client();
    ~Client();

    Client(const Client&) = delete;
    Client& operator=(const Client&) = delete;

    // 建立到 addr:port 的 TCP 连接
    bool dial(const std::string& addr, int port);

    // 发起 RPC 调用
    CallResult call(const std::string& method,
                    const std::vector<ValueHolder>& args);

    void close();

private:
    std::unique_ptr<Codec> codec_; // transport::Codec
    bool connected_;
};

} // namespace minirpc
