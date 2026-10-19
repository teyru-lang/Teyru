# Teyru

[繁體中文](README.md) · [简体中文](README.zh-CN.md) · **English** · [日本語](README.ja.md)

**Write in the shape of Java, compile to a native executable.**

No JVM, no bytecode, no JAR, no JDK, and the standard library is written in Teyru itself. The
compiler is written in Go and uses only the standard library.

```teyru
class Main {
  public static void main(String[] args) {
    System.out.println("hello, Teyru")
  }
}
```

```sh
go install github.com/teyru-lang/Teyru/cmd/teyru@latest
teyru run hello.teyru
```

## Two back ends

| | |
|---|---|
| `teyru build` (default) | Emits C and hands it to clang or gcc. The entire standard library is validated on this path |
| `teyru build --backend=llvm` | **Generates LLVM IR itself**; the program never passes through C, and clang only assembles and links. It currently covers a large part of the language, and the rest is **explicitly rejected** with `TY-INT-` diagnostics rather than falling back to the C back end. It compiles linux/amd64 only; other targets are rejected with `TY-INT-0101` |

## Platforms

"It compiles" and "it runs" are two different questions with two different kinds of evidence, so
they get two columns:

| Target | Build | Run |
|---|---|---|
| linux/amd64 | ✅ | ✅ native; the whole test suite runs here (`go test ./...`, `TEYRU=<compiler> sh tests/run.sh`) |
| windows/amd64 | ✅ cross-compiled with `x86_64-w64-mingw32-gcc`; **except programs that touch TLS**, which are named rejections (mingw-w64 has no OpenSSL) | ✅ runs under Wine; measured 2026-10-20 over the **318** programs in `tests/programs`: **302 produce byte-for-byte identical output (stdout, stderr, exit code)**, **11 touch TLS and cannot be linked (refused by name)**, **4 skip because Windows has no ucontext for virtual threads**, **1 (`t99_file_shell_meta`) is skipped because NTFS cannot hold that file name** — **0 mismatches**. Built at `-O1` and run with `TEYRU_RUNNER=wine` |
| linux/arm64 | ✅ needs a compiler pointed at an arm64 sysroot (`CC=aarch64-linux-gnu-gcc` plus `QEMU_LD_PREFIX`; the recipe is in `AGENTS.md` §10) | ✅ runs under qemu: the whole suite **passes 250 of 250 items, 0 mismatches** (222 programs + 3 packages + 23 diagnostics + 2 native), measured 2026-09-17 |
| darwin/amd64, darwin/arm64 | ✅ with `--cc <zig cc -target <arch>-macos>` (`resolveTarget` now looks at the compiler that will actually be used this time) | ❌ there is no macOS here: **not a single line of any program has ever run on it**, it goes only as far as "compiles and links" |

The build column was measured on eight representative programs — sealed switch (`t84_sealed_switch`),
arrow blocks (`t133_arrow_blocks`), reflection (`t146_reflect`), threads (`t159_threads`), time zones
(`t188_timezone`), the Spring-shaped layer (`t102_web`), TLS (`t163_https_roundtrip`) and Gson
(`t101_gson`) — each built for four reachable targets with `teyru build --no-lto --target <os>/<arch>`.
**The evidence for each row of the table above is what was actually measured in that run** (the full
test suite on linux/amd64, the full suite under qemu on arm64, 290 of the 302 programs under Wine on
windows), not a ratio from a cross-compilation probe; the "12 successes out of 40" written here
earlier was an old probe from before the arm64 sysroot and the macOS SDK were ready, and the two are
not the same thing — once arm64 was measured it should have been deleted rather than kept as the
current state.

**All four targets compile, but this machine can run only three of them.** Cross-compiling to macOS
needs an SDK, and it is not on this machine; the macOS cell means "it compiles, **nobody has ever run
it**", not "it compiles but is untested". What has actually been verified is three: linux/amd64 runs
the whole test suite natively, linux/arm64 runs the whole suite under qemu (with an arm64 sysroot
installed on top), and windows/amd64 runs under Wine (the 302 programs above).

At runtime, platform dependencies are collected behind `internal/runtime/src/tyrt_plat.h` (one copy
for POSIX, one for Windows), and `--target <os>/<arch>` picks the compiler, the flags and the output
file name. TLS is the only part of the runtime that links an external library (OpenSSL): it lives in
its own file, it is compiled and linked only when the program's **reachable code** can touch it, and
targets without OpenSSL (windows, macOS) make it a **named rejection** — before any output file is
written, not a link-stage failure. "Reachable" means **where the program's own code can get to**;
reflection tables do not count: the reflection path carries a marker in the generated C, and
reachability is analysed twice, once following reflection references (that is the pass vtable
trimming needs) and once without (the TLS link decision reads the latter). So a program that only
reflects and never touches TLS does **not** link OpenSSL — `tests/programs/t241_reflect_tls_unlinked`
and `TestTLSReachability` in `link_test.go` guard this cell; when reflection really does call a TLS
method at runtime, what it gets is a named failure from the runtime weak stub, not a jump to NULL.

There is currently **no CI** — no GitHub Actions at all: every row in this table was produced on this
one linux/amd64 machine, so "it compiles" and "it runs" are two questions here.

## Standard library

* **Collections and utilities**: `List`/`Set`/`Map`/`Deque` and their implementations, `Arrays`,
  `Collections`, `Comparator`, `Optional`, `UUID`, `StringBuilder`, `Stream`, `java.util.function`.
* **Text and time**: the full `String` interface, `String.format`, `java.util.regex`, the
  `LocalDate` family and **time zones** (`ZoneId`/`ZoneOffset`/`ZoneRules`/`ZonedDateTime`, reading
  the host's own IANA tzdata, pure Teyru; a host without tzdata is a named rejection), `Scanner`,
  `java.io` buffered and binary streams (including Java's modified UTF-8), `HexFormat`.
* **JSON**: `JsonElement` trees, parsing and output, plus Gson-shaped binding — `Gson`, `GsonBuilder`
  (`serializeNulls`/`setPrettyPrinting`/`setLenient`/field naming policies/exclusion modifiers),
  `@Expose`/`@Since`/`@Until`/`@SerializedName`/`@JsonAdapter`,
  `JsonSerializer`/`JsonDeserializer`, streaming `JsonReader`/`JsonWriter`. Containers bind objects
  by **declared element type**, arrays included.
* **Web**: HTTP/1.1 server (keep-alive, chunked, cookies, forms, multipart, gzip, and TLS through
  `ssl(cert, key)`), WebSocket (RFC 6455), HTTP client (including `https://`), and a Spring
  Boot-shaped container — `@RestController`, the `@RequestMapping` family,
  `@RequestBody`/`@PathVariable`/`@RequestParam`, `@ConfigurationProperties`, `@Profile`,
  `@PreDestroy`, `@ControllerAdvice` + `@ExceptionHandler`, `HandlerInterceptor`, static files,
  CORS, `ResponseEntity`, `MockServer`, sessions and timeouts, validation annotations.
* **Concurrency**: real OS threads (`Thread`, `Runnable`, real `synchronized`,
  `Object.wait`/`notify`, `interrupt`, daemons, `ThreadLocal`), **virtual threads**
  (`Thread.startVirtualThread` / `Thread.ofVirtual()`, M:N on ucontext; refused by name on
  Windows), and the shape of `java.util.concurrent` — `Executors`,
  `ExecutorService`, `Future`, `Callable`, `CountDownLatch`, `AtomicInteger`/`AtomicLong`,
  `ConcurrentHashMap`.
* **Other**: `MessageDigest` (MD5, SHA-1/224/256/384/512), `CRC32`, `Adler32`, deflate/inflate and
  gzip, `BigInteger`/`BigDecimal`, reflection (the shape of `java.lang.reflect`, including annotation
  metadata), `java.util.logging`-shaped logging.

## Tooling

Four tools live inside the same `teyru`, not in plugins:

| Tool | What it does |
|---|---|
| `teyru fmt [-w]` | Formats in the canonical style; idempotent, comments preserved byte for byte, same program output before and after |
| `teyru lsp` | A language server on stdin/stdout: `TY-*` diagnostics and same-file go-to-definition |
| `teyru test` | `@Test` / `@BeforeEach` / `Assert`; exit code 0/1/2; the package's own `main` is not called |
| `teyru build -g` | The generated C carries `#line` and DWARF points back at the `.teyru`; without `-g` the output is byte-identical |

CI runs `go build`, `go vet` and the whole `go test` suite on every push and pull request, and the
`CC=gcc` leg once a day. Details in <https://docs.teyru.dev/en/docs/tooling>.

## Measurements

The full table is at <https://docs.teyru.dev>; the method is `RUNS=5 sh scripts/bench.sh` (best of
five, **whole-program wall clock including process startup**, `-O2`), both sides get **the same
scale parameter**, and the comparison is against HotSpot (OpenJDK 21, default flags) on the same
machine. The script prints which tree it measured, on which machine, and the load averages on
entering and leaving each table. This is one run from **2026-10-20** on `nobara-pc` (Ryzen 7 5700X,
16 threads, clang 22.1.8) with the compiler at `6cb64d6`: 1-minute load 3.33 entering, 2.97 leaving.

**Short scale** (each program's own default; the JVM's startup is most of this number, so it is a
statement about startup and small programs, **not throughput**):

| Program | Teyru | Java | Teyru/Java |
|---|---|---|---|
| `bench_alloc` | 0.1920 s | 0.0327 s | 0.17x the time |
| `bench_escape` | 0.4571 s | 0.1070 s | 0.23x |
| `bench_fib` | 0.0080 s | 0.0288 s | **3.60x faster** |
| `bench_invoke` | 1.7531 s | 0.2592 s | 0.15x |
| `bench_loop` | 0.0431 s | 0.0474 s | 1.10x faster |
| `bench_oop` | 0.0146 s | 0.0288 s | 1.97x faster |
| `bench_string` | 0.0120 s | 0.0569 s | **4.74x faster** |
| `bench_string_cjk` | 0.0146 s | 0.0806 s | **5.52x faster** |

**Long scale** (every row at least a few hundred milliseconds; the JVM is warm, so the answer is
about the program):

| Long scale | Teyru | Java | Who wins | Peak RSS (Teyru) |
|---|---|---|---|---|
| `bench_alloc` (800M allocations) | 7.6013 s | 0.2054 s | **Java 37x** (below) | 4,240 kB |
| `bench_escape` | 0.9186 s | 0.1539 s | **Java 6.0x** | 8,604 kB |
| `bench_fib` (fib(44)) | 1.8511 s | 1.8044 s | a tie (Java 2.6% faster) | 4,292 kB |
| `bench_invoke` (200M reflective calls) | 17.6147 s | 1.8807 s | **Java 9.4x** | 8,664 kB |
| `bench_loop` (1M rounds) | 2.0201 s | 1.0075 s | **Java 2.0x** | 4,292 kB |
| `bench_oop` (500k rounds) | 3.0662 s | 0.3657 s | **Java 8.4x** | 4,364 kB |
| `bench_string` (16M concatenations) | 0.5242 s | 0.3826 s | **Java 1.4x** | 8,600 kB |
| `bench_string_cjk` (4M) | 0.1783 s | 0.1610 s | Java 1.1x | 8,856 kB |

**On long runs Teyru wins no column; `bench_fib` is the only tie.** The short scale says the
opposite because most of that number is JVM startup (about 20 ms against 0.75 ms on this machine) —
a difference in startup, not in throughput.

**This version's long-run numbers are much worse than the 2026-09-17 set, and the reason is a
correctness trade that was made on purpose.** That set (`130565a`) was `bench_alloc` 1.398 s,
`bench_oop` 1.002 s, `bench_invoke` 4.066 s, `bench_loop` 1.134 s; today they are 7.60, 3.07, 17.61
and 2.02. The difference is precise GC roots: every function that can allocate registers its frame
with the collector (a frame map), and **a function with a frame cannot be inlined** — its frame
address is part of the map, and an inlined copy would describe somebody else's frame.
`bench_alloc`'s inner loop calls `new Cell(i)` and `Cell.value()` every iteration; those two small
methods used to be inlined by clang and are now real calls. `TEYRU_NO_FRAME_MAPS=1` (a measurement
switch, not a shippable setting) measures 1.39 s on the same tree, which is the version without the
map. What has been fixed: pointers to C primitives and pointers to objects laid out in the frame are
no longer registered, which took `bench_loop` from 2.91 s back to 2.02 s. The rest is the next
performance item, and `AGENTS.md` section 10 says how to measure it.

**`bench_string` got faster instead**: 0.750 s to 0.524 s. A concatenation chain now computes the
total length once and fills one buffer (`ty_str_concatN`) instead of allocating an intermediate
string per `+`; `bench_string_cjk` went from 0.30 s to 0.178 s on the same change.

**The `bench_alloc` row has nothing to do with the collector.** `TEYRU_GCTRACE=1` reports **zero
collections** at 800M allocations: escape analysis keeps `Cell` on the stack, so that row measures
calls and the loop, not the allocator and not the GC.

**The three rows that are not programs**: startup over 100 runs 0.0744 s against 2.0684 s
(**27.8x**); the hello executable **105,416 bytes** (`-O2`); peak RSS 4,400 kB against 51,436 kB
(**11.7x less**). The memory row takes the **largest** of five runs, not the smallest: the smallest
reflects a run that never touched the heap rather than a program that needs less.

## Install and build

```sh
go install github.com/teyru-lang/Teyru/cmd/teyru@latest     # or from source:
git clone --recurse-submodules https://github.com/teyru-lang/Teyru
cd Teyru
make build     # 產生 ./teyru
make test      # 單元 + 端到端（需要 clang 或 gcc）
```

`tests/` is a submodule, and **it runs on its own now**: `TEYRU=<compiler> sh tests/run.sh` needs
neither Go nor the compiler source tree. If you forget `--recurse-submodules`, `make test` says so
outright instead of quietly running zero tests.

## Documentation

The full documentation is at **<https://docs.teyru.dev>**: language reference, diagnostic codes,
Lombok compatibility layer, JSON binding, web framework, module system, native interop,
architecture. The sources are in
[`teyru-lang/docs`](https://github.com/teyru-lang/docs) (fumadocs, three locales: Traditional Chinese
is the default, plus Simplified Chinese and English; page content must be consistent across the three
locales). This README itself also comes in four languages: Traditional Chinese (the canonical file),
[简体中文](README.zh-CN.md), [English](README.en.md) and [日本語](README.ja.md); when the canonical
changes, the other three must be brought in sync, copying numbers, paths and code blocks verbatim and
translating only the prose.

The language statistics on GitHub have been corrected by `.gitattributes` as well: `*.teyru` is
declared as Teyru, and `*.java.ref` (that is the spec — `.expected` is generated from javac's output)
and `*.expected` (test data) are marked as not counted. Before that, "Java" was once the largest
language in this repository, and it barely exists. To be clear, the `linguist-language` line **will
not make Teyru appear**: Linguist only counts languages it recognises, so Teyru will show up in the
statistics only after the language itself and the grammar in `teyru-lang/editors` are accepted
upstream.

## Other repositories in this organisation

| Repository | Contents |
|---|---|
| [`teyru-lang/tests`](https://github.com/teyru-lang/tests) | End-to-end test data, mounted in this repository as the `tests/` submodule; `run.sh` lets it run on its own |
| [`teyru-lang/docs`](https://github.com/teyru-lang/docs) | Documentation site (<https://docs.teyru.dev>) |
| [`teyru-lang/editors`](https://github.com/teyru-lang/editors) | VS Code/Zed/JetBrains IDEA extensions, tree-sitter grammar |
| [`teyru-lang/website`](https://github.com/teyru-lang/website) | Website sources (<https://teyru.dev>) |

Read [`AGENTS.md`](AGENTS.md) before contributing: code style, testing rules, documentation rules and
the pre-submit checklist are all there. `AGENTS.md` §10 is the **known limitations** section,
including what is not implemented yet and what is still regressing — that is where to look for an
honest status.

Licensing is in [LICENSE](LICENSE) and [THIRD-PARTY-NOTICES.md](THIRD-PARTY-NOTICES.md).
