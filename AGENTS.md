# AGENTS.md — Teyru 專案工作規範

> 適用對象：本儲存庫內所有人類貢獻者與 Coding Agents。
> 版本 3.1（2026-09-14）。v1.0 是 Java/JVM 時期的規範，已於 v2.0 作廢；本版補齊
> util 分離、禁止佔位、文件與測試的完整規範，並記下拆分之後的倉庫佈局。

---

## 倉庫佈局

| 倉庫 | 內容 |
|---|---|
| `teyru-lang/Teyru`（本倉庫） | 編譯器（Go）、執行期（C）、標準程式庫（`lib/*.teyru`）、範例（`examples/`） |
| `teyru-lang/tests` | 端到端測試資料，以 **submodule** 掛在 `tests/`：第一次 clone 要 `git submodule update --init` |
| `teyru-lang/docs` | 使用者文件（fumadocs），發佈在 <https://docs.teyru.dev> |
| `teyru-lang/editors` | VS Code／Zed／JetBrains 擴充、tree-sitter 文法 |
| `teyru-lang/website` | 官網原始碼，發佈在 <https://teyru.dev> |

**產品的文件只有一份**，在 `teyru-lang/docs`：改了行為就改那裡，不要在程式庫裡再放
一份。本倉庫只留 `README.md`（倉庫門面與安裝方式）、`AGENTS.md`（本檔）與範例自己的
說明。

---

## 0. 產品契約（不可協商）

1. **語言名稱固定 Teyru**：副檔名 `.teyru`、語言 ID `teyru`、CLI `teyru`。
   不要重新命名的是這些；程式庫的模組路徑跟著倉庫走，目前是
   `github.com/teyru-lang/Teyru`（語言的家在 `teyru-lang` 組織）。
2. **編譯器用 Go 撰寫，只用標準函式庫。** `go.mod` 目前沒有任何 `require`，
   必須維持。要引入第三方模組前，先證明標準函式庫做不到。
3. **不依賴 JVM、javac、bytecode。** 產物是原生執行檔。任何「先產生 Java 再交給
   javac」、「產生 bytecode」、或執行期需要 JVM 的設計，都違反產品契約。
4. **編譯路徑固定**：`.teyru → Go 前後端 → C → clang/LLVM（或 gcc）→ 原生執行檔`。
   執行期在 `internal/runtime/src`，以 C 撰寫。
5. **標準程式庫以 Teyru 本身撰寫**（`lib/` 下的 `*.teyru`）。只有在不能用
   Teyru 表達的原生操作才能標記 `native`，並在執行期提供實作。
6. **效能是產品目標，但宣稱必須可重現。** 任何「比 X 快」的說法都要附上
   `examples/bench_*.teyru` 與 `scripts/bench.sh` 可重現的量測環境，並誠實標出劣勢情境。

---

## 1. 事實與證據

1. 不得偽造測試、效能、部署或相容性結果。跑不動就說跑不動。
2. 文件中的數字必須來自實際執行，並註明量測方式與環境。
3. 「支援某語言特性」的定義是：`tests/programs/` 有對應程式，且
   `go test ./...` 通過。只會剖析不算支援。
4. 不確定的事情寫「未驗證」，不要寫成已完成。

---

## 2. 套件邊界與 util 分離

依賴方向固定，**不得反向**：

```
source → lexer → parser → ast → sema → codegen
             util ←───────────┴────────┘
       runtime（C）與 prelude（Teyru）獨立於 Go 端
```

- `internal/util` 放**前後端共用**的工具，目前有：
  - `name.go`：`Mangle`（C 識別字修飾）、`Capitalize`（JavaBeans 命名）、
    `Descriptor`（型別單字元描述）、`Signature`（參數列表查詢鍵）、
    `FloatLiteral`（Java 的浮點字面值印刷）。
  - `layout.go`：`SizeOf`、`AlignOf`、`Align`、`FieldLayout`、`IsRef`、`IsPrim`。
- **禁止在兩個套件各寫一份相同的工具。** 只要一段邏輯同時被 `sema` 與 `codegen`
  需要（型別描述、名稱修飾、C 版面配置…），就必須放進 `internal/util`，
  由兩邊呼叫。`codegen` 只保留極薄的轉呼（例如 `func mangle(s string) string`）。
- 只在單一套件內使用的輔助函式留在原套件，不要為了「集中」而搬到 util。
- 新增 util 函式時要附單元測試（`internal/util/*_test.go`）。

---

## 3. Go 程式碼風格

- 一律 `gofmt`；提交前 `go vet ./...` 必須乾淨。
- 匯入分三組（標準函式庫 → 第三方 → 本專案），不使用全限定名稱、不使用
  wildcard import、不使用未使用的匯入。
- 錯誤用診斷回報（`diags.Errorf` / `ctx.errf`），**不要在編譯器路徑 panic**。
  panic 只允許出現在「不可能發生」的內部不變式，且必須附註解說明為何不可能。
- 註解解釋**為什麼**，不要逐行翻譯程式碼。匯出符號要有 doc comment。
- 不用魔法數字：抽成具名常數（例如 `util.SizeRef`、`TY_CHUNK`）。
- 偏好既有模式：新功能先找同類節點怎麼做，再照著做。

---

## 4. C 程式碼（執行期與產生的程式碼）

- 產生的 C 以 `-std=gnu11` 編譯，允許 GNU 敘述運算式（statement expression）；
  這是刻意的選擇，讓運算式型別轉換與臨時變數保持單一運算式。
- 執行期必須在 `gcc -c -O2 -Wall -Wextra` 下沒有警告。
- 需要被 GC 追蹤的物件一律走 `ty_alloc`／`ty_alloc_arr`，**不得直接 `malloc`**。
- 新增或調整類別欄位時，`tyclass.refoffs` 必須同步更新。產生器會依 C 的對齊規則
  用 `util.FieldLayout` 計算；**修改欄位配置一定要重跑測試**，
  漏掉位移會讓 GC 回收存活物件，多一個位移會讓它解引用垃圾。
- 執行期函式只要參數或回傳是物件，就用 `void*`；呼叫端負責轉型。

---

## 5. 禁止佔位與多行未實作

1. **禁止留下多行的 TODO、`/* unimplemented */`、或回傳未定義值的空殼。**
   若某條路徑真的無法實作，必須：
   - 讓它呼叫明確的失敗路徑（例如 `ty_unimplemented("Class.method")`，以狀態 70 結束），
   - 並在文件站（`teyru-lang/docs`）的「尚未實作」列出。
2. 禁止「編譯器接受了但執行期默默回傳垃圾」的組合。寧可拒絕編譯，也不要產生
   行為未定義的程式。
3. 禁止用註解掉的程式碼當作待辦事項；要嘛刪掉，要嘛在文件站開一節說明。
4. 文件與程式碼必須一致：改了行為就同步改 `teyru-lang/docs` 的對應頁面
   （語言參考、診斷碼、該主題那一頁），並在 §9 的表格裡找到它。

---

## 6. 測試規範

- **計時要量「跑起來」的那支程式，不是 `teyru run`**：`teyru run` 會先編譯再執行，`/usr/bin/time -v`
  看到的 user／sys 幾乎全是編譯（clang＋LTO），一支「整支程式只有 `Thread.sleep(4000)`」的程式把該行刪
  掉後兩者一樣約 1.9 s／0.2 s，而編譯好的執行檔是 user 0.00／sys 0.00、wall 4.00 s、1 次 voluntary
  context switch。要量時間就 `teyru build` 一次再量那支執行檔（`sh scripts/bench.sh` 即如此做）。
  另一個同類陷阱：**shell 自己的 `time` 關鍵字報的是外層 job 的 CPU，不是程式的**——同一個執行檔，
  shell 的 `time` 說 user 14.3／sys 37.6，`/usr/bin/time -v` 說 0.00／sys 0.00。要數字就別用 shell 的。
- **端到端**：在 `tests/programs/` 放 `xxx.teyru` 與 `xxx.expected`。
  需要命令列參數時另外放 `xxx.args`（每行一個）。`go test` 會自動編譯並比對輸出。
  `tests/` 是 `teyru-lang/tests` 的 submodule：改測試要在**那個**倉庫提交，這裡只會
  動到 gitlink。submodule 沒 checkout 時 `make test` 與 `go test` 都會直接說清楚，
  不會安靜地零測試通過。
- **診斷**：在 `driver_test.go` 的 `TestDiagnostics` 加入「應該被拒絕」的案例與期望
  錯誤碼。
- **單元**：`internal/util` 等純函式要有 table-driven 測試。
- 修 bug 的順序固定：先寫一個會失敗的測試 → 修到通過 → 再提交。
- 提交前至少跑：

  ```sh
  go build ./... && go vet ./... && go test ./... -count=1
  ```

- 效能相關的改動要附上 `sh scripts/bench.sh` 的前後數字。

---

## 7. 診斷碼規範

- 格式 `TY-<階段>-<四位數字>`，階段為 `SYN`（詞法與語法）、`TYP`（語意）、
  `PROP`（property）、`INT`（編譯器內部）、`IO`（檔案）。
- 代碼一旦發布就不再改變意義；新增要往後編號，不要重複使用已刪除的號碼。
- 每個代碼都要在文件站的〈診斷碼〉那一頁有一列說明（訊息、原因、修法）。
- 訊息格式：小寫開頭、不超過一行、用 `%s` 帶入符號名稱，不要有大寫縮寫。

---

## 8. 提交與 PR 規範

- 訊息用範圍前綴：`feat(compiler):`、`fix(runtime):`、`feat(codegen):`、`test:`、
  `docs:`、`chore:`。
- 一個提交做一件事；不要把改名、重構與新功能混在一起。
- PR 描述要寫：動機、做法、如何驗證、已知限制。
- **不得向任何不屬於 `teyru-lang` 的倉庫提交**，PR、issue、commit 皆然。這包括
  **GitHub 的語言倉庫 `github-linguist/linguist`**（`.teyru` 要被 GitHub 辨識成語言
  就是往那裡送；先前送過一次 `Add Teyru`，被關閉，那次的教訓是這種事不該由 agent
  代勞）與我們拿來當規格參考的上游：OpenJDK、Go、Gson、tree-sitter、VS Code／Zed 等。
  讀它們的原始碼、抄它們的設計是工作的一部分，往那裡送東西不是；要不要讓 GitHub
  認得這個語言，由維護者自己決定。需要上游的東西就在本倉庫開 issue 說明。

---

## 9. 文件規範

| 檔案 | 內容 | 什麼時候要改 |
|---|---|---|
| `README.md`（本倉庫） | 倉庫門面：這是什麼、怎麼安裝、其他倉庫在哪 | 專案定位或安裝方式改變時 |
| `examples/*/README.md`（本倉庫） | 範例自己的說明 | 範例改變時 |
| `AGENTS.md`（本倉庫） | 本檔 | 流程改變時 |
| `teyru-lang/docs` 的〈語言參考〉 | 完整語言參考 | 語法或語意改變時 |
| `teyru-lang/docs` 的〈診斷碼〉 | 每個診斷碼的說明 | 新增／修改診斷碼時 |
| `teyru-lang/docs` 的〈Lombok〉 | 支援狀態、產生的成員、與 Lombok 的差異 | 新增或調整標註支援時 |
| `teyru-lang/docs` 的〈原生互通〉 | 以 C 實作 native 方法、符號命名與型別對應 | native 介面或 CLI 旗標改變時 |
| `teyru-lang/docs` 的〈架構〉 | 編譯流程與執行期模型 | 架構改變時 |
| `teyru-lang/docs` 的〈JSON〉／〈Web 框架〉／〈模組〉 | 各主題的完整說明 | 該主題行為改變時 |
| `teyru-lang/tests` | 端到端測試資料 | 新增或修改測試時 |
| `teyru-lang/editors` | 編輯器擴充與文法 | 語法或關鍵字改變時 |

文件站在 `teyru-lang/docs`，`README` 的英／日／簡中版本也在那裡（對應語言的頁面），
四種語言的內容要一致：改了其中一份就要改其餘的結構。

---

## 10. 已知限制（不要當成已完成）

- checked exception 沒有編譯期檢查。
- `sealed` 的 `permits` 子句沒有被驗證：沒有 `permits` 的 sealed 型別在 switch
  窮盡性上被視為不可判定而要求 `default`。
- 反射在 `lib/26_reflect.teyru`：`Class`、`Field`、`Method`、`Constructor`、
  `Modifier`、`Array` 與 `java.lang.reflect` 的六個例外，讀的是編譯器為每個類別
  產生的靜態表（欄位、方法、修飾子、列舉常數），所以查一次資料是走一次陣列，
  執行期不建表。與 Java 的差異（都已實測，不是未驗證）：
  - 類別名是 Teyru 的：`String.class.getName()` 是 `teyru.String`，
    `Class.forName` 兩種寫法都收（`java.lang.String` 會找到同一類別）。
  - 註解反射有，但元素是**按名字讀**：`Class`／`Field`／`Method`／`Constructor`
    上的 `getAnnotations()`、`getAnnotation(Class)`、`isAnnotationPresent(Class)` 是
    Java 的，`Annotation` 則沒有「每個註解型別一個實作類別」——所以要
    `ann.stringValue("value")`／`intValue`／`booleanValue`／`doubleValue`／
    `classValue`／`enumValue`，而不是 Java 的 `ann.value()`。沒寫的元素讀得到介面
    宣告的預設值（parser 保留 `default`）；`@Retention` 收得下但沒有作用；陣列型別
    的元素值不帶（讀它會說不支援）。
  - 所有陣列共用一個類別，因此沒有 `getComponentType`、沒有每個元素型別的陣列
    類別，`forName("[I")` 也沒有東西可回答。
  - 沒有泛型型別參數的反射（`getGenericType` 等不存在）。
  - 原生型別的取值器只收完全相符的裝箱型別，Java 的拓寬（例如對 `byte` 欄位
    呼叫 `getInt`）在這裡是 `IllegalArgumentException`。
  - 存取控制不檢查：私有成員可以直接讀寫。final 一律攔，`setAccessible(true)`
    之後可以寫 instance final，但 static final 一律拒絕（javac 也是這樣：JDK 不再
    讓它通過，即使 setAccessible 過）。
  - 內部類別與區域類別不能被反射建構（沒有外圍實例可用）。
  - 成員表（欄位、方法與 invoker）只在使用者程式真的可能用到反射時才寫進執行檔
    （`emit.go` 的 `reflectUsed`）：它們是唯一會指名別的類別的中繼資料，擺在檔案
    層級會把整個標準程式庫釘進每一支程式（實測 hello world 從 445.9 KB 漲到
    2.9 MB）。用到反射的程式仍要付出整份約 3 MB，這是這個設計尚未解決的成本。
  - 啟動時的尖峰記憶體（hello world）從 2.2 MB 變成 4.1 MB：加入反射翻譯單元之後，
    LTO 不再丟掉那塊 8 MB 的 `ty_roots` 保留區與相關符號。差異已量到，根因未定——
    同一支程式額外帶一塊 8 MB 的未觸碰 `.bss` 時 RSS 只多 64 kB，所以不是那塊保留區
    本身；把 `forName` 的表拿掉也只降回約 4.1 MB。
- JSON 綁定（`lib/27_json_binding.teyru`）讀的是類別本身：基本型別、字串、`char`、
  列舉、`Object`、巢狀類別、record，以及**容器**（`List`／`Set`／`Map`／`Deque` 等
  介面與其實作、陣列）都已往返，容器裡的**物件元素**也已綁定：元素型別由編譯器寫
  進欄位描述子（`internal/codegen/reflect.go` 的 `tyfield.elem`），
  `Field.getElementType()` 讀得到——反射本身仍然沒有泛型型別（沒有
  `getGenericType`），元素型別是另外記下來的。`GsonBuilder` 的旋鈕
  （`serializeNulls`／`setPrettyPrinting`／`setLenient`／`setFieldNamingPolicy`
  的五種政策／`excludeFieldsWithModifiers`／`excludeFieldsWithoutExposeAnnotation`／
  `setVersion`）、`@Expose`／`@Since`／`@Until`／`@SerializedName` 的過濾與改名、
  `JsonSerializer`／`JsonDeserializer` 轉接器（`registerTypeAdapter`／
  `registerTypeHierarchyAdapter`／`@JsonAdapter`）與串流 `JsonReader`／`JsonWriter`
  都已完成。尚未完成的是：
  - 元素型別只記**一層**：`List<List<Person>>` 的內層元素按文件的形狀判讀，裡面是
    物件或陣列就明確拒絕；宣告為 `Object` 的元素（含 raw `List` 與未定界型別變數的
    抹除結果）也一樣。
  - 陣列的 component 是 `? super` 或未定界型別變數時沒有類別可以建立陣列，該成員
    明確拒絕。
  - `@SerializedName` 的 `alternate` 沒有實作：元素是陣列型別的註解值反射不帶（讀它
    會說不是 String），所以多個名稱要用**重複標註**寫，第一個是寫出用的名字，其餘是
    讀入也接受的替代名。
  - `@JsonAdapter` 用字串命名轉接器類別（`Class` 型別的註解元素留不到執行期），名稱
    以 `Class.forName` 查。
- 執行緒只有一部分：`Thread`（`Runnable`、`start`／`join`／`sleep`／`yield`／
  `currentThread`／`getId`／`getName`／`isAlive`）、真的 `synchronized`（含
  `synchronized` 方法修飾子，方法會持有監視器整段）與 `Object.wait`／`notify`／
  `notifyAll`。執行期在 `internal/runtime/src/tyrt_thread.c`，端到端測試是
  `tests/programs/t159_threads.teyru`。**沒有的**：`interrupt`、daemon、優先權、
  `ThreadGroup`、`ThreadLocal`、堆疊大小與逾時的 `join(long)`，以及未處理例外的
  handler（執行期印出 Java 預設處理常式那一行，然後結束那個執行緒、行程繼續）。
  GC 是**合作式**停止世界：安全點在每個迴圈回邊（產生器會放）、配置慢路徑、等
  heap 鎖，以及每個會阻塞的呼叫。因此一個既不迴圈、不配置也不阻塞的執行緒
  （例如卡在原生 `read()` 裡）會讓收集等它，直到它回來；這是已知限制，不是未
  驗證的不確定性。每個執行緒有自己的配置區（slab），單執行緒程式的配置速度不變。
- **執行檔大小：vtable 讓 LTO 丟不掉整個標準程式庫（已修）**：C 後端把每個方法主體與類別表
  整份寫出，靠 LTO 丟掉不可達的——但 **LTO 丟不掉「位址被取用」的函式，而 vtable 正是一堆取
  位址**（`vt_X[i] = (void*)M_X_i`）。`cls_X` 一旦存活（main 會安裝 String、Object、陣列、八個
  裝箱類別與十八個例外類別），它的每個實例方法就跟著存活，每個方法再指名它用到的類別，閉包於
  是吞掉大半 prelude：hello world 只印一個字串，卻因為 `String.lines()` 與 `String.length()`
  並排在同一個活著的表裡而帶進 `java.util.stream`。實測 hello 裡 1262 個存活函式中**只有 42 個
  能從 main 直接呼叫到達**，其餘 1233 個是靠取位址活下來的。LTO 本身沒變（`--no-lto` 前後差
  17～54 KB），所以責任在後端發出的內容。
  修法在 `internal/codegen/prune.go`：對產生的 C 做不動點掃描，只把「沒有任何可達分派會讀到
  的」slot 初始化改成 `NULL`——**從不重編號或縮短表**（分派索引因此不變），slots 0／1／2 對任
  何存活類別都保留（執行期就是靠索引呼叫那三個），介面表完全不動（原生方法可能用編譯器看不到
  的 selector 經由它分派），文字形狀不符預期就原樣返回。掃描必須是程式可執行範圍的**超集**：
  它第一次的版本用「第一個單獨的 `}`」當函式結尾，而 pattern-switch 的降階會在左邊界產生一個
  內部的收尾大括號，於是那段之後的分派被歸給不存在的定義、slot 被清成 NULL，
  `t133_arrow_blocks`／`t84_sealed_switch`／`t51_java25_tour` 直接 SIGSEGV；改成字串與註解感知
  的大括號計數後才正確。**尺寸（以下皆為 `-O2`，也就是預設值與 `bench.sh` 用的等級；同一支 hello 在 `-O1` 是
  75,232、`-O3` 是 59,408 位元組）**：hello 508,304 → **95,064 位元組**（全部 slot 皆空的下限是
  56,392；9/13 的 `74fa648` 是 48,840），t84 520,752 → 104,408，t133 509,456 → 101,160；用到
  反射的程式不變（`t146_reflect` 4,110,448、`t101_gson` 4,097,840），因為反射會從 main 接上每
  一張成員表，不動點自然留下全部。**速度沒有可量測的變化**（六個基準 best-of-5、每次 5–37 ms
  的差異都在量測解析度內，checksum 相同）。尚未拿到的餘裕：95 KB 對 60 KB 的下限，差在 slots
  0..2 與四個索引（7／12／13／16，來自 `Class.toString/isArray/isInterface/isPrimitive`）對每個
  存活類別都填，而不是只對分派擁有者的子類別填——產生 C 的內文有擁有者資訊，需要一個由類別
  記錄建出的 isSubclass 走訪。



- **配置器的執行緒批次（`tyrt.c`／`tyrt.h`）**：執行緒的 slab 用盡後，慢路徑會一條一條走全域 free
  list 並每次取 heap 鎖；而每次收集都把整塊 slab 塞回去，所以這個「吸乾」會自我延續。現在每個執行
  緒有一張小表（`void *batch[64]`，以尺寸類別索引）：慢路徑先試它，命中就直接發放、**不取鎖也不經過
  安全點**；未命中才取鎖，slab 用盡時一次搬 64 塊同類別進來。大於 1 KB 的區塊不批次。
  **量測**（CPU 時間、綁單核、五次取最小、8 種程式碼佈局取中位數）：`bench_invoke`
  0.5192 → 0.3613 秒（1.44 倍；我自己用 wall time 量到 1.55 倍），`bench_string` 1.04 倍，
  alloc／loop 不變。計數器（儀器化暫存建置）：`bench_invoke` 4 千萬次配置中慢路徑 6,237,022 →
  2,503,735、其中 2,459,660 由批次供應、**heap 鎖取得 6,237,022 → 44,075（少 142 倍）**，收集次數
  不變（305）；`bench_string` 的鎖取得 34,547 → 669。**這個勝利有一半不是配置器**：把 glibc 的
  arena 行為固定住之後只剩 1.12 倍，其餘來自 page fault 從 16,800 降到 2,367。
  **為什麼安全**（比速度重要）：區塊的尺寸類別就是它的尺寸，所以批次發放寫出的內容與取鎖路徑逐位
  元組相同；`collect_slabs` 的**第一個動作**是清空每個註冊執行緒的表（在停止世界之後、建立 block
  starts／標記／掃描之前），因為掃描會重寫 free list，留著舊連結就會把同一塊發兩次；無鎖取用由
  「執行緒狀態為 RUNNING」把關，而那正是收集器不計為已停止的集合，已停止的執行緒會落回取鎖路徑；
  預算檢查留在批次路徑上（執行緒不能靠吸批次延後收集）；批次命中**刻意不是**安全點（程式碼如此註
  記：產生的程式碼在每個迴圈頂端與呼叫處都會停，而預算用盡就會走取鎖路徑）。內聯快路徑未動。
  **記憶體**（批次是收集器無法重用的記憶體）：大約每個執行緒一個批次，64 執行緒壓力下最差一次收
  集總共 3,888 塊／124 kB（每執行緒約 61 塊、1.9 kB）；硬上限 2.1 MB/執行緒；實測成本是**每個曾建
  立的執行緒 512 位元組**，不在執行緒結束時釋放——買回那 512 B 得在執行緒自己的 roots 旁加一個競
  態，不划算。
- **既存缺陷（非上述改動造成，正在查）**：4 執行緒以上的配置壓力測試在**兩個版本都會約 16% 的執行
  崩潰**（交錯量測 61/400 對 70/400）。套件裡的 GC／執行緒程式夠穩定所以看不到它——它們跑的是有人
  想到的形狀，而這是没人想到的那個。這是目前最值得處理的正確性問題。
- **`bench_string` 比 `74fa648` 慢 67%（成因已查明，主因已修）**：交錯 A/B（20 對、雜訊約
  1%、同一台機器）顯示 `bench_alloc` 快 2.19 倍、`bench_loop` 慢 19%（迴圈回邊安全點的既定代
  價）。慢的其實不是字串，**而是配置器終於真的在配置**：在 `39218f6` 之前，慢路徑會把
  `ty_bump` 繞回過期的 `used` 水位，所以 60 萬次配置一直重複使用**同一個 256 KB slab**
  （chunknew=1、只掃 455 個 block、144 次 page fault）——舊版的「較快」是在量一個沒有真正配置
  的配置器。修正水位之後程式第一次真的有堆積（8.6 MB、2205 次 fault、約 3.5 ms），而
  `39218f6` 的 sweep 每次把整塊 slab 的死 block 塞回 free list、配置器再**一條一條走慢路徑**
  把它吸乾（60 萬次配置有 59.5 萬次＝99.2% 走慢路徑，收集次數 8 → 123）；後段
  0.0136 → 0.0157 是執行緒化：每個迴圈頂端一個 safepoint（本程式 20 萬次 atomic load），以及
  慢路徑前後各一對 world mutex ＋ heap mutex（實測 34,547 次慢配置約 0.48 ms、14 ns/次）。
  **已修**：sweep 改為單趟（mark 階段自己記帳 `live_any` 與 `live_bytes`，不再為了判斷整塊
  slab 能否釋放而先走訪每個 block）；等價性用獨立 walk 逐次收集重新推導兩個值比對，六個基準
  加上四個 GC／執行緒測試零不符。`bench_invoke` 587.9 → 534.1 ms（1.101x，與移除的那一趟實測
  58 ms 相符）、`bench_string` 14.43 → 13.87 ms（1.040x），其餘中性；量測取 CPU 時間、taskset
  綁核、8 種程式碼佈局取中位數（熱迴圈機器碼兩版相同，單一佈局會有 ±10% 的假差異）。
  **未修且已量測**：配置器吸乾 free list 的成本（`bench_invoke` 的 31%），修法是在持有 heap
  lock 時整批取進 per-thread 清單，屬配置器設計變更。另一個被量測後否決的想法：把
  block-start bitmap 改成配置時寫入——600k 次 read-modify-write 落在 inline 快路徑上比它省下
  的 walk 更貴（14.19 ms 對 13.89 ms），所以維持每次收集重建。對 Java 仍是 ~3.7 倍快，所以對
  外宣稱沒有變成錯的。

- **基準的量測方法與注意事項**（`/tmp/teyru-bench-report.md`，38 分鐘、10 節）：Java 那一欄
  每次都是全新的 JVM，短程式由暖機主導——同一個 fib(32) 暖機後 Java 只要 5–6 ms，而 Teyru 是
  4 ms，所以「fib 快 4.58 倍」大部分是冷解譯器造成的。`bench_invoke` 必須這樣讀：Teyru 的
  **直接呼叫**迴圈本身就已比 Java 慢 2.6 倍（26 對 10 ms），反射再加機器成本 15.9 對
  6.15 ns/次、裝箱 5.6 對 3.0 ns/次，以及每輪新建一個 `Object[]`（+6.1 對 −1.9 ns/次，Java
  的逃逸分析把它消掉）。所以 0.43 倍不是反射特有的懲罰。
- **收集器的掃描範圍檢查（#57 期間暴露，已修）**：`trace_object` 會把「沒有參照欄位
  的類別」的類別描述子當成候選標記，而 `valid_obj` 原本只用一個 16 位元組對齊測試就打
  發掉它。#57 讓執行期自己的靜態資料位移改變，描述子剛好落在 16 位元組邊界上，於是同
  一個值落進 slab 搜尋，而 slab 快取永遠不會命中 `.data` 位址：每次標記 489 ×
  1,048,627 = 5.129 億次探測。修法是先用 heap 的 slab 範圍篩掉候選（範圍外的字組不屬
  於任何 slab，逐一探測本來也會回 0，所以只會少做事、不會改答案）。同一個 400 萬物件
  的程式：兩次收集由 282.0 + 281.3 ms 變成 37.3 + 37.4 ms，而 #57 之前的基準是
  38.1 + 36.5 ms。單執行緒配置快速路徑不受影響（2000 萬次 `ty_alloc(16)` 11.5 ns）；
  `bench_loop` 仍比 #57 之前慢約 10%，那是迴圈回邊安全點的代價。
- **執行期寫在 Teyru 到哪裡為止（包裝型別那一組已完成）**：八個包裝類別的值語意
- **vtable 剪裁的第二段（已落地）**：第一段只會保留「沒有任何可達分派會讀到的 slot」以外的
  全部；7／12／13／16 這四個索引當時對**每個**存活類別都填，因為掃描看不到那些分派。但產生
  的 C 內文其實帶著擁有者
  （`((RET(*)(OWNER*, ...))((recv)->obj.cls->vtable[N]))`），所以問題只是「接收者能不能是該擁
  有者的子類別」，而類別記錄（`.super`、`.ifaces`）就在同一份 C 裡。掃描現在走這個超集：
  hello 由 95,064 掉到 **55,920 位元組**（`-O2`；`-O1` 是 75,232、`-O3` 是 59,408），
  t84 由 104,408 到 74,888、t133 由 101,160 到 61,064、t51 由 442,040 到 253,328。
  **仍然無條件的是 slots 0／1／2**，而且不能用子類別測試收窄：執行期是拿 `void *`／`tyobj *`
  按索引讀它們的（`print_uncaught` 與字串輔助函式讀 `[0]`、`ty_obj_hash` 讀 `[1]`、
  `ty_obj_equal` 讀 `[2]`），所以它們的擁有者是繼承樹的根。要再往下砍就需要「某個類別永遠不會
  有實例」這個事實，而 C 內文不決定它（物件可以來自存活方法裡的 `ty_alloc`、該類別的區域變數、
  執行期自己的字串／陣列／裝箱／例外，或反射），而且那裡猜錯的代價是跳到 NULL，不是浪費幾個
  位元組。剪裁後的 C 裡量到：628 個 vtable 陣列、511 個有任何填入、**只有一個**在索引 3 以上有
  填入（`Class` 自己的 7／12／13／16）。用到反射的程式不變（各約 4.1～4.9 MB），因為反射會從
  main 接上每一張成員表；五個基準的時間與 checksum 都不變。

- **浮點轉整數的窄化轉換（已修，兩個後端都改）**：C 的 `(int)`／`(long)` 對「放不下的 double」
  是 undefined，所以同一支 `main` 裡的 `(long) 1.0e20` 在四個建置給出四個不同的答案：C 後端
  `-O2` 是 **160**、`-O0` 是 **-9223372036854775808**、`-O3` 是 **0**、LLVM 後端是 **48**——
  常數摺疊的那一份更糟（0、140727421376656、140736752435552、0）。Java 由 JLS 5.1.3 指定：
  NaN → 0、±∞ → 最大／最小值、太大 → 最大值、太小 → 最小值、其餘往零截斷。現在兩個後端都呼叫
  `ty_d2i`／`ty_d2l`（實作在 `tyrt.c`，原型在 `tyrt.h`——LLVM 後端的原型本來就是從那個標頭讀
  的，所以兩邊是同一個語意）：C 後端在 `cast`、`coerce` 與複合指定（`narrowTarget`）三條路徑都會
  發出呼叫，LLVM 後端在 `convertTo` 的浮點→整數分支發出 `call`；窄於 `int` 的目標再做第二步
  （C 是窄化轉型、IR 是 `trunc`），與 Java 的「先到 int 再截斷」相同。因為發出的是有定義的呼叫，
  **常數摺疊也跟著變成有定義**：四個最佳化等級與兩個後端現在都給 9223372036854775807。測試
  `tests/programs/t193_narrowing_saturation`（336 行，期望值由 javac 產生，兩個後端逐行相同）。
  **同族、已修的第二個**：LLVM 後端的複合 `/=`、`%=` 遇到浮點右運算元，會先把右邊窄化到目標型別
  再做整數除法，違反 JLS 15.26.2 的二元數值提升——`int a = 7; a /= 2.5` 得 3（Java 是 2）、
  `a %= 2.5` 得 1（Java 2）、`int m = 2147483647; m /= 0.5` 直接丟 ArithmeticException（Java 是
  2147483647）。C 後端這幾條一直是對的，所以這是一個只在 LLVM 後端存在的缺陷，也是「兩個後端各自
  實作語意」的代價。修法：`compoundDiv` 改用檢查器算好的 `OpType`（與 `+=`／`*=`／`-=` 同一條提升）
  再把結果轉回目標型別。測試 `tests/programs/t194_compound_div_promotion`（期望值由 javac 21 產生）。
- **`IndexOutOfBoundsException` 的繼承階層（已修）**：原本 `lib/06_errors.teyru` 裡
  `ArrayIndexOutOfBoundsException` 直接繼承 `RuntimeException`、`IndexOutOfBoundsException`
  是它的兄弟，`StringIndexOutOfBoundsException` 根本不存在，而 `"abc".charAt(9)` 丟的是陣列那一
  個。後果不是編譯錯誤而是**靜默走錯分支**：Java 的慣用寫法
  `catch (IndexOutOfBoundsException)` 接不到任何越界，程式落到更廣的 catch 或直接死掉
  （修前 `x8_oob_hierarchy` 三個案例都落到 `RuntimeException`，Java 三個都是
  `IndexOutOfBounds`）。修法：三個類別照 Java 的階層重建（`IndexOutOfBoundsException` 為父、
  陣列與字串兩個為子，三個都有 `()` 與 `(String)` 建構子、都可被命名）；runtime 新增
  `ty_sioobe`（`tyrt.c`，與 `ty_aioobe` 同一個錯誤路徑）並把**字串與緩衝區**的越界檢查全部改走
  它——`ty_str_sub`／`ty_str_charat`／`ty_str_of_chars_part`／`StringBuilder`、`StringBuffer` 的
  `charAt`／`setCharAt`／`insert`／`delete`／`substring`／`setLength` 共 18 處——而**陣列**的檢查
  （`ty_arr_ptr`、`ty_arr_slot_ref`、`ty_arraycopy`、header 內的陣列讀、`tyrt_reflect.c` 兩處）
  維持 `ty_aioobe` 不變。類別 handle 走既有的 builtin 管線（`TY_SIOOBE`：`sema/checker.go` 的
  Builtins、`codegen/emit.go` 與 `codegen/llvm.go` 的兩張表），所以兩個後端都能丟它。測試
  `t195_index_out_of_bounds`（期望值 javac 產生；以**父類別** catch 並印出「哪一個到了」，因為
  這才是那個慣用寫法；11 個案例涵蓋字串、緩衝區、`String.valueOf(char[],off,count)`、陣列讀寫與
  一個沒越界的對照，兩個後端逐行相同）。同族、**已命名但未提供**的 API（記錄下來，不是佔位）：
  `PrintStream.write(byte[])` 與 `PrintStream.flush()` 不存在，用到它們的程式在編譯期就會被拒
  （`TY-TYP-0076`），而不是執行時才發現——名字留在這裡是為了讓下一個讀的人知道它們是「已知且刻
  意沒有」，不是被忘記。
- **已檢查且正確的形狀（暫存程式在 `/tmp/scout`，方法：`teyru build` 後跑執行檔，Java 21 當規格
  逐行比對）**：14 個收集器形狀在**確實發生收集**（儀器化探針量到 gc=4～15）下全部正確、無崩潰
  ——物件只被陣列元素／static／lambda 捕獲／回傳值指向、1000 與 10 萬節的鏈、13 萬節的樹、容器
  擴容換掉底層陣列、4500 個裝箱值、30 個區域變數跨過會收集的呼叫、運算式進行中的物件跨過會收集
  的呼叫、兩個都在收集的執行緒、256 KB 陣列夾著 24 B 的小物件。8 個例外／清理形狀與 Java 逐位元
  組相同：finally 內配置、catch 內重丟、finally 內的迴圈包 try、finally 內丟出、catch 內 return、
  巢狀 finally 的順序、longjmp 前後讀區域變數（另一個 agent 修過的那個形狀，這裡仍正確）、運算式
  進行中丟出。字串與數字除上面那條之外全部相同：各原生型別邊界值的格式化（含 NaN、∞、MIN／MAX、
  1e±308）、`substring`／`indexOf`／`charAt` 的邊界、10 萬字元單行進出 iostream、整數與長整數除
  以零、`MIN_VALUE / -1`、位移量超過型別寬度、陣列與字串越界（都有檢查、記憶體未被寫壞）。
  未涵蓋：Web 堆疊自重的那一組（空 body／邊界大小的 body／沒有值的標頭／keep-alive 上的轉導／
  過期或超大的 session cookie／被切成兩次讀的 WebSocket frame）——那一組我沒有寫程式，不宣稱它
  沒問題。

- **`bench_invoke` 的每一次反射呼叫拆開來看，貴的是配置不是反射（數字已由消去法更正）**：20M 次呼叫的 invoke
  迴圈實測 540／612／601 ms（約 27 ns/次）。**配置快速路徑本身只要約 2 ns**——消去法（把真實標頭逐一拿掉、加上
  不透明屏障防止最佳化折疊）量到：兩個標頭寫入 0.5、payload 歸零 0.23、預算檢查與計數 0.27、執行緒指標 0.06 ns。
  **先前 §10 引用 `ty_alloc(16)` 的 11.5 ns 當成快速路徑的成本是錯的**（那是另一個情境的數字，不該拿來當
  「路徑成本」）；而且快速路徑裡**沒有** safepoint 檢查（安全點在產生程式碼的迴圈頂端）也**沒有** free list 測試
  （那是慢路徑的事）。真正迴圈裡（20M 次、計時在程式內）是：`new Object()` 進 64 格環狀緩衝 Teyru 5.8 ns 對
  Java 2.4 ns；bench_invoke 自己的形狀（裝箱一個 int、存進一格 `Object[]`、再讀回來）Teyru 9.6 對 Java 3.4 ns。
  所以兩次裝箱約 **19 ns、佔 27 ns 的三分之二**（不是 85%），其餘是機制：一次方法表走訪、`call_invoker` 的
  setjmp catch frame（獨立量測 5.63／5.91／5.70 ns/次）、拆箱與欄位存取。**主要項目不是比較次數而是物件大小**：
  Teyru 的 `new Object()` 是 **32 位元組**（8 的 payload＋16 的標頭，對齊後 32），Java 是 16——迴圈裡每物件兩倍
  的記憶體流量，那才是這一項的槓桿。**Java 一樣要付那兩次配置**（它的 `invoke` 也收 `Object[]`），差別在配置器
  與物件大小，不在反射。所以「反射呼叫比直接呼叫慢約 17 倍」是對的，但讀成「反射很貴」是錯的。
  （一個**不可用**的消去法也記在這裡：把 bump 拿掉、每次發同一塊，程式會 SIGSEGV（exit 139）——用弄壞程式的
  方式量配置，數字本身就不成立，所以那條路的 0.0019 s 是崩潰不是加速，只跑一次、診斷、不重試。）
---

## 11. 送出前檢查清單

- [ ] `go build ./...`、`go vet ./...`、`go test ./... -count=1` 全綠（`tests/` submodule 已 checkout）
- [ ] 新功能有端到端測試；新診斷碼有拒絕測試
- [ ] 沒有多行 TODO 或空殼實作；未實作路徑會明確失敗
- [ ] 共用邏輯在 `internal/util`，沒有兩份實作
- [ ] 執行期 C 在 `-Wall -Wextra` 下無警告
- [ ] 行為改變時，`teyru-lang/docs` 的對應頁面已同步（文件只有那一份）
- [ ] commit 訊息符合 §8，且沒有 AI 署名
