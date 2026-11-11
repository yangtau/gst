// Command gstkit is gst as a C library for the phone apps:
// internal/mobile behind the functions of gstkit.h. ios/build-core.sh
// builds it for the iOS app (-buildmode=c-archive), and
// android/build-core.sh for the Android app (-buildmode=c-shared), with
// jni.c, which Java calls.
package main

/*
#include <stdlib.h>

// As in gstkit.h, which cgo's own declarations of the functions below
// would contradict.
typedef char *(*gst_apply_fn)(void *context, const char *settings, int *fd);
typedef unsigned char *(*gst_forward_fn)(void *context, const unsigned char *query, int length, int *answer_length);
char *gst_call_apply(gst_apply_fn apply, void *context, const char *settings, int *fd);
unsigned char *gst_call_forward(gst_forward_fn forward, void *context, const unsigned char *query, int length, int *answer_length);
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

var (
	errNotOpen  = errors.New("gst is not open")
	errNoAnswer = errors.New("the system's resolver gave no answer")
)

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
func gst_start(apply C.gst_apply_fn, forward C.gst_forward_fn, context unsafe.Pointer) *C.char {
	a := app.Load()
	if a == nil {
		return text(errNotOpen)
	}
	sys := mobile.System{Apply: func(s mobile.Settings) (int, error) {
		settings, _ := json.Marshal(s)
		in := C.CString(string(settings))
		defer C.free(unsafe.Pointer(in))
		fd := C.int(-1)
		if failed := C.gst_call_apply(apply, context, in, &fd); failed != nil {
			defer C.free(unsafe.Pointer(failed))
			return -1, errors.New(C.GoString(failed))
		}
		return int(fd), nil
	}}
	if forward != nil {
		sys.Forward = func(query []byte) ([]byte, error) {
			in := C.CBytes(query)
			defer C.free(in)
			var length C.int
			answer := C.gst_call_forward(forward, context, (*C.uchar)(in), C.int(len(query)), &length)
			if answer == nil {
				return nil, errNoAnswer
			}
			defer C.free(unsafe.Pointer(answer))
			return C.GoBytes(unsafe.Pointer(answer), length), nil
		}
	}
	return text(a.Start(sys))
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
