# jni.c in gstkit calls these by name.
-keep class gst.app.Core { native <methods>; }
-keep interface gst.app.Core$System { *; }
-keep class * implements gst.app.Core$System { public int apply(byte[]); public byte[] forward(byte[]); }
