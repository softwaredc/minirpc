# minirpc C++ 构建系统
# 零依赖，纯 POSIX + C++17 stdlib
cmake_minimum_required(VERSION 3.14)
project(minirpc_cpp VERSION 0.1.0 LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

# 编译选项：Wall/Wextra，O2 优化（教学项目也保证性能）
add_compile_options(-Wall -Wextra -O2)

# 核心库：transport + serialize + protocol + server + client
add_library(minirpc STATIC
    src/transport.cpp
    src/serialize.cpp
    src/protocol.cpp
    src/server.cpp
    src/client.cpp
)
target_include_directories(minirpc PUBLIC include)

# 示例可执行
add_executable(rpc_server examples/server_main.cpp)
target_link_libraries(rpc_server PRIVATE minirpc pthread)

add_executable(rpc_client examples/client_main.cpp)
target_link_libraries(rpc_client PRIVATE minirpc)
