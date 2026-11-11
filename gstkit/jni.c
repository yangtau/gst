//go:build android || jni

// gstkit's face to Java, for the Android app: the native methods of the
// class gst.app.Core. Texts cross as UTF-8 in byte arrays, which Java's
// own encoding of strings in JNI is not.
#include <jni.h>
#include <stdlib.h>
#include <string.h>

#include "_cgo_export.h"

static JavaVM *vm;
// The Core.System of gst_start, and its methods, while the daemon runs.
static jobject sys;
static jmethodID sysApply, sysForward;

JNIEXPORT jint JNI_OnLoad(JavaVM *v, void *reserved) {
	vm = v;
	return JNI_VERSION_1_6;
}

// text returns the bytes of array, and a zero byte after them, from malloc.
static char *text(JNIEnv *env, jbyteArray array) {
	jsize n = (*env)->GetArrayLength(env, array);
	char *s = malloc(n + 1);
	(*env)->GetByteArrayRegion(env, array, 0, n, (jbyte *)s);
	s[n] = 0;
	return s;
}

// bytes returns s, a text of gst, as a byte array, and frees it; NULL
// for NULL.
static jbyteArray bytes(JNIEnv *env, char *s) {
	if (s == NULL) {
		return NULL;
	}
	jsize n = strlen(s);
	jbyteArray array = (*env)->NewByteArray(env, n);
	if (array != NULL) {
		(*env)->SetByteArrayRegion(env, array, 0, n, (const jbyte *)s);
	}
	gst_free(s);
	return array;
}

JNIEXPORT jbyteArray JNICALL Java_gst_app_Core_open(JNIEnv *env, jclass c, jbyteArray dir, jbyteArray name) {
	char *d = text(env, dir), *n = text(env, name);
	char *failed = gst_open(d, n);
	free(d);
	free(n);
	return bytes(env, failed);
}

JNIEXPORT jbyteArray JNICALL Java_gst_app_Core_call(JNIEnv *env, jclass c, jbyteArray request) {
	char *r = text(env, request);
	char *answer = gst_call(r);
	free(r);
	return bytes(env, answer);
}

// attach returns the JNIEnv of this thread, a thread of Go's, which it
// attaches to Java if it is not; *attached says whether it did.
static JNIEnv *attach(int *attached) {
	void *env = NULL;
	*attached = 0;
	if ((*vm)->GetEnv(vm, &env, JNI_VERSION_1_6) == JNI_EDETACHED) {
		if ((*vm)->AttachCurrentThread(vm, (void *)&env, NULL) != JNI_OK) {
			return NULL;
		}
		*attached = 1;
	}
	return env;
}

static void detach(int attached) {
	if (attached) {
		(*vm)->DetachCurrentThread(vm);
	}
}

// thrown clears the exception that Java threw, and returns its message,
// from malloc; NULL if Java threw none.
static char *thrown(JNIEnv *env) {
	jthrowable e = (*env)->ExceptionOccurred(env);
	if (e == NULL) {
		return NULL;
	}
	(*env)->ExceptionClear(env);
	char *out = NULL;
	jclass c = (*env)->FindClass(env, "java/lang/Throwable");
	jmethodID getMessage = c ? (*env)->GetMethodID(env, c, "getMessage", "()Ljava/lang/String;") : NULL;
	jstring message = getMessage ? (jstring)(*env)->CallObjectMethod(env, e, getMessage) : NULL;
	(*env)->ExceptionClear(env);
	if (message != NULL) {
		const char *s = (*env)->GetStringUTFChars(env, message, NULL);
		if (s != NULL) {
			out = strdup(s);
			(*env)->ReleaseStringUTFChars(env, message, s);
		}
		(*env)->DeleteLocalRef(env, message);
	}
	if (c != NULL) {
		(*env)->DeleteLocalRef(env, c);
	}
	(*env)->DeleteLocalRef(env, e);
	return out ? out : strdup("the system did not set the interface up");
}

static char *apply(void *context, const char *settings, int *fd) {
	int attached;
	JNIEnv *env = attach(&attached);
	if (env == NULL) {
		return strdup("gst cannot call Java");
	}
	jsize n = strlen(settings);
	jbyteArray s = (*env)->NewByteArray(env, n);
	(*env)->SetByteArrayRegion(env, s, 0, n, (const jbyte *)settings);
	jint got = (*env)->CallIntMethod(env, sys, sysApply, s);
	(*env)->DeleteLocalRef(env, s);
	char *failed = thrown(env);
	if (failed == NULL) {
		*fd = got;
	}
	detach(attached);
	return failed;
}

static unsigned char *forward(void *context, const unsigned char *query, int length, int *answer_length) {
	int attached;
	JNIEnv *env = attach(&attached);
	if (env == NULL) {
		return NULL;
	}
	jbyteArray q = (*env)->NewByteArray(env, length);
	(*env)->SetByteArrayRegion(env, q, 0, length, (const jbyte *)query);
	jbyteArray a = (jbyteArray)(*env)->CallObjectMethod(env, sys, sysForward, q);
	(*env)->DeleteLocalRef(env, q);
	unsigned char *answer = NULL;
	char *failed = thrown(env);
	if (failed != NULL) {
		free(failed);
	} else if (a != NULL) {
		*answer_length = (*env)->GetArrayLength(env, a);
		answer = malloc(*answer_length > 0 ? *answer_length : 1);
		(*env)->GetByteArrayRegion(env, a, 0, *answer_length, (jbyte *)answer);
	}
	if (a != NULL) {
		(*env)->DeleteLocalRef(env, a);
	}
	detach(attached);
	return answer;
}

static void release(JNIEnv *env) {
	if (sys != NULL) {
		(*env)->DeleteGlobalRef(env, sys);
		sys = NULL;
	}
}

JNIEXPORT jbyteArray JNICALL Java_gst_app_Core_start(JNIEnv *env, jclass c, jobject system) {
	if (sys != NULL) {
		return bytes(env, strdup("gst runs already"));
	}
	jclass type = (*env)->GetObjectClass(env, system);
	sysApply = (*env)->GetMethodID(env, type, "apply", "([B)I");
	sysForward = (*env)->GetMethodID(env, type, "forward", "([B)[B");
	(*env)->DeleteLocalRef(env, type);
	if (sysApply == NULL || sysForward == NULL) {
		(*env)->ExceptionClear(env);
		return bytes(env, strdup("Core.System lacks a method"));
	}
	sys = (*env)->NewGlobalRef(env, system);
	char *failed = gst_start(apply, forward, NULL);
	if (failed != NULL) {
		release(env);
	}
	return bytes(env, failed);
}

JNIEXPORT void JNICALL Java_gst_app_Core_stop(JNIEnv *env, jclass c) {
	gst_stop();
	release(env);
}
