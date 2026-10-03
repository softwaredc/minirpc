# minirpc

<div align="center">

[![English](https://img.shields.io/badge/EN-English-blue)](README.md)
[![中文](https://img.shields.io/badge/CN-中文-red)](README_CN.md)

![Go Build](https://github.com/softwaredc/minirpc/actions/workflows/go-ci.yml/badge.svg)
![C++ Build](https://github.com/softwaredc/minirpc/actions/workflows/cpp-ci.yml/badge.svg)
![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)

</div>

> 面向教学的极简 RPC 框架 — Go + C++，逐行注释，无魔法，零依赖。

minirpc 是一个用于学习 RPC 原理的极简实现。每一层代码都有详细注释，帮你看清 RPC 到底在做什么。Go 版用标准库 `gob` + 反射实现，C++ 版用 POSIX socket + `std::variant` 手写类型化二进制编解码，双语言对照学习。

## 六层架构

| 层 | 名称 | 职责 |
|---|---|---|
| 1 | Server 监听与接受 | 打开 TCP 端口，循环接受新连接 |
| 2 | Transport 编解码器 | 4 字节大端长度前缀，处理 partial read |
| 3 | 序列化器 | Go: gob + AnyBox 包装绕过 interface{} 限制；C++: 自研 TypeTag 二进制格式 |
| 4 | 协议层 | 魔数校验、版本号、wire struct 编解码 |
| 5 | 服务注册表 | Go: `map[string]reflect.Value`；C++: `map[string]std::function` |
| 6 | 客户端调用 | Dial → Marshal → Write → Read → Unmarshal → 返回 |

## 快速开始 — Go

```bash
cd go
# 终端 1：启动服务端
go run cmd/server/main.go

# 终端 2：发起调用
go run cmd/client/main.go
# 或者跑全量测试（19 个用例）
go test ./... -v
```

## 快速开始 — C++

```bash
cd cpp
cmake -B build
cmake --build build

# 启动服务端（后台）
./build/rpc_server &

# 发起调用（9 个 demo 调用）
./build/rpc_client
```

## 项目结构

```
minirpc/
├── README.md / README_CN.md          # 项目首页
├── docs/
│   ├── ARCHITECTURE.md / _CN.md      # 六层架构深潜
│   └── PROTOCOL.md / _CN.md          # 线上字节格式
├── go/                                # Go 实现
│   ├── server.go                     # Layer 1 Listen + Layer 5 Registry + 反射调用
│   ├── client.go                     # Layer 6 Caller
│   ├── cmd/server/main.go            # 示例服务端入口
│   ├── cmd/client/main.go            # 示例客户端入口
│   ├── tests/minirpc_test.go         # 19 个单元测试
│   └── pkg/
│       ├── transport/tcp.go          # Layer 2 Codec
│       ├── serialize/gob.go          # Layer 3 GobSerializer + AnyBox
│       └── protocol/protocol.go      # Layer 4 魔数 + wire struct
└── cpp/                               # C++ 实现
    ├── CMakeLists.txt
    ├── include/minirpc/*.hpp         # 头文件
    ├── src/*.cpp                     # 实现（Layer 1-6 全齐）
    └── examples/                     # server_main / client_main
```

## 关键设计决策

**Go `gob` 的 `AnyBox` 修复** —— Go 标准库 `encoding/gob` 不允许把 concrete type 解码到 `*interface{}`。我们自己做了一个 `AnyBox{TypeName string, Payload []byte}`，把类型名 + gob 编码值塞进去，整体作为 concrete struct 再 gob 一次，绕开了这个限制。详见 [serialize/gob.go](go/pkg/serialize/gob.go) 里的注释。

**C++ `std::variant` 代替 `interface{}`** —— C++ 没有运行时反射，也没有 `interface{}`。我们用 `std::variant<monostate, int32, int64, double, string, bool, vector<ValueHolder>>` 表达"任意类型值"，再配合 TypeTag 做类型化二进制编码。不需要 protobuf，零依赖。

**魔数校验** —— 请求前 2 字节固定 `0xA5A5`，响应固定 `0xA5A6`。收到脏数据立即拒绝，避免 gob/C++ serializer 解析崩溃。版本号当前为 1，留了未来升级空间。

## 为什么又一个 RPC 框架？

- **教学优先** —— 每一行关键代码都有注释解释"为什么"，不是只给"是什么"。读完你就能自己写一个。
- **零依赖** —— Go 只用标准库，C++ 只用 POSIX socket + STL。没有 protobuf、没有 grpc、没有第三方库要装。
- **双语言对照** —— Go 的 `reflect.Call` vs C++ 的 `std::function`、Go 的 `gob AnyBox` vs C++ 的 TypeTag variant。同一套架构两种实现，帮你理解语言特性如何影响框架设计。

## License

Apache-2.0
