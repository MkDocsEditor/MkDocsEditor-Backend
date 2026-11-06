// Package nativemem keeps the memory of C code in check, which is mostly Automerge.
//
// Automerge allocates with malloc, and automerge-go frees a document only in a finalizer, so
// after a garbage collection of the Go heap. The release builds link glibc statically, and
// glibc's malloc gives each thread that allocates its own arena. Go runs cgo calls on many
// threads, so loading and forking documents spreads the memory over many arenas, and glibc
// keeps what is freed there instead of returning it to the system: the resident memory grows
// with every load while the documents in use stay small.
//
// Start limits the arenas and returns the free memory to the system regularly. Without glibc
// (macOS, musl) it does nothing.
package nativemem

/*
// stdlib.h defines __GLIBC__, stddef.h of the compiler does not
#include <stdlib.h>

#if defined(__GLIBC__)
#include <malloc.h>
#endif

static int nm_supported(void) {
#if defined(__GLIBC__)
	return 1;
#else
	return 0;
#endif
}

static void nm_limit_arenas(int n) {
#if defined(__GLIBC__)
	mallopt(M_ARENA_MAX, n);
#endif
}

static void nm_trim(void) {
#if defined(__GLIBC__)
	malloc_trim(0);
#endif
}

// nm_info returns the bytes in use and the free bytes that malloc keeps
static void nm_info(size_t *in_use, size_t *free_bytes) {
#if defined(__GLIBC__) && (__GLIBC__ > 2 || (__GLIBC__ == 2 && __GLIBC_MINOR__ >= 33))
	struct mallinfo2 m = mallinfo2();
	*in_use = m.uordblks + m.hblkhd;
	*free_bytes = m.fordblks;
#else
	*in_use = 0;
	*free_bytes = 0;
#endif
}
*/
import "C"

import (
	"runtime"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/metrics"
)

const (
	// arenas allows two threads to allocate at the same time. One arena kept the memory
	// lowest in a load test, two were close to it.
	arenas = 2
	// interval between two releases
	interval = time.Minute
)

// Supported tells whether the C library is glibc, the only one this package manages
func Supported() bool {
	return C.nm_supported() == 1
}

// Start limits the malloc arenas and releases the free memory every minute until stop is
// called. Call it before the first document is loaded, arenas that exist stay.
func Start() (stop func()) {
	if !Supported() {
		return func() {}
	}
	C.nm_limit_arenas(C.int(arenas))
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				Release()
			}
		}
	}()
	return func() { close(done) }
}

// Release returns the free memory of malloc to the system. The garbage collection lets the
// finalizers free the documents that are no longer used. They run in the background, so what
// they free now is returned with the next release.
func Release() {
	C.nm_trim()
	runtime.GC()
	var inUse, free C.size_t
	C.nm_info(&inUse, &free)
	metrics.NativeHeap.WithLabelValues(metrics.NativeHeapInUse).Set(float64(inUse))
	metrics.NativeHeap.WithLabelValues(metrics.NativeHeapFree).Set(float64(free))
}
