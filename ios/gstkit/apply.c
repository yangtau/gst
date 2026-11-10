// cgo cannot call a function pointer, so this calls it.
typedef char *(*gst_apply_fn)(void *context, const char *settings);

char *gst_apply(gst_apply_fn apply, void *context, const char *settings) {
	return apply(context, settings);
}
