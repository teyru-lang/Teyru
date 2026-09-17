# Third-party notices / 第三方元件聲明

本文件列出 Teyru 專案實際使用與散布的第三方元件。專案本身的授權見 `LICENSE`。

> 編譯器只用 Go 標準函式庫，`go.mod` 沒有任何外部模組依賴。產生執行檔會連結到的第三方程式碼
> 有三處：POSIX 目標上的 OpenSSL（TLS，動態連結）、`windows/amd64` 目標上的 mingw-w64 執行期
> （`-static`，靜態連結進產物），以及**主機自己**的 IANA tzdata（由主機散布，本專案不散布、
> 不內建）。執行期本身（`internal/runtime/src`）與標準程式庫（`lib/*.teyru`）都是本專案的原創
> 程式碼。
>
> 這份清單不是靠人記住的：`scripts/check-notices.sh`（`make notices`）拿它對照倉庫——執行期的
> 檔案清單、OpenSSL 的版本下限、`windows/amd64` 的連結旗標、tzdata 的讀取路徑、授權檔案是否
> 還在、Unicode 資料檔出現時有沒有列名。兩邊不一致時它以非零狀態結束。

---

## 1. 建置編譯器所需

| 元件 | 用途 | 授權 | 散布方式 |
|---|---|---|---|
| Go 標準函式庫（`go1.26` 以上） | 建置 `cmd/teyru` 與 `internal/*` | BSD-3-Clause（Copyright The Go Authors） | 不散布，使用者自行安裝 |

Go 標準函式庫以 BSD 3-Clause 授權釋出，條文見
<https://go.dev/LICENSE>。本專案未修改標準函式庫，也未將其嵌入產物。

---

## 2. 產生執行檔所需（後端）

| 元件 | 用途 | 授權 | 散布方式 |
|---|---|---|---|
| clang / LLVM（預設後端） | 將產生的 C 編譯成原生執行檔 | Apache-2.0 with LLVM Exceptions | 不散布，使用者自行安裝；亦可用 gcc 取代 |
| GCC（替代後端） | 同上 | GPL-3.0-or-later（執行期例外） | 不散布，使用者自行安裝 |
| libc / libm | 執行期使用的系統函式庫 | 依系統而異（多為 LGPL-2.1+ 或 MIT） | 動態連結，不散布 |

產生的執行檔**連結**這些函式庫，但不包含其原始碼。Teyru 執行期
（`internal/runtime/src`）本身是本專案的原創程式碼，與上述元件無授權關聯。

執行緒用的是系統的 pthread（glibc 2.34 之後併入 libc），另外連結的是 `-lm` 與 `libc`：
`ldd` 一支只有 `System.out.println` 的程式只看得到 `libm.so.6` 與 `libc.so.6`。這是**動態**
連結，所以那支執行檔不含 libc 的程式碼。

---

## 3. 執行檔連結的第三方函式庫

| 元件 | 用途 | 授權 | 散布方式 |
|---|---|---|---|
| OpenSSL（`libssl`、`libcrypto`） | TLS；實作在 `internal/runtime/src/tyrt_tls.c`，只有程式的可達程式碼碰得到 TLS 時才編譯與連結 | 1.1.1 是 OpenSSL／SSLeay 雙授權；3.0 以後是 Apache-2.0 | 動態連結（`-lssl -lcrypto`），不散布 |
| mingw-w64 執行期（crt 與 winpthreads） | `windows/amd64` 目標的 C 執行期與執行緒；那個目標用 `x86_64-w64-mingw32-gcc` 加上 `-static` 建置 | Zope Public License 2.1（`mingw64-crt` 套件附的 `COPYING`；同一個套件的 `DISCLAIMER.PD` 另外標明部分檔案為 public domain，實際檔案另有標成 BSD 或 LGPL 的部分） | **靜態連結進產物**，所以散布執行檔就是散布它 |
| Microsoft C 執行期與 Windows 系統 DLL（`msvcrt.dll`、`KERNEL32.dll`、`WS2_32.dll`） | Windows 目標的 C 函式庫與 Winsock | Windows 的一部分 | 不散布；產物只有 import，載入時由系統提供 |
| 主機的 IANA tzdata（TZif 檔） | `lib/46_timezone.teyru` 讀時區資料庫 | 由散布它的主機決定。本機（Fedora 44）的 `tzdata` 套件標為 `LicenseRef-Fedora-Public-Domain AND (GPL-2.0-only WITH ClassPath-exception-2.0)` | **不散布、不內建**；見下面〈tzdata〉 |

### OpenSSL

TLS 是這個專案唯一**不自己實作**的一層，而它只有碰得到才進執行檔。`internal/driver/driver.go`
的目標表在 `linux/amd64` 與 `linux/arm64` 的 `tlsLibs` 寫著 `-lssl`、`-lcrypto`：兩個都要，
因為 X509 與 EVP 的呼叫是 libcrypto 的，而 `-lssl` 不是每個平台的連結器都會順帶拉進它。
`ldd` 一支用到 TLS 的程式（`tests/programs/t191_tls_keepalive.teyru`）看得到
`libssl.so.3` 與 `libcrypto.so.3`——**動態**連結，所以產物裡沒有 OpenSSL 的程式碼，
libssl 的條款不隨我們的執行檔散布。

**版本下限是 1.1，而且它是一個前置處理器的 `#error`**，寫在 `internal/runtime/src/tyrt_tls.c`：

```c
#if OPENSSL_VERSION_NUMBER < 0x10100000L
#error "the TLS layer needs OpenSSL 1.1 or newer (...)"
#endif
```

`SSL_set1_host`、`BIO_meth_new`、`TLS_client_method` 與 `SSL_CTX_set_min_proto_version`
都不在 1.0.x 裡。下限寫在編譯期，所以拿 1.0.2 的標頭建置看到的是那句話，而不是一個他沒寫過的
執行期檔案裡成頁的「未宣告的識別字」。`scripts/check-notices.sh` 從那個 `#if` 讀出十六進位的
下限，比對的就是本節這一個字串。

### mingw-w64 執行期

`windows/amd64` 的連結旗標是 `-lws2_32 -static`（Winsock，以及讓產物自足——不加 `-static`
每個程式都要旁邊放一個 `libwinpthread-1.dll`）。因此 mingw-w64 的執行期（crt 與 winpthreads）
是**靜態連結**進去的：實測 `x86_64-w64-mingw32-objdump -p` 對一支 hello 只看到
`KERNEL32.dll`、`msvcrt.dll`、`WS2_32.dll` 三個 import，而 `pthread_create` 這類符號定義在
產物裡面、沒有任何 `libwinpthread` 或 `libgcc_s` 的 import。散布那支執行檔時，mingw-w64 執行期
的條款（ZPL-2.1，與檔案上另外標明為 public domain、BSD 或 LGPL 的部分）跟著走。

### tzdata

時區資料在**主機**上，不在這個倉庫裡，也不在任何產物裡：`lib/46_timezone.teyru` 讀主機的 TZif
檔，目錄取自 `$TZDIR`（有設且非空時）否則 `/usr/share/zoneinfo`，預設時區取自 `TZ` 或
`/etc/localtime`。UTC 與 `GMT`／`+08:00` 這種數值 id 是內建的，所以只用到 UTC 的程式不需要
tzdata；其餘的區域在沒有 tzdata 的主機（Windows，或沒裝 tzdata 的容器）上以
`ZoneRulesException` **指名拒絕**，不會答錯。資料的著作權跟著散布它的主機（發行版的 `tzdata`
套件），本專案不對它主張任何權利，也不重新散布它。

---

## 4. 執行期與標準程式庫

| 元件 | 來源 | 授權 |
|---|---|---|
| Teyru 執行期（`internal/runtime/src/` 整個目錄） | 本專案原創 | 見 `LICENSE` |
| Teyru 標準程式庫（`lib/*.teyru`） | 本專案原創，以 Teyru 撰寫 | 見 `LICENSE` |

執行期不再逐檔列名，而是**按目錄宣告**，清單由腳本產生、也由同一個腳本核對（下面的區塊是產生
的，不要手改；改執行期之後執行 `make notices` 重新產生）：

<!-- BEGIN GENERATED: internal/runtime/src -->
執行期目前有 11 個檔案：

- `tyrt.c`
- `tyrt.h`
- `tyrt2.c`
- `tyrt_net.c`
- `tyrt_net.h`
- `tyrt_plat.h`
- `tyrt_plat_posix.c`
- `tyrt_plat_win.c`
- `tyrt_reflect.c`
- `tyrt_thread.c`
- `tyrt_tls.c`
<!-- END GENERATED: internal/runtime/src -->

垃圾回收器、字串、陣列、例外與 box 類別都是本專案自行實作，沒有 Boehm GC、沒有 libgc、
沒有 mimalloc 之類的配置器。執行期唯一的第三方程式碼是碰得到 TLS 時連結的 OpenSSL（見 §3），
它是動態連結，執行期檔案自己不含它的程式碼。

---

## 5. Unicode 資料與產生的查表

`Character` 與大小寫映射需要 Unicode 的資料，所以倉庫會帶著 Unicode 15.0 的資料檔
（`UnicodeData.txt`、`SpecialCasing.txt`、`CaseFolding.txt`、`PropList.txt` 與同批的其他檔案）
以及由它們產生、入庫的兩級查表。這些檔案依 **Unicode License v3**（UNICODE LICENSE V3，
Copyright © 1991-2024 Unicode, Inc.，條文見 <https://www.unicode.org/license.txt>）散布，
著作權聲明隨檔案保留。

版本固定 15.0 是為了與參考實作對齊：JDK 21 用的是 Unicode 15.0，所以 `Character.isLetter`
與大小寫映射的答案以它為準。這一段在資料檔入庫前後都成立；`scripts/check-notices.sh` 會在樹裡
出現這些資料檔時要求本節已經列名。

---

## 6. 編輯器工具（另一個倉庫：[`teyru-lang/editors`](https://github.com/teyru-lang/editors)）

編輯器支援不在這個倉庫裡（`08dd6ca` 之後，測試、文件與編輯器支援各自獨立成庫），所以下表描述的
是那個倉庫的內容，不是本倉庫的檔案：

| 元件 | 來源 | 授權 |
|---|---|---|
| `tree-sitter-teyru/src/tree_sitter/alloc.h`、`array.h`、`parser.h` | 從 [tree-sitter](https://github.com/tree-sitter/tree-sitter) 複製，供產生的剖析器使用 | MIT（Copyright (c) 2018 Max Brunsfeld） |
| `tree-sitter-teyru/src/parser.c` | 由 tree-sitter CLI（v0.25.10）從 `grammar.js` 產生，內含 tree-sitter 的執行期程式碼 | MIT（同上）；`.gitattributes` 標為 generated |
| `tree-sitter-teyru/grammar.js`、`queries/`、`test/` | 本專案原創 | GPL-2.0-only，見同倉庫的 `tree-sitter-teyru/LICENSE` |
| `vscode/`（TextMate 語法、語言設定、片段） | 本專案原創 | 見本倉庫的 `LICENSE` |

tree-sitter 的 MIT 條文要求著作權聲明與許可聲明隨所有副本保留，本文件這一節與
上游的 `LICENSE` 一併滿足。這些檔案只被編輯器用來做語法高亮與剖析，不會進入
編譯器，也不會進入任何產生的執行檔。

---

## 7. 測試與工具

| 元件 | 用途 | 授權 |
|---|---|---|
| Go 測試框架（`testing`） | `go test ./...` | BSD-3-Clause（Go 標準函式庫的一部分） |
| OpenJDK / HotSpot | **只用於** `scripts/bench.sh` 的對照量測 | GPL-2.0 with Classpath Exception |

`scripts/bench.sh` 需要 `java` / `javac` 才會執行 JVM 那一半；若系統沒有安裝，
腳本只會跳過該部分，不影響 Teyru 的建置與測試。JVM 不是 Teyru 的執行環境，
也不是任何產物的依賴。

---

## 8. 授權檔案

| 檔案 | 說明 |
|---|---|
| `LICENSE` | 本專案的主要授權 |
| `LICENSE-CLASSPATH-EXCEPTION-2.0` | Classpath Exception 全文（條文來自 OpenJDK，隨主要授權一併保留） |
| `tree-sitter-teyru/LICENSE`（在 `teyru-lang/editors`） | 該文法本身的授權（GPL-2.0-only，與本專案主要授權相同） |

若發行時修改了上述任何一項（例如把 clang 或 OpenSSL 靜態連結進產物，或把 Go 標準函式庫
嵌入發行包），必須重新產生本文件並附上對應的授權全文；執行期的檔案清單則執行
`make notices` 重新產生。

---

## 9. 需要 owner 確認

本節只記下問題，**不自行下結論**（依計畫 W11 第 3 條）：

- 本專案的主要授權是 GPL-2.0 with Classpath Exception（`LICENSE` 與
  `LICENSE-CLASSPATH-EXCEPTION-2.0`）。OpenSSL 3.0 以後是 Apache-2.0，而 `linux/amd64` 與
  `linux/arm64` 的產物**動態連結**它；1.1.1 則是 OpenSSL／SSLeay 雙授權。Classpath Exception
  的條文（見 `LICENSE-CLASSPATH-EXCEPTION-2.0`）允許連結獨立模組並以自己選擇的條款散布產物，
  但它與 Apache-2.0 的相容性、以及散布「靜態連結了 mingw-w64 執行期（ZPL-2.1）」的
  `windows/amd64` 產物時的義務，屬於授權判斷，由 owner 決定。
- 在這條有答案之前，本文件只敘述事實：哪個目標連到什麼、怎麼連、各自的條款是什麼。
