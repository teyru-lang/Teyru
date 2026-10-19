# Teyru

[繁體中文](README.md) · [简体中文](README.zh-CN.md) · [English](README.en.md) · **日本語**

**Java の形で書いて、ネイティブ実行ファイルにコンパイルする。**

JVM なし、バイトコードなし、JAR なし、JDK なし。標準ライブラリは Teyru 自身で書いている。コンパイラは Go
で書き、標準ライブラリだけを使う。

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

## 2 つのバックエンド

| | |
|---|---|
| `teyru build`（デフォルト） | C を生成し、clang または gcc に渡す。標準ライブラリ全体がこの経路で検証されている |
| `teyru build --backend=llvm` | **自分で LLVM IR を生成する**。プログラムは C を通らない。clang はアセンブルとリンクだけを担当する。現在は言語の大部分をカバーし、残りは `TY-INT-` 診断で**明示的に拒否**され、C バックエンドへはフォールバックしない。コンパイルできるのは linux/amd64 だけで、それ以外のターゲットは `TY-INT-0101` で拒否する |

## プラットフォーム

「ビルドできる」と「実行できる」は別の問題で、証拠も違う。だから 2 列に分けている：

| ターゲット | ビルド | 実行 |
|---|---|---|
| linux/amd64 | ✅ | ✅ ネイティブ。テスト一式はここで走る（`go test ./...`、`TEYRU=<コンパイラ> sh tests/run.sh`） |
| windows/amd64 | ✅ `x86_64-w64-mingw32-gcc` によるクロスコンパイル。**ただし TLS に到達するプログラムは除く**（mingw-w64 に OpenSSL がない）。それらは名前付きの拒否である | ✅ Wine 上で実行。2026-10-20 に `tests/programs` の **318 本**を計測：**302 本が出力（stdout、stderr、終了コード）バイト単位で同一**、**11 本は TLS に到達するためリンクできず（名前付きの拒否）**、**4 本は Windows に ucontext がなく仮想スレッドを使えないためスキップ**、**1 本（`t99_file_shell_meta`）は NTFS がそのファイル名を許さないためスキップ**——**不一致 0 本**。`-O1` でビルドし、`TEYRU_RUNNER=wine` で実行した |
| linux/arm64 | ✅ arm64 sysroot を指すコンパイラが必要（`CC=aarch64-linux-gnu-gcc` ＋ `QEMU_LD_PREFIX`、手順は `AGENTS.md` §10 に記載） | ✅ qemu 上で実行：一式 **250 項目すべて合格、不一致 0 項目**（222 本のプログラム＋3 パッケージ＋23 診断＋2 ネイティブ）、2026-09-17 に計測 |
| darwin/amd64、darwin/arm64 | ✅ `--cc <zig cc -target <arch>-macos>` を使用（`resolveTarget` が見ているのは、今回実際に使うあのコンパイラ） | ❌ ここに macOS はない：**一行もプログラムがその上で実行されたことはない**。到達点は「コンパイルしてリンクする」まで |

ビルドの列で計測したのは 8 本の代表的なプログラム——sealed switch（`t84_sealed_switch`）、arrow blocks
（`t133_arrow_blocks`）、リフレクション（`t146_reflect`）、スレッド（`t159_threads`）、タイムゾーン
（`t188_timezone`）、Spring 形のレイヤー（`t102_web`）、TLS（`t163_https_roundtrip`）、Gson（`t101_gson`）——
をそれぞれ `teyru build --no-lto --target <os>/<arch>` で 4 つの到達可能なターゲット向けにコンパイルした。
**上の表の各行の証拠は、そのとき実際に計測したもの**（linux/amd64 はテスト一式、arm64 は qemu 下で一式、
windows は Wine 下で 302 本のうち 290 本）であり、クロスコンパイルのプローブ 1 回分の比率ではない。以前ここに
書かれていた「40 回のうち 12 回成功」は arm64 sysroot と macOS SDK のどちらもまだ準備できていなかった頃の
古いプローブで、両者は同じことではない。arm64 を計測した後は、それを現状として残すのではなく削除すべきだった。

**4 つのターゲットすべてでコンパイルできるが、このマシンで実行できるのはそのうち 3 つだけである。** macOS への
クロスコンパイルには SDK が必要で、それはこのマシンにはない。macOS の欄は「ビルドはできるが、**誰も実行した
ことがない**」であって、「ビルドはできるがテストしていない」ではない。実際に検証されたのは 3 つ：linux/amd64
はネイティブでテスト一式、linux/arm64 は qemu 下で一式（別途インストールした arm64 sysroot が必要）、
windows/amd64 は Wine 下（上述の 302 本）。

実行時はプラットフォーム依存を `internal/runtime/src/tyrt_plat.h` の背後に集約している（POSIX と Windows で
1 つずつ）。`--target <os>/<arch>` がコンパイラ、フラグ、出力ファイル名を選ぶ。TLS は実行時に外部ライブラリ
（OpenSSL）をリンクする唯一の部分である：それ自体が 1 つのファイルで、プログラムの**到達可能なコード**が
そこに到達する場合にだけコンパイル・リンクされ、OpenSSL を持たないターゲット（windows、macOS）では**名前を
挙げて拒否**する——出力ファイルを書き出す前であり、リンク段階での失敗ではない。「到達可能」とは**プログラム
自身のコードがどこまで届くか**を指し、リフレクションの表は含まない：リフレクションの経路は生成された C の
中にマーカーを持ち、到達可能性解析は 2 回走る。1 回はリフレクションの参照を追い（vtable の剪定が求めるのは
その回）、もう 1 回は追わない（TLS のリンク判断が読むのは後者）。だからリフレクションだけを使い TLS に
触れないプログラムは OpenSSL を**リンクしない**——`tests/programs/t241_reflect_tls_unlinked` と
`link_test.go` の `TestTLSReachability` がこの欄を見張っている。リフレクションが実行時に実際に TLS の
メソッドを呼んだとき、得られるのは実行時の weak stub による名前付きの失敗であって、NULL へのジャンプではない。

現在**CI はない**——GitHub Actions は一切ない：この表の各行はすべてこの 1 台の linux/amd64 マシンで出した
ものなので、ここでは「ビルドできる」と「実行できる」は別の問題である。

## 標準ライブラリ

* **コレクションとユーティリティ**：`List`／`Set`／`Map`／`Deque` とその実装、`Arrays`、`Collections`、
  `Comparator`、`Optional`、`UUID`、`StringBuilder`、`Stream`、`java.util.function`。
* **文字列と時間**：`String` の完全なインターフェース、`String.format`、`java.util.regex`、
  `LocalDate` ファミリと**タイムゾーン**（`ZoneId`／`ZoneOffset`／`ZoneRules`／`ZonedDateTime`、
  ホスト自身の IANA tzdata を読む。純粋な Teyru。tzdata のないホストでは名前を挙げて拒否）、
  `Scanner`、`java.io` のバッファ付き・バイナリデータストリーム（Java の modified UTF-8 を含む）、`HexFormat`。
* **JSON**：`JsonElement` ツリー、パースと出力、そして Gson 形のバインディング——`Gson`、`GsonBuilder`
  （`serializeNulls`／`setPrettyPrinting`／`setLenient`／フィールド命名戦略／修飾子の除外）、
  `@Expose`／`@Since`／`@Until`／`@SerializedName`／`@JsonAdapter`、
  `JsonSerializer`／`JsonDeserializer`、ストリーミング `JsonReader`／`JsonWriter`。コンテナは**宣言された
  要素型**に従ってオブジェクトをバインドする。配列も含む。
* **Web**：HTTP/1.1 サーバー（keep-alive、chunked、cookie、フォーム、multipart、gzip、
  および `ssl(cert, key)` の TLS）、WebSocket（RFC 6455）、HTTP クライアント（`https://` を含む）、
  そして Spring Boot 形のコンテナ——`@RestController`、
  `@RequestMapping` ファミリ、`@RequestBody`／`@PathVariable`／`@RequestParam`、
  `@ConfigurationProperties`、`@Profile`、`@PreDestroy`、`@ControllerAdvice` ＋
  `@ExceptionHandler`、`HandlerInterceptor`、静的ファイル、CORS、`ResponseEntity`、
  `MockServer`、セッションとタイムアウト、バリデーションアノテーション。
* **並行処理**：本物の OS スレッド（`Thread`、`Runnable`、本物の `synchronized`、
  `Object.wait`／`notify`、`interrupt`、daemon、`ThreadLocal`）、**仮想スレッド**
  （`Thread.startVirtualThread`／`Thread.ofVirtual()`、ucontext 上の M:N。Windows では
  名前付きの例外で拒否）、そして `java.util.concurrent` の形——`Executors`、
  `ExecutorService`、`Future`、`Callable`、`CountDownLatch`、`AtomicInteger`／
  `AtomicLong`、`ConcurrentHashMap`。
* **その他**：`MessageDigest`（MD5、SHA-1／224／256／384／512）、`CRC32`、`Adler32`、
  deflate／inflate と gzip、`BigInteger`／`BigDecimal`、リフレクション（`java.lang.reflect` の形、
  アノテーションのメタデータを含む）、`java.util.logging` 形のログ。

## ツール

4 つのツールはプラグインではなく、同じ `teyru` の中にあります。

| ツール | すること |
|---|---|
| `teyru fmt [-w]` | 固定スタイルで整形。冪等で、コメントは 1 バイトも変えず、整形の前後で出力は同じ |
| `teyru lsp` | stdin/stdout 上の言語サーバー：`TY-*` 診断と同一ファイル内の定義ジャンプ |
| `teyru test` | `@Test`／`@BeforeEach`／`Assert`、終了コード 0／1／2。パッケージ自身の `main` は呼ばれない |
| `teyru build -g` | 生成される C に `#line` が入り、DWARF が `.teyru` を指す。`-g` なしの出力はバイト単位で不変 |

CI は push と PR のたびに `go build`、`go vet`、`go test` 一式を実行し、`CC=gcc` の脚は
毎日 1 回実行します。詳細は <https://docs.teyru.dev/en/docs/tooling>。

## 計測

完全な表は <https://docs.teyru.dev> にあります。方法は `RUNS=5 sh scripts/bench.sh`（5 回の
最良、**プロセス起動を含むプログラム全体の wall clock**、`-O2`）で、両側に**同じ規模の引数**を
渡し、同じマシンで HotSpot（OpenJDK 21、既定のフラグ）と比べます。スクリプトはどの木をどのマシンで
測ったか、2 つの表に入るときと出るときの負荷平均を自分で出力します。以下は **2026-10-20**、
`nobara-pc`（Ryzen 7 5700X、16 スレッド、clang 22.1.8）、コンパイラは `6cb64d6` の 1 回の実行で、
表に入るときの 1 分負荷は 3.33、出るときは 2.97 でした。

**短い規模**（各プログラムの既定の規模。この数字の大半は JVM の起動なので、起動と小さな
プログラムについての話であり、**スループットではありません**）：

| プログラム | Teyru | Java | Teyru／Java |
|---|---|---|---|
| `bench_alloc` | 0.1920 s | 0.0327 s | 時間で 0.17 倍 |
| `bench_escape` | 0.4571 s | 0.1070 s | 0.23 倍 |
| `bench_fib` | 0.0080 s | 0.0288 s | **3.60 倍速い** |
| `bench_invoke` | 1.7531 s | 0.2592 s | 0.15 倍 |
| `bench_loop` | 0.0431 s | 0.0474 s | 1.10 倍速い |
| `bench_oop` | 0.0146 s | 0.0288 s | 1.97 倍速い |
| `bench_string` | 0.0120 s | 0.0569 s | **4.74 倍速い** |
| `bench_string_cjk` | 0.0146 s | 0.0806 s | **5.52 倍速い** |

**長い規模**（各行とも数百ミリ秒以上。JVM が温まった後の、プログラムそのものの答え）：

| 長い規模 | Teyru | Java | どちらが速い | ピーク RSS（Teyru） |
|---|---|---|---|---|
| `bench_alloc`（8 億回の割り当て） | 7.6013 s | 0.2054 s | **Java が 37 倍**（下記） | 4,240 kB |
| `bench_escape` | 0.9186 s | 0.1539 s | **Java が 6.0 倍** | 8,604 kB |
| `bench_fib`（fib(44)） | 1.8511 s | 1.8044 s | 引き分け（Java が 2.6% 速い） | 4,292 kB |
| `bench_invoke`（2 億回のリフレクション呼び出し） | 17.6147 s | 1.8807 s | **Java が 9.4 倍** | 8,664 kB |
| `bench_loop`（100 万ラウンド） | 2.0201 s | 1.0075 s | **Java が 2.0 倍** | 4,292 kB |
| `bench_oop`（50 万ラウンド） | 3.0662 s | 0.3657 s | **Java が 8.4 倍** | 4,364 kB |
| `bench_string`（1600 万回の連結） | 0.5242 s | 0.3826 s | **Java が 1.4 倍** | 8,600 kB |
| `bench_string_cjk`（400 万回） | 0.1783 s | 0.1610 s | Java が 1.1 倍 | 8,856 kB |

**長い規模では Teyru が勝つ列は 1 つもなく、`bench_fib` だけが引き分けです。** 短い規模が
逆に見えるのは、その数字の大半が JVM の起動（このマシンで約 20 ms 対 0.75 ms）だからで、
起動の差であってスループットの差ではありません。

**この版の長い規模の数字は 2026-09-17 の組よりずっと悪く、その理由は意図して選んだ正しさ
です。** その組（`130565a`）は `bench_alloc` 1.398 s、`bench_oop` 1.002 s、`bench_invoke`
4.066 s、`bench_loop` 1.134 s でしたが、今日は 7.60／3.07／17.61／2.02 です。差は正確な GC の
根から来ています。割り当てを行える関数は自分のフレームをコレクタに登録し（frame map）、
**フレームを持つ関数はインライン化できません**——フレームのアドレスが地図の一部で、インライン
化された写しは他人のフレームを指してしまうからです。`bench_alloc` の内側のループは毎回
`new Cell(i)` と `Cell.value()` を呼びます。その 2 つの小さなメソッドは以前 clang が
インライン化していましたが、今は本物の呼び出しです。`TEYRU_NO_FRAME_MAPS=1`（計測用の
スイッチで、出荷できる設定ではありません）は同じ木で 1.39 s を測ります。これが地図のない版
です。直した部分：C のプリミティブへのポインタと、フレームに置かれたオブジェクトへのポインタは
登録しなくなり、`bench_loop` は 2.91 s から 2.02 s に戻りました。残りは次の性能項目で、
測り方は `AGENTS.md` §10 にあります。

**`bench_string` はむしろ速くなりました**：0.750 s → 0.524 s。連結の連鎖は今、全体の長さを
一度計算して 1 つのバッファを埋めます（`ty_str_concatN`）。`+` ごとに中間文字列を作りません。
`bench_string_cjk` は 0.30 s から 0.178 s へ、同じ変更によるものです。

**`bench_alloc` の行はコレクタとは関係ありません。** `TEYRU_GCTRACE=1` は 8 億回の割り当てで
**収集 0 回**を報告します。エスケープ解析が `Cell` をスタックに置くので、この行が測っているのは
呼び出しとループであり、アロケータでも GC でもありません。

**プログラムではない 3 行**：起動 100 回で 0.0744 s 対 2.0684 s（**27.8 倍**）、hello の
実行ファイルは **105,416 バイト**（`-O2`）、ピーク RSS は 4,400 kB 対 51,436 kB（**11.7 倍少ない**）。
メモリの行は 5 回のうち**最大**を取ります。最小はその回がヒープに触れなかったことを映している
だけで、プログラムが少なくて済むわけではありません。

## インストールとビルド

```sh
go install github.com/teyru-lang/Teyru/cmd/teyru@latest     # またはソースから：
git clone --recurse-submodules https://github.com/teyru-lang/Teyru
cd Teyru
make build     # 產生 ./teyru
make test      # 單元 + 端到端（需要 clang 或 gcc）
```

`tests/` は submodule だが、**今はそれ自体で実行できる**：`TEYRU=<コンパイラ> sh tests/run.sh` は Go も
コンパイラのソースツリーも必要としない。`--recurse-submodules` を忘れた場合、`make test` ははっきりそう言うのであって、
テストをゼロ件、黙って流すことはない。

## ドキュメント

完全なドキュメントは **<https://docs.teyru.dev>** にある：言語リファレンス、診断コード、Lombok 互換レイヤー、
JSON バインディング、Web フレームワーク、モジュールシステム、ネイティブ相互運用、アーキテクチャ。ソースは
[`teyru-lang/docs`](https://github.com/teyru-lang/docs)（fumadocs、3 つのロケール：繁體中文
がデフォルトで、ほかに簡体字中国語と英語。3 つのロケールのページ内容は一致していなければならない）。この
README 自体にも 4 つの言語がある：繁體中文（本ファイル、正本）、[简体中文](README.zh-CN.md)、
[English](README.en.md)、[日本語](README.ja.md)。正本を変えたら残りの 3 つも同期させること。数字、パス、
コードブロックはそのまま写し、翻訳するのは散文だけである。

GitHub 上の言語統計も `.gitattributes` で補正してある：`*.teyru` は Teyru と宣言し、`*.java.ref`
（これは仕様である——`.expected` は javac の出力から生成される）と `*.expected`（テストデータ）は集計対象外と
した。それ以前は「Java」がこのリポジトリで最大の言語だったが、それはほとんど存在していなかった。はっきり
言っておくと、`linguist-language` の行は**Teyru を出現させない**：Linguist は自分が知っている言語しか集計
しないので、言語自体と `teyru-lang/editors` の文法が上流に取り込まれて初めて、統計に Teyru の項目が現れる。

## この組織のほかのリポジトリ

| リポジトリ | 内容 |
|---|---|
| [`teyru-lang/tests`](https://github.com/teyru-lang/tests) | エンドツーエンドのテストデータ。本リポジトリでは submodule として `tests/` に置いている；`run.sh` により単体で実行できる |
| [`teyru-lang/docs`](https://github.com/teyru-lang/docs) | ドキュメントサイト（<https://docs.teyru.dev>） |
| [`teyru-lang/editors`](https://github.com/teyru-lang/editors) | VS Code／Zed／JetBrains IDEA 拡張、tree-sitter 文法 |
| [`teyru-lang/website`](https://github.com/teyru-lang/website) | 公式サイトのソース（<https://teyru.dev>） |

コントリビュートの前に [`AGENTS.md`](AGENTS.md) を読むこと：コードスタイル、テスト規範、ドキュメント規範、
送信前チェックリストはすべてそこにある。`AGENTS.md` §10 は**既知の制限**で、まだ実装していないものと、まだ
退行しているものを含む——正直な状態を知りたいならそこを見ること。

ライセンスは [LICENSE](LICENSE) と [THIRD-PARTY-NOTICES.md](THIRD-PARTY-NOTICES.md) を参照。
