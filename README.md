# Teyru

**繁體中文** · [简体中文](README.zh-CN.md) · [English](README.en.md) · [日本語](README.ja.md)

**以 Java 的形狀寫，編成原生執行檔。**

沒有 JVM、沒有 bytecode、沒有 JAR、沒有 JDK，標準程式庫用 Teyru 自己寫。編譯器用 Go
寫，只用標準函式庫。

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

## 兩個後端

| | |
|---|---|
| `teyru build`（預設） | 產生 C，交給 clang 或 gcc。整個標準程式庫都在這條路上驗證 |
| `teyru build --backend=llvm` | **自己產生 LLVM IR**，程式不經過 C；clang 只負責組譯與連結。目前涵蓋語言的一大部分，其餘用 `TY-INT-` 診斷**明確拒絕**，不會退回 C 後端；它只編 linux/amd64，其他目標以 `TY-INT-0101` 拒絕 |

## 平台

「編得出來」與「跑得起來」是兩個問題，證據也不一樣，所以分兩欄：

| 目標 | 建置 | 執行 |
|---|---|---|
| linux/amd64 | ✅ | ✅ 原生，整套測試都在這裡跑（`go test ./...`、`TEYRU=<編譯器> sh tests/run.sh`） |
| windows/amd64 | ✅ 以 `x86_64-w64-mingw32-gcc` 交叉編譯；**碰得到 TLS 的程式除外**（mingw-w64 沒有 OpenSSL），那些是具名拒絕 | ✅ 在 Wine 下跑，2026-10-20 量 `tests/programs` 的 **318 支**：**302 支輸出（stdout、stderr、結束碼）逐位元組相同**、**11 支碰得到 TLS 而無法連結（具名拒絕）**、**4 支虛擬執行緒因為 Windows 沒有 ucontext 而跳過**、**1 支（`t99_file_shell_meta`）因 NTFS 不能有那種檔名而跳過**——**0 支不符**。整套以 `-O1` 建、以 `TEYRU_RUNNER=wine` 跑 |
| linux/arm64 | ✅ 需一支指向 arm64 sysroot 的編譯器（`CC=aarch64-linux-gnu-gcc` ＋ `QEMU_LD_PREFIX`，做法寫在 `AGENTS.md` §10） | ✅ 在 qemu 下跑：整套 **250 項全過、0 項不符**（222 支程式＋3 個套件＋23 個診斷＋2 個原生），量於 2026-09-17 |
| darwin/amd64、darwin/arm64 | ✅ 以 `--cc <zig cc -target <arch>-macos>`（`resolveTarget` 現在看的是這次真的會用的那支編譯器） | ❌ 這裡沒有 macOS：**沒有任何一行程式在它上面執行過**，只到「編譯並連結」 |

建置那一欄量的是八支代表性程式——sealed switch（`t84_sealed_switch`）、arrow blocks
（`t133_arrow_blocks`）、反射（`t146_reflect`）、執行緒（`t159_threads`）、時區
（`t188_timezone`）、Spring 形狀的那一層（`t102_web`）、TLS（`t163_https_roundtrip`）與
Gson（`t101_gson`）——各以 `teyru build --no-lto --target <os>/<arch>` 編四個可達的目標。
**上表每一列的證據是那一次真正量到的東西**（linux/amd64 整套測試、arm64 在 qemu 下整套、
windows 在 Wine 下 302 支裡的 290 支），不是一次跨編譯探針的比例；先前這裡寫過的「40 次裡 12 次成功」
是 arm64 sysroot 與 macOS SDK 都還沒準備好時的舊探針，兩件事都不是同一件事，量到 arm64
之後它就該被刪掉而不是留著當現況。

**四個目標都能編譯，但這台機器只能執行其中三個。** 交叉編譯到 macOS 需要一份 SDK，它不在這台
機器上；macOS 那一格是「編得出來、**沒有任何人跑過**」，不是「編得出來但沒測」。真正被驗證過
的是三個：linux/amd64 原生跑整套測試、linux/arm64 在 qemu 下跑整套（需要另外裝上去的 arm64
sysroot）、windows/amd64 在 Wine 下跑（上面那 302 支）。

執行期把平台相依集中在 `internal/runtime/src/tyrt_plat.h` 後面（POSIX 與 Windows 各一份），
`--target <os>/<arch>` 選擇編譯器、旗標與輸出檔名。TLS 是執行期唯一連結外部函式庫
（OpenSSL）的部分：它自己是一個檔案，只有程式的**可達程式碼**碰得到它時才編譯與連結，而沒有
OpenSSL 的目標（windows、macOS）對它是**指名拒絕**——在寫出任何輸出檔之前，不是連結階段
失敗。「可達」指的是**程式自己的程式碼到得了哪裡**，反射的表格不算：反射那條路徑在產生的 C
裡帶一個標記，可達性分析跑兩次，一次跟著反射的參考（vtable 剪裁要的就是那一次），一次不跟
（TLS 的連結決策讀的是後者）。所以一個只會反射、不碰 TLS 的程式**不連** OpenSSL——
`tests/programs/t241_reflect_tls_unlinked` 與 `link_test.go` 的 `TestTLSReachability`
盯著這一格；反射真的在執行期叫到 TLS 方法時，拿到的是執行期 weak stub 的指名失敗，不是
跳到 NULL。

目前**沒有 CI**——沒有任何 GitHub Actions：這份表上的每一列都是在這一台 linux/amd64 機器上
跑出來的，所以「編得出來」與「跑得起來」在這裡是兩個問題。

## 標準程式庫

* **集合與工具**：`List`／`Set`／`Map`／`Deque` 與其實作、`Arrays`、`Collections`、
  `Comparator`、`Optional`、`UUID`、`StringBuilder`、`Stream`、`java.util.function`。
* **文字與時間**：`String` 的完整介面、`String.format`、`java.util.regex`、
  `LocalDate` 家族與**時區**（`ZoneId`／`ZoneOffset`／`ZoneRules`／`ZonedDateTime`，
  讀主機自己的 IANA tzdata，純 Teyru；沒有 tzdata 的主機是具名拒絕）、
  `Scanner`、`java.io` 的緩衝與二進位資料流（含 Java 的 modified UTF-8）、`HexFormat`。
* **JSON**：`JsonElement` 樹、解析與輸出，以及 Gson 形狀的綁定——`Gson`、`GsonBuilder`
  （`serializeNulls`／`setPrettyPrinting`／`setLenient`／欄位命名策略／排除修飾子）、
  `@Expose`／`@Since`／`@Until`／`@SerializedName`／`@JsonAdapter`、
  `JsonSerializer`／`JsonDeserializer`、串流 `JsonReader`／`JsonWriter`。容器會依**宣告的
  元素型別**綁定物件，包含陣列。
* **Web**：HTTP/1.1 伺服器（keep-alive、chunked、cookie、表單、multipart、gzip，
  以及 `ssl(cert, key)` 的 TLS）、WebSocket（RFC 6455）、HTTP 客戶端（含 `https://`），
  以及 Spring Boot 形狀的容器——`@RestController`、
  `@RequestMapping` 家族、`@RequestBody`／`@PathVariable`／`@RequestParam`、
  `@ConfigurationProperties`、`@Profile`、`@PreDestroy`、`@ControllerAdvice` ＋
  `@ExceptionHandler`、`HandlerInterceptor`、靜態檔案、CORS、`ResponseEntity`、
  `MockServer`、會話與逾時、驗證註解。
* **並行**：真正的作業系統執行緒（`Thread`、`Runnable`、真正的 `synchronized`、
  `Object.wait`／`notify`、`interrupt`、daemon、`ThreadLocal`），**虛擬執行緒**
  （`Thread.startVirtualThread`／`Thread.ofVirtual()`，ucontext 上的 M:N；Windows
  以具名例外拒絕），以及 `java.util.concurrent` 的形狀——`Executors`、
  `ExecutorService`、`Future`、`Callable`、`CountDownLatch`、`AtomicInteger`／
  `AtomicLong`、`ConcurrentHashMap`。
* **其他**：`MessageDigest`（MD5、SHA-1／224／256／384／512）、`CRC32`、`Adler32`、
  deflate／inflate 與 gzip、`BigInteger`／`BigDecimal`、反射（`java.lang.reflect` 的形狀，
  含註解的中繼資料）、`java.util.logging` 形狀的日誌。

## 工具鏈

四個工具在同一支 `teyru` 裡，沒有外掛：

| 工具 | 做什麼 |
|---|---|
| `teyru fmt [-w]` | 固定風格格式化；冪等，註解逐字保留，格式化前後輸出相同 |
| `teyru lsp` | stdin/stdout 上的語言伺服器：`TY-*` 診斷與同檔定義跳轉 |
| `teyru test` | `@Test`／`@BeforeEach`／`Assert`，結束碼 0／1／2；套件自己的 `main` 不會被呼叫 |
| `teyru build -g` | 產生的 C 帶 `#line`，DWARF 指回 `.teyru`；不加 `-g` 時輸出逐位元組不變 |

CI 在每次 push 與每個 PR 上跑 `go build`、`go vet` 與整套 `go test`，`CC=gcc` 那條腿每天
跑一次。細節見 <https://docs.teyru.dev/docs/tooling>。

## 量測

完整表格在 <https://docs.teyru.dev>，方法是 `RUNS=5 sh scripts/bench.sh`（五次取最佳、**整支
程式的 wall clock、含行程啟動**、`-O2`），兩邊拿到**同一個規模參數**，在同一台機器上與 HotSpot
（OpenJDK 21、預設參數）對照。腳本自己會印出它量在哪棵樹、哪台機器上、以及進入與離開兩組表時的
1／5／15 分鐘負載平均——這台機器是共用的，而一張沒有負載的壁鐘表，沒人分得出它是不是在忙的時候
量的。以下是 **2026-10-20** 在 `nobara-pc`（Ryzen 7 5700X、16 執行緒、clang 22.1.8）上的一次
執行，編譯器取自 `6cb64d6`，進入兩組表時 1 分鐘負載 3.33、離開時 2.97。

**短規模**（各程式自己的預設規模；這個數字裡 JVM 的啟動佔掉大半，說明的是啟動與小程式、**不是
吞吐**）：

| 程式 | Teyru | Java | Teyru／Java |
|---|---|---|---|
| `bench_alloc` | 0.1920 s | 0.0327 s | 0.17 倍時間 |
| `bench_escape` | 0.4571 s | 0.1070 s | 0.23 倍 |
| `bench_fib` | 0.0080 s | 0.0288 s | **3.60 倍快** |
| `bench_invoke` | 1.7531 s | 0.2592 s | 0.15 倍 |
| `bench_loop` | 0.0431 s | 0.0474 s | 1.10 倍快 |
| `bench_oop` | 0.0146 s | 0.0288 s | 1.97 倍快 |
| `bench_string` | 0.0120 s | 0.0569 s | **4.74 倍快** |
| `bench_string_cjk` | 0.0146 s | 0.0806 s | **5.52 倍快** |

**長規模**（每列至少數百毫秒；JVM 暖機完，答案才是程式的）：

| 長規模 | Teyru | Java | 誰快 | 尖峰 RSS（Teyru） |
|---|---|---|---|---|
| `bench_alloc`（8 億次配置） | 7.6013 s | 0.2054 s | **Java 37 倍**（見下） | 4,240 kB |
| `bench_escape` | 0.9186 s | 0.1539 s | **Java 6.0 倍** | 8,604 kB |
| `bench_fib`（fib(44)） | 1.8511 s | 1.8044 s | 打平（Java 快 2.6%） | 4,292 kB |
| `bench_invoke`（2 億次反射呼叫） | 17.6147 s | 1.8807 s | **Java 9.4 倍** | 8,664 kB |
| `bench_loop`（100 萬輪） | 2.0201 s | 1.0075 s | **Java 2.0 倍** | 4,292 kB |
| `bench_oop`（50 萬輪） | 3.0662 s | 0.3657 s | **Java 8.4 倍** | 4,364 kB |
| `bench_string`（1600 萬次串接） | 0.5242 s | 0.3826 s | **Java 1.4 倍** | 8,600 kB |
| `bench_string_cjk`（400 萬次） | 0.1783 s | 0.1610 s | Java 1.1 倍 | 8,856 kB |

**長跑上沒有一列是 Teyru 贏的，`bench_fib` 是唯一打平的一列。** 短規模看起來相反，是因為整支
程式的時間有很大一部分是 JVM 啟動（同一台機器約 20 ms 對 0.75 ms），那是啟動的差別，不是吞吐的
差別。

**這一版的長跑數字比 2026-09-17 那一組差很多，原因是一個刻意換來的正確性。** 那一組（`130565a`）
是 `bench_alloc` 1.398 s、`bench_oop` 1.002 s、`bench_invoke` 4.066 s、`bench_loop` 1.134 s；
今天分別是 7.60／3.07／17.61／2.02。差別來自精確的 GC 根：每一個會配置的函式都要向收集器登記
自己的框（frame map），而**有框的函式不能被內聯**——它的框位址是那張地圖的一部分，內聯之後
描述的就是別人的框。`bench_alloc` 的內迴圈每一輪都呼叫一次 `new Cell(i)` 與 `Cell.value()`，
那兩個小方法從前被 clang 內聯掉，現在是真正的呼叫。`TEYRU_NO_FRAME_MAPS=1`（量測開關，不是
可以出貨的設定）在同一棵樹上量到 1.39 s，就是沒有地圖的版本。已修掉的部分：原生型別指標與
「指向堆疊物件的指標」不再登記，`bench_loop` 因此從 2.91 s 回到 2.02 s。剩下的差額是下一個
要處理的效能項，`AGENTS.md` §10 有量測方式。

**`bench_string` 反而變快了**：0.750 s → 0.524 s。串接鏈現在一次算好總長度、配置一個緩衝區
（`ty_str_concatN`），不再每個 `+` 配置一個中間字串；`bench_string_cjk` 從 0.30 s 到 0.178 s，
是同一個改動。

**`bench_alloc` 那一列與收集器無關。** `TEYRU_GCTRACE=1` 在 8 億次配置下量到**零次收集**：
逃逸分析把 `Cell` 留在堆疊上，所以這一列量到的是呼叫與迴圈，不是配置器，也不是 GC。

**不是程式的三列**：啟動 100 次 0.0744 s 對 2.0684 s（**27.8 倍**）；hello 執行檔 **105,416
位元組**（`-O2`）；尖峰記憶體 4,400 kB 對 51,436 kB（**11.7 倍少**）。記憶體那一列取五次裡
**最大**的一次，不是最小的一次：最小的一次反映的是那一輪沒有把堆積碰進記憶體，不是程式需要的
比較少。

## 安裝與建置

```sh
go install github.com/teyru-lang/Teyru/cmd/teyru@latest     # 或從原始碼：
git clone --recurse-submodules https://github.com/teyru-lang/Teyru
cd Teyru
make build     # 產生 ./teyru
make test      # 單元 + 端到端（需要 clang 或 gcc）
```

`tests/` 是 submodule，而**它現在自己就能跑**：`TEYRU=<編譯器> sh tests/run.sh` 不需要 Go、
不需要編譯器原始碼樹。忘記 `--recurse-submodules` 時 `make test` 會直接說，不會安靜地跑零個
測試。

## 文件

完整文件在 **<https://docs.teyru.dev>**：語言參考、診斷碼、Lombok 相容層、JSON 綁定、
Web 框架、模組系統、原生互通、架構。原始檔在
[`teyru-lang/docs`](https://github.com/teyru-lang/docs)（fumadocs，三個 locale：繁體中文
是預設，另有簡體中文與英文；三個 locale 的頁面內容要一致）。這份 README 本身也有四個語言：
繁體中文（本檔，正本）、[简体中文](README.zh-CN.md)、[English](README.en.md) 與
[日本語](README.ja.md)；改了正本就要把其餘三份同步，數字、路徑與程式碼區塊照抄，只翻散文。

GitHub 上的語言統計也由 `.gitattributes` 校正過：`*.teyru` 宣告成 Teyru，`*.java.ref`
（那是規格——`.expected` 是由 javac 的輸出產生的）與 `*.expected`（測試資料）標成不計入。
在那之前，「Java」曾經是這個倉庫裡最大的語言，而它幾乎不存在。要說清楚的是
`linguist-language` 這一行**不會讓 Teyru 出現**：Linguist 只統計它認得的語言，所以要等
語言本身與 `teyru-lang/editors` 那份文法被上游收下，統計裡才會有 Teyru 這一項。

## 這個組織的其他倉庫

| 倉庫 | 內容 |
|---|---|
| [`teyru-lang/tests`](https://github.com/teyru-lang/tests) | 端到端測試資料，本倉庫以 submodule 掛在 `tests/`；`run.sh` 讓它自己就能執行 |
| [`teyru-lang/docs`](https://github.com/teyru-lang/docs) | 文件站（<https://docs.teyru.dev>） |
| [`teyru-lang/editors`](https://github.com/teyru-lang/editors) | VS Code／Zed／JetBrains IDEA 擴充、tree-sitter 文法 |
| [`teyru-lang/website`](https://github.com/teyru-lang/website) | 官網原始檔（<https://teyru.dev>） |

貢獻前請讀 [`AGENTS.md`](AGENTS.md)：程式風格、測試規範、文件規範與送出前檢查清單
都在那裡。`AGENTS.md` §10 是**已知限制**，包括尚未實作與仍在退步的東西——要看誠實的
狀態就看那裡。

授權見 [LICENSE](LICENSE) 與 [THIRD-PARTY-NOTICES.md](THIRD-PARTY-NOTICES.md)。
