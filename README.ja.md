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
| windows/amd64 | ✅ `x86_64-w64-mingw32-gcc` によるクロスコンパイル。**ただし TLS に到達するプログラムは除く**——それらは名前付きの拒否である（mingw-w64 に OpenSSL がない） | ✅ Wine 上で実行。2026-10-17 に `tests/programs` の **302 本**を計測：**290 本の出力（stdout、stderr、終了コード）がバイト単位で一致**、**11 本が TLS に到達するため名前付きで拒否**、**1 本（`t99_file_shell_meta`）は NTFS がそのようなファイル名を許さないためスキップ**——**不一致 0 本**。一式は `-O0` でビルド（302 本のビルドに約 3 分）。`-O1` では最大級の数本で mingw の `cc1` が 2 時間以上かかるが、それはツールチェーンのビルド時間であり、挙動の差ではない |
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
  `Object.wait`／`notify`）、そして `java.util.concurrent` の形——`Executors`、
  `ExecutorService`、`Future`、`Callable`、`CountDownLatch`、`AtomicInteger`／
  `AtomicLong`、`ConcurrentHashMap`。
* **その他**：`MessageDigest`（MD5、SHA-1／224／256／384／512）、`CRC32`、`Adler32`、
  deflate／inflate と gzip、`BigInteger`／`BigDecimal`、リフレクション（`java.lang.reflect` の形、
  アノテーションのメタデータを含む）、`java.util.logging` 形のログ。

## 計測

完全な表は <https://docs.teyru.dev> にあり、方法は `RUNS=5 sh scripts/bench.sh`（5 回のうち最良、**プログラム
全体の wall clock、プロセス起動を含む**、`-O2`）。両側が**同じ規模パラメータ**を受け取り、同じマシン上で HotSpot
（OpenJDK 21、デフォルトパラメータ）と比較する。スクリプトは自分で、どのツリーで、どのマシンで計測したか、
そして 2 組の表に入るときと出るときの 1／5／15 分負荷平均を出力する——このマシンは共用であり、負荷の記録がない実時間の
表では、忙しいときに計測したものかどうか誰にも判別できない。以下は 2026-09-17 に `nobara-pc`（Ryzen 7 5700X、16 スレッド、
clang 22.1.8）で行った 1 回の実行で、コンパイラは `130565a` のもの：2 組の表に入るときの 1 分負荷は 2.73、出るときは
2.79、実行期間中の 13 回のサンプルはすべて 1 未満だった。両側が異なる答えを出力する行では、比の欄は数値ではなく
`!(out)` になる——そのような行の比は 2 つの実装を比べているのではない。

**スクリプトが 2 組の表を出力するのは、1 つの数字では 2 つの問いに答えられないからだ。** 短い規模は各プログラム
自身のデフォルト規模（1 行 6〜410 ms）で、その数字の大半は JVM の起動が占める：それが示すのは起動と小さな
プログラムであって、**スループットではない**。README に以前書かれていた「`bench_fib`／`bench_oop`／`bench_string`
が 3.5〜5 倍速い」はこの組（**短い規模**、今日の計測は 3.51／4.27／3.81 倍）であり、短い規模をスループットと
読むのがあの誤りだった。長い規模は同じプログラムを**各行が少なくとも数百ミリ秒**になるまで拡大し（5 行が 1 秒超、
文字列の 2 行は 0.3〜0.8 秒の間）、JVM のウォームアップが済んだ後の答えこそがプログラムの答えになる：

| 長い規模（各行が少なくとも数百ミリ秒） | Teyru | Java | どちらが速い |
|---|---|---|---|
| `bench_fib`（fib(44)） | 1.877 s | 1.800 s | 引き分け（Java が 4% 速い） |
| `bench_loop`（100 万回） | 1.134 s | 1.007 s | Java が 1.13 倍 |
| `bench_oop`（50 万回） | 1.002 s | 0.357 s | **Java が 2.8 倍** |
| `bench_string`（1600 万回の連結） | 0.750 s | 0.285 s | **Java が 2.6 倍** |
| `bench_string_cjk`（400 万回、下記参照） | 0.30 s | 0.15 s | **Java が 2.0 倍** |
| `bench_alloc`（8 億回の割り当て） | 1.398 s | 0.204 s | **Java が 6.8 倍** |
| `bench_invoke`（2 億回のリフレクション呼び出し） | 4.066 s | 1.859 s | **Java が 2.2 倍** |

**長い実行では Teyru が勝った行は 1 つもなく、`bench_fib` が唯一の引き分けの行である。** 短い規模で勝った
3 行は、長い実行ではそれぞれ引き分け、2.8 倍遅い、2.6 倍遅いに変わる：同じ 2 つの実装で、仕事量を拡大した
だけで差が逆転する。

**`bench_alloc` の行はコレクタとは関係がない。** `TEYRU_GCTRACE=1` で 8 億回の割り当てを計測すると**コレクション
は 0 回**だった：コンパイラのエスケープ解析が `Cell` をスタック上に残し、そのフィールドをレジスタに昇格させる
（`AGENTS.md` §10）ので、この行が計測しているのはループそのものであってアロケータではない——6.8 倍遅いことも
「GC が遅い」では説明できない。

**`bench_invoke` は 2.2 倍遅く、コストが高いのはリフレクションではなく割り当てである。** 短い規模の版
（2000 万回）は 1.63 倍遅い（0.4094 s 対 0.2517 s）。分解すると、Teyru 自身の**直接呼び出し**のループがすでに
Java より 2.6 倍遅く（2000 万回で 26 ms 対 10 ms）、リフレクションがさらに機構のコストを上乗せする：リフレクション
呼び出し 1 回あたり 19 ns（プログラム自身が出力した 2 億回で 3.798 s、表の 4.066 s はこれに直接呼び出しの 1 周分
を加えたもの）。`AGENTS.md` §10 の消去法による分解では、コストが高いのは `Object` API に強いられる 2 回の割り当て
（呼び出し側の引数のボックス化、実行時 invoker thunk 内の結果のボックス化）で、Java もその 2 回の割り当てを
払っている——違いはアロケータとオブジェクトサイズ（Teyru の `new Object()` は 32 バイト、Java は 16）。
**その分解は古い全体コスト（1 回 27 ns、20M 回で 540〜601 ms）で計測したもので、今回は 19 ns であり、分解自体は
再計測していない。**

**`bench_string_cjk` の行は今は比較になっているが、以前はそうではなかった。** 以前こちらの `String` はまだ UTF-8
のバイトで、`charAt` が読むのは文字ではなくバイトだったので、両側が同じ規模パラメータを受け取っても**違う仕事**を
していた——400 万回で Teyru は `walked=0`、Java は `walked=1000` を出力し、スクリプトはそれに対して比ではなく
`!(out)` を出力した（2 つの実装を比べる表に、2 つの違うことを比べる行があってはならない）。文字列セマンティクス
（W5）が着地した後、両側が出力するのは**同じ行**（`concat=88890 walked=1000 sliced=4`、10,000 と 4,000,000 の
2 つの規模でバイト単位に同一）なので、これは比の行である：**Teyru 0.30 s、Java 0.15 s、Java が 2.0 倍速い**
（ローカル linux/amd64、load 0.77、5 回のうち最良、2026-10-16）。この行はこの組で最も高価な行の 1 つである。
インデックスごとに UTF-16 から WTF-8 への変換が 1 回必要だからだ——それこそがこの行の存在理由である。

**プログラムではない 3 行**：起動 100 回で 0.0763 s 対 2.1093 s（**27.6 倍**）。hello 実行ファイルは **66,808
バイト**（`-O2`、同じプログラムで `-O1` は 85,440、`-O3` は 70,248。この数字はほぼ着地のたびに動くので、
それは上のツリーに属する——**2026-10-16 に今日のツリーで計り直すと 97,888 バイト**、`-O0` 233,680、
`-O1` 209,472、`-O3` 106,032。差はフレームマップの設計コストと、修正済みの `CharSequence` の退行によるもので、
<https://docs.teyru.dev> のサイズの節を参照）；ピークメモリは 4,348 kB 対 51,420 kB（**11.8 倍少ない**）。メモリの
行は 5 回のうち**最大**を取るのであって最小ではない：最小の回が反映しているのは、そのラウンドがヒープをメモリに
触れさせなかったことであって、プログラムが必要とする量が少ないことではない（同じ hello で 20 回のうち 1 回は
2,116 kB、残りは 4,150〜4,450）。

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
