/*
Package serialize 负责 RPC 框架的序列化层（Layer 3）。

本包定义 Serializer 接口 + GobSerializer 实现。

═══════════════════════════════════════════════════════════════════════
为什么 GobSerializer 要自己处理 interface{} concrete type？
═══════════════════════════════════════════════════════════════════════

Go gob 有一个知名限制：
  gob 不允许把 concrete type（比如 int 42）解码到 *interface{} 变量里。
  这不是 bug，是 gob 的设计：它只把 "remote interface type" 解到 interface{}。
  当你 `enc.Encode(interface{}(42))` 时，gob 写的是 "concrete type"，
  解码端必须用 var v int 来接，不能用 var v interface{}。

这在 RPC 框架里是个大问题——我们想在 Request.Args / Response.Body 里
放 interface{}（这样就能传任意类型）。

解决方案：AnyBox —— 自己做一个 "类型 + 数据" 的盒子
  1. 用 reflect 拿到 concrete type
  2. 把 value gob 编码成 []byte
  3. 把 type name + []byte 塞进 AnyBox struct（全是具体类型，无 interface{}）
  4. gob 编码 AnyBox → 这样 gob 永远不碰 interface{}

解码时反过来：
  1. gob 解出 AnyBox（无 interface{}，安全）
  2. 用 type name 找到 reflect.Type
  3. 创建该类型的零值，把 []byte gob 解进去
  4. 赋值给 interface{}

这相当于自己实现了一层 typed-blob 包装，绕开 gob 的限制。
═══════════════════════════════════════════════════════════════════════
*/
package serialize

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"reflect"
	"sync"
)

// Serializer 序列化接口。
// Encode 把 Go 值编成 []byte；Decode 把 []byte 解进目标。
type Serializer interface {
	Encode(v interface{}) ([]byte, error)
	Decode(data []byte, v interface{}) error
}

// AnyBox 是我们的 "类型 + 数据" 盒子。
// 两个字段都是 gob-friendly 的具体类型，不碰 interface{}。
//
// TypeName:  concrete type 的完整包路径名，比如 "main.Calculator"
//            对于基础类型用短名："int", "string", "float64" 等
// Payload:   该类型值的 gob 编码
type AnyBox struct {
	TypeName string
	Payload  []byte
}

// GobSerializer 是 Serializer 的 gob 实现。
type GobSerializer struct {
	// typeCache 缓存 type name → reflect.Type 的映射，避免每次 reflect 开销。
	// 用 sync.Map 是因为不同 goroutine 可能并发调用 Encode/Decode。
	typeCache sync.Map // map[string]reflect.Type
}

// NewGobSerializer 构造 gob 序列化器，预注册基础类型。
func NewGobSerializer() *GobSerializer {
	g := &GobSerializer{}
	g.typeCache.Store("int", reflect.TypeOf(int(0)))
	g.typeCache.Store("int64", reflect.TypeOf(int64(0)))
	g.typeCache.Store("float64", reflect.TypeOf(float64(0)))
	g.typeCache.Store("string", reflect.TypeOf(""))
	g.typeCache.Store("bool", reflect.TypeOf(true))
	return g
}

// Encode 把任意 v 编码成 gob 字节流。
//
// 核心逻辑：检查 v 的 concrete type，用 AnyBox 包装后再 gob 编码。
// 这样 gob 永远只处理 AnyBox（全具体类型），不碰 interface{}。
func (g *GobSerializer) Encode(v interface{}) ([]byte, error) {
	// v 可能是 nil
	if v == nil {
		// 约定：nil 用一个空 AnyBox 表示（TypeName == ""）
		var buf bytes.Buffer
		enc := gob.NewEncoder(&buf)
		if err := enc.Encode(AnyBox{}); err != nil {
			return nil, fmt.Errorf("serialize.gob.encode.nil: %w", err)
		}
		return buf.Bytes(), nil
	}

	rt := reflect.TypeOf(v)

	// 1. 把 v 本身 gob 编码成 Payload（这时 gob 处理的是 concrete type）
	var payloadBuf bytes.Buffer
	enc := gob.NewEncoder(&payloadBuf)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("serialize.gob.encode.inner: %w", err)
	}

	// 2. 构造 AnyBox —— 这里的关键是给类型命名
	typeName := g.getTypeName(rt)
	box := AnyBox{TypeName: typeName, Payload: payloadBuf.Bytes()}

	// 3. 把 AnyBox gob 编码（AnyBox 全是具体类型，gob 没问题）
	var buf bytes.Buffer
	enc2 := gob.NewEncoder(&buf)
	if err := enc2.Encode(box); err != nil {
		return nil, fmt.Errorf("serialize.gob.encode.outer: %w", err)
	}
	return buf.Bytes(), nil
}

// Decode 把 gob 字节流解码进 v。
//
// v 应该是一个指针（gob 的 Decode 需要往里面填值）。
// 核心：先解出 AnyBox，再用 AnyBox 里的 type name 找 reflect.Type，
// 创建零值，把 Payload gob 解进去，最后赋值给 *v。
func (g *GobSerializer) Decode(data []byte, v interface{}) error {
	if v == nil {
		return fmt.Errorf("serialize.gob.decode: v is nil")
	}

	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Ptr {
		return fmt.Errorf("serialize.gob.decode: v must be a pointer, got %s", rv.Kind())
	}

	// 1. 先把 gob data 解成 AnyBox（安全——AnyBox 全是具体类型）
	var box AnyBox
	dec := gob.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&box); err != nil {
		return fmt.Errorf("serialize.gob.decode.outer: %w", err)
	}

	// 2. nil AnyBox → 说明编码的就是 nil
	if box.TypeName == "" {
		rv.Elem().Set(reflect.Zero(rv.Elem().Type()))
		return nil
	}

	// 3. 找到 concrete reflect.Type
	rt, err := g.resolveType(box.TypeName)
	if err != nil {
		return fmt.Errorf("serialize.gob.decode.resolve: %w", err)
	}

	// 4. 创建该类型的零值容器，把 Payload gob 解进去
	// gob.Decode 需要一个指针或可寻址的值
	var holder reflect.Value
	if rt.Kind() == reflect.Ptr {
		// 如果目标是指针类型，先创建一个 *T 指向 T 的零值
		holder = reflect.New(rt.Elem())
	} else {
		holder = reflect.New(rt) // 创建 *T
	}
	dec2 := gob.NewDecoder(bytes.NewReader(box.Payload))
	if err := dec2.DecodeValue(holder); err != nil {
		return fmt.Errorf("serialize.gob.decode.inner: %w", err)
	}

	// 5. 把结果赋值给 v 指向的目标
	// 如果 v 是 *interface{}，直接 set
	targetElem := rv.Elem()
	if targetElem.Kind() == reflect.Interface {
		targetElem.Set(holder.Elem())
	} else {
		// 否则尝试 Convert（比如 int → int64）
		if !holder.Elem().Type().ConvertibleTo(targetElem.Type()) {
			return fmt.Errorf("serialize.gob.decode: cannot convert %s to %s",
				holder.Elem().Type(), targetElem.Type())
		}
		targetElem.Set(holder.Elem().Convert(targetElem.Type()))
	}
	return nil
}

/*
 * 类型名处理 —— 这是 AnyBox 方案的另一半
 *
 * getTypeName 把 reflect.Type 变成一个字符串名字
 * resolveType 根据字符串名字找 reflect.Type
 *
 * 基础类型直接用 "int", "string" 等短名。
 * 命名的 struct/type 用 "包路径.类型名"，比如
 *   "github.com/minirpc/minirpc/pkg/protocol.GobWireReq"
 *
 * 进阶：匿名 struct 没法唯一命名。我们的 RPC 场景里 Args 和 Body
 * 通常是基础类型或命名 struct，匿名 struct 罕见，先不处理。
 */

func (g *GobSerializer) getTypeName(rt reflect.Type) string {
	// 基础类型用短名
	switch rt.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return "int"
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "uint64"
	case reflect.Float32, reflect.Float64:
		return "float64"
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "bool"
	}

	// slice / map / pointer 等复合类型：用完整的 String()
	// 比如 "[]interface {}" 或 "*main.Calculator"
	if rt.Name() != "" {
		// 命名类型：用 "包路径.类型名"
		return rt.PkgPath() + "." + rt.Name()
	}
	// 匿名复合类型（比如 []int）：用 String()
	return rt.String()
}

func (g *GobSerializer) resolveType(name string) (reflect.Type, error) {
	// 先查缓存
	if v, ok := g.typeCache.Load(name); ok {
		return v.(reflect.Type), nil
	}

	// 基础类型快捷路径
	switch name {
	case "int":
		g.typeCache.Store(name, reflect.TypeOf(int(0)))
		return reflect.TypeOf(int(0)), nil
	case "int64":
		g.typeCache.Store(name, reflect.TypeOf(int64(0)))
		return reflect.TypeOf(int64(0)), nil
	case "float64":
		g.typeCache.Store(name, reflect.TypeOf(float64(0)))
		return reflect.TypeOf(float64(0)), nil
	case "string":
		g.typeCache.Store(name, reflect.TypeOf(""))
		return reflect.TypeOf(""), nil
	case "bool":
		g.typeCache.Store(name, reflect.TypeOf(true))
		return reflect.TypeOf(true), nil
	case "[]interface {}":
		g.typeCache.Store(name, reflect.TypeOf([]interface{}{}))
		return reflect.TypeOf([]interface{}{}), nil
	case "[]byte":
		g.typeCache.Store(name, reflect.TypeOf([]byte(nil)))
		return reflect.TypeOf([]byte(nil)), nil
	}

	// 命名类型：需要用 reflect.TypeOf 来拿到
	// 但 Go 没有 "根据字符串名找 reflect.Type" 的内置函数
	// 进阶版可以用 runtime/reflect 包做类型注册
	// 这里返回一个友好的错误提示
	return nil, fmt.Errorf("unknown type name %q (advanced: register non-basic types)", name)
}

// init 注册 gob 基础类型（避免 gob 解码时报 "no registered type for int"）
func init() {
	gob.Register(int(0))
	gob.Register(int64(0))
	gob.Register(float64(0))
	gob.Register("")
	gob.Register(true)
	gob.Register([]interface{}{})
}
