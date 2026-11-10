// gstkit is gst for the iOS app: the Go code of internal/mobile, as a C
// library that the app and its packet tunnel extension both link.
//
// Every text is UTF-8 and ends in a zero byte. A function that returns
// text gives it to the caller, who frees it with gst_free.
#ifndef GSTKIT_H
#define GSTKIT_H

// gst_apply_fn sets the system's network interface up as settings says,
// a JSON object {"address", "subnet", "dns", "domain", "mtu"}, and
// returns once that is done: NULL, or what failed, as text from malloc,
// which gst frees. gst calls it on a thread of its own.
typedef char *(*gst_apply_fn)(void *context, const char *settings);

// gst_open opens gst with its files in dir, which the app and the
// extension share, before anything else. The first time, it names the
// machine after name. It returns NULL, or what failed.
char *gst_open(const char *dir, const char *name);

// gst_call calls a method: request is the JSON object {"method",
// "params"}, and the result the JSON object {"result"} or {"error":
// {"code", "message"}}. The methods "app.…" answer in either process;
// the daemon's (internal/wire) only where gst_start runs it.
char *gst_call(const char *request);

// gst_start runs the daemon in this process until gst_stop, and calls
// apply, with context, whenever the interface's settings change. It
// returns NULL, or what failed.
char *gst_start(gst_apply_fn apply, void *context);

// gst_stop stops the daemon and returns once it has.
void gst_stop(void);

// gst_free frees a text that a function here returned.
void gst_free(char *text);

#endif
