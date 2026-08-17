#!/usr/bin/env bash
# Build all examples into bin/ (see README "构建可执行文件（bin/）").
set -euo pipefail
cd "$(dirname "$0")"

echo "Building examples to bin/ ..."
mkdir -p bin

go build -o bin/pty-cli ./examples/pty-cli
go build -o bin/pty-web ./examples/pty-web
go build -o bin/pty-web-signalr ./examples/pty-web-signalr
go build -o bin/pty-session ./examples/pty-session

# Windows + winpty: copy the winpty DLLs next to the executables so
# -winpty / ForceWinPty work when running the built binaries.
if [ -d ptygo/deps/winpty ]; then
    mkdir -p bin/deps/winpty
    cp ptygo/deps/winpty/* bin/deps/winpty/ 2>/dev/null || true
fi

echo
echo "Build OK. Binaries are in bin/."