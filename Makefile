# Teyru - native compiler written in Go.
#
# Common tasks. `make` alone builds the compiler into ./teyru.

GO      ?= go
CC      ?= clang
BIN     ?= teyru
OPT     ?= -O2
PREFIX  ?= /usr/local

.PHONY: all build test test-go test-programs submodule bench backend-matrix cache fmt vet lint notices unicode-tables check ci jdk-diff progen hooks clean install examples

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

## ci: everything a CI job would run, here, by hand
#
# GitHub Actions is switched off for this project -- the one workflow in
# .github/workflows runs when a release is published and calls these targets --
# so the jobs a CI would run live here instead. `ci` is W1's minimal matrix:
# the format and vet checks, the whole suite building with clang, and the whole
# suite again building with gcc. TEYRU_JDK adds the JDK differential, which
# needs a JDK 21 and is the only check here that compares against another
# implementation rather than against this repository.
#
# The two compiler legs are serialised (-p 1 -parallel 1) because each program
# in the suite is compiled and linked, and running sixteen of those at once on
# a shared machine is how a timing measurement becomes noise.
ci: lint
	$(GO) test ./... -count=1 -timeout 45m -p 1 -parallel 1
	@if command -v clang >/dev/null 2>&1 && command -v gcc >/dev/null 2>&1; then \
	  echo "== the suite again, building with gcc"; \
	  TEYRU_CC=gcc $(GO) test ./... -count=1 -timeout 45m -p 1 -parallel 1; \
	else \
	  echo "== only one C compiler is installed: the gcc leg of the matrix did not run"; \
	fi
	@if [ -n "$$TEYRU_JDK" ]; then \
	  echo "== the JDK differential"; \
	  $(GO) test -run TestJDKDiff -count=1 -v .; \
	else \
	  echo "== TEYRU_JDK is not set: the JDK differential did not run"; \
	fi

## java-compat: compile and run the unmodified-Java corpus
#
# tests/java-compat holds Java programs that javac compiles and that this
# compiler must compile as they stand, with the JDK's own output as the
# expectation. It is the subset the documentation's "Java source compiles
# unchanged" names. The release workflow calls this target (make ci runs the
# same cases again through `go test`, as TestJavaCompat), and it needs no JDK:
# the expectations are committed beside the programs.
java-compat: build submodule
	TEYRU=$(abspath $(BIN)) sh tests/run.sh java-compat

## jdk-diff: compile every translatable program with the JDK and compare
#
# The reference implementation is OpenJDK 21, and the comparison is stdout and
# exit status. A program the Java printer refuses is not a failure: it is a
# program the differential says nothing about, and the refusal is logged with
# its reason. tests/jdk-diff-allow.txt holds the differences that are decided,
# tests/known-failures.txt the ones a work item is going to remove.
jdk-diff:
	@test -n "$$TEYRU_JDK" || { \
	  echo "jdk-diff: set TEYRU_JDK to a JDK 21 home:"; \
	  echo "          TEYRU_JDK=/opt/jdk21/jdk-21.0.11+10 make jdk-diff"; exit 2; }
	$(GO) test -run TestJDKDiff -count=1 -v -timeout 45m .

## progen: the random-program differential, 200 seeds
#
# internal/tools/progen generates the common subset of the language in both
# spellings, compiles and runs both, and diffs them. The seeds are fixed so the
# run is reproducible; -seeds and -start take any number locally.
progen: build
	$(GO) run ./internal/tools/progen -teyru ./$(BIN) -seeds 200

## bench: compile and time the benchmark programs
bench: build
	./scripts/bench.sh

## backend-matrix: every program, in every cell of the back-end matrix
#
# { C back end + clang, C back end + gcc, LLVM back end } x { -O0, -O2 }, each
# cell compared with the suite's expectation and with the other cells. This is
# W8's matrix: the divergences it finds between the cells are what says whether
# the two back ends can be made to share one lowering. It builds its own
# compiler from this tree (BIN= names one to use instead), JOBS= sets how many
# programs are built at a time, and OUT= keeps the record somewhere you can
# look at it. A divergence is accounted for in
# scripts/backend-matrix-allow.txt; anything not listed there fails the target.
backend-matrix:
	./scripts/backend-matrix.sh

## unicode-tables: regenerate the Unicode tables the runtime reads
#
# The tables in internal/runtime/src/tyrt_unicode.c are generated from the
# Unicode 15.0.0 data files in internal/tools/genunicode/data, which are in the
# tree (no network, no JDK). `make check` does not run this -- it checks that the
# file is current instead, through the generator's own test, so that a tree that
# regenerated it without committing cannot land quietly.
unicode-tables:
	$(GO) run ./internal/tools/genunicode

## cache: the standard library cache, checked rather than assumed
#
# W13's cache answers builds with a standard library compiled once, so its
# failure mode is a wrong program rather than a slow build. This target is what
# the release workflow calls: scripts/cache-check.sh builds a hello world three
# ways and checks that the cache is filled, reused across programs, keyed by the
# optimisation level, that a removed object is rebuilt, that TEYRU_NOCACHE=1
# still builds, and that the split and whole-program builds run the same
# program. It takes about fifteen seconds and needs no network.
cache: build
	./scripts/cache-check.sh

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
