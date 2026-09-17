# AGENTS.md — Teyru 專案工作規範

> 適用對象：本儲存庫內所有人類貢獻者與 Coding Agents。
> 版本 3.2（2026-09-17）。v1.0 是 Java/JVM 時期的規範，已於 v2.0 作廢；3.1 補齊
> util 分離、禁止佔位、文件與測試的完整規範，並記下拆分之後的倉庫佈局；3.2 加入
> 三條規則（JDK 差分測試、網路程式碼的對抗測試、文件聲明要指得到測試）與文件站的
> 手動部署步驟。

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
5. **文件裡的每一條能力聲明都要指得到測試。** 文件站上寫「支援 X」的地方要說得出是哪一個
   `tests/programs/*.teyru`（或 `internal/**/*_test.go`）在驗它；指不出來的就刪掉，或改寫成
   「尚未實作」／「未驗證」。第 3 條定義什麼算支援，這一條管文件怎麼寫，兩者是同一條規則的兩端。

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
- **Java 語意的改動一定要附 JDK 差分測試。** 語意的參考實作是 OpenJDK 21，期望輸出由真的
  JDK 跑出來、不許手寫。動到任何可觀察語意（數值轉換與窄化、字串與 Unicode 的索引、裝箱
  的身分、集合的迭代順序、例外的類別名與訊息、格式化）就在 `tests/programs/` 或
  `tests/java-compat/` 加一個兩邊都跑的程式，並在 PR 裡貼差分結果。理由不是形式：這個專案
  有兩個後端，而「與 Java 相同」是可以查 JDK 驗證的一句話，沒有測試撐著它就只是宣稱。
- **面向外部輸入的程式碼一定要附對抗測試。** `lib/15_net.teyru`、`lib/18_web.teyru`、
  `lib/30_websocket.teyru`、`lib/32_http_client.teyru` 這類讀網路（以及 `lib/46_timezone.teyru`
  這類讀主機檔案）的程式碼，每一個上限、逾時與長度檢查都要有測試盯著，而且測的是**不利的
  形狀**：慢速客戶端（一條連線一次一個位元組）、宣告長度遠超上限
  （`Content-Length: 1000000000`）、請求頭不結束、對端連上就不說話（逾時）、以及一個正常
  情形當對照。**沒有測試的上限不算上限**——失敗模式不是自己的程式壞掉，是別人的程式把伺服器
  帶走。
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

文件站在 `teyru-lang/docs`，`README` 的**英**與**簡中**版本也在那裡（對應語言的頁面）。
網站有三個 locale：`zh-TW`（正本，不翻譯也不改寫）、`zh-CN` 與 `en`，內容要一致——
改了其中一份就要改其餘兩份的結構（程式碼區塊、識別字與路徑照抄，只翻散文）。

**文件站的部署是手動的，push 不會部署。** 2026-09-17 查過 Vercel 專案 `teyru-docs`
（team `langyas-projects`）：它的 `link` 是空的，也就是**沒有接 Git**；每一筆 production
部署的來源都是 `cli`、建立者是 owner 的帳號。所以改完文件要自己部署，否則線上站就落後於
倉庫（實際發生過：線上站少了 `main` 上已有的三節內容）：

```sh
cd ../docs
vercel link            # 第一次：選專案 teyru-docs
vercel deploy --prod   # 建置並發佈；完成後 docs.teyru.dev 就是它
```

網域 `docs.teyru.dev` 在 Cloudflare 是一筆 **DNS-only** 的 CNAME 指到
`cname.vercel-dns.com`（不要開代理，憑證由 Vercel 簽）。要改成 push 自動部署，得先由 owner
把 Vercel 的 GitHub App 授權給 `teyru-lang` 組織；在那之前一律手動，清單上就有這一步。

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
- **類別初始化的兩個形狀，與延遲那一半的並行語意**：這個編譯器把**程式自己的類別**在 `main` 之前
  就跑完（`emit.go` 在啟動時、單執行緒，只點名非 builtin 且有初始化區塊的類別），而**標準程式庫的
  類別延遲到第一次使用**（呼叫端的守衛是 `(cls.flags & TY_CLS_INIT) ? 0 : ty_clinit(&cls)`，接著才
  呼叫該類別的靜態方法）。**啟動那一半與 JLS 12.4.1 的「第一次主動使用」不同**：`t31_init_order`
  的期望輸出把靜態區塊印在 `main start` 之前，那是 Teyru 的順序而不是 JDK 的；**維持現狀是 owner
  的決定**（改成惰性等於重寫類別安裝路徑，而套件裡沒有東西依賴那個差異），所以這條記在這裡。
  延遲那一半過去是錯的：`ty_clinit` **先發布 `TY_CLS_INIT` 再跑初始化區塊**，所以第二條執行緒看到
  旗標就跳過初始化，直接呼叫靜態方法、讀到還沒被賦值的靜態欄位——拿到的是 `ty_npe()`，也就是**訊息
  為空的** `NullPointerException`，代價是**一整個執行緒的工作**。`t164_concurrent_collections` 在
  帶有 `lib/04_boxing.teyru` 快取的樹上：2000 次失敗 113 次（100 次損失 1 條執行緒、11 次 2 條、
  2 次 3 條），**每一次都在 round 1**、round 2／3 完美；**乾淨的 main 跑 21000 次是 0 次失敗**，
  因為它路徑上沒有任何類別的初始化區塊是真正的計算。所以「main 沒事」不是結論，要量的是「main 加上
  那一個在途改動」——**這個對照（0/21000 對 113/2000）才是把一個 flaky gate 變成規格條款的東西，
  數字不是形容詞**。現在延遲那一半照 JLS 12.4.2 走：初始化區塊在**類別自己的監視器**下執行、完成
  旗標只在它**返回之後**才發布、`TY_CLS_BUSY`（bit 32）讓「正在初始化的執行緒自己遞迴進來」（快取
  由讀它的工廠建起來就是這個形狀）繼續、初始化區塊丟出時類別留在未初始化而監視器被放掉。
  **仍未驗證或未涵蓋**：呼叫端的守衛是普通載入，release 那一半沒有對應的 acquire，弱記憶體模型的
  目標（arm64）沒驗證；初始化區塊整段持有類別監視器，所以兩個互相碰到對方類別的初始化區塊可以
  死鎖（Java 在同樣兩把監視器上也會）；初始化區塊丟出例外的類別之後會被重跑，而 Java 是 erroneous
  一次、之後每次拒絕，這個差異**沒有測試盯著**。測試 `tests/programs/t217_class_initializer_race`
  （八條執行緒同時當六個程式庫類別的第一次使用者；期望輸出由 JDK 21 產生）。

---

- **遞迴過深的堆疊上限，以及執行期的兩個診斷開關（`tyrt.c`／`tyrt_thread.c`／`tyrt_plat_posix.c`／
  `tyrt_plat_win.c`）**：遞迴到把堆疊用完時，Java 丟出可攔截的 `StackOverflowError`，機器則是
  丟出結束行程的錯誤。這裡有兩層，缺一不可。第一層是**函式序言檢查**：每個產生的函式主體第一行
  都是 `ty_stack_check()`（`tyrt.h` 的 `always_inline`；C 後端用 `__builtin_frame_address(0)`，
  LLVM 後端用 `llvm.frameaddress.p0`），拿自己的框架位址跟執行緒區域變數 `ty_stack_limit` 比，
  低於就呼叫 `ty_stack_overflow()`。`ty_stack_limit` 由 `stack_limit_init()` 在每個執行緒啟動時
  設定，值是 `typlat_thread_stack_bounds` 回報的堆疊底端加 **256 KB 餘裕**；主執行緒與每一個
  `Thread` 都一樣（`ty_thread_init`／`ty_thread_start`）。沒有呼叫的葉函式並不特別排除，因為
  「省下來的那一行」換不到可觀測的好處，而少插一個地方就是少一個會漏的路徑。第二層是**保底**：
  `typlat_fault_handler_install`（`ty_init` 呼叫一次）裝上 SIGSEGV 處理器，每個執行緒再用
  `typlat_fault_altstack_install` 拿到自己的 `sigaltstack`；故障位址落在堆疊底端以下 64 KB 內
  （`TY_STACK_GUARD_REACH`；實測 gcc 與 clang 在 64 KB 到 4 MB 的框架下都落在 17 KB 內）時，
  `ty_stack_fault` 印出 `stack overflow in native code` 然後 `abort()`——**不從訊號處理器
  longjmp**，那不是可靠的事。其餘的故障把預設動作裝回去重送，因此除錯工具看到的仍是原本的錯誤。
  丟出的物件是**每個執行緒預先配置**的（`ty_stack_overflow_reserve`，產生的啟動碼與每個執行緒
  啟動時各呼叫一次；`tythread.soe`，由收集器在 `ty_gc_locked` 標記為根），因為丟出的框架只剩
  餘裕可用，配置會把配置器、堆鎖與安全點一起拉進最後 256 KB。餘裕要放得下**未攔截時的列印**：
  那是產生的程式碼（`toString`）而它自己也開頭就是同一個檢查，所以以前會從「列印中」再丟一個、
  無限遞迴到真的用完堆疊、什麼都沒印就死掉；現在 `print_uncaught` 會先把該執行緒的
  `ty_stack_limit` 設成 NULL（報告是執行緒的最後一件事，之後不再回到程式的程式碼）。**量測**：
  把 `TY_STACK_MARGIN` 逐級調小，未攔截的報告在 **4 KB** 餘裕下仍能完整印出，256 KB 是留給
  「兩次檢查之間的框架可能很大」這件事的。已知限制：**gcc -O0 連結不了任何程式**（與本項無關、
  早於 a84b32f）：產生的 C 保留著 dispatch slot 已被剪掉的函式主體，而那些主體呼叫 `ty_tls_*`，
  gcc -O0 不像 clang 與 gcc ≥ -O1 會把沒人引用的 static 丟掉；因此 `scripts/stack-matrix.sh` 的
  gcc -O0 那一格是 BLOCKED，不是通過。LLVM 後端不吃 `Thread`（`TY-INT-0100`，指名拒絕），
  矩陣因此對它改用主執行緒的程式。
- **這個機制的代價：`bench_fib` 長跑回退 32%（owner 已裁決，不是沒量到）**：同一台機器、
  `/usr/bin/time -v` 量**建好的執行檔**、十次一批：`bench_fib` 放大到 fib(38) 是
  **0.81 s → 1.07 s** 牆鐘（每次 81 ms → 107 ms），user 0.80 → 1.04。來源就是每次呼叫多一次
  框架位址檢查（約 0.4 ns／呼叫，而 fib(38) 有約 6300 萬次呼叫）。計畫 §W4 的驗收寫「回退不超過
  10%，超過就先做葉函式與 SCC 優化再測」，而**那兩個補救到不了 10%**：`fib` 不是葉函式（它呼叫
  自己），而「只對呼叫圖裡在強連通分量內的函式插入」仍然會保留**每一個自我遞迴函式**的檢查——`fib`
  正是那個自我遞迴函式，SCC 只有它一個。因此這一項以「機制落地、六格建置矩陣全綠、代價如實記錄」
  收尾；回退幅度、理由與「不改計畫指定的做法」由 owner 記為裁決（2026-09-17），不是安靜接受。
  還沒有試的方向（留給下一個量的人，不要當成未驗證的結論）：讓檢查本身更便宜——`__builtin_frame_address(0)`
  會逼出 frame pointer 並可能擋掉內聯，改用區域變數的位址或 `__builtin_stack_address` 可能更省；
  以及 D4 的備選（純保護頁＋訊號）每次呼叫零成本，但那樣就得從訊號處理器 longjmp 才能丟出可攔截的
  `StackOverflowError`，而計畫明文禁止。
- **執行期的兩個診斷開關（`tyrt.c` 的 `ty_gc_init`）**：`TEYRU_GC_STRESS=N` 讓**每 N 次配置**
  強制收集一次（把 `ty_gc_threshold` 設成 -1，內聯快速路徑因此把每一次配置都交給
  `ty_alloc_slow` 計數；N=1 就是每次配置都收集）；`TEYRU_GCTRACE=1` 讓每次收集在 stderr 印一行
  `teyru gc: trigger=<budget|stress|explicit> pause=<毫秒>ms heap=<收集前kB>-><收集後kB>
  live=<存活kB>`（`trigger` 是要求這次收集的人，`pause` 是世界停下來的時間，不含列印本身；
  兩個大小取自 chunk 的 watermark 而不是 `gc_slabs`，後者還指著已經釋放掉的 slab）。
  `TEYRU_GC_STRESS` 給了不是正整數的值會印一行說明並當作沒開，而不是安靜地不開。
  W3 與 W14 的量測讀的就是這兩個開關。

- **平台矩陣：arm64 已實測，macOS 只到「編譯並連結」（2026-09-17 在 linux/amd64 上量測，
  編譯器 2f78d0d、tests 21b8a07）**：`linux/arm64` 的整套在 qemu 下 **250 項全過、0 項不符**
  （222 支程式＋3 個套件＋23 個診斷＋2 個原生）。做法沒有動 `tests/run.sh`：sysroot 用 Debian sid
  的 arm64 套件裝進 `aarch64-linux-gnu-gcc` 的預設 sysroot（libc 2.43 與標頭、`linux-libc-dev`、
  `libatomic`、OpenSSL 3.6.4 及它要的 zlib／zstd；Debian 的 `libc.so`／`libm.so` 連結腳本要改寫成
  sysroot 內路徑，Fedora 無條件加的 `-latomic_asneeded` 要指向 Debian 的 libatomic），目標由一個
  加上 `--target linux/arm64` 的編譯器包裝帶著走，`CC=aarch64-linux-gnu-gcc`、`QEMU_LD_PREFIX` 指
  向 sysroot（Fedora 的 qemu-user-static 已註冊 binfmt handler，但那支 qemu 沒有預設 sysroot）。
  同一次暴露兩個還沒解的空隙：
  - **`tests/run.sh` 沒有地方可以指名目標**：它只喊 `teyru build -O1 -o <輸出> <來源>`，也沒有任何
    環境變數可介入，所以交叉目標目前只能靠替換編譯器那個名字（一個包裝腳本）達成——可行，但這是
    沒有文件的路徑。**（已補：`TEYRU_TARGET`，見後面兩條。）**
  - **`--cc` 補不上目標表裡沒有編譯器的目標**：`darwin/amd64` 與 `darwin/arm64` 的 `cc` 是空的，
    `resolveTarget` 在 `Compile` 讀 `opts.CC` 之前就拒絕，所以即使裝了 `zig cc -target <arch>-macos`
    也不能用 `teyru build` 編給 macOS。**（已補：`resolveTarget` 現在看的是這次真的要用的那支編譯器，
    見後面兩條。）**
  `darwin/amd64` 與 `darwin/arm64` 上**沒有任何一行程式被執行過**：繞過目標表、把 `teyru emit` 的 C
  交給 `zig cc -target <arch>-macos`，**不碰 TLS 的 188 支全部編譯並連結成功**（Mach-O 執行檔），碰
  得到 TLS 的 34 支編不過（`tyrt_tls.c` include `openssl/err.h`，macOS 沒有那個標頭），而且這批連
  結不帶 `-flto`（zig 回 `LTO requires using LLD`，那是編譯器自己對沒有 LTO 的工具鏈的退回路徑）。
  這個「模擬器上跑過」與「只編過」的差別，在 README 的平台表與 docs 的 index 上必須分得清楚。

- **TLS 的可達性由程式自己的呼叫圖決定，反射中繼資料不再把 OpenSSL 拖進每一個會反射的程式（W9.1）**：
  `link.TLS` 一直是「產生的 C 裡有沒有活的 `ty_tls_*`」，而那個可達性分析（`internal/codegen/prune.go`）
  把**反射中繼資料**當成一般的參考跟著走：程式只要用到反射（`Class.forName`、`Method.invoke`、註解
  掃描——也就是整個 web 框架與 Gson 那一層），`reflect.go` 的 `attach` 就會在啟動碼裡把每個類別的成員
  表接上去，成員表指名每個方法的 invoker，invoker 的函式體再呼叫方法本身，於是**每一個 TLS 方法體都
  是活的**。實測（本機 linux/amd64、clang）：`web.teyru`（兩個路由、從不呼叫 `ssl()`）在基線上 `ldd`
  有 `libssl.so.3` 與 `libcrypto.so.3`；`t101_gson` 用 `--target windows/amd64` 被 `checkTLS` 以
  「mingw-w64 ships no OpenSSL」拒絕。
  修法是把「這一行只有反射會走」變成**產生器寫下、掃描讀懂**的記號：`reflect.go` 的 `attach`（把成員表
  接到類別上的啟動陳述）與 `forNameTable`（`Class.forName` 搜尋的類別表）在行尾寫
  `/* reflection-only */`（常數 `reflectOnly`），`scanLine` 把這些行的參考另外記進 `cdef.refl`。可達性
  因此跑兩次：`reach(false)` 是原本那一次，仍然跟隨每個參考——vtable 剪裁要的就是這個，反射的
  `Method.invoke` 是從成員表走到 invoker、再從接收者的 vtable 分派出去，**讀到卻沒填的槽是跳到
  NULL**；`reach(true)` 不跟隨記號行的參考，回答「程式自己的程式碼到得了哪裡」，`reachesTLS` 讀的是
  後者。**編出來的 C 與基線逐位元組相同**（只多了那些註解；`emit` 對 `web.teyru`、`t146_reflect`、
  `t163_https_roundtrip`、`t101_gson`、`t221_http_limits` 逐一比對過），所以剪裁的判斷沒有變，變的只有
  連結決策；第二次 fixpoint 在 13.6 萬行的程式上量到 0.03 s（`teyru emit` 0.48 → 0.51 s）。反射真的在
  執行期叫到 TLS 方法時，得到的是 `tyrt_net.c` weak stub 的**指名**失敗
  （`Net.tlsClientContext0: this program was not linked against OpenSSL`，可攔截），不是跳到 NULL。
  **驗收的三個重播**（皆本機）：①把 OpenSSL 標頭藏起來的 `cc` 包裝（掃 `*.c` 有沒有
  `#include <openssl/`，有就照編譯器回 `fatal error: 'openssl/err.h' file not found`）——基線的
  `web.teyru` 在 `tyrt_tls.c` 上失敗，修後建置成功、`ldd` 沒有 libssl、`/ok` 與 `/echo` 都回 200；
  ②`t101_gson` 用 `--target windows/amd64` 建出 PE32+（`objdump -p` 的 imports 只有 KERNEL32／
  WS2_32／msvcrt），並在 Wine 下跑出與 `.expected` 相同的輸出；③`t163_https_roundtrip`、
  `t191_tls_keepalive`、`t207_http_server_certificate` 前後都連 `libssl` 且輸出逐行相同。
  測試：`tests/programs/t241_reflect_tls_unlinked`（只用反射；基線印 `invoked=1` 且連 libssl，修後不連
  libssl、印出上面那行指名失敗）與 `link_test.go` 的 `TestTLSReachability`（windows/amd64：反射的程式
  可建、直接呼叫 `Tls.clientContext` 的程式被指名拒絕）。LLVM 後端不受影響，而且它本來就不需要這個
  修正：它的連結決策是對自己的 IR 做字串搜尋（`LinkForIR`），而它的 IR 只帶可達的程式碼——同一支
  `sock.teyru`，C 的文字裡有 12 處 `ty_tls_`（未可達的方法體照樣寫出來）、IR 裡 0 處（3,915 行對
  81,830 行），且它對用到註解的程式是具名拒絕（`TY-INT-0100`）。實測呼叫 TLS 的程式在兩個建置上
  一模一樣（都連 `libssl`、都印 `ctx=1`）。

- **平台矩陣那條的兩個空隙已補（`TEYRU_TARGET` 與 `--cc`）**：
  - `tests/run.sh` 讀 `TEYRU_TARGET=<os>/<arch>`：每一次 build 都經過一個 `build` 函式帶著 `--target`
    （放在一處，後加的 build 不會忘記），`.skip` 也改以**目標平台**判讀（跨目標的程式跑在目標平台上，
    能不能跑是那個平台的事）。編譯器倉庫的 `go test` driver 讀同一個變數、同一種語意，
    `tests/README.md` 把規則寫在兩個 driver 的共同契約裡。
  - `--cc` 現在填得上目標表裡沒有編譯器的目標：`resolveTarget(name, cc)` 檢查的是**這次真的要用的那支
    編譯器**（`--cc` 優先），所以 `teyru build --target darwin/arm64 --cc <zig cc -target aarch64-macos
    的包裝>` 真的會編給 macOS（zig 對 `-flto` 回 `LTO requires using LLD`，編譯器自己退回不帶 LTO 的
    第二次嘗試，那條路徑本來就有）；沒有 `--cc` 時仍然具名拒絕，句子多了「and neither this table nor
    --cc names one」。**macOS 仍然只到「編譯並連結」**：這裡沒有 macOS，沒有任何一行程式在它上面跑過，
    而上面那條的 macOS 數字是舊規則下、繞過目標表量的：現在碰得到 TLS 的程式是**在讀檔之前就被
    `checkTLS` 具名拒絕**（`TLS is not available for darwin/arm64: macOS ships SecureTransport…`），
    不會走到 C 編譯器，所以那一格要以新的方式重量。

## 11. 送出前檢查清單

- [ ] `go build ./...`、`go vet ./...`、`go test ./... -count=1` 全綠（`tests/` submodule 已 checkout）
- [ ] 新功能有端到端測試；新診斷碼有拒絕測試
- [ ] 沒有多行 TODO 或空殼實作；未實作路徑會明確失敗
- [ ] 共用邏輯在 `internal/util`，沒有兩份實作
- [ ] 執行期 C 在 `-Wall -Wextra` 下無警告
- [ ] `make notices` 綠；動過 `internal/runtime/src` 或第三方宣告時已用
      `scripts/check-notices.sh --write` 重新產生清單
- [ ] 行為改變時，`teyru-lang/docs` 的對應頁面已同步（文件只有那一份）
- [ ] 改了 `teyru-lang/docs` 就自己部署（`cd ../docs && vercel deploy --prod`，見 §9）
- [ ] commit 訊息符合 §8，且沒有 AI 署名
