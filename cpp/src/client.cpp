/*
 * client.cpp — RPC Client 实现
 *
 * 关键修复：Client 持有一个 persistent Codec，Call() 复用。
 * 之前 Call() 每次临时建 Codec → 析构 close(fd_) → socket 断了。
 */
#include "minirpc/client.hpp"
#include "minirpc/protocol.hpp"

#include <sys/socket.h>
#include <netinet/in.h>
#include <arpa/inet.h>
#include <unistd.h>
#include <cstring>
#include <iostream>

namespace minirpc {

Client::Client() : codec_(nullptr), connected_(false) {}

Client::~Client() { close(); }

void Client::close() {
    codec_.reset(); // unique_ptr 析构 Codec → Codec 析构 close(fd_)
    connected_ = false;
}

bool Client::dial(const std::string& addr, int port) {
    int fd = ::socket(AF_INET, SOCK_STREAM, 0);
    if (fd < 0) return false;

    struct sockaddr_in sa;
    std::memset(&sa, 0, sizeof(sa));
    sa.sin_family = AF_INET;
    sa.sin_port = htons(port);
    ::inet_pton(AF_INET, addr.c_str(), &sa.sin_addr);

    if (::connect(fd, (struct sockaddr*)&sa, sizeof(sa)) < 0) {
        ::close(fd);
        return false;
    }

    codec_ = std::make_unique<Codec>(fd);
    connected_ = true;
    std::cout << "[minirpc-cpp] dialed " << addr << ":" << port << "\n";
    return true;
}

CallResult Client::call(const std::string& method,
                        const std::vector<ValueHolder>& args) {
    CallResult result;
    if (!connected_ || !codec_) {
        result.err = "not connected";
        return result;
    }

    // 1. 编码请求
    auto req_bytes = marshal_request(method, args);
    if (!codec_->write_msg(req_bytes)) {
        result.err = "write failed";
        return result;
    }

    // 2. 等响应
    std::vector<uint8_t> resp_bytes;
    if (!codec_->read_msg(resp_bytes)) {
        result.err = "read failed";
        return result;
    }

    // 3. 解响应
    WireResp resp;
    try {
        resp = unmarshal_response(resp_bytes.data(), resp_bytes.size());
    } catch (const std::exception& e) {
        result.err = std::string{"decode: "} + e.what();
        return result;
    }

    // 4. 解 body_blob 成 Value
    if (!resp.body_blob.empty()) {
        try {
            result.body = Serializer::decode_all(resp.body_blob);
        } catch (const std::exception& e) {
            result.err = std::string{"body decode: "} + e.what();
            return result;
        }
    }

    // 5. 检查错误标志
    if (resp.is_error) {
        std::string msg;
        if (auto* s = std::get_if<std::string>(&result.body)) msg = *s;
        result.err = msg.empty() ? "rpc error" : msg;
        result.ok = false;
    } else {
        result.ok = true;
    }
    return result;
}

} // namespace minirpc
