/*
 * server.hpp — RPC Server（Layer 1 监听 + Layer 5 服务注册）
 *
 * C++ 比 Go 麻烦的点：没有 reflect 包，没法把任意类型的函数
 * 统一反射调用。所以我们用 std::function 做一个统一签名：
 *
 *   std::function<Value(const std::vector<ValueHolder>&)>
 *
 * 每个注册的方法都要符合这个签名。服务端注册时把具体方法
 * 包装成这个签名。调用时查表、执行、返回。
 *
 * Go 版用 reflect.Call 支持"直接传裸函数"，C++ 版不行，
 * 所以注册表强制统一签名——这也是教学点：
 *   不同语言的元编程能力不同，RPC 框架设计随之不同。
 */
#pragma once

#include <cstdint>
#include <cstddef>
#include <functional>
#include <map>
#include <string>
#include <vector>

#include "minirpc/serialize.hpp"

namespace minirpc {

// Handler 是所有 RPC 方法的统一签名。
// 输入：一组参数（ValueHolder 向量，可能为空）
// 输出：一个返回值（Value，可能是 nil）
using Handler = std::function<Value(const std::vector<ValueHolder>&)>;

// Server 是极简 RPC 服务端。
class Server {
public:
    Server();
    ~Server();

    // 注册一个 handler，名字是 "Type.Method" 形式
    void register_handler(const std::string& name, Handler h);

    // 在 addr:port 上监听并提供服务
    // 阻塞运行，直到出错或进程退出
    int listen_and_serve(const std::string& addr, int port);

    // 也可以直接注册一个类实例（Go 版有 sv.Register(Calculator{})，
    // C++ 版因为没有 reflect，需要手动 register_handler）
    // 进阶版可以用成员函数指针 + 模板包装

private:
    std::map<std::string, Handler> registry_;
};

} // namespace minirpc
