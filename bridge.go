// C ABI layout follows CLIProxyAPI's MIT-licensed examples/plugin/management-api.
package main

/*
#include <stdint.h>
#include <stdlib.h>
typedef struct { void* ptr; size_t len; } cliproxy_buffer;
typedef int (*host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*free_fn)(void*, size_t);
typedef struct {
    uint32_t abi_version; void* host_ctx; host_call_fn call; free_fn free_buffer;
} cliproxy_host_api;
typedef int (*plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*shutdown_fn)(void);
typedef struct {
    uint32_t abi_version; plugin_call_fn call; free_fn free_buffer; shutdown_fn shutdown;
} cliproxy_plugin_api;
extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
static const cliproxy_host_api* stored_host;
static int host_call(const char* method, const uint8_t* req, size_t len, cliproxy_buffer* out) {
    if (!stored_host || !stored_host->call) return 1;
    return stored_host->call(stored_host->host_ctx, method, req, len, out);
}
static void host_free(cliproxy_buffer out) {
    if (stored_host && stored_host->free_buffer && out.ptr)
        stored_host->free_buffer(out.ptr, out.len);
}
static void init_api(const cliproxy_host_api* host, cliproxy_plugin_api* plugin) {
    stored_host = host;
    plugin->abi_version = 1;
    plugin->call = (plugin_call_fn)cliproxyPluginCall;
    plugin->free_buffer = (free_fn)cliproxyPluginFree;
    plugin->shutdown = (shutdown_fn)cliproxyPluginShutdown;
}
*/
import "C"

import (
	"encoding/json"
	"errors"
	"unsafe"
)

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if host == nil || plugin == nil || host.abi_version != 1 {
		return 1
	}
	C.init_api(host, plugin)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, size C.size_t, out *C.cliproxy_buffer) C.int {
	if out == nil {
		return 1
	}
	out.ptr, out.len = nil, 0
	var result any
	var err error
	if method == nil || uint64(size) > 4<<20 || (size > 0 && request == nil) {
		err = errors.New("invalid plugin request")
	} else {
		result, err = dispatch(C.GoString(method), C.GoBytes(unsafe.Pointer(request), C.int(size)), callHost)
	}
	response := map[string]any{"ok": true, "result": result}
	if err != nil {
		response = map[string]any{"ok": false, "error": map[string]string{"code": "ollama_usage_error", "message": err.Error()}}
	}
	raw, marshalErr := json.Marshal(response)
	if marshalErr != nil {
		return 1
	}
	out.ptr, out.len = C.CBytes(raw), C.size_t(len(raw))
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, size C.size_t) { C.free(ptr) }

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}

func callHost(method string, payload any, result any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return errors.New("could not encode host request")
	}
	m := C.CString(method)
	req := C.CBytes(raw)
	defer C.free(unsafe.Pointer(m))
	defer C.free(req)
	var out C.cliproxy_buffer
	code := C.host_call(m, (*C.uint8_t)(req), C.size_t(len(raw)), &out)
	defer C.host_free(out)
	if code != 0 || out.ptr == nil || uint64(out.len) > 4<<20 {
		return errors.New("CLIProxyAPI host callback failed")
	}
	var envelope struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
	}
	if json.Unmarshal(C.GoBytes(out.ptr, C.int(out.len)), &envelope) != nil || !envelope.OK {
		// A host error may contain request headers; never relay it.
		return errors.New("CLIProxyAPI host callback failed")
	}
	if result != nil && json.Unmarshal(envelope.Result, result) != nil {
		return errors.New("invalid CLIProxyAPI host response")
	}
	return nil
}
