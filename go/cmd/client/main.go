// 这个文件是 RPC 客户端的主程序。
//
// 运行：
//   cd go && go run cmd/client/main.go
// 前提是服务端已经在跑（先开一个终端跑 cmd/server/main.go）。
package main

import (
	"fmt"

	minirpc "github.com/minirpc/minirpc"
)

func main() {
	// 1. 创建 client，Dial 到服务端
	cli := minirpc.NewClient()
	caller, err := cli.Dial("127.0.0.1:9999")
	if err != nil {
		fmt.Printf("dial failed: %v\n", err)
		return
	}
	defer caller.Close()

	fmt.Println("=== Calculator Service ===")

	// 2. 逐个调用，演示不同参数和返回值
	result, err := caller.Call("Calculator.Add", 3, 4)
	fmt.Printf("Add(3,4) = %v (err=%v)\n", result, err)

	result, err = caller.Call("Calculator.Sub", 10, 3)
	fmt.Printf("Sub(10,3) = %v (err=%v)\n", result, err)

	result, err = caller.Call("Calculator.Mul", 5, 6)
	fmt.Printf("Mul(5,6) = %v (err=%v)\n", result, err)

	result, err = caller.Call("Calculator.Div", 20, 4)
	fmt.Printf("Div(20,4) = %v (err=%v)\n", result, err)

	result, err = caller.Call("Calculator.Div", 10, 0)
	fmt.Printf("Div(10,0) = %v (err=%v)\n", result, err)

	fmt.Println("\n=== Greeting Service ===")

	result, err = caller.Call("Greeting.Hi")
	fmt.Printf("Hi() = %v (err=%v)\n", result, err)

	result, err = caller.Call("Greeting.Hello", "world")
	fmt.Printf("Hello(\"world\") = %v (err=%v)\n", result, err)

	result, err = caller.Call("Greeting.Echo", "minirpc is great!")
	fmt.Printf("Echo(\"...\") = %v (err=%v)\n", result, err)

	fmt.Println("\n=== Non-existent method ===")
	result, err = caller.Call("Foo.Bar", 1, 2, 3)
	fmt.Printf("Foo.Bar(...) = %v (err=%v)\n", result, err)

	fmt.Println("\nAll calls done!")
}
