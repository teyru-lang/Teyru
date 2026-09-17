# Teyru - native compiler written in Go.
#
# Common tasks. `make` alone builds the compiler into ./teyru.

GO      ?= go
CC      ?= clang
BIN     ?= teyru
OPT     ?= -O2
PREFIX  ?= /usr/local

.PHONY: all build test test-go test-programs submodule bench fmt vet lint notices check hooks clean install examples

all: build

## build: compile the compiler
build:
	$(GO) build -trimpath -o $(BIN) ./cmd/teyru

## test: everything (unit + end-to-end + diagnostics)
#
# The deadline is raised because the suite compiles and links every program in
# tests/programs, and Go's default ten minutes stopped being enough once the
# suite passed a hundred and fifty of them: a package that hits the deadline is
# reported as a failure with no test named, which reads like a real one.
test: submodule
	$(GO) test ./... -count=1 -timeout 30m

## submodule: the end-to-end suite lives in teyru-lang/tests, mounted at tests/
submodule:
	@ls tests/programs/*.teyru >/dev/null 2>&1 || { \
	  echo "tests/ is empty -- the suite is the teyru-lang/tests repository."; \
	  echo "run: git submodule update --init"; exit 1; }

## test-go: compiler unit tests only
test-go:
	$(GO) test ./internal/... -count=1

## test-programs: compile and run every program under tests/programs
test-programs: build submodule
	@fail=0; total=0; \
	for f in tests/programs/*.teyru; do \
	  b=$$(basename $$f .teyru); \
	  [ -f tests/programs/$$b.expected ] || continue; \
	  total=$$((total+1)); \
	  args=""; [ -f tests/programs/$$b.args ] && args="-- $$(cat tests/programs/$$b.args)"; \
	  got=$$(./$(BIN) run $$f $$args 2>&1); \
	  want=$$(cat tests/programs/$$b.expected); \
	  if [ "$$got" != "$$want" ]; then echo "FAIL $$b"; fail=$$((fail+1)); fi; \
	done; \
	echo "$$((total-fail))/$$total programs pass"; \
	[ $$fail -eq 0 ]

## examples: run the example programs
examples: build
	@for f in examples/*.teyru; do echo "== $$f"; ./$(BIN) run $$f; done

## bench: compile and time the benchmark programs
bench: build
	./scripts/bench.sh

## notices: check THIRD-PARTY-NOTICES.md against the tree
#
# Fails when the notices and the repository disagree: the runtime file list, the
# OpenSSL version floor, the Windows target's link flags, where the time zone
# data comes from, the licence files, and the Unicode data files once they are
# in the tree. `scripts/check-notices.sh --write` regenerates the file list.
notices:
	./scripts/check-notices.sh

## fmt: format Go sources
fmt:
	$(GO) fmt ./...

## vet: static checks
vet:
	$(GO) vet ./...

## lint: vet plus gofmt diff check
lint: vet
	@out=$$(gofmt -l . | grep -v '^$$' || true); \
	if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

## check: what to run before every commit (fast, no network)
check: lint notices test
	@echo "check: ok"

## hooks: install the pre-commit hook that runs `make check`
hooks:
	@mkdir -p .git/hooks
	@cp scripts/pre-commit .git/hooks/pre-commit
	@chmod +x .git/hooks/pre-commit
	@echo "pre-commit hook installed"

## install: copy the compiler to $(PREFIX)/bin
install: build
	install -d $(PREFIX)/bin
	install -m 0755 $(BIN) $(PREFIX)/bin/$(BIN)

## clean: remove build outputs
clean:
	rm -f $(BIN)
	$(GO) clean -cache -testcache 2>/dev/null || true
