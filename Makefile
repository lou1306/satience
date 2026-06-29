.PHONY: all debug test clean

# Default target: build release binary
all: satience

# Release build with full optimizations and stripped debug code
# -s -w: Strip symbol table and DWARF debug info
# -trimpath: Remove file system paths for reproducible builds
# -tags release: Enable release mode (excludes verbose debug output)
# GOAMD64=v3: Enable AVX2/BMI2 optimizations for modern x86_64
satience: cmd/satience/main.go
	GOAMD64=v3 go build -tags release -ldflags="-s -w -buildid=" -trimpath -o satience ./cmd/satience

# Debug build with debug symbols and disabled optimizations
# -gcflags="all=-N -l": Disable optimizations and inlining for debugging
# -tags debug: Enable debug mode (includes verbose output, assertions)
debug:
	go build -tags debug -gcflags="all=-N -l" -o satience_debug ./cmd/satience

# Run tests
test:
	go test ./...

# Run tests with verbose output
test-verbose:
	go test -v ./...

# Run tests with race detector
test-race:
	go test -race ./...

# Benchmark
bench:
	go test -bench=. -benchmem ./...

# Clean build artifacts
clean:
	rm -f satience satience_debug satience_bench satience_prof
	rm -rf /tmp/*.prof

# Install to GOPATH/bin
install:
	GOAMD64=v3 go install ./cmd/satience

# Profile CPU (usage: make profile INSTANCE=path/to/file.cnf)
profile: satience
	./satience -cpuprofile=/tmp/profile.prof $(INSTANCE)
	go tool pprof -top /tmp/profile.prof

# Help
help:
	@echo "Satience SAT Solver - Makefile Targets"
	@echo ""
	@echo "  make          - Build optimized release binary (default)"
	@echo "                  Strips debug symbols, uses GOAMD64=v3 optimizations"
	@echo "  make debug    - Build debug binary with symbols and no optimizations"
	@echo "                  Enables verbose output and debug assertions"
	@echo "  make test     - Run all tests"
	@echo "  make test-verbose - Run tests with verbose output"
	@echo "  make test-race   - Run tests with race detector"
	@echo "  make bench    - Run benchmarks"
	@echo "  make clean    - Remove build artifacts"
	@echo "  make install  - Install to GOPATH/bin"
	@echo "  make profile INSTANCE=file.cnf - Profile CPU usage"
