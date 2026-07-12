.PHONY: all satience fuzz debug test test-verbose test-race vet lint bench clean install profile help

# Default target: build release binaries
all: satience fuzz

# --- Build targets ---

# Release build with full optimizations and stripped debug code
# -s -w: Strip symbol table and DWARF debug info
# -trimpath: Remove file system paths for reproducible builds
# -tags release: Enable release mode (excludes verbose debug output)
# GOAMD64=v3: Enable AVX2/BMI2 optimizations for modern x86_64
satience:
	GOAMD64=v3 go build -tags release -ldflags="-s -w -buildid=" -trimpath -o satience ./cmd/satience

# Fuzzer binary (release build)
fuzz:
	GOAMD64=v3 go build -tags release -ldflags="-s -w -buildid=" -trimpath -o fuzz ./cmd/fuzz

# Debug build with debug symbols and disabled optimizations
# -gcflags="all=-N -l": Disable optimizations and inlining for debugging
# -tags debug: Enable debug mode (includes verbose output, assertions)
debug:
	go build -tags debug -gcflags="all=-N -l" -o satience_debug ./cmd/satience

# --- Test / lint targets ---

# Run all tests
test:
	go test ./...

# Run tests with verbose output
test-verbose:
	go test -v ./...

# Run tests with race detector
test-race:
	go test -race ./...

# Run go vet (static analysis)
vet:
	go vet ./...

# Alias for vet
lint: vet

# Run benchmarks
bench:
	go test -bench=. -benchmem ./...

# --- Misc targets ---

# Clean build artifacts
clean:
	rm -f satience satience_debug satience_bench satience_prof
	rm -f satience_baseline satience_new satience_old satience_stash
	rm -f fuzz solver.test

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
	@echo "Build:"
	@echo "  make              - Build release binaries (satience + fuzz)"
	@echo "  make satience     - Build optimized release solver"
	@echo "  make fuzz         - Build fuzzer binary"
	@echo "  make debug        - Build debug binary (symbols, no opts)"
	@echo ""
	@echo "Test / Lint:"
	@echo "  make test         - Run all tests"
	@echo "  make test-verbose - Run tests with verbose output"
	@echo "  make test-race    - Run tests with race detector"
	@echo "  make vet          - Run go vet (static analysis)"
	@echo "  make lint         - Alias for vet"
	@echo "  make bench        - Run benchmarks"
	@echo ""
	@echo "Misc:"
	@echo "  make clean        - Remove build artifacts"
	@echo "  make install      - Install satience to GOPATH/bin"
	@echo "  make profile INSTANCE=file.cnf - Profile CPU usage"
