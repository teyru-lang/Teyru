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
| linux/arm64 | ❌ 這台機器有 `aarch64-linux-gnu-gcc`，但它的 sysroot 裡沒有 libc 標頭（`fatal error: stdint.h`），連編譯都過不去 | ❌ 沒有 aarch64 sysroot |
| darwin/amd64、darwin/arm64 | ❌ 這裡沒有 macOS SDK，也沒有可用的交叉編譯器（`teyru: no C compiler for darwin/amd64 on a linux/amd64 host`） | ❌ 這裡沒有 macOS |

建置那一欄量的是八支代表性程式——sealed switch（`t84_sealed_switch`）、arrow blocks
（`t133_arrow_blocks`）、反射（`t146_reflect`）、執行緒（`t159_threads`）、時區
（`t188_timezone`）、Spring 形狀的那一層（`t102_web`）、TLS（`t163_https_roundtrip`）與
Gson（`t101_gson`）——各以 `teyru build --no-lto --target <os>/<arch>` 編五個目標，判定看
的是編譯器自己的結束碼：**40 次裡 12 次成功**（linux/amd64 八支全過、windows/amd64 四支
過），其餘 28 次是上面那兩種「這台機器做不到」，加上 windows 上那一種「這個目標沒有
TLS」。

**五個目標都實作了，但這台機器只能演練其中兩個。** 交叉編譯到 linux/arm64 需要那個目標的
sysroot，到 macOS 需要一份 SDK，兩者都不在這台機器上——那三個目標是「實作了、**沒有任何人
跑過**」，不是「編得出來但沒測」。真正被驗證過的是另外兩個：linux/amd64 原生跑整套測試，
windows/amd64 在 Wine 下跑（上面那 179 支）。

執行期把平台相依集中在 `internal/runtime/src/tyrt_plat.h` 後面（POSIX 與 Windows 各一份），
`--target <os>/<arch>` 選擇編譯器、旗標與輸出檔名。TLS 是執行期唯一連結外部函式庫
（OpenSSL）的部分：它自己是一個檔案，只有程式的可達程式碼碰得到它時才編譯與連結，而沒有
OpenSSL 的目標（windows、macOS）對它是**指名拒絕**——在寫出任何輸出檔之前，不是連結階段
失敗。要注意「碰得到」算的是**可達性**：會用反射的程式會連帶碰得到 TLS（反射帶著一份指名
每個類別的表格），所以 Gson 與 Spring 形狀的那一層在 windows 與 macOS 上是拒絕，而不用
反射也不用 TLS 的程式完全不受影響。

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
3.51／4.27／3.81 倍），把短規模讀成吞吐就是那個錯。長規模把同一個程式放大到每列至少跑 1 秒，
JVM 暖機完，答案才是程式的：

| 長規模（每列至少 1 秒） | Teyru | Java | 誰快 |
|---|---|---|---|
| `bench_fib`（fib(44)） | 1.877 s | 1.800 s | 打平（Java 快 4%） |
| `bench_loop`（100 萬輪） | 1.134 s | 1.007 s | Java 1.13 倍 |
| `bench_oop`（50 萬輪） | 1.002 s | 0.357 s | **Java 2.8 倍** |
| `bench_string`（1600 萬次串接） | 0.750 s | 0.285 s | **Java 2.6 倍** |
| `bench_string_cjk`（400 萬次，見下） | 0.234 s | 0.160 s | 不比 |
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

**`bench_string_cjk` 這一列現在不是比較，是一個時間。** 兩邊拿到同一個規模參數，但做的是不同的
工作：這邊的 `String` 還是 UTF-8 位元組，`charAt` 讀到的是位元組不是字元，所以 400 萬次時 Teyru
印 `concat=70888890 walked=0 sliced=4`、Java 印 `concat=46888890 walked=1000 sliced=4`——
`walked=0` 對 1000 就是那件事的證據。腳本因此對這一列印 `!(out)` 而不是比值，等字串語意（W5）
落地、兩邊輸出相同的那一天，它才會變成一列比較。

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
