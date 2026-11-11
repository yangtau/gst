// gstkit is gst for the phone apps: the Go code of internal/mobile, as a
// C library that the iOS app and its packet tunnel extension link
// statically, and that the Android app loads (jni.c is its face to Java).
//
// Every text is UTF-8 and ends in a zero byte. A function that returns
// text gives it to the caller, who frees it with gst_free.
#ifndef GSTKIT_H
#define GSTKIT_H

// gst_apply_fn sets the system's network interface up as settings says,
// a JSON object {"address", "routes", "dns", "domain", "mtu"}, and
// returns once that is done: NULL, or what failed, as text from malloc,
// which gst frees. A system that hands the interface over as a
// descriptor, as Android does, stores it in *fd, and gst owns it then;
// one that keeps the interface, as iOS does, leaves *fd at -1. gst calls
// it on a thread of its own.
typedef char *(*gst_apply_fn)(void *context, const char *settings, int *fd);

// gst_forward_fn asks the system's own resolver the DNS query of length
// bytes, for a name outside .gst, and returns the answer, from malloc,
// which gst frees, with its length in *answer_length; or NULL. gst calls
// it on a thread of its own, for a system that sends every query to the
// DNS of the interface, as Android does.
typedef unsigned char *(*gst_forward_fn)(void *context, const unsigned char *query, int length, int *answer_length);

// gst_open opens gst with its files in dir, which the app and the
// extension share, before anything else. The first time, it names the
// machine after name. It returns NULL, or what failed.
char *gst_open(const char *dir, const char *name);

// gst_call calls a method: request is the JSON object {"method",
// "params"}, and the result the JSON object {"result"} or {"error":
// {"code", "message"}}. The methods "app.…" answer in any process; the
// daemon's (internal/wire) only where gst_start runs it.
char *gst_call(const char *request);

// gst_start runs the daemon in this process until gst_stop. It calls
// apply, with context, whenever the interface's settings change, and
// forward, unless it is NULL, for each query that it does not answer
// itself. It returns NULL, or what failed.
char *gst_start(gst_apply_fn apply, gst_forward_fn forward, void *context);

// gst_stop stops the daemon and returns once it has.
void gst_stop(void);

// gst_free frees a text that a function here returned.
void gst_free(char *text);

#endif
