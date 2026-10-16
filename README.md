# Teyru

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
| windows/amd64 | ✅ 以 `x86_64-w64-mingw32-gcc` 交叉編譯；**碰得到 TLS 的程式除外**，那些是具名拒絕 | ✅ 在 Wine 下跑：當時 195 支測試程式有 179 支輸出逐位元組相同，差異的 16 支已逐一歸因（14 支在改動前的編譯器上用 gcc 也會失敗，2 支是 NTFS 檔名與 POSIX 路徑的事實） |
| linux/arm64 | ✅ 需一支指向 arm64 sysroot 的編譯器（`CC=aarch64-linux-gnu-gcc` ＋ `QEMU_LD_PREFIX`，做法寫在 `AGENTS.md` §10） | ✅ 在 qemu 下跑：整套 **250 項全過、0 項不符**（222 支程式＋3 個套件＋23 個診斷＋2 個原生），量於 2026-09-17 |
| darwin/amd64、darwin/arm64 | ✅ 以 `--cc <zig cc -target <arch>-macos>`（`resolveTarget` 現在看的是這次真的會用的那支編譯器） | ❌ 這裡沒有 macOS：**沒有任何一行程式在它上面執行過**，只到「編譯並連結」 |

建置那一欄量的是八支代表性程式——sealed switch（`t84_sealed_switch`）、arrow blocks
（`t133_arrow_blocks`）、反射（`t146_reflect`）、執行緒（`t159_threads`）、時區
（`t188_timezone`）、Spring 形狀的那一層（`t102_web`）、TLS（`t163_https_roundtrip`）與
Gson（`t101_gson`）——各以 `teyru build --no-lto --target <os>/<arch>` 編四個可達的目標。
**上表每一列的證據是那一次真正量到的東西**（linux/amd64 整套測試、arm64 在 qemu 下整套、
windows 在 Wine 下 179 支），不是一次跨編譯探針的比例；先前這裡寫過的「40 次裡 12 次成功」
是 arm64 sysroot 與 macOS SDK 都還沒準備好時的舊探針，兩件事都不是同一件事，量到 arm64
之後它就該被刪掉而不是留著當現況。

**四個目標都能編譯，但這台機器只能執行其中兩個。** 交叉編譯到 linux/arm64 需要那個目標的
sysroot、到 macOS 需要一份 SDK，兩者都不在這台機器上；macOS 那一格是「編得出來、
**沒有任何人跑過**」，不是「編得出來但沒測」。真正被驗證過的是三個：linux/amd64 原生跑整套
測試、linux/arm64 在 qemu 下跑整套、windows/amd64 在 Wine 下跑（上面那 179 支）。

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
  `Object.wait`／`notify`），以及 `java.util.concurrent` 的形狀——`Executors`、
  `ExecutorService`、`Future`、`Callable`、`CountDownLatch`、`AtomicInteger`／
  `AtomicLong`、`ConcurrentHashMap`。
* **其他**：`MessageDigest`（MD5、SHA-1／224／256／384／512）、`CRC32`、`Adler32`、
  deflate／inflate 與 gzip、`BigInteger`／`BigDecimal`、反射（`java.lang.reflect` 的形狀，
  含註解的中繼資料）、`java.util.logging` 形狀的日誌。

## 量測

完整表格在 <https://docs.teyru.dev>，方法是 `RUNS=5 sh scripts/bench.sh`（五次取最佳、**整支
程式的 wall clock、含行程啟動**、`-O2`），兩邊拿到**同一個規模參數**，在同一台機器上與 HotSpot
（OpenJDK 21、預設參數）對照。腳本自己會印出它量在哪棵樹、哪台機器上、以及進入與離開兩組表時的
1／5／15 分鐘負載平均——這台機器是共用的，而一張沒有負載的壁鐘表，沒人分得出它是不是在忙的時候
量的。以下是 2026-09-17 在 `nobara-pc`（Ryzen 7 5700X、16 執行緒、clang 22.1.8）上的一次執行，
編譯器取自 `130565a`：進入兩組表時 1 分鐘負載 2.73、離開時 2.79，執行期間的 13 次取樣全部低於 1。
兩邊印出不同答案的列，比值那一欄是 `!(out)` 而不是數字——那種列的比值不是在比兩個實作。

**腳本印兩組表，因為一個數字答不了兩個問題。** 短規模是各程式自己的預設規模（一列 6～410 ms），
那個數字裡 JVM 的啟動佔掉大半：它說明的是啟動與小程式、**不是吞吐**。README 從前寫的
「`bench_fib`／`bench_oop`／`bench_string` 快 3.5～5 倍」就是這一組（**短規模**，今天量到
3.51／4.27／3.81 倍），把短規模讀成吞吐就是那個錯。長規模把同一個程式放大到**每列至少數百
毫秒**（五列超過 1 秒，兩列字串的在 0.3～0.8 秒之間），JVM 暖機完，答案才是程式的：

| 長規模（每列至少數百毫秒） | Teyru | Java | 誰快 |
|---|---|---|---|
| `bench_fib`（fib(44)） | 1.877 s | 1.800 s | 打平（Java 快 4%） |
| `bench_loop`（100 萬輪） | 1.134 s | 1.007 s | Java 1.13 倍 |
| `bench_oop`（50 萬輪） | 1.002 s | 0.357 s | **Java 2.8 倍** |
| `bench_string`（1600 萬次串接） | 0.750 s | 0.285 s | **Java 2.6 倍** |
| `bench_string_cjk`（400 萬次，見下） | 0.30 s | 0.15 s | **Java 2.0 倍** |
| `bench_alloc`（8 億次配置） | 1.398 s | 0.204 s | **Java 6.8 倍** |
| `bench_invoke`（2 億次反射呼叫） | 4.066 s | 1.859 s | **Java 2.2 倍** |

**長跑上沒有一列是 Teyru 贏的，`bench_fib` 是唯一打平的一列。** 短規模贏的那三列，在長跑上分別
變成打平、慢 2.8 倍、慢 2.6 倍：同樣兩個實作，只把工作量放大，差距就反過來了。

**`bench_alloc` 那一列與收集器無關。** `TEYRU_GCTRACE=1` 在 8 億次配置下量到**零次收集**：編譯器
的逃逸分析把 `Cell` 留在堆疊上、再把它的欄位提升成暫存器（`AGENTS.md` §10），所以這一列量到的
是迴圈本身，不是配置器——它慢 6.8 倍也就不是「GC 慢」能解釋的。

**`bench_invoke` 慢 2.2 倍，而貴的是配置不是反射。** 短規模的版本（2000 萬次）是慢 1.63 倍
（0.4094 s 對 0.2517 s）。拆開來看，Teyru 自己的**直接呼叫**迴圈就已經比 Java 慢 2.6 倍
（2000 萬次 26 ms 對 10 ms），反射再加機制成本；每次反射呼叫 19 ns（程式自己印的 2 億次
3.798 s，表格那 4.066 s 是它加上直接呼叫那一圈）。`AGENTS.md` §10 的消去法拆解量到貴的是
`Object` API 逼出來的兩次配置（呼叫端的引數裝箱、執行期 invoker thunk 裡的結果裝箱），而 Java
也付那兩次配置——差別在配置器與物件大小（Teyru 的 `new Object()` 是 32 位元組、Java 是 16）。
**那次拆解是在舊的整體成本上量的（每次 27 ns、20M 次 540～601 ms），本次是 19 ns，拆解本身沒有
重測。**

**`bench_string_cjk` 這一列現在是比較了，而它以前不是。** 以前這邊的 `String` 還是 UTF-8
位元組、`charAt` 讀到的是位元組不是字元，所以兩邊拿到同一個規模參數卻做**不同的工作**——
400 萬次時 Teyru 印 `walked=0`、Java 印 `walked=1000`——腳本因此對它印 `!(out)` 而不是比值
（一張在比兩個實作的表，不能有一列在比兩件不同的事）。字串語意（W5）落地之後兩邊印的是
**同一行**（`concat=88890 walked=1000 sliced=4`，在 10,000 與 4,000,000 兩個規模上逐位元組
相同），所以它是一列比值：**Teyru 0.30 s、Java 0.15 s，Java 快 2.0 倍**（本機 linux/amd64、
load 0.77、五次取最佳、2026-10-16）。這一列是這一組裡最貴的幾列之一，因為每一個索引都要做
一次 UTF-16 到 WTF-8 的換算——那正是它存在的理由。

**不是程式的三列**：啟動 100 次 0.0763 s 對 2.1093 s（**27.6 倍**）；hello 執行檔 **66,808
位元組**（`-O2`，同一支程式 `-O1` 是 85,440、`-O3` 是 70,248；這個數字幾乎每次落地都會動，
所以它屬於上面那棵樹）；尖峰記憶體 4,348 kB 對 51,420 kB（**11.8 倍少**）。記憶體那一列取五次
裡**最大**的一次，不是最小的一次：最小的一次反映的是那一輪沒有把堆積碰進記憶體，不是程式需要的
比較少（同一支 hello 二十次裡有一次量到 2,116 kB，其餘在 4,150～4,450）。

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
是預設，另有簡體中文與英文；英文與簡中的總覽頁就是這份 README 的對應版本，三個 locale
的頁面內容要一致）。

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
