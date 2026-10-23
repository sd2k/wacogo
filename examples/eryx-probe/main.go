// Command eryx-probe runs eryx's Python sandbox component on wacogo.
//
// Usage: eryx-probe <runtime.wasm> [python code]
//
// It expects the eryx Python stdlib extracted at testdata/python-stdlib.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/partite-ai/wacogo"
	"github.com/partite-ai/wacogo/host"
	"github.com/partite-ai/wacogo/wasi"
	"github.com/partite-ai/wacogo/wasi/filesystem/preopens"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
)

const defaultCode = `
import asyncio, time
print("1 + 1 =", 1 + 1)
print("echo:", await echo(msg="hi"))
t = time.monotonic()
a, b = await asyncio.gather(slow(n=1), slow(n=2))
print("gather:", a, b, f"{time.monotonic() - t:.2f}s")
t = time.monotonic()
await asyncio.sleep(0.1)
print(f"sleep: {time.monotonic() - t:.2f}s")
result = {"ok": True}
`

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage: eryx-probe <runtime.wasm> [python code]")
	}
	code := defaultCode
	if len(os.Args) > 2 {
		code = os.Args[2]
	}
	ctx := context.Background()
	// A file-backed compilation cache skips recompiling eryx's core modules
	// on later runs.
	cache, err := wazero.NewCompilationCacheWithDir(filepath.Join(os.TempDir(), "eryx-probe-cache"))
	if err != nil {
		log.Fatal(err)
	}
	e := wacogo.NewEngine(ctx, wacogo.WithRuntimeConfig(wazero.NewRuntimeConfig().
		WithCoreFeatures(api.CoreFeaturesV2|experimental.CoreFeaturesExtendedConst).
		WithCompilationCache(cache)))
	defer e.Close(ctx)

	f, err := os.Open(os.Args[1])
	if err != nil {
		log.Fatal(err)
	}
	t := time.Now()
	comp, err := e.LoadComponent(ctx, f)
	if err != nil {
		log.Fatalf("load: %v", err)
	}
	log.Printf("load: %v", time.Since(t))

	w, err := wasi.NewWorld(ctx, e, &wasi.Config{
		Env: [][2]string{
			{"PYTHONHOME", "/python-stdlib"},
			{"PYTHONPATH", "/python-stdlib"},
		},
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		Preopens: preopens.NewMultiFSPreopens([]*preopens.PreopenEntry{
			{Path: "/python-stdlib", Root: ".", FS: preopens.ImmutableFS{FS: os.DirFS("testdata/python-stdlib")}},
		}),
	})
	if err != nil {
		log.Fatal(err)
	}
	defer w.Close(ctx)

	eryxHost, err := newEryxHost(ctx, e)
	if err != nil {
		log.Fatalf("eryx host: %v", err)
	}
	tcp, tls, err := newNetHosts(ctx, e)
	if err != nil {
		log.Fatalf("net hosts: %v", err)
	}

	opts := w.Imports()
	opts = append(opts,
		wacogo.WithInstanceImport("eryx:net/tcp@0.1.0", tcp.Core()),
		wacogo.WithInstanceImport("eryx:net/tls@0.1.0", tls.Core()),
	)
	for _, name := range []string{"callback-info", "execution-options", "execute-output"} {
		opts = append(opts, wacogo.WithTypeImport(name, eryxHost.Core().ExportedType(name)))
	}
	for _, name := range []string{"invoke", "sleep", "list-callbacks", "get-execution-options", "report-trace", "report-output"} {
		opts = append(opts, wacogo.WithFuncImport(name, eryxHost.Core().ExportedFunc(name)))
	}

	t = time.Now()
	inst, err := comp.Instantiate(ctx, opts...)
	if err != nil {
		log.Fatalf("instantiate: %v", err)
	}
	defer inst.Close(ctx)
	log.Printf("instantiate: %v", time.Since(t))

	t = time.Now()
	if _, err := inst.ExportedFunc("set-result-variable").Call(ctx, wacogo.ValString("result")); err != nil {
		log.Fatalf("set-result-variable: %v", err)
	}
	log.Printf("set-result-variable: %v", time.Since(t))

	for i := range 2 {
		t = time.Now()
		out, err := inst.ExportedFunc("execute").Call(ctx, wacogo.ValString(code))
		if err != nil {
			log.Fatalf("execute: %v", err)
		}
		log.Printf("execute #%d: %v", i+1, time.Since(t))
		res := out[0].(*wacogo.ValResult)
		if !res.IsOk() {
			log.Fatalf("execute failed: %s", res.Err().(wacogo.ValString))
		}
		rec := res.Ok().(*wacogo.ValRecord)
		fmt.Printf("stdout:\n%s", bytesOf(rec.Field("stdout")))
		fmt.Printf("stderr:\n%s", bytesOf(rec.Field("stderr")))
		fmt.Printf("result-json: %s\n", rec.Field("result-json").(wacogo.ValString))
	}
}

func bytesOf(v wacogo.Val) []byte {
	l := v.(*wacogo.ValList)
	b := make([]byte, l.Len())
	for i := range b {
		b[i] = byte(l.Get(i).(wacogo.ValU8))
	}
	return b
}

// newEryxHost builds the root-level imports of eryx's sandbox world.
func newEryxHost(ctx context.Context, e *wacogo.Engine) (*host.ComponentInstance, error) {
	b := e.NewHostBuilder("eryx-host")
	callbackInfo := b.AddType("callback-info", host.Record{Fields: []host.Field{
		{Name: "name", Type: host.String},
		{Name: "description", Type: host.String},
		{Name: "parameters-schema-json", Type: host.String},
	}})
	execOptions := b.AddType("execution-options", host.Record{Fields: []host.Field{
		{Name: "python-tracing", Type: host.Bool},
		{Name: "reuse-empty-callbacks", Type: host.Bool},
	}})
	b.AddType("execute-output", host.Record{Fields: []host.Field{
		{Name: "stdout", Type: host.List{Elem: host.U8}},
		{Name: "stderr", Type: host.List{Elem: host.U8}},
		{Name: "result-json", Type: host.String},
		{Name: "result-error", Type: host.String},
	}})

	b.AddGoFunction("invoke", &host.FuncType{
		Async: true,
		Params: []host.Param{
			{Name: "name", Type: host.String},
			{Name: "arguments-json", Type: host.String},
		},
		Results: []host.ResultDecl{{Type: host.Result{Ok: host.String, Err: host.String}}},
	}, func(ctx context.Context, args []wacogo.Val) ([]wacogo.Val, error) {
		name, argsJSON := string(args[0].(wacogo.ValString)), string(args[1].(wacogo.ValString))
		switch name {
		case "echo":
			return []wacogo.Val{wacogo.ValResultOk(wacogo.ValString(argsJSON))}, nil
		case "slow":
			select {
			case <-time.After(300 * time.Millisecond):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			var a struct{ N int }
			_ = json.Unmarshal([]byte(argsJSON), &a)
			return []wacogo.Val{wacogo.ValResultOk(wacogo.ValString(fmt.Sprint(a.N * 10)))}, nil
		}
		return []wacogo.Val{wacogo.ValResultErr(wacogo.ValString("unknown callback " + name))}, nil
	})
	b.AddGoFunction("sleep", &host.FuncType{
		Async:  true,
		Params: []host.Param{{Name: "duration-ns", Type: host.U64}},
	}, func(ctx context.Context, args []wacogo.Val) ([]wacogo.Val, error) {
		select {
		case <-time.After(time.Duration(args[0].(wacogo.ValU64))):
			return nil, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	b.AddGoFunction("list-callbacks", &host.FuncType{
		Results: []host.ResultDecl{{Type: host.List{Elem: callbackInfo}}},
	}, func(context.Context, []wacogo.Val) ([]wacogo.Val, error) {
		info := func(name, desc, schema string) *wacogo.ValRecord {
			return wacogo.NewValRecord(
				wacogo.Field{Name: "name", Val: wacogo.ValString(name)},
				wacogo.Field{Name: "description", Val: wacogo.ValString(desc)},
				wacogo.Field{Name: "parameters-schema-json", Val: wacogo.ValString(schema)},
			)
		}
		return []wacogo.Val{wacogo.NewValListOf(
			info("echo", "Echo the arguments", `{"type":"object","properties":{"msg":{"type":"string"}}}`),
			info("slow", "Wait, then return n*10", `{"type":"object","properties":{"n":{"type":"integer"}}}`),
		)}, nil
	})
	b.AddGoFunction("get-execution-options", &host.FuncType{
		Results: []host.ResultDecl{{Type: execOptions}},
	}, func(context.Context, []wacogo.Val) ([]wacogo.Val, error) {
		return []wacogo.Val{wacogo.NewValRecord(
			wacogo.Field{Name: "python-tracing", Val: wacogo.ValBool(false)},
			wacogo.Field{Name: "reuse-empty-callbacks", Val: wacogo.ValBool(false)},
		)}, nil
	})
	b.AddGoFunction("report-trace", &host.FuncType{
		Params: []host.Param{
			{Name: "lineno", Type: host.U32},
			{Name: "event-json", Type: host.String},
			{Name: "context-json", Type: host.String},
		},
	}, func(context.Context, []wacogo.Val) ([]wacogo.Val, error) { return nil, nil })
	b.AddGoFunction("report-output", &host.FuncType{
		Params: []host.Param{
			{Name: "stream-id", Type: host.U32},
			{Name: "data", Type: host.List{Elem: host.U8}},
		},
	}, func(context.Context, []wacogo.Val) ([]wacogo.Val, error) { return nil, nil })

	c, err := b.Build(ctx)
	if err != nil {
		return nil, err
	}
	return c.Instantiate(ctx)
}

// newNetHosts builds eryx:net/tcp and eryx:net/tls instances whose every
// call fails as not permitted.
func newNetHosts(ctx context.Context, e *wacogo.Engine) (tcp, tls *host.ComponentInstance, err error) {
	tcpErrorCases := []host.Case{
		{Name: "connection-refused"}, {Name: "connection-reset"}, {Name: "timed-out"},
		{Name: "host-not-found"}, {Name: "io-error", Payload: host.String},
		{Name: "not-permitted", Payload: host.String}, {Name: "invalid-handle"},
	}
	const notPermitted = 5

	build := func(name string, extra func(b *host.Builder, errType host.TypeExpr, notPermitted func() wacogo.Val)) (*host.ComponentInstance, error) {
		b := e.NewHostBuilder(name)
		tcpError := b.AddType("tcp-error", host.Variant{Cases: tcpErrorCases})
		tcpNotPermitted := func() wacogo.Val {
			return wacogo.NewValVariant(notPermitted, wacogo.ValString("networking is disabled"))
		}
		extra(b, tcpError, tcpNotPermitted)
		c, err := b.Build(ctx)
		if err != nil {
			return nil, err
		}
		return c.Instantiate(ctx)
	}
	fail := func(v func() wacogo.Val) host.GoFunc {
		return func(context.Context, []wacogo.Val) ([]wacogo.Val, error) {
			return []wacogo.Val{wacogo.ValResultErr(v())}, nil
		}
	}
	noop := func(context.Context, []wacogo.Val) ([]wacogo.Val, error) { return nil, nil }
	// conn adds connect-style read/write/close functions over handle type h.
	conn := func(b *host.Builder, h, errType host.TypeExpr, errVal func() wacogo.Val) {
		b.AddGoFunction("read", &host.FuncType{
			Params:  []host.Param{{Name: "handle", Type: h}, {Name: "len", Type: host.U32}, {Name: "timeout-ms", Type: host.U32}},
			Results: []host.ResultDecl{{Type: host.Result{Ok: host.List{Elem: host.U8}, Err: errType}}},
		}, fail(errVal))
		b.AddGoFunction("write", &host.FuncType{
			Params:  []host.Param{{Name: "handle", Type: h}, {Name: "timeout-ms", Type: host.U32}, {Name: "data", Type: host.List{Elem: host.U8}}},
			Results: []host.ResultDecl{{Type: host.Result{Ok: host.U32, Err: errType}}},
		}, fail(errVal))
		b.AddGoFunction("close", &host.FuncType{Params: []host.Param{{Name: "handle", Type: h}}}, noop)
	}

	tcp, err = build("eryx:net/tcp", func(b *host.Builder, tcpError host.TypeExpr, np func() wacogo.Val) {
		h := host.U32
		b.AddGoFunction("connect", &host.FuncType{
			Params:  []host.Param{{Name: "host", Type: host.String}, {Name: "port", Type: host.U16}, {Name: "timeout-ms", Type: host.U32}},
			Results: []host.ResultDecl{{Type: host.Result{Ok: h, Err: tcpError}}},
		}, fail(np))
		conn(b, h, tcpError, np)
	})
	if err != nil {
		return nil, nil, err
	}
	tls, err = build("eryx:net/tls", func(b *host.Builder, tcpError host.TypeExpr, np func() wacogo.Val) {
		tcpHandle := host.U32
		tlsError := b.AddType("tls-error", host.Variant{Cases: []host.Case{
			{Name: "tcp", Payload: tcpError}, {Name: "handshake-failed", Payload: host.String},
			{Name: "certificate-error", Payload: host.String}, {Name: "invalid-handle"},
		}})
		tlsNP := func() wacogo.Val { return wacogo.NewValVariant(0, np()) }
		h := host.U32
		b.AddGoFunction("upgrade", &host.FuncType{
			Params:  []host.Param{{Name: "tcp", Type: tcpHandle}, {Name: "hostname", Type: host.String}, {Name: "timeout-ms", Type: host.U32}},
			Results: []host.ResultDecl{{Type: host.Result{Ok: h, Err: tlsError}}},
		}, fail(tlsNP))
		conn(b, h, tlsError, tlsNP)
	})
	return tcp, tls, err
}
