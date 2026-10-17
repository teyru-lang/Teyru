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
| windows/amd64 | ✅ cross-compiled with `x86_64-w64-mingw32-gcc`; **except programs that touch TLS**, which are named rejections (mingw-w64 has no OpenSSL) | ✅ runs under Wine; measured on 2026-10-17 over the **302** programs in `tests/programs`: **290 produce byte-for-byte identical output (stdout, stderr, exit code)**, **11 touch TLS and are rejected by name**, **1 (`t99_file_shell_meta`) is skipped because NTFS cannot hold that file name** — **0 mismatches**. The whole set was built with `-O0` (302 programs in about 3 minutes); with `-O1`, mingw's `cc1` takes more than two hours on the largest programs, which is toolchain build time, not a behavioural difference |
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
  `Object.wait`/`notify`), and the shape of `java.util.concurrent` — `Executors`,
  `ExecutorService`, `Future`, `Callable`, `CountDownLatch`, `AtomicInteger`/`AtomicLong`,
  `ConcurrentHashMap`.
* **Other**: `MessageDigest` (MD5, SHA-1/224/256/384/512), `CRC32`, `Adler32`, deflate/inflate and
  gzip, `BigInteger`/`BigDecimal`, reflection (the shape of `java.lang.reflect`, including annotation
  metadata), `java.util.logging`-shaped logging.

## Measurements

The full table is at <https://docs.teyru.dev>; the method is `RUNS=5 sh scripts/bench.sh` (best of
five, **wall clock of the whole program, including process startup**, `-O2`), both sides receive
**the same size parameter**, and the comparison is against HotSpot (OpenJDK 21, default flags) on the
same machine. The script prints which tree it measured, on which machine, and the 1/5/15-minute load
averages on entering and on leaving each of the two tables — this machine is shared, and nobody can
tell whether a wall-clock table with no load figures was measured while the machine was busy. Below
is one run from 2026-09-17 on `nobara-pc` (Ryzen 7 5700X, 16 threads, clang 22.1.8), with the
compiler taken from `130565a`: 1-minute load 2.73 on entering the two tables and 2.79 on leaving, and
all 13 samples taken during the run below 1. For rows where the two sides print different answers,
the ratio column is `!(out)` rather than a number — a ratio in such a row is not comparing two
implementations.

**The script prints two tables because one number cannot answer two questions.** Short scale is each
program's own default size (rows from 6 to 410 ms), and JVM startup takes most of that number: it
describes startup and small programs, **not throughput**. What the README used to say —
"`bench_fib`/`bench_oop`/`bench_string` are 3.5 to 5 times faster" — is that set (**short scale**,
measuring 3.51/4.27/3.81 times today), and reading short scale as throughput is exactly that
mistake. Long scale enlarges the same program to **at least a few hundred milliseconds per row**
(five rows over 1 second, the two string rows between 0.3 and 0.8 seconds); with the JVM warmed up,
the answer is the program's:

| Long scale (at least a few hundred ms per row) | Teyru | Java | Faster |
|---|---|---|---|
| `bench_fib` (fib(44)) | 1.877 s | 1.800 s | Tie (Java 4% faster) |
| `bench_loop` (1 million rounds) | 1.134 s | 1.007 s | Java 1.13 times |
| `bench_oop` (500 thousand rounds) | 1.002 s | 0.357 s | **Java 2.8 times** |
| `bench_string` (16 million concatenations, 1600 × 10,000) | 0.750 s | 0.285 s | **Java 2.6 times** |
| `bench_string_cjk` (4 million, see below) | 0.30 s | 0.15 s | **Java 2.0 times** |
| `bench_alloc` (800 million allocations) | 1.398 s | 0.204 s | **Java 6.8 times** |
| `bench_invoke` (200 million reflective calls) | 4.066 s | 1.859 s | **Java 2.2 times** |

**No row at long scale is won by Teyru; `bench_fib` is the only tie.** The three rows it won at short
scale become a tie, 2.8 times slower and 2.6 times slower at long scale: the same two
implementations, only with the work scaled up, and the gap reverses.

**The `bench_alloc` row has nothing to do with the collector.** `TEYRU_GCTRACE=1` measured **zero
collections** across 800 million allocations: the compiler's escape analysis keeps `Cell` on the
stack and then promotes its fields to registers (`AGENTS.md` §10), so this row measures the loop
itself, not the allocator — which means its 6.8 times slowdown is not something "slow GC" can
explain.

**`bench_invoke` is 2.2 times slower, and the expensive part is allocation, not reflection.** The
short-scale version (20 million calls, 2000 × 10,000) is 1.63 times slower (0.4094 s against
0.2517 s). Broken down, Teyru's own **direct call** loop is already 2.6 times slower than Java
(26 ms against 10 ms for 20 million calls), and reflection adds mechanism cost on top; each
reflective call is 19 ns (the program itself prints 3.798 s for 200 million, and the 4.066 s in the
table is that plus the direct-call loop). The elimination breakdown in `AGENTS.md` §10 measured the
expensive part as the two allocations the `Object` API forces (argument boxing at the call site,
result boxing inside the runtime invoker thunk), and Java pays those two allocations as well — the
difference is the allocator and the object size (Teyru's `new Object()` is 32 bytes, Java's is 16).
**That breakdown was measured on the old overall cost (27 ns per call, 540 to 601 ms for 20M
calls); this run is 19 ns, and the breakdown itself has not been re-measured.**

**The `bench_string_cjk` row is a comparison now, and it used not to be.** Back then `String` here
was still UTF-8 bytes and `charAt` read a byte rather than a character, so the two sides took the
same size parameter and did **different work** — at 4 million (400 × 10,000), Teyru printed
`walked=0` and Java printed `walked=1000` — so the script printed `!(out)` for it instead of a
ratio (a table comparing two implementations cannot have a row comparing two different things).
After string semantics (W5) landed, both sides print **the same line**
(`concat=88890 walked=1000 sliced=4`, byte-for-byte identical at both the 10,000 and the 4,000,000
sizes), so it is a ratio row: **Teyru 0.30 s, Java 0.15 s, Java 2.0 times faster** (local
linux/amd64, load 0.77, best of five, 2026-10-16). This row is one of the most expensive in the set,
because every index needs a UTF-16 to WTF-8 conversion — and that is precisely why it exists.

**The three rows that are not programs**: startup, 100 runs, 0.0763 s against 2.1093 s (**27.6
times**); the hello executable **66,808 bytes** (`-O2`; the same program at `-O1` is 85,440 and at
`-O3` is 70,248; this number moves on almost every landing, so it belongs to the tree above —
**re-measured on 2026-10-16 on today's tree it is 97,888 bytes**, with `-O0` 233,680, `-O1` 209,472
and `-O3` 106,032, the difference being the design cost of the frame maps plus a since-fixed
`CharSequence` regression, see the size section of <https://docs.teyru.dev>); peak memory 4,348 kB
against 51,420 kB (**11.8 times less**). The memory row takes the **largest** of five runs, not the
smallest: the smallest reflects a round that never touched the heap into memory, not a program that
needs less (of twenty runs of the same hello, one measured 2,116 kB and the rest sat between 4,150
and 4,450).

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
