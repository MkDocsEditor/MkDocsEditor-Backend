# automerge-go, patched

A copy of [automerge-go](https://github.com/automerge/automerge-go) at
`v0.0.0-20241030180337-6fb4f2d08244` (Automerge core automerge-rs 0.5.0), used through a
`replace` in the backend's `go.mod`. The Go and C sources are unchanged, tests and tools are
left out.

## The patch

The prebuilt static libraries in `deps/` have the LLVM bitcode removed (`.llvmbc` and
`.llvmcmd` in the ELF libraries, `__LLVM,__bitcode` and `__LLVM,__cmdline` in the Mach-O
ones), with `strip-bitcode.sh`. Nothing else changed.

Why: since Go 1.27.4 (and 1.26.10), `go build` refuses cgo linker inputs that contain LTO
sections (golang/go#82572, "disallowed LTO section" in #cgo LDFLAGS). The bitcode is a second
copy of the code in LLVM's intermediate format that only link-time optimization reads. The
backend does not link with LTO, so the linker never used it: the backend binary is
byte-identical with the original and the stripped libraries (checked for linux/amd64, both
the dynamic build and the static build of the Docker image).

## Why not upstream

automerge-go has had no change since October 2024 and has no fix for this. The backend stays
on Automerge core 0.5.0 (Automerge 2) on purpose: Automerge 3 was a lot slower in the
backend. See docs/sync-protocol.md section 1.

## Updating

Replace the files with those of the new automerge-go version, run `./strip-bitcode.sh`, and
check that `go vet ./...` and the tests pass with the current Go release.
