// cgo cannot call a function pointer, so these call them.
#include "gstkit.h"

char *gst_call_apply(gst_apply_fn apply, void *context, const char *settings, int *fd) {
	return apply(context, settings, fd);
}

unsigned char *gst_call_forward(gst_forward_fn forward, void *context, const unsigned char *query, int length, int *answer_length) {
	return forward(context, query, length, answer_length);
}
