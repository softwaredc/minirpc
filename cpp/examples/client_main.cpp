// C++ 版 RPC 客户端主程序。
// 和 Go 版 cmd/client/main.go 完全对应。
#include "minirpc/client.hpp"
#include <iostream>

using namespace minirpc;

// 辅助：打印 CallResult
static void print_result(const CallResult& r) {
    if (r.ok) {
        std::visit([](auto&& v) {
            using T = std::decay_t<decltype(v)>;
            if constexpr (std::is_same_v<T, std::monostate>) {
                std::cout << "nil";
            } else if constexpr (std::is_same_v<T, int32_t> ||
                                 std::is_same_v<T, int64_t>) {
                std::cout << int64_t(v);
            } else if constexpr (std::is_same_v<T, double>) {
                std::cout << v;
            } else if constexpr (std::is_same_v<T, std::string>) {
                std::cout << v;
            } else if constexpr (std::is_same_v<T, bool>) {
                std::cout << (v ? "true" : "false");
            } else {
                std::cout << "<vector>";
            }
        }, r.body);
        std::cout << " (ok)\n";
    } else {
        std::cout << "<nil> (err=" << r.err << ")\n";
    }
}

static ValueHolder i(int64_t x) { return ValueHolder{Value{int64_t(x)}}; }

int main() {
    Client cli;
    if (!cli.dial("127.0.0.1", 9999)) {
        std::cout << "dial failed\n";
        return 1;
    }

    std::cout << "=== Calculator Service ===\n";

    auto r = cli.call("Calculator.Add", {i(3), i(4)});
    std::cout << "Add(3,4) = "; print_result(r);

    r = cli.call("Calculator.Sub", {i(10), i(3)});
    std::cout << "Sub(10,3) = "; print_result(r);

    r = cli.call("Calculator.Mul", {i(5), i(6)});
    std::cout << "Mul(5,6) = "; print_result(r);

    r = cli.call("Calculator.Div", {i(20), i(4)});
    std::cout << "Div(20,4) = "; print_result(r);

    r = cli.call("Calculator.Div", {i(10), i(0)});
    std::cout << "Div(10,0) = "; print_result(r);

    std::cout << "\n=== Greeting Service ===\n";

    r = cli.call("Greeting.Hi", {});
    std::cout << "Hi() = "; print_result(r);

    ValueHolder name{Value{std::string{"world"}}};
    r = cli.call("Greeting.Hello", {name});
    std::cout << "Hello(\"world\") = "; print_result(r);

    ValueHolder msg{Value{std::string{"minirpc-cpp is great!"}}};
    r = cli.call("Greeting.Echo", {msg});
    std::cout << "Echo(\"...\") = "; print_result(r);

    std::cout << "\n=== Non-existent method ===\n";
    r = cli.call("Foo.Bar", {i(1), i(2), i(3)});
    std::cout << "Foo.Bar(...) = "; print_result(r);

    std::cout << "\nAll calls done!\n";
    return 0;
}
