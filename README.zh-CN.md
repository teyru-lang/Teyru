# Teyru

[繁體中文](README.md) · **简体中文** · [English](README.en.md) · [日本語](README.ja.md)

**以 Java 的形状写，编成原生可执行文件。**

没有 JVM、没有 bytecode、没有 JAR、没有 JDK，标准库用 Teyru 自己写。编译器用 Go
写，只用标准库。

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

## 两个后端

| | |
|---|---|
| `teyru build`（默认） | 生成 C，交给 clang 或 gcc。整个标准库都在这条路上验证 |
| `teyru build --backend=llvm` | **自己生成 LLVM IR**，程序不经过 C；clang 只负责汇编与链接。目前覆盖语言的一大部分，其余用 `TY-INT-` 诊断**明确拒绝**，不会退回 C 后端；它只编 linux/amd64，其他目标以 `TY-INT-0101` 拒绝 |

## 平台

「编得出来」与「跑得起来」是两个问题，证据也不一样，所以分两栏：

| 目标 | 构建 | 执行 |
|---|---|---|
| linux/amd64 | ✅ | ✅ 原生，整套测试都在这里跑（`go test ./...`、`TEYRU=<编译器> sh tests/run.sh`） |
| windows/amd64 | ✅ 以 `x86_64-w64-mingw32-gcc` 交叉编译；**会碰到 TLS 的程序除外**（mingw-w64 没有 OpenSSL），那些是具名拒绝 | ✅ 在 Wine 下运行，2026-10-20 测 `tests/programs` 的 **318 支**：**302 支输出（stdout、stderr、退出码）逐字节相同**、**11 支碰到 TLS 而无法链接（具名拒绝）**、**4 支虚拟线程因为 Windows 没有 ucontext 而跳过**、**1 支（`t99_file_shell_meta`）因 NTFS 不能有那种文件名而跳过**——**0 支不符**。整套以 `-O1` 构建、以 `TEYRU_RUNNER=wine` 运行 |
| linux/arm64 | ✅ 需要一个指向 arm64 sysroot 的编译器（`CC=aarch64-linux-gnu-gcc` ＋ `QEMU_LD_PREFIX`，做法写在 `AGENTS.md` §10） | ✅ 在 qemu 下跑：整套 **250 项全过、0 项不符**（222 个程序＋3 个包＋23 个诊断＋2 个原生），测量于 2026-09-17 |
| darwin/amd64、darwin/arm64 | ✅ 以 `--cc <zig cc -target <arch>-macos>`（`resolveTarget` 现在看的是这次真的会用的那个编译器） | ❌ 这里没有 macOS：**没有任何一行程序在它上面执行过**，只到「编译并链接」 |

构建那一栏测的是八个代表性程序——sealed switch（`t84_sealed_switch`）、arrow blocks
（`t133_arrow_blocks`）、反射（`t146_reflect`）、线程（`t159_threads`）、时区
（`t188_timezone`）、Spring 形状的那一层（`t102_web`）、TLS（`t163_https_roundtrip`）与
Gson（`t101_gson`）——各以 `teyru build --no-lto --target <os>/<arch>` 编四个可达的目标。
**上表每一列的证据是那一次真正测到的东西**（linux/amd64 整套测试、arm64 在 qemu 下整套、
windows 在 Wine 下 302 个里的 290 个），不是一次跨编译探针的比例；先前这里写过的「40 次里 12 次成功」
是 arm64 sysroot 与 macOS SDK 都还没准备好时的旧探针，两件事都不是同一件事，测到 arm64
之后它就应当被删掉而不是留着当现状。

**四个目标都能编译，但这台机器只能执行其中三个。** 交叉编译到 macOS 需要一份 SDK，它不在这台
机器上；macOS 那一格是「编得出来、**没有任何人跑过**」，不是「编得出来但没测」。真正被验证过
的是三个：linux/amd64 原生跑整套测试、linux/arm64 在 qemu 下跑整套（需要另外装上去的 arm64
sysroot）、windows/amd64 在 Wine 下跑（上面那 302 个）。

运行时把平台相关的东西集中在 `internal/runtime/src/tyrt_plat.h` 后面（POSIX 与 Windows 各一份），
`--target <os>/<arch>` 选择编译器、标志与输出文件名。TLS 是运行时唯一链接外部库
（OpenSSL）的部分：它自己是一个文件，只有程序的**可达代码**能碰到它时才编译与链接，而没有
OpenSSL 的目标（windows、macOS）对它是**指名拒绝**——在写出任何输出文件之前，不是链接阶段
失败。「可达」指的是**程序自己的代码到得了哪里**，反射的表格不算：反射那条路径在生成的 C
里带一个标记，可达性分析跑两次，一次跟着反射的引用（vtable 裁剪要的就是那一次），一次不跟
（TLS 的链接决策读的是后者）。所以一个只会反射、不碰 TLS 的程序**不连** OpenSSL——
`tests/programs/t241_reflect_tls_unlinked` 与 `link_test.go` 的 `TestTLSReachability`
盯着这一格；反射真的在运行时调用到 TLS 方法时，拿到的是运行时 weak stub 的指名失败，不是
跳到 NULL。

CI 在每次推送到 `main` 与每个 PR 上运行 `go build`、`go vet` 与整套 `go test`（`.github/workflows/ci.yml`），
`CC=gcc` 那条腿每天运行一次（`nightly.yml`）。这份表上的每一列仍然是在这一台 linux/amd64 机器上
跑出来的：CI 守的是「不会悄悄退步」，不是「每个平台都测过」。

## 标准库

* **集合与工具**：`List`／`Set`／`Map`／`Deque` 与其实现、`Arrays`、`Collections`、
  `Comparator`、`Optional`、`UUID`、`StringBuilder`、`Stream`、`java.util.function`。
* **文字与时间**：`String` 的完整接口、`String.format`、`java.util.regex`、
  `LocalDate` 家族与**时区**（`ZoneId`／`ZoneOffset`／`ZoneRules`／`ZonedDateTime`，
  读主机自己的 IANA tzdata，纯 Teyru；没有 tzdata 的主机是具名拒绝）、
  `Scanner`、`java.io` 的缓冲与二进制数据流（含 Java 的 modified UTF-8）、`HexFormat`。
* **JSON**：`JsonElement` 树、解析与输出，以及 Gson 形状的绑定——`Gson`、`GsonBuilder`
  （`serializeNulls`／`setPrettyPrinting`／`setLenient`／字段命名策略／排除修饰符）、
  `@Expose`／`@Since`／`@Until`／`@SerializedName`／`@JsonAdapter`、
  `JsonSerializer`／`JsonDeserializer`、流式 `JsonReader`／`JsonWriter`。容器会依**声明的
  元素类型**绑定对象，包含数组。
* **Web**：HTTP/1.1 服务器（keep-alive、chunked、cookie、表单、multipart、gzip，
  以及 `ssl(cert, key)` 的 TLS）、WebSocket（RFC 6455）、HTTP 客户端（含 `https://`），
  以及 Spring Boot 形状的容器——`@RestController`、
  `@RequestMapping` 家族、`@RequestBody`／`@PathVariable`／`@RequestParam`、
  `@ConfigurationProperties`、`@Profile`、`@PreDestroy`、`@ControllerAdvice` ＋
  `@ExceptionHandler`、`HandlerInterceptor`、静态文件、CORS、`ResponseEntity`、
  `MockServer`、会话与超时、校验注解。
* **并发**：真正的操作系统线程（`Thread`、`Runnable`、真正的 `synchronized`、
  `Object.wait`／`notify`、`interrupt`、daemon、`ThreadLocal`），**虚拟线程**
  （`Thread.startVirtualThread`／`Thread.ofVirtual()`，ucontext 上的 M:N；Windows
  以具名异常拒绝），以及 `java.util.concurrent` 的形状——`Executors`、
  `ExecutorService`、`Future`、`Callable`、`CountDownLatch`、`AtomicInteger`／
  `AtomicLong`、`ConcurrentHashMap`。
* **其他**：`MessageDigest`（MD5、SHA-1／224／256／384／512）、`CRC32`、`Adler32`、
  deflate／inflate 与 gzip、`BigInteger`／`BigDecimal`、反射（`java.lang.reflect` 的形状，
  含注解的元数据）、`java.util.logging` 形状的日志。

## 工具链

四个工具在同一个 `teyru` 里，不是插件：

| 工具 | 做什么 |
|---|---|
| `teyru fmt [-w]` | 固定风格格式化；幂等，注释逐字保留，格式化前后输出相同 |
| `teyru lsp` | stdin/stdout 上的语言服务器：`TY-*` 诊断与同文件定义跳转 |
| `teyru test` | `@Test`／`@BeforeEach`／`Assert`，退出码 0／1／2；包自己的 `main` 不会被调用 |
| `teyru build -g` | 生成的 C 带 `#line`，DWARF 指回 `.teyru`；不加 `-g` 时输出逐字节不变 |

CI 在每次推送与每个 PR 上运行 `go build`、`go vet` 与整套 `go test`，`CC=gcc` 那条腿每天
运行一次。细节见 <https://docs.teyru.dev/zh-CN/docs/tooling>。

## 测量

完整表格在 <https://docs.teyru.dev>，方法是 `RUNS=5 sh scripts/bench.sh`（五次取最佳、**整支
程序的 wall clock、含进程启动**、`-O2`），两边拿到**同一个规模参数**，在同一台机器上与 HotSpot
（OpenJDK 21、默认参数）对照。脚本自己会打印它量在哪棵树上、哪台机器上、以及进入与离开两组表时的
1／5／15 分钟负载平均。以下是 **2026-10-20** 在 `nobara-pc`（Ryzen 7 5700X、16 线程、clang 22.1.8）
上的一次执行，编译器取自 `6cb64d6`，进入两组表时 1 分钟负载 3.33、离开时 2.97。

**短规模**（各程序自己的默认规模；这个数字里 JVM 的启动占了大半，说明的是启动与小程序、**不是
吞吐**）：

| 程序 | Teyru | Java | Teyru／Java |
|---|---|---|---|
| `bench_alloc` | 0.1920 s | 0.0327 s | 0.17 倍时间 |
| `bench_escape` | 0.4571 s | 0.1070 s | 0.23 倍 |
| `bench_fib` | 0.0080 s | 0.0288 s | **3.60 倍快** |
| `bench_invoke` | 1.7531 s | 0.2592 s | 0.15 倍 |
| `bench_loop` | 0.0431 s | 0.0474 s | 1.10 倍快 |
| `bench_oop` | 0.0146 s | 0.0288 s | 1.97 倍快 |
| `bench_string` | 0.0120 s | 0.0569 s | **4.74 倍快** |
| `bench_string_cjk` | 0.0146 s | 0.0806 s | **5.52 倍快** |

**长规模**（每列至少数百毫秒；JVM 预热完，答案才是程序的）：

| 长规模 | Teyru | Java | 谁快 | 峰值 RSS（Teyru） |
|---|---|---|---|---|
| `bench_alloc`（8 亿次分配） | 7.6013 s | 0.2054 s | **Java 37 倍**（见下） | 4,240 kB |
| `bench_escape` | 0.9186 s | 0.1539 s | **Java 6.0 倍** | 8,604 kB |
| `bench_fib`（fib(44)） | 1.8511 s | 1.8044 s | 打平（Java 快 2.6%） | 4,292 kB |
| `bench_invoke`（2 亿次反射调用） | 17.6147 s | 1.8807 s | **Java 9.4 倍** | 8,664 kB |
| `bench_loop`（100 万轮） | 2.0201 s | 1.0075 s | **Java 2.0 倍** | 4,292 kB |
| `bench_oop`（50 万轮） | 3.0662 s | 0.3657 s | **Java 8.4 倍** | 4,364 kB |
| `bench_string`（1600 万次拼接） | 0.5242 s | 0.3826 s | **Java 1.4 倍** | 8,600 kB |
| `bench_string_cjk`（400 万次） | 0.1783 s | 0.1610 s | Java 1.1 倍 | 8,856 kB |

**长跑上没有一列是 Teyru 赢的，`bench_fib` 是唯一打平的一列。** 短规模看起来相反，是因为整支
程序的时间有很大一部分是 JVM 启动（同一台机器约 20 ms 对 0.75 ms），那是启动的差别，不是吞吐的
差别。

**这一版的长跑数字比 2026-09-17 那一组差很多，原因是一个刻意换来的正确性。** 那一组（`130565a`）
是 `bench_alloc` 1.398 s、`bench_oop` 1.002 s、`bench_invoke` 4.066 s、`bench_loop` 1.134 s；
今天分别是 7.60／3.07／17.61／2.02。差别来自精确的 GC 根：每一个会分配的函式都要向收集器登记
自己的框（frame map），而**有框的函式不能被内联**——它的框地址是那张地图的一部分，内联之后
描述的就是别人的框。`bench_alloc` 的内循环每一轮都调用一次 `new Cell(i)` 与 `Cell.value()`，
那两个小方法从前被 clang 内联掉，现在是真正的调用。`TEYRU_NO_FRAME_MAPS=1`（测量开关，不是
可以出厂的设定）在同一棵树上量到 1.39 s，就是没有地图的版本。已修掉的部分：原生类型指针与
「指向栈对象的指针」不再登记，`bench_loop` 因此从 2.91 s 回到 2.02 s。剩下的差额是下一个
要处理的性能项，`AGENTS.md` §10 有测量方式。

**`bench_string` 反而变快了**：0.750 s → 0.524 s。拼接链现在一次算好总长度、分配一个缓冲区
（`ty_str_concatN`），不再每个 `+` 分配一个中间字符串；`bench_string_cjk` 从 0.30 s 到 0.178 s，
是同一个改动。

**`bench_alloc` 那一列与收集器无关。** `TEYRU_GCTRACE=1` 在 8 亿次分配下量到**零次收集**：
逃逸分析把 `Cell` 留在栈上，所以这一列量到的是调用与循环，不是分配器，也不是 GC。

**不是程序的三列**：启动 100 次 0.0744 s 对 2.0684 s（**27.8 倍**）；hello 可执行文件 **105,416
字节**（`-O2`）；峰值内存 4,400 kB 对 51,436 kB（**11.7 倍少**）。内存那一列取五次里**最大**的
一次，不是最小的一次：最小的一次反映的是那一轮没有把堆碰进内存，不是程序需要的比较少。

## 安装与构建

```sh
go install github.com/teyru-lang/Teyru/cmd/teyru@latest     # 或从源码：
git clone --recurse-submodules https://github.com/teyru-lang/Teyru
cd Teyru
make build     # 產生 ./teyru
make test      # 單元 + 端到端（需要 clang 或 gcc）
```

`tests/` 是 submodule，而**它现在自己就能跑**：`TEYRU=<编译器> sh tests/run.sh` 不需要 Go、
不需要编译器源码树。忘记 `--recurse-submodules` 时 `make test` 会直接说，不会安静地跑零个
测试。

## 文档

完整文档在 **<https://docs.teyru.dev>**：语言参考、诊断码、Lombok 兼容层、JSON 绑定、
Web 框架、模块系统、原生互操作、架构。源文件在
[`teyru-lang/docs`](https://github.com/teyru-lang/docs)（fumadocs，三个 locale：繁体中文
是默认，另有简体中文与英文；三个 locale 的页面内容要一致）。这份 README 本身也有四种语言：
繁体中文（正本）、[简体中文](README.zh-CN.md)、[English](README.en.md) 与
[日本語](README.ja.md)；改了正本就要把其余三份同步，数字、路径与代码块照抄，只翻散文。

GitHub 上的语言统计也由 `.gitattributes` 校正过：`*.teyru` 声明成 Teyru，`*.java.ref`
（那是规格——`.expected` 是由 javac 的输出产生的）与 `*.expected`（测试数据）标成不计入。
在那之前，「Java」曾经是这个仓库里最大的语言，而它几乎不存在。要说清楚的是
`linguist-language` 这一行**不会让 Teyru 出现**：Linguist 只统计它认得的语言，所以要等
语言本身与 `teyru-lang/editors` 那份文法被上游收下，统计里才会有 Teyru 这一项。

## 这个组织的其他仓库

| 仓库 | 内容 |
|---|---|
| [`teyru-lang/tests`](https://github.com/teyru-lang/tests) | 端到端测试数据，本仓库以 submodule 挂在 `tests/`；`run.sh` 让它自己就能执行 |
| [`teyru-lang/docs`](https://github.com/teyru-lang/docs) | 文档站（<https://docs.teyru.dev>） |
| [`teyru-lang/editors`](https://github.com/teyru-lang/editors) | VS Code／Zed／JetBrains IDEA 扩展、tree-sitter 文法 |
| [`teyru-lang/website`](https://github.com/teyru-lang/website) | 官网源文件（<https://teyru.dev>） |

贡献前请读 [`AGENTS.md`](AGENTS.md)：代码风格、测试规范、文档规范与提交前检查清单
都在那里。`AGENTS.md` §10 是**已知限制**，包括尚未实现与仍在退步的东西——要看诚实的
状态就看那里。

授权见 [LICENSE](LICENSE) 与 [THIRD-PARTY-NOTICES.md](THIRD-PARTY-NOTICES.md)。
