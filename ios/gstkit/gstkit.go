// Command gstkit is gst as a C library for the iOS app (-buildmode=c-archive):
// internal/mobile behind the functions of gstkit.h, which the app and its
// packet tunnel extension call. ios/build-core.sh builds it.
package main

/*
#include <stdlib.h>

typedef char *(*gst_apply_fn)(void *context, const char *settings);
char *gst_apply(gst_apply_fn apply, void *context, const char *settings);
*/
import "C"

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/yangtau/ssh-tunnel/internal/mobile"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// callTimeout bounds a call, as the gst command bounds the ones that ask
// other machines.
const callTimeout = 40 * time.Second

var app atomic.Pointer[mobile.App]

func main() {}

// text returns err's text for the caller to free, or nil.
func text(err error) *C.char {
	if err == nil {
		return nil
	}
	return C.CString(err.Error())
}

var errNotOpen = errors.New("gst is not open")

//export gst_open
func gst_open(dir, name *C.char) *C.char {
	a, err := mobile.Open(C.GoString(dir), C.GoString(name))
	if err != nil {
		return text(err)
	}
	app.Store(a)
	return nil
}

//export gst_call
func gst_call(request *C.char) *C.char {
	var req wire.Request
	var resp wire.Response
	switch a := app.Load(); {
	case a == nil:
		resp.Error = wire.AsError(errNotOpen)
	case json.Unmarshal([]byte(C.GoString(request)), &req) != nil:
		resp.Error = wire.Errorf(wire.CodeInvalidParams, "invalid request")
	default:
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		resp = a.Call(ctx, req)
		cancel()
	}
	out, _ := json.Marshal(resp)
	return C.CString(string(out))
}

//export gst_start
func gst_start(apply C.gst_apply_fn, context unsafe.Pointer) *C.char {
	a := app.Load()
	if a == nil {
		return text(errNotOpen)
	}
	return text(a.Start(func(s mobile.Settings) error {
		settings, _ := json.Marshal(s)
		in := C.CString(string(settings))
		defer C.free(unsafe.Pointer(in))
		if failed := C.gst_apply(apply, context, in); failed != nil {
			defer C.free(unsafe.Pointer(failed))
			return errors.New(C.GoString(failed))
		}
		return nil
	}))
}

//export gst_stop
func gst_stop() {
	if a := app.Load(); a != nil {
		a.Stop()
	}
}

//export gst_free
func gst_free(text *C.char) {
	C.free(unsafe.Pointer(text))
}
