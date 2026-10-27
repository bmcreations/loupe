// JVMTI agent for `am attach-agent`. It only bridges into Java: it loads
// loupe-compose.dex (the agent options are its path) under a DexClassLoader
// and calls dev.loupe.compose.Bootstrap.run, which does the rest on its own
// thread. See internal/composetree for the protocol the dex serves.
#include <jni.h>
#include <android/log.h>

#define TAG "LoupeCompose"

static int failed(JNIEnv *env, const char *what) {
  if (!(*env)->ExceptionCheck(env)) return 0;
  __android_log_print(ANDROID_LOG_ERROR, TAG, "%s failed", what);
  (*env)->ExceptionDescribe(env);
  (*env)->ExceptionClear(env);
  return 1;
}

static jint start(JavaVM *vm, char *dex) {
  JNIEnv *env;
  if ((*vm)->GetEnv(vm, (void **)&env, JNI_VERSION_1_6) != JNI_OK || dex == NULL) return 1;

  // The app's class loader isn't reachable yet from an attach callback in
  // every case, so the dex starts under the system loader and Bootstrap
  // reloads itself under the app's.
  jclass cl = (*env)->FindClass(env, "java/lang/ClassLoader");
  jobject sys = (*env)->CallStaticObjectMethod(env, cl,
      (*env)->GetStaticMethodID(env, cl, "getSystemClassLoader", "()Ljava/lang/ClassLoader;"));
  jclass dcl = (*env)->FindClass(env, "dalvik/system/DexClassLoader");
  jmethodID ctor = (*env)->GetMethodID(env, dcl, "<init>",
      "(Ljava/lang/String;Ljava/lang/String;Ljava/lang/String;Ljava/lang/ClassLoader;)V");
  jstring path = (*env)->NewStringUTF(env, dex);
  jobject loader = (*env)->NewObject(env, dcl, ctor, path, NULL, NULL, sys);
  if (failed(env, "DexClassLoader")) return 1;

  jmethodID load = (*env)->GetMethodID(env, cl, "loadClass", "(Ljava/lang/String;)Ljava/lang/Class;");
  jclass boot = (jclass)(*env)->CallObjectMethod(env, loader, load,
      (*env)->NewStringUTF(env, "dev.loupe.compose.Bootstrap"));
  if (failed(env, "loadClass Bootstrap")) return 1;
  jmethodID run = (*env)->GetStaticMethodID(env, boot, "run", "(Ljava/lang/String;)V");
  (*env)->CallStaticVoidMethod(env, boot, run, path);
  return failed(env, "Bootstrap.run");
}

JNIEXPORT jint JNICALL Agent_OnAttach(JavaVM *vm, char *options, void *reserved) { return start(vm, options); }
JNIEXPORT jint JNICALL Agent_OnLoad(JavaVM *vm, char *options, void *reserved) { return start(vm, options); }
