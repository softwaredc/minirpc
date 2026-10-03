// 这个文件是 RPC 服务端的主程序。
//
// 它做三件事：
//  1. 定义一个服务 struct（Calculator + Greeting）
//  2. 把它注册到 minirpc.Server
//  3. 监听端口开始提供服务
//
// 运行：
//   cd go && go run cmd/server/main.go
// 服务端会阻塞住，等客户端来调。
package main

import (
	"fmt"
	"time"

	minirpc "github.com/minirpc/minirpc"
)

// Calculator 服务——提供基础四则运算。
//
// 为什么要每个方法一行注释？因为这就是教学项目，
// 每个 RPC 方法都应该像正式 API 一样有文档。
type Calculator struct{}

// Add 返回 a + b。
func (Calculator) Add(a, b int) int {
	return a + b
}

// Sub 返回 a - b。
func (Calculator) Sub(a, b int) int {
	return a - b
}

// Mul 返回 a * b。
func (Calculator) Mul(a, b int) int {
	return a * b
}

// Div 返回 a / b。注意：这里故意不处理除零，
// 让除零变成一个 RPC 错误（方便测试错误路径）。
func (Calculator) Div(a, b int) (int, error) {
	if b == 0 {
		return 0, fmt.Errorf("divide by zero")
	}
	return a / b, nil
}

// Greeting 服务——演示不同签名的方法（无参数、多参数）。
type Greeting struct{}

// Hi 返回一个固定字符串。演示"无参数"方法。
func (Greeting) Hi() string {
	return "hello from minirpc server"
}

// Hello 带参数的问候。
func (Greeting) Hello(name string) string {
	return "hello, " + name + "!"
}

// Echo 返回原样（演示"任意类型参数传递"）。
func (Greeting) Echo(s string) string {
	return s
}

// main 是服务端入口。
func main() {
	server := minirpc.NewServer()

	// 注册两个服务——一个 Server 可以注册多个 service。
	server.Register(Calculator{})
	server.Register(Greeting{})

	// ListenAndServe 会阻塞住，直到收到错误或手动 kill。
	// 在 WSL 里运行后，另开一个终端跑 client 即可。
	addr := ":9999"
	fmt.Printf("minirpc server starting on %s ...\n", addr)

	// 用 goroutine 跑 server，让它在后台转，
	// 前台留个打印说明（演示用）。
	go func() {
		if err := server.ListenAndServe(addr); err != nil {
			fmt.Printf("server error: %v\n", err)
		}
	}()

	// 等 1s 让 server 起来再打印
	time.Sleep(1 * time.Second)
	fmt.Println("ready! connect with: go run cmd/client/main.go")

	// 阻塞住，等 Ctrl+C
	select {}
}
