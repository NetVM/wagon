package modules

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"reflect"
	"time"

	"github.com/go-interpreter/wagon/exec"
	"github.com/go-interpreter/wagon/wasm"
)

type ref uint64

// Go is the Go WASM runtime module (emulates wasm_exec.js shipped with Go)
type Go struct {
	*wasm.Module

	timeOrigin time.Time

	values     map[ref]Value
	valueIndex ref
}

func stub(name string) func(proc *exec.Process, sp int32) {
	return func(proc *exec.Process, sp int32) {
		fmt.Println("called", name, "with val", sp)
	}
}

type Value struct {
	ref ref
	v   interface{}
}

type jsObject map[string]interface{}

func (o jsObject) Get(v string) interface{} {
	return o[v]
}

func newJSError(err error) jsObject {
	return jsObject{"message": err.Error()}
}

type jsInt8Array []int8

func (a jsInt8Array) New(args ...interface{}) interface{} {
	a = make(jsInt8Array, len(args))
	for i, v := range args {
		a[i] = v.(int8)
	}
	return a
}

type jsArray []interface{}

func (a jsArray) New(args ...interface{}) interface{} {
	return jsArray(args)
}

type getter interface {
	Get(v string) interface{}
}

type newer interface {
	New(args ...interface{}) interface{}
}

var (
	nan       = struct{}{}
	undefined = struct{}{}
	jsGlobal  = jsObject{
		"Array":     jsArray{},
		"Int8Array": jsInt8Array{},
	}
)

const (
	jsValueNaN       = 0
	jsValueUndefined = 1
	jsValueNull      = 2
	jsValueTrue      = 3
	jsValueFalse     = 4
	jsValueGlobal    = 5
	jsValueMemory    = 6 // WebAssembly linear memory
	jsVsGo           = 7 // instance of the Go class in JavaScript
)

var defaultValues = map[ref]Value{
	jsValueNaN:       Value{ref: jsValueNaN, v: nan},
	jsValueUndefined: Value{ref: jsValueUndefined, v: undefined},
	jsValueNull:      Value{ref: jsValueNull, v: nil},
	jsValueTrue:      Value{ref: jsValueTrue, v: true},
	jsValueFalse:     Value{ref: jsValueFalse, v: false},
	jsValueGlobal:    Value{ref: jsValueGlobal, v: jsGlobal},
	jsValueMemory:    Value{ref: jsValueMemory, v: jsValueMemory},
	jsVsGo:           Value{ref: jsVsGo, v: jsVsGo},
}

// NewGo creates a new Go WASM runtime module
func NewGo() (*Go, error) {
	g := &Go{
		Module:     wasm.NewModule(),
		timeOrigin: time.Now(),
		values:     defaultValues,
		valueIndex: ref(len(defaultValues)),
	}

	g.loadExports()

	return g, nil
}

func (g *Go) loadExports() {
	exports := []struct {
		Name    string
		Func    func(proc *exec.Process, sp int32)
		Params  []wasm.ValueType
		Returns []wasm.ValueType
	}{
		{Name: "debug", Func: stub("debug"), Params: []wasm.ValueType{wasm.ValueTypeI32}},
		{Name: "runtime.wasmExit", Func: g.exportExit, Params: []wasm.ValueType{wasm.ValueTypeI32}},
		{Name: "runtime.wasmWrite", Func: g.exportWasmWrite, Params: []wasm.ValueType{wasm.ValueTypeI32}},
		{Name: "runtime.nanotime", Func: g.exportNanotime, Params: []wasm.ValueType{wasm.ValueTypeI32}},
		{Name: "runtime.walltime", Func: g.exportWalltime, Params: []wasm.ValueType{wasm.ValueTypeI32}},
		{Name: "runtime.scheduleCallback", Func: stub("runtime.scheduleCallback"), Params: []wasm.ValueType{wasm.ValueTypeI32}},
		{Name: "runtime.clearScheduledCallback", Func: stub("runtime.clearScheduledCallback"), Params: []wasm.ValueType{wasm.ValueTypeI32}},
		{Name: "runtime.getRandomData", Func: g.exportGetRandomData, Params: []wasm.ValueType{wasm.ValueTypeI32}},
		{Name: "syscall/js.stringVal", Func: g.exportStringVal, Params: []wasm.ValueType{wasm.ValueTypeI32}},
		{Name: "syscall/js.valueGet", Func: g.exportValueGet, Params: []wasm.ValueType{wasm.ValueTypeI32}},
		{Name: "syscall/js.valueCall", Func: g.exportValueCall, Params: []wasm.ValueType{wasm.ValueTypeI32}},
		{Name: "syscall/js.valueNew", Func: g.exportValueNew, Params: []wasm.ValueType{wasm.ValueTypeI32}},
		{Name: "syscall/js.valuePrepareString", Func: g.exportValuePrepareString, Params: []wasm.ValueType{wasm.ValueTypeI32}},
		{Name: "syscall/js.valueLoadString", Func: g.exportValueLoadString, Params: []wasm.ValueType{wasm.ValueTypeI32}},
	}

	g.Export.Entries = map[string]wasm.ExportEntry{}

	for i, e := range exports {
		g.Types.Entries = append(g.Types.Entries,
			wasm.FunctionSig{
				Form:        0,
				ParamTypes:  e.Params,
				ReturnTypes: e.Returns,
			})

		g.FunctionIndexSpace = append(g.FunctionIndexSpace, wasm.Function{
			Sig:  &g.Types.Entries[i],
			Host: reflect.ValueOf(e.Func),
			Body: &wasm.FunctionBody{},
		})

		g.Export.Entries[e.Name] = wasm.ExportEntry{
			FieldStr: e.Name,
			Kind:     wasm.ExternalFunction,
			Index:    uint32(i),
		}
	}
}

// BufferAt is anything that can return a buffer at an offset and length
type BufferAt interface {
	BufferAt(offset, length int64) ([]byte, error)
}

func (g *Go) setUInt8(ba BufferAt, addr int32, v uint8) {
	buf, err := ba.BufferAt(int64(addr), 8)
	if err != nil {
		panic(err)
	}
	buf[0] = byte(v)
}

func (g *Go) setUInt32(ba BufferAt, addr int32, v uint32) {
	buf, err := ba.BufferAt(int64(addr), 8)
	if err != nil {
		panic(err)
	}
	binary.LittleEndian.PutUint32(buf, v)
}

func (g *Go) setRef(ba BufferAt, addr int32, v ref) {
	g.setUInt32(ba, addr, uint32(v))
}

func (g *Go) setInt32(ba BufferAt, addr int32, v int32) {
	g.setUInt32(ba, addr, uint32(v))
}

func (g *Go) setUInt64(ba BufferAt, addr int32, v uint64) {
	buf, err := ba.BufferAt(int64(addr), 8)
	if err != nil {
		panic(err)
	}
	binary.LittleEndian.PutUint64(buf, v)
}

func (g *Go) setInt64(ba BufferAt, addr int32, v int64) {
	g.setUInt64(ba, addr, uint64(v))
}

func (g *Go) setFloat64(ba BufferAt, addr int32, v float64) {
	g.setUInt64(ba, addr, uint64(v))
}

func (g *Go) getUInt64(ba BufferAt, addr int32) uint64 {
	buf, err := ba.BufferAt(int64(addr), 8)
	if err != nil {
		panic(err)
	}
	return binary.LittleEndian.Uint64(buf)
}

func (g *Go) getInt64(ba BufferAt, addr int32) int64 {
	return int64(g.getUInt64(ba, addr))
}

func (g *Go) getUInt32(ba BufferAt, addr int32) uint32 {
	buf, err := ba.BufferAt(int64(addr), 8)
	if err != nil {
		panic(err)
	}
	return binary.LittleEndian.Uint32(buf)
}

func (g *Go) getFloat64(ba BufferAt, addr int32) float64 {
	return float64(g.getUInt64(ba, addr))
}

func (g *Go) getRef(ba BufferAt, addr int32) ref {
	return ref(g.getUInt32(ba, addr))
}

func (g *Go) getInt32(ba BufferAt, addr int32) int32 {
	return int32(g.getUInt32(ba, addr))
}

func (g *Go) loadSlice(ba BufferAt, sp int32) ([]byte, error) {
	offset := g.getInt64(ba, sp)
	length := g.getInt64(ba, sp+8)

	return ba.BufferAt(offset, length)
}

func (g *Go) setSlice(ba BufferAt, sp int32, v []byte) {
	buf, err := g.loadSlice(ba, sp)
	if err != nil {
		panic(err)
	}

	copy(buf, v)
}

func (g *Go) loadString(ba BufferAt, sp int32) (string, error) {
	buf, err := g.loadSlice(ba, sp)
	if err != nil {
		return "", err
	}
	fmt.Println("loaded string", string(buf))
	return string(buf), nil
}

func (g *Go) loadValue(ba BufferAt, addr int32) Value {
	r := g.getRef(ba, addr)
	fmt.Println("getting id", r)
	if int(r) > len(g.values) {
		return g.values[jsValueUndefined] // this is how javascript acts when index out of bounds occurs
	}
	return g.values[r]
}

func (g *Go) loadSliceOfValues(ba BufferAt, addr int32) []interface{} {
	arrayAddr := g.getInt64(ba, addr)
	length := g.getInt64(ba, addr+8)

	array := make([]interface{}, length)
	for i := int64(0); i < length; i++ {
		id := g.getRef(ba, int32(arrayAddr+i*4))
		array[i] = g.values[id]
	}
	return array
}

func (g *Go) storeValue(ba BufferAt, addr int32, v interface{}) {
	const nanHead = 0x7FF80000

	fmt.Println("storeValue", addr, v)

	if v == nil {
		g.setUInt32(ba, addr+4, nanHead)
		g.setUInt32(ba, addr, jsValueNull)
		return
	}

	var r ref
	switch tv := v.(type) {
	case float64:
		if math.IsNaN(tv) {
			g.setUInt32(ba, addr+4, nanHead)
			g.setUInt32(ba, addr, 0)
			return
		}
		g.setFloat64(ba, addr, tv)
	case bool:
		g.setUInt32(ba, addr+4, nanHead)
		if tv {
			g.setUInt32(ba, addr, jsValueTrue)
		} else {
			g.setUInt32(ba, addr, jsValueFalse)
		}
		return
	case Value:
		if tv == g.values[jsValueUndefined] {
			g.setInt32(ba, addr, 0)
			return
		}

		if tv == g.values[jsValueNull] {
			g.setInt32(ba, addr, 1)
			return
		}

		r = tv.ref
	default:
		r = g.valueIndex
		g.values[r] = Value{
			ref: r,
			v:   tv,
		}
		g.valueIndex++
	}

	const (
		typeFlagString   = 1
		typeFlagSymbol   = 2
		typeFlagFunction = 3
	)

	var tf uint32
	switch reflect.TypeOf(v).Kind() {
	case reflect.String:
		tf = typeFlagString
	case reflect.Func:
		tf = typeFlagFunction
	}

	g.setUInt32(ba, addr+4, nanHead|tf)
	g.setRef(ba, addr, r)
}

func (g *Go) exportValuePrepareString(proc *exec.Process, sp int32) {
	fmt.Println("exportValuePrepareString")
	str, err := g.loadString(proc, sp+8)
	if err != nil {
		panic(err)
	}
	g.storeValue(proc, sp+16, str)
	g.setInt64(proc, sp+24, int64(len(str)))
}

func (g *Go) exportValueLoadString(proc *exec.Process, sp int32) {
	fmt.Println("exportValueLoadString")
	str := g.loadValue(proc, sp+8)
	g.setSlice(proc, sp+8, []byte(str.v.(string))) // will panic if not a string value
}

func (g *Go) exportStringVal(proc *exec.Process, sp int32) {
	fmt.Println("exportStringVal")
	s, err := g.loadString(proc, sp+8)
	if err != nil {
		panic(err)
	}
	g.storeValue(proc, sp+24, s)
}

func (g *Go) exportValueNew(proc *exec.Process, sp int32) {
	fmt.Println("exportValueNew")
	v := g.loadValue(proc, sp+8)
	args := g.loadSliceOfValues(proc, sp+16)

	n, ok := v.v.(newer)
	if !ok {
		err := fmt.Errorf("value %v of type %q is not a newer", v, reflect.TypeOf(v))

		g.storeValue(proc, sp+40, newJSError(err))
		g.setUInt8(proc, sp+48, 0)
		return
	}

	g.storeValue(proc, sp+40, n.New(args...))
	g.setUInt8(proc, sp+48, 1)
}

func (g *Go) exportValueCall(proc *exec.Process, sp int32) {
	fmt.Println("exportValueCall")

	v := g.loadValue(proc, sp+8)
	f, err := g.loadString(proc, sp+16)
	if err != nil {
		panic(err)
	}

	_, ok := v.v.(getter)
	if !ok {
		fmt.Println(g.values)
		log.Printf("value %v of type %v is not a getter wanting %q", v.ref, reflect.TypeOf(v.v), f)
		proc.Terminate()
		return
	}

	args := g.loadSliceOfValues(proc, sp+32)

	fmt.Printf("Calling %q on %v with args %v\n", f, v, args)
}

func (g *Go) exportValueGet(proc *exec.Process, sp int32) {
	fmt.Println("exportValueGet")
	v := g.loadValue(proc, sp+8)

	s, err := g.loadString(proc, sp+16)
	if err != nil {
		panic(err)
	}

	gtr, ok := v.v.(getter)
	if !ok {
		fmt.Println(g.values)
		log.Printf("value %v of type %v is not a getter wanting %q", v.ref, reflect.TypeOf(v.v), s)
		proc.Terminate()
		return
	}

	g.storeValue(proc, sp+32, gtr.Get(s))
}

func (g *Go) exportWalltime(proc *exec.Process, sp int32) {
	fmt.Println("exportWalltime")
	nsec := time.Now().UnixNano()
	secs := nsec / 1e9
	nsec = nsec - (secs * 1e9)
	g.setInt64(proc, sp+8, secs)
	g.setInt32(proc, sp+16, int32(nsec))
}

func (g *Go) exportNanotime(proc *exec.Process, sp int32) {
	fmt.Println("exportNanotime")
	g.setInt64(proc, sp+8, time.Since(g.timeOrigin).Nanoseconds())
}

func (g *Go) exportGetRandomData(proc *exec.Process, sp int32) {
	fmt.Println("exportGetRandomData")
	s, err := g.loadSlice(proc, sp)
	if err != nil {
		panic(err)
	}
	rand.Read(s)
}

func (g *Go) exportWasmWrite(proc *exec.Process, sp int32) {
	fmt.Println("exportWasmWrite")
	s, err := g.loadString(proc, sp+16)
	if err != nil {
		panic(err)
	}

	fmt.Print(s)
}

func (g *Go) exportExit(proc *exec.Process, sp int32) {
	fmt.Println("exportExit")
	proc.Terminate()
}
