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
| windows/amd64 | ✅ 以 `x86_64-w64-mingw32-gcc` 交叉编译；**会碰到 TLS 的程序除外**，那些是具名拒绝（mingw-w64 没有 OpenSSL） | ✅ 在 Wine 下跑，2026-10-17 测 `tests/programs` 的 **302 个**：**290 个输出（stdout、stderr、退出码）逐字节相同**、**11 个会碰到 TLS 而被具名拒绝**、**1 个（`t99_file_shell_meta`）因 NTFS 不能有那种文件名而跳过**——**0 个不符**。整套以 `-O0` 构建（约 3 分钟建完 302 个）；`-O1` 在最大的几个上 mingw 的 `cc1` 要跑超过两小时，那是工具链的构建时间，不是行为差异 |
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

目前**没有 CI**——没有任何 GitHub Actions：这份表上的每一列都是在这一台 linux/amd64 机器上
跑出来的，所以「编得出来」与「跑得起来」在这里是两个问题。

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
  `Object.wait`／`notify`），以及 `java.util.concurrent` 的形状——`Executors`、
  `ExecutorService`、`Future`、`Callable`、`CountDownLatch`、`AtomicInteger`／
  `AtomicLong`、`ConcurrentHashMap`。
* **其他**：`MessageDigest`（MD5、SHA-1／224／256／384／512）、`CRC32`、`Adler32`、
  deflate／inflate 与 gzip、`BigInteger`／`BigDecimal`、反射（`java.lang.reflect` 的形状，
  含注解的元数据）、`java.util.logging` 形状的日志。

## 测量

完整表格在 <https://docs.teyru.dev>，方法是 `RUNS=5 sh scripts/bench.sh`（五次取最佳、**整个
程序的 wall clock、含进程启动**、`-O2`），两边拿到**同一个规模参数**，在同一台机器上与 HotSpot
（OpenJDK 21、默认参数）对照。脚本自己会打印出它测在哪棵树、哪台机器上、以及进入与离开两组表时的
1／5／15 分钟负载平均——这台机器是共用的，而一张没有负载的耗时表，没人分得出它是不是在忙的时候
测的。以下是 2026-09-17 在 `nobara-pc`（Ryzen 7 5700X、16 线程、clang 22.1.8）上的一次执行，
编译器取自 `130565a`：进入两组表时 1 分钟负载 2.73、离开时 2.79，执行期间的 13 次采样全部低于 1。
两边打印出不同答案的行，比值那一栏是 `!(out)` 而不是数字——那种行的比值不是在比两个实现。

**脚本打印两组表，因为一个数字答不了两个问题。** 短规模是各程序自己的默认规模（一行 6～410 ms），
那个数字里 JVM 的启动占掉大半：它说明的是启动与小程序、**不是吞吐**。README 从前写的
「`bench_fib`／`bench_oop`／`bench_string` 快 3.5～5 倍」就是这一组（**短规模**，今天测到
3.51／4.27／3.81 倍），把短规模读成吞吐就是那个错。长规模把同一个程序放大到**每行至少数百
毫秒**（五行超过 1 秒，两行字符串的在 0.3～0.8 秒之间），JVM 预热完，答案才是程序的：

| 长规模（每行至少数百毫秒） | Teyru | Java | 谁快 |
|---|---|---|---|
| `bench_fib`（fib(44)） | 1.877 s | 1.800 s | 打平（Java 快 4%） |
| `bench_loop`（100 万轮） | 1.134 s | 1.007 s | Java 1.13 倍 |
| `bench_oop`（50 万轮） | 1.002 s | 0.357 s | **Java 2.8 倍** |
| `bench_string`（1600 万次串接） | 0.750 s | 0.285 s | **Java 2.6 倍** |
| `bench_string_cjk`（400 万次，见下） | 0.30 s | 0.15 s | **Java 2.0 倍** |
| `bench_alloc`（8 亿次分配） | 1.398 s | 0.204 s | **Java 6.8 倍** |
| `bench_invoke`（2 亿次反射调用） | 4.066 s | 1.859 s | **Java 2.2 倍** |

**长跑上没有一行是 Teyru 赢的，`bench_fib` 是唯一打平的一行。** 短规模赢的那三行，在长跑上分别
变成打平、慢 2.8 倍、慢 2.6 倍：同样两个实现，只把工作量放大，差距就反过来了。

**`bench_alloc` 那一行与收集器无关。** `TEYRU_GCTRACE=1` 在 8 亿次分配下测到**零次收集**：编译器
的逃逸分析把 `Cell` 留在栈上、再把它的字段提升成寄存器（`AGENTS.md` §10），所以这一行测到的
是循环本身，不是分配器——它慢 6.8 倍也就不是「GC 慢」能解释的。

**`bench_invoke` 慢 2.2 倍，而贵的是分配不是反射。** 短规模的版本（2000 万次）是慢 1.63 倍
（0.4094 s 对 0.2517 s）。拆开来看，Teyru 自己的**直接调用**循环就已经比 Java 慢 2.6 倍
（2000 万次 26 ms 对 10 ms），反射再加机制成本；每次反射调用 19 ns（程序自己打印的 2 亿次
3.798 s，表格那 4.066 s 是它加上直接调用那一圈）。`AGENTS.md` §10 的消去法拆解测到贵的是
`Object` API 逼出来的两次分配（调用端的实参装箱、运行时 invoker thunk 里的结果装箱），而 Java
也付那两次分配——差别在分配器与对象大小（Teyru 的 `new Object()` 是 32 字节、Java 是 16）。
**那次拆解是在旧的总体成本上测的（每次 27 ns、20M 次 540～601 ms），本次是 19 ns，拆解本身没有
重测。**

**`bench_string_cjk` 这一行现在是比较了，而它以前不是。** 以前这边的 `String` 还是 UTF-8
字节、`charAt` 读到的是字节不是字符，所以两边拿到同一个规模参数却做**不同的工作**——
400 万次时 Teyru 打印 `walked=0`、Java 打印 `walked=1000`——脚本因此对它打印 `!(out)` 而不是比值
（一张在比两个实现的表，不能有一行在比两件不同的事）。字符串语义（W5）落地之后两边打印的是
**同一行**（`concat=88890 walked=1000 sliced=4`，在 10,000 与 4,000,000 两个规模上逐字节
相同），所以它是一行比值：**Teyru 0.30 s、Java 0.15 s，Java 快 2.0 倍**（本机 linux/amd64、
load 0.77、五次取最佳、2026-10-16）。这一行是这一组里最贵的几行之一，因为每一个索引都要做
一次 UTF-16 到 WTF-8 的换算——那正是它存在的理由。

**不是程序的三行**：启动 100 次 0.0763 s 对 2.1093 s（**27.6 倍**）；hello 可执行文件 **66,808
字节**（`-O2`，同一个程序 `-O1` 是 85,440、`-O3` 是 70,248；这个数字几乎每次落地都会动，
所以它属于上面那棵树——**2026-10-16 在今天的树上测到的是 97,888 字节**，`-O0` 233,680、
`-O1` 209,472、`-O3` 106,032，差在框架地图的设计代价，以及一段已修的 `CharSequence` 退步，
见 <https://docs.teyru.dev> 的大小那一节）；峰值内存 4,348 kB 对 51,420 kB（**少 11.8 倍**）。内存那一行取五次
里**最大**的一次，不是最小的一次：最小的一次反映的是那一轮没有把堆碰进内存，不是程序需要的
比较少（同一个 hello 二十次里有一次测到 2,116 kB，其余在 4,150～4,450）。

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
