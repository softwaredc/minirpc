// C++ 版 RPC 服务端主程序。
//
// 和 Go 版 cmd/server/main.go 结构完全对应：
//   1. 创建 Server
//   2. 注册 handler（Go 版用 reflect 自动注册 struct 方法，
//      C++ 版因为没有 reflect，手动 register_handler）
//   3. listen_and_serve（阻塞）
#include "minirpc/server.hpp"
#include <iostream>

using namespace minirpc;

// 辅助：从 Value 里安全取出 int64（handler 参数通常是 int32 或 int64）
// Go 版用 reflect.Type.In(i) + coerceArg 做类型转换，
// C++ 版用 std::get_if 做类似的事情
static int64_t as_int(const Value& v) {
    if (auto* i = std::get_if<int32_t>(&v)) return *i;
    if (auto* i = std::get_if<int64_t>(&v)) return *i;
    if (auto* d = std::get_if<double>(&v)) return int64_t(*d);
    return 0;
}

int main() {
    Server server;

    // 注册 Calculator.Add：args[0] + args[1]
    server.register_handler("Calculator.Add",
        [](const std::vector<ValueHolder>& args) -> Value {
            int64_t a = as_int(args[0].v);
            int64_t b = as_int(args[1].v);
            return Value{int64_t(a + b)};
        });

    // 注册 Calculator.Sub / Mul / Div
    server.register_handler("Calculator.Sub",
        [](const std::vector<ValueHolder>& args) -> Value {
            return Value{int64_t(as_int(args[0].v) - as_int(args[1].v))};
        });

    server.register_handler("Calculator.Mul",
        [](const std::vector<ValueHolder>& args) -> Value {
            return Value{int64_t(as_int(args[0].v) * as_int(args[1].v))};
        });

    server.register_handler("Calculator.Div",
        [](const std::vector<ValueHolder>& args) -> Value {
            int64_t b = as_int(args[1].v);
            if (b == 0) return Value{std::string{"divide by zero"}};
            return Value{int64_t(as_int(args[0].v) / b)};
        });

    // 注册 Greeting.Hi / Hello / Echo
    server.register_handler("Greeting.Hi",
        [](const std::vector<ValueHolder>&) -> Value {
            return Value{std::string{"hello from minirpc-cpp server"}};
        });

    server.register_handler("Greeting.Hello",
        [](const std::vector<ValueHolder>& args) -> Value {
            std::string name = std::get<std::string>(args[0].v);
            return Value{std::string{"hello, " + name + "!"}};
        });

    server.register_handler("Greeting.Echo",
        [](const std::vector<ValueHolder>& args) -> Value {
            return args[0].v;
        });

    std::cout << "minirpc-cpp server starting on :9999 ...\n";
    server.listen_and_serve("0.0.0.0", 9999);
    return 0;
}
