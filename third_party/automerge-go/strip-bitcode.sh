#!/usr/bin/env bash
# Removes the LLVM bitcode from the prebuilt automerge-core libraries of automerge-go, see
# README.md. Run it on the original libraries after replacing them (with an update of
# automerge-go). Needs llvm-objcopy.
set -euo pipefail
cd "$(dirname "$0")/deps"
for lib in libautomerge_core_*.a; do
	case "$lib" in
	*darwin*) llvm-objcopy --remove-section=__LLVM,__bitcode --remove-section=__LLVM,__cmdline "$lib" ;;
	*) llvm-objcopy --remove-section=.llvmbc --remove-section=.llvmcmd "$lib" ;;
	esac
	echo "stripped $lib"
done
