/*
 * server.cpp — RPC Server 实现
 *
 * POSIX socket listen/accept 循环，为每个连接起一个处理函数。
 * C++ 没有 goroutine，用 std::thread 代替。
 */
#include "minirpc/server.hpp"
#include "minirpc/transport.hpp"
#include "minirpc/protocol.hpp"

#include <sys/socket.h>
#include <netinet/in.h>
#include <arpa/inet.h>
#include <unistd.h>
#include <thread>
#include <iostream>
#include <cstring>
#include <stdexcept>

namespace minirpc {

Server::Server() = default;
Server::~Server() = default;

void Server::register_handler(const std::string& name, Handler h) {
    registry_[name] = std::move(h);
}

// 处理一条连接：循环读请求 → 调用 → 写响应，直到对端关闭
static void handle_conn(int fd, const std::map<std::string, Handler>& registry) {
    Codec codec(fd);
    std::vector<uint8_t> msg;

    while (true) {
        if (!codec.read_msg(msg)) break; // 对端关闭或出错

        try {
            WireReq req = unmarshal_request(msg.data(), msg.size());

            auto it = registry.find(req.method);
            if (it == registry.end()) {
                auto err_val = Value{std::string{"method not found: " + req.method}};
                codec.write_msg(marshal_response(true, err_val));
                continue;
            }

            Value result = it->second(req.args);
            codec.write_msg(marshal_response(false, result));

        } catch (const std::exception& e) {
            // 解码/handler 异常 → 错误响应
            auto err_val = Value{std::string{e.what()}};
            codec.write_msg(marshal_response(true, err_val));
        }
    }
}

int Server::listen_and_serve(const std::string& addr, int port) {
    int listen_fd = ::socket(AF_INET, SOCK_STREAM, 0);
    if (listen_fd < 0) return -1;

    // 允许地址重用（快速重启 server 时避免 "address already in use"）
    int opt = 1;
    ::setsockopt(listen_fd, SOL_SOCKET, SO_REUSEADDR, &opt, sizeof(opt));

    struct sockaddr_in sa;
    std::memset(&sa, 0, sizeof(sa));
    sa.sin_family = AF_INET;
    sa.sin_port = htons(port);
    if (addr == "" || addr == "0.0.0.0") {
        sa.sin_addr.s_addr = htonl(INADDR_ANY);
    } else {
        ::inet_pton(AF_INET, addr.c_str(), &sa.sin_addr);
    }

    if (::bind(listen_fd, (struct sockaddr*)&sa, sizeof(sa)) < 0) {
        ::close(listen_fd);
        return -1;
    }
    if (::listen(listen_fd, 128) < 0) {
        ::close(listen_fd);
        return -1;
    }

    std::cout << "[minirpc-cpp] listening on " << addr << ":" << port
              << ", registered " << registry_.size() << " handlers\n";

    while (true) {
        int client_fd = ::accept(listen_fd, nullptr, nullptr);
        if (client_fd < 0) continue;
        // 每个连接起一个 std::thread（等价于 Go 的 go handleConn(conn)）
        std::thread t(handle_conn, client_fd, std::cref(registry_));
        t.detach(); // 线程独立运行，server 关闭时会被强杀
    }

    ::close(listen_fd);
    return 0;
}

} // namespace minirpc
