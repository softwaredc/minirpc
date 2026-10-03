# Contributing to minirpc

Thanks for taking the time to help! This is a small project, so the
contribution flow is intentionally lightweight.

## Prerequisites

- Go 1.22+ (for the Go implementation)
- A C++17 compiler + CMake 3.16+ (for the C++ implementation)
  - On Ubuntu: `sudo apt install build-essential cmake`
  - On macOS: `xcode-select --install && brew install cmake`

## Build and test locally

Clone the repo:

```bash
git clone https://github.com/softwaredc/minirpc.git
cd minirpc
```

Go:

```bash
cd go
go build ./...
go test ./... -v
```

C++:

```bash
cd cpp
cmake -S . -B build -DCMAKE_BUILD_TYPE=Debug
cmake --build build -j
cd build && ctest --output-on-failure
```

## Pull request flow

1. Fork the repo and create a feature branch from `main`.
2. Make your changes. Keep diffs focused — one topic per PR.
3. Run `go test ./...` and the C++ build/tests locally.
4. Push and open a PR. CI (`.github/workflows/ci.yml`) runs automatically
   on Go 1.22/1.23 and GCC/Clang on Ubuntu 24.04.

## Code style

- C++: `.clang-format` is at the repo root. Run
  `clang-format -i -style=file <files>` before committing.
- Go: follow `gofmt` and `go vet`. No extra linters enforced.
- No style nazi — if your code is clean and the tests pass, we're good.

## What to contribute

Bug fixes, protocol edge cases, example improvements, docs — all welcome.
If you're not sure, open an issue first.
