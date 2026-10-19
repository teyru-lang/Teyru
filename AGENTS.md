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
   **短於 1 秒的整程序計時（wall clock、含行程啟動）只能說明啟動和小程序場景，不能寫成
   吞吐結論**：那個數字裡行程啟動佔掉一大半，把它的差距讀成「這個實作比較快」，就是把啟動的
   差距當成程式的差距。`scripts/bench.sh` 為此印兩組表——短規模是前者，長規模（每個至少 1 秒）
   才是後者——所以對外的句子要指明是哪一組，而引用短規模的數字時必須標明它是短規模。

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
- **replica 與真品不一致時，先假設 replica 錯**：微基準、抽出來的片段、另寫的小程式是 replica，
  產品實際產生的東西是**真品**，而兩者一致的意思是**同一個執行檔、同一條指令流**，不是「看起來
  差不多」。今天三個實例都是這一條：一個 C 微基準把 helper 標成 `noinline` 好「定價一次呼叫」，
  但 `-O2` 是整程式 LTO、那個 helper 早就被 inline 折掉了（`nm` 在改前的 `-O2` 執行檔上根本找不到
  那個符號），所以「占 47%」在 `-O2` 不成立；另一個的 replica 被 clang **向量化**（58 條 vector
  指令）而真品的熱迴圈沒有（24 條純量），於是「四個嫌疑都是 0」不成立，而**真品自己的組語**顯示
  那些包裝佔了迴圈一半以上的指令數；第三個是建置本身——worktree 忘了建 `TMPDIR`，每個建置都失敗，
  數字就變成「314 個失敗」。遇到不一致，先去讀真品的執行檔或它的組語，不要先改 replica 的結論。
  同一條規則的第二個名字是**寫死的期望值**：一個固定的期望是行為的**模型**，不是行為本身，所以
  兩者不一致時先問「**這個期望值是從哪來的**」——那個答案與「程式錯了」是**完全不同的結論**，
  而差別會決定你改哪一邊。實例：時區測試把「作者機器上的**一種** tzdata 建構佈局」當成真品，
  而函式庫對它拿到的那個檔案在**兩種佈局下都讀對**（vanguard 與 rearguard 對愛爾蘭給出相同的
  offset 與縮寫、相反的 `isdst`），所以沒有任何固定的期望能同時滿足兩者；差一點就去改讀取程式
  那一邊，而該改的是測試。
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
| `README.md`（本倉庫，正本）與 `README.zh-CN.md`／`README.en.md`／`README.ja.md` | 倉庫門面：這是什麼、怎麼安裝、其他倉庫在哪；四個語言 | 專案定位、安裝方式或任何數字改變時——**四份一起改** |
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

文件站在 `teyru-lang/docs`。網站有三個 locale：`zh-TW`（正本，不翻譯也不改寫）、`zh-CN` 與
`en`，內容要一致——改了其中一份就要改其餘兩份的結構（程式碼區塊、識別字與路徑照抄，只翻散文）。
**本倉庫的 `README` 有四個語言**：`README.md` 是繁體中文正本，`README.zh-CN.md`、
`README.en.md`、`README.ja.md` 是它的翻譯，規則相同——數字、日期、路徑、指令與程式碼區塊
逐字照抄，只翻散文；每一份最上面一行是語言切換連結。改了正本的任何數字，其餘三份在同一個
提交裡跟著改，否則「同一件事在四個地方各說各話」就是這個專案最常犯的那種錯。

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

**2026-10-16 複查，上面每一句都還成立**，而且這次是直接量到的：`docs` 的 PR #2 併進 `main`
之後線上站**沒有動**——`vercel ls teyru-docs` 的最新一筆是 **29 天前**，頁面還是舊的
（`grep -c "只讀位置形式"` 是 3、新字串是 2，兩者都不是 `main` 現在的數字）。所以「push 就會
部署」是**錯的**，`main` 已經有內容而線上站落後，正是這一節警告的那件事。手動部署照上面做
（新的 clone 要先 `vercel link --yes --project teyru-docs`），發佈後一分鐘內兩個 locale
（`/docs/lombok` 與 `/en/docs/lombok`）都拿到新的列。

**自動化還缺什麼，也量到了**：`vercel git connect https://github.com/teyru-lang/docs` 失敗，
訊息是 `Make sure there aren't any typos and that you have access to the repository if it's
private`——那是 **Vercel 的 GitHub App 沒有裝在 `teyru-lang` 組織上**的樣子（CLI 沒有辦法裝
它，要在 Vercel 的 dashboard 按「Connect Git Repository」走一次授權）。裝好之後這一段就可以
改成「push 就部署」，在那之前**每一次改完文件都要自己跑一次部署**，清單上那一步不能省。

---

## 10. 已知限制（不要當成已完成）

- **產生的 C 不得依賴 C 從未承諾的求值順序。** Java 把運算元的求值順序寫死成由左至右：
  二元運算子的左運算元完整求值完才碰右運算元（JLS 15.7.1）、呼叫的目標先於引數而引數照寫的
  順序（JLS 15.12.4）、陣列的參考先於索引（JLS 15.10.3）、賦值目標的各部分先於存入的值——
  而 null 與邊界檢查在那個值之後（JLS 15.26.1）——複合賦值則先讀目標值再算右運算元
  （JLS 15.26.2）、`new int[a][b]` 的維度運算式各自求值一次且由左至右（JLS 15.10.2）。
  C 一個都沒承諾：運算子的運算元、呼叫的引數、賦值的兩側，求值順序是**未指定**的
  （C11 6.5p3）。因此當一個運算式有兩個以上的運算元、而其中任一個**有副作用**（呼叫、配置、
  賦值、`++`／`--`、欄位或陣列寫入、建立閉包或方法參考）時，產生器必須依 Java 的順序把運算元
  綁進各自型別的暫時變數再組合——`internal/codegen/emit_expr.go` 的 `sequence`，它的呼叫點就是
  本文所列的每一個多運算元位置（二元運算子、字串串接鏈、呼叫的目標與引數、原生呼叫、建構子、
  `new` 的維度、陣列元素賦值、經由運算式的欄位賦值、複合賦值）。不需要綁的就不要綁：字面值、
  `this`、以及沒有兄弟運算元寫入的區域變數維持原樣，所以絕大多數運算式產生的 C 與以前相同。
  **症狀是答案錯，不是當掉**，而且 `-O0`、`-O1`、`-O2` 完全相同，所以那不是最佳化器在利用
  未定義行為，是產生的程式碼依賴了 C 從未承諾的順序。這個缺陷在專案生命期內對 clang 完全隱形
  ——clang 對這些形狀剛好由左至右——只有 `make ci`（§6）的**第二條腿**（`TEYRU_CC=gcc`）看得見，
  而它從未綠過；回歸測試是 `tests/programs/t260_eval_order.teyru`，它的 `.expected` 來自
  `t260_eval_order.java.ref` 在 OpenJDK 21 上的輸出。
- checked exception 對**程式自己的程式碼**做編譯期檢查（JLS 11.2）：呼叫或 `throw`
  一個受檢例外時，外層 `try` 的 catch 子句必須能接住它，或外圍方法／建構子／初始化器
  必須宣告它；catch 子型別不涵蓋宣告的超型別、`finally` 不接例外，lambda 的 body 以
  函式介面的 `throws` 為準。診斷是 `TY-TYP-0122`，測試是
  `tests/programs/t306_checked_exceptions.teyru` 與
  `tests/diagnostics/{uncaughtCheckedCall,uncaughtCheckedThrow,finallyDoesNotCatch,checkedInLambda}.teyru`。
  標準程式庫**自己的 body** 不在這條規則內，因為它被當成已編譯的相依（javac 也不檢查
  JDK 原始碼）：它的 140 個違規（12 個檔案；ZipException 51、IOException 26、
  DataFormatException 16、EOFException 12、WebSocketProtocolException 8、
  SocketException 7、UTFDataFormatException 5、ParseException 4，其餘 11 個分屬五類）是
  整份程式庫例外模型的改寫，不是這條規則的一部分。程式庫**宣告**的受檢例外仍會在使用端
  被要求處理（例如 `MessageFormat.parse` 的 `ParseException`）。未模型化的是 precise
  rethrow（`catch (Exception e) { throw e; }` 只宣告 try 區塊實際會丟的型別）與方法參考的
  `throws`；兩者都是從嚴拒絕，不會放過。
- `sealed` 的 `permits` 子句會被驗證（JLS 8.1.6／9.1.4）：`permits` 只能指名直接子型別、
  每個直接子型別都必須被允許（沒有 `permits` 時，同一個編譯單元裡的直接子型別就是隱式
  允許，與 Java 相同）、每個被允許的子型別必須是 `final`、`sealed` 或 `non-sealed`。
  因此沒有 `permits` 的 sealed 型別在 switch 窮盡性上不再不可判定：同檔案的直接子型別
  就是 permit 集合。診斷是 `TY-TYP-0119`／`0120`／`0121`，測試是
  `tests/programs/t307_sealed_permits.teyru` 與 `tests/diagnostics/sealed*.teyru`。
- 反射在 `lib/26_reflect.teyru`：`Class`、`Field`、`Method`、`Constructor`、
  `Modifier`、`Array` 與 `java.lang.reflect` 的六個例外，讀的是編譯器為每個類別
  產生的靜態表（欄位、方法、修飾子、列舉常數），所以查一次資料是走一次陣列，
  執行期不建表。與 Java 的差異（都已實測，不是未驗證）：
  - 類別名是 JDK 的（D8）：`String.class.getName()` 是 `java.lang.String`，
    `Class.forName` 兩種寫法都收（`java.lang.String` 與 `teyru.String` 會找到
    同一類別）。陣列的 `getName()` 是原始碼形式（`int[]`、`java.lang.String[]`，
    等同 JDK 的 `getTypeName()`）；二元描述子（`[I`、`[Ljava.lang.String;`）
    是 `Class.forName` 解析的名字。
  - 註解反射有，但元素是**按名字讀**：`Class`／`Field`／`Method`／`Constructor`
    上的 `getAnnotations()`、`getAnnotation(Class)`、`isAnnotationPresent(Class)` 是
    Java 的，`Annotation` 則沒有「每個註解型別一個實作類別」——所以要
    `ann.stringValue("value")`／`intValue`／`booleanValue`／`doubleValue`／
    `classValue`／`enumValue`，而不是 Java 的 `ann.value()`。沒寫的元素讀得到介面
    宣告的預設值（parser 保留 `default`）；`@Retention` 收得下但沒有作用；陣列型別
    的元素值不帶（讀它會說不支援）。
  - 每個元素型別有自己的陣列類別：`int[].class`、`String[].class`、
    `Person[].class` 是不同類別，`getComponentType()` 回答元素類別，
    `forName("[I")`／`forName("[[I")`／`forName("[Ljava.lang.String;")` 解析
    描述子。陣列值由產生程式碼與 `Array.newInstance` 建立時帶上自己的類別；
    **執行期自己建的陣列**（例如 `String.toCharArray()`）仍帶共用的
    `teyru.Array`，`Array.newInstance` 對一個沒有被發出的元素類別也退回它。
    陣列類別只為可達的元素型別發出（`internal/codegen/arrayclass.go`，與成員表
    同一套可達性分析），所以沒有反射、也沒有寫出陣列型別的程式不付這筆成本。
    所有陣列類別的父類別是 `teyru.Array`，因此 `instanceof` 與轉型到陣列型別
    的答案不變；`getSuperclass()` 依 Java 回答 `Object`。
  - 泛型型別反射是**一層**：`Field.getGenericType()`、`Method` 的
    `getGenericReturnType()`／`getGenericParameterTypes()`、
    `Constructor.getGenericParameterTypes()` 與 `Class.getGenericSuperclass()`
    回答 `ParameterizedType`（`getRawType()`／`getActualTypeArguments()`，
    元素是 `Class`）。沒有型別引數的宣告回答它的 `Class`，raw 宣告也是。
    **不能表示的就明確拒絕**：萬用字元、型別變數、超過一層的參數化型別都拋
    `UnsupportedOperationException` 並說明原因，不會回傳抹除後的型別。
    `java.lang.reflect.Type` 只有 `Class` 與 `ParameterizedType` 兩個實作，
    沒有 `WildcardType`、`TypeVariable`、`GenericArrayType`。
  - 原生型別的取值器只收完全相符的裝箱型別，Java 的拓寬（例如對 `byte` 欄位
    呼叫 `getInt`）在這裡是 `IllegalArgumentException`。
  - 存取控制會檢查：`Field.get`／`set`、`Method.invoke`、`Constructor.newInstance`
    對 private／protected／package-private 成員拋 `IllegalAccessException`，
    `setAccessible(true)` 之後放行。這裡沒有「呼叫者類別」可查，所以不像 Java
    那樣允許同套件或子類別的存取——非 public 一律拒絕，直到程式明確放行。
    final 一律攔，`setAccessible(true)` 之後可以寫 instance final，但 static
    final 一律拒絕（javac 也是這樣：JDK 不再讓它通過，即使 setAccessible 過）。
    標準程式庫自己讀私有成員的地方（Gson、容器注入、驗證、路由）都先呼叫
    `setAccessible(true)`。
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
  `tests/programs/t159_threads.teyru`。
  中斷與 daemon 已實作：`interrupt`／`isInterrupted`／`Thread.interrupted`（狀態
  在每個執行緒的執行期條目裡；`sleep`、`join`、`Object.wait`／`wait(long)` 用各自的
  條件變數等待並在醒來時檢查，所以不需要 signal；被中斷時清除旗標並丟
  `InterruptedException`；阻塞的檔案與 socket I/O 不可中斷，與 classic `java.io`
  相同）、`setDaemon`／`isDaemon`（`start` 之後丟
  `IllegalThreadStateException`；建構時繼承建立者的 daemon 狀態；行程在最後一個
  非 daemon 執行緒結束時結束，`ty_thread_join_all` 等的是
  `n_live - n_daemon`）。`ThreadLocal` 是純 Teyru：值放在 `Thread` 物件自己的
  `HashMap` 欄位，GC 看到的是普通引用，執行期不參與。端到端測試是
  `tests/programs/t288_interrupt.teyru`、`t289_interrupt_status.teyru`、
  `t290_daemon_exit.teyru`、`t291_thread_local.teyru`。**沒有的**：優先權、
  `ThreadGroup`、堆疊大小與逾時的 `join(long)`、中斷阻塞 I/O，以及未處理例外的
  handler（執行期印出 Java 預設處理常式那一行，然後結束那個執行緒、行程繼續）。
  GC 是**合作式**停止世界：安全點在每個迴圈回邊（產生器會放）、配置慢路徑、等
  heap 鎖，以及執行期自己每個會等的呼叫——socket 的讀、寫、accept 與 connect 等待，TLS
  的握手與讀寫，檔案與管線的讀寫迴圈，stdin 讀一行，以及 sleep、join、monitor wait
  （清單與理由在 `tyrt_thread.c` 開頭；會等物件的那些把那個物件掛上 shadow stack，因為
  執行期自己的框在停止點下方、掃描不到）。實測（`tests/programs/t277_blocked_read_safepoint`
  的形狀，6 秒的 socket 讀 + 另一個執行緒配置 100 MB）：修前第一次收集暫停 3799ms、配置花
  3805ms，修後最大暫停 0.3ms、配置花 5ms（再跑一次是 3831／3844ms 對 1.3／20ms，修後的數字
  隨機器負載游動，修前的不會——它等的是讀的逾時）；兩邊都是 25 次收集。因此一個既不迴圈、
  不配置也不等待的執行緒（例如原生方法裡的長計算，或執行期那些「問了就回來」的呼叫：
  `open`／`stat`／`opendir`／`getaddrinfo`）會讓收集等它，直到它回來；這是已知限制，
  不是未驗證的不確定性。每個執行緒有自己的配置區（slab），單執行緒程式的配置速度不變。
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
- **那個「4 執行緒以上的配置壓力測試約 16% 崩潰」的既存缺陷**已經修好（`c3ab10b`，2026-09-15），
  而**這一條先前寫的是「正在查」**：那是落後的敘述，不是現況。修的是 `ty_heap_unlock` 先解鎖堆
  互斥鎖、才跑 `stopped_end`，所以在「把一個 block 從 free list 拿下來」到「交出去」之間的那個
  窗口裡，配置的執行緒仍被算在 `n_stopped` 裡；在那個窗口開始的收集不等它，走過那塊 block 的
  slab、讀到它仍然寫著的「空閒」，就把它當成可用空間——重新接到 free list 上，或把整塊 slab
  還回去——而那個執行緒接著醒來、往那塊記憶體寫、再把它發出去。同一塊 block 發兩次，或寫進
  已經還給系統的記憶體。
  **量測（本機 linux/amd64、16 核，數的是非零結束碼，不是時間）**：`c3ab10b^` 的編譯器在
  `4 執行緒 × 20 萬次 × new long[1024]`（8 KB，大於最大的 size class 63 × `TY_ALIGN` = 1008
  位元組，所以每一次配置都走取鎖的路徑、不經 per-thread 批次）下 **26/30 崩潰**，在
  `8 × 20 萬 × new long[8192]`（64 KB）下 **30/30**，而在
  `4 執行緒 × 500 萬次 × 32 位元組的物件` 下只有 **1/100**，同一組形狀用**一條執行緒**是
  **0/100**——**區塊的大小與執行緒的數目決定了暴露量**，
  這是這個形狀唯一需要解釋的地方，也是為什麼這一條要把那三個數字並排寫出來：
  `size_class()` 只認到 63，再上去就是大區塊、永遠走取鎖的路徑；立刻丟掉才會讓 slab 回到
  free list，而那正是窗口所在；live set 不能是空的，否則每塊 slab 都可以還回去，量到的就不是
  這個形狀。`c3ab10b` 之後的乾淨建置在同樣兩組大區塊形狀下是 **0/30** 與 **0/50**。
  套件現在有一支盯著它的程式：`tests/programs/t285_alloc_thread_stress`（同一組形狀，輸出是
  `threads * n * n` 這個定值，所以沒有任何計時成分）——它在 `c3ab10b^` 上 **20/20 崩潰**、
  在現在的樹上 **0/20**。修好之前這一條寫著「這是目前最值得處理的正確性問題」；現在它是
  **已結案**，而它的形狀留在一支程式裡，不是只留在這一段文字裡。
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
  **但那個「~3.7 倍快」只成立在短規模（W10 更正）**：短規模（20 萬次串接）今天量到 3.81 倍，
  而長規模（1600 萬次）是**反過來**——Teyru 0.750 s 對 Java 0.285 s，Java 快 2.6 倍。這一條
  原本拿短規模的比值去支撐對外宣稱，那就正是計畫 §1.5 講的那個錯：整支程式短於 1 秒的計時
  說明的是啟動與小程式，不是吞吐。

- **基準的量測方法與注意事項**（`/tmp/teyru-bench-report.md`，38 分鐘、10 節）：Java 那一欄
  每次都是全新的 JVM，短程式由暖機主導——同一個 fib(32) 暖機後 Java 只要 5–6 ms，而 Teyru 是
  4 ms，所以「fib 快 4.58 倍」大部分是冷解譯器造成的。**W10 把兩邊的量級一起量了出來**：短規模
  fib(32) 今天 7.9 ms 對 27.7 ms（3.51 倍），長規模 fib(44) 是 1.877 s 對 1.800 s（**打平**，
  Java 快 4%）；`bench_loop` 同一個形狀——短規模 1.88 倍，長規模 0.89 倍。冷啟動買到的倍數在
  長跑上會消失，所以這裡的 4.58 與 3.51 都不該被引用成「fib 快幾倍」。`bench_invoke` 必須這樣讀：Teyru 的
  **直接呼叫**迴圈本身就已比 Java 慢 2.6 倍（26 對 10 ms），反射再加機器成本 15.9 對
  6.15 ns/次、裝箱 5.6 對 3.0 ns/次，以及每輪新建一個 `Object[]`（+6.1 對 −1.9 ns/次，Java
  的逃逸分析把它消掉）。所以那一列的低比值不是反射特有的懲罰（這一條原本記的是 0.43 倍，
  W10 今天在 20M 次上量到 0.4094 s 對 0.2517 s，也就是 0.61 倍）。

- **`bench_alloc` 的長跑一次都沒收集，所以那一列不是收集器的量測**：`TEYRU_GCTRACE=1 ./bench_alloc 800000000`
  在 stderr 上**一行都沒有**（20M 次也一樣），因為編譯器的逃逸分析把 `new Cell(i)` 提升到堆疊上、
  LLVM 再把欄位提升成暫存器——這一列量到的是迴圈本身。它的 0.15x（1.398 s 對 0.204 s）因此不是
  「GC 慢」，而 800M 次算下來 Teyru 每次迭代 1.75 ns、Java 0.26 ns：四分之一奈秒放不下一次配置，
  所以兩邊都是被最佳化過的迴圈，差距在那個迴圈的程式碼品質。**成因沒有進一步拆解**（沒有 profile、
  沒有計數器），執行期效能是 W14。

- **附錄 B 的倍數與 W10 的倍數不是同一台機器，兩者互不更正**：附錄 B 是單核容器、clang 18.1.3、
  fib(38)；W10 是 Ryzen 7 5700X、clang 22.1.8、fib(44) 與上面那些規模，而且落在附錄 B 之後的是
  `StackOverflowError` 的函式序言檢查（§W4 記的代價是 fib 長跑回退 32%）。所以「長跑只有 fib 是
  Teyru 快」與「長跑一列都沒贏、fib 打平」都是各自機器上的事實，要引用就一起引用它的機器與樹。

- **W10 沒有量到的兩項（工具在、機器不允許，不是做不到）**：**HTTP 吞吐**——`ab` 在機器上、
  `examples/webapp` 也起得來（一次 200 請求的冒煙測試 0 失敗、`-c 2` 約 5.9k req/s），但在一個
  別的 agent 正在編譯的共用機器上量 loopback 的請求數，量到的一半是排程器而不是伺服器，所以
  沒有把它寫成一個數字。要量的話：`teyru build -O2 -o webapp ./...`（在 `examples/webapp`），
  起它，然後 `ab -n 50000 -c 16 http://127.0.0.1:8080/todos`，同時記下負載。**GraalVM
  `native-image` 對照**——這個 image 裡沒有 GraalVM（`which native-image gu` 空、`/usr/lib/jvm`
  裡只有 OpenJDK 21 與 25），所以沒有猜，直接寫未測。
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
  **2026-09-17 的尺寸是 `-O2` 66,808 位元組**（`-O1` 85,440、`-O3` 70,248，以 `wc -c` 量同一支
  hello、編譯器取自 `427244f`）：裝箱快取 +6,016（`lib/04_boxing.teyru` 的六個快取陣列）、
  `StackOverflowError` 的函式序言檢查 +2,256、之後 TLS 可達性那次的 +120。**這種數字只有被量
  才會對**——它先前是 54.6 KB，兩個月沒人重量的結果。
  **2026-10-16 重量：97,888 位元組**，而中間有一段**一個月的退步**，成因是兩個獨立的東西：
  ①**`CharSequence` 的 `chars()`／`codePoints()` 預設方法**（PR #182 帶進來的）回傳 `IntStream`，
  於是**每一個實作它的類別的 vtable 都讓整個 stream 程式庫可達**——而 `String` 就是其中一個，
  在每一支程式裡：hello 由 66,808 變成 **618,080**，把那兩個預設從介面拿掉（`String` 與兩個
  builder 自己的實作留著）之後回到 97,888，量測在 `lib/01_core.teyru` 的註解裡；②**框架地圖**
  （W14）讓產生的 C 大一截（`t146_reflect` 的全程式輸出 +51%），那是設計上的代價，不是缺陷。
  其餘的程式（同一天、同一支編譯器、`-O2`）：t84 **137,472**、t133 **112,600**、t51 **661,664**、
  t146_reflect **1,851,888**（反射中繼資料裁剪之後，先前記的 4.1 MB 是裁剪之前的）。往下的餘裕
  還是那句：要再砍需要「某個類別永遠不會有實例」這個事實，而 C 內文不決定它。
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
  迴圈實測 540／612／601 ms（約 27 ns/次）；**W10 在同一棵樹上用同一個程式自己印的那一對重量到 381 ms
  （19 ns/次，直接呼叫那一圈 26 ms），整體已經低了三成——下面的拆解是在舊的整體成本上量的，絕對值要跟著
  更新，拆解本身沒有重測**。**配置快速路徑本身只要約 2 ns**——消去法（把真實標頭逐一拿掉、加上
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
  **呼叫端的守衛現在是 acquire 載入**（C 後端 `__atomic_load_n(..., __ATOMIC_ACQUIRE)`、LLVM 後端
  `load atomic ... acquire`），所以 release／acquire 這一對是完整的——先前那一半是普通載入，
  而一個沒有 acquire 的 release 不是 synchronises-with：看到旗標的執行緒有權讀到初始化區塊
  **還沒寫**的那些靜態欄位，也就是這個機制存在的理由本身。**但這條修正在這台機器上沒有可觀測的
  測試**：x86-64 上 acquire 就是原本那個載入（所以套件不會有任何差異），而 qemu 的 arm64 不會
  重現這個弱模型的交錯，所以它靠的是指令語意而不是一次量測——寫在這裡而不是寫成「已驗證」。
  **仍未驗證或未涵蓋**：初始化區塊整段持有類別監視器，所以兩個互相碰到對方類別的初始化區塊可以
  死鎖（Java 在同樣兩把監視器上也會）。初始化區塊**丟出**時的行為（JLS 12.4.2 的兩半）見下面
  那一條。測試 `tests/programs/t217_class_initializer_race`
  （八條執行緒同時當六個程式庫類別的第一次使用者；期望輸出由 JDK 21 產生）。

- **arm64 的框架範圍算錯，所以框架地圖在 arm64 上等於沒用（`t245_dropped_reference`；已修）**：
  整套 301 支程式在 qemu 的 arm64 上本來是 **299 過、2 失敗**（2026-10-16 的 `main`），修好之後
  是 **300 過、1 失敗**（剩下那一支是 `t285`，見下一條），而
  `t245` 是其中一支：x86-64 上 `System.liveBytes()` 是 **0**（框架地圖把丟掉的 8 MB 陣列放掉），
  arm64 上是 **8**（等於 `TEYRU_NO_FRAME_MAPS=1` 的答案）。**原因不是「少掃到根」，是相反**：
  框架記錄的範圍（`ty_frame_enter_` 的 `lo`／`hi`）在 arm64 上落在框架儲存區**下面**的
  16 個位元組，所以框架自己的字**不在**被精確覆蓋的範圍裡，而落進保守掃描——於是那格已經
  清掉（`ty_frame_put(&_tyfr, k, NULL)`）但區域變數裡還留著舊指標的字被讀到，8 MB 就留下來了。
  證據（同一個 `main`、同一個程式、兩邊各跑一次 `TEYRU_GCTRACE=1`）：
  x86-64 是 `f=0x…908 slots=0x…7d0 lo=0x…7c8 hi=0x…9e8`（範圍包住 slots 與每個 slot 指的
  位址），arm64 是 `f=0x…770 slots=0x…710 lo=0x…538 hi=0x…548`（範圍在 0x538–0x548，
  而 slots 與所有 slot 指的位址都在 0x568 以上）。**量到的差別在 live 而不是在字數**：
  同一個程式、同一個編譯器，第一次收集的 live 是 **0 kB 對 8192 kB**（最後一次顯式收集是
  2 kB 對 8192 kB），而保守掃描的字數兩邊差不多（440／436 對 431／377）——因為被讀到的
  是**哪些**字，不是讀了幾個字。
  兩個 prologue 說明為什麼：x86-64 是 `push rbp; mov rbp,rsp; …; sub rsp,N`（**區域變數在
  rbp 下面**），arm64 是 `sub sp,sp,#N; stp x29,x30,[sp]; mov x29,sp`（**區域變數在 x29 上面**），
  而 `lo`／`hi` 是 `__builtin_frame_address(0)+8` 與 `frame_addr+8`——那個算式假設的是
  x86-64 的版面。**修好了，而且修在執行期**（上面的推論「修法不在執行期」是錯的）：範圍不再
  只由框架指標決定，而是**由那張地圖自己註冊的字決定**——`ty_frame_enter_` 把框架記錄與
  `slots` 陣列算進去，而每一次 `ty_frame_put(f, k, p)` 在 `p` 非 NULL 時把 `lo`／`hi` 擴到
  `p` 與 `p+8`。**只擴不縮**，所以框架指標原本就涵蓋的（框架的地板、被呼叫者濺出的暫存器）
  照舊涵蓋，而**一個框架自己的字在兩種版面上都在範圍裡**。驗證：`t245_dropped_reference`
  在 arm64 上 **8 → 0**（x86-64 仍是 0），`gcc`／`aarch64-linux-gnu-gcc -c -O2 -Wall -Wextra`
  對 `tyrt.c` 都是 0 警告，而 `TEYRU_GC_STRESS=1` 下 `t57_gc_pressure` 與 `t245` 都符合期望。
- **windows/amd64 的平台表是 2026-10-17 重量的（Wine，302 支）**：**290 支**輸出（stdout、stderr、
  結束碼）逐位元組相同、**11 支**碰得到 TLS 而被具名拒絕（mingw-w64 沒有 OpenSSL）、**1 支**
  （`t99_file_shell_meta`）因 NTFS 不能有那種檔名而跳過、**0 支不符**。先前記的「195 支裡 179
  支」是被取代的數字。**這一輪找到並修掉的 Windows 專屬缺陷**（每一項都是量到的，不是推論）：
  ①`-lws2_32` 排在目的檔**之前**，靜態連結因此 `undefined reference to __imp_WSAStartup`
  （目標表新增 `sysLibs`，排在目的檔之後）；②MinGW 不宣告 `aligned_alloc`（`-std=gnu11` 下），
  slab 對齊修正因此讓整個目標編不過（`slab_alloc` 在 `_WIN32` 用 `_aligned_malloc`）；③MinGW
  對函式**不實作** `__attribute__((weak))`（符號被整個丟掉），debug 建置的 TLS 弱定義因此變成
  `undefined reference to ty_tls_close`；④`System.getProperty` 在 `_WIN32` 對 `os.name`／
  `user.dir` 等一律回 null，`File.getAbsolutePath` 因此回**相對**路徑，而 `isAbsolutePath` 又不認
  `C:\`；⑤**`longjmp` 在 x86-64 Windows 是展開式的**（`RtlUnwindEx` 從 throw 走回 setjmp 記下的
  框架），而產生的 C 沒有任何 SEH 終止處理器可跑，所以那趟走訪只會出錯——在 Wine 下它**跑出
  執行緒的堆疊**：執行緒死在 ntdll 裡（`virtual_setup_exception stack overflow 2496 bytes`），
  行程卡住、該印的那行 `Exception in thread …` 永遠沒印（`t227_websocket_handler_error`，
  失敗時 0/12、加上除錯輸出就恢復正常——典型的版面敏感）。修法是 Windows 目標的旗標
  `-D__USE_MINGW_SETJMP_NON_SEH`，讓 `<setjmp.h>` 選 MinGW 自己的 `__mingw_setjmp`／
  `__mingw_longjmp`（單純的暫存器存取）；修後同一個執行檔 **30/30** 通過。**這個缺陷沒有被任何
  已存在的測試單獨釘住的原因**也寫在這裡：t216、最小的「執行緒內丟 Error」重現
  （`try`／`finally`／`ThreadPool` 四種形狀）在展開式 `longjmp` 下全都通過，只有 ws 那條路
  （`finally` 關 socket、worker 的 `catch (Throwable)` 重丟、替補執行緒同時啟動）會踩到。
  **這個數字是整套以 `-O0` 建出來的**（302 支約 3 分鐘建完、跑完）：mingw 的 `cc1` 在最大的幾支
  `-O1` 上跑超過兩小時，所以 `-O1` 的整套在這台機器上不是一個可行的量測——那是工具鏈的建置時間，
  不是行為差異（`-O1` 下 274 支建得完的也都逐位元組相同）。**另一個量測陷阱**：`WINEDEBUG` 沒設時 Wine 把自己的
  版本橫幅與註冊表抱怨寫進 stderr，`.experr` 的比對因此失敗——`tests/run.sh` 現在預設
  `WINEDEBUG=-all`。
- **配置器的快速路徑從 null 造指標（已修），與它是怎麼被找到的**：`ty_alloc`（`tyrt.h`）與
  `ty_alloc_slow`（`tyrt.c`）都用 `p + total > me->bump_end` 判斷 slab 還有沒有空間，而
  `p = me->bump` 在**一個收集把這條執行緒的 slab 拆掉之後就是 NULL**——對一個 null 指標做
  `p + total` 是未定義行為，編譯器因此可以假設 `p` 非 null，並把後面的寫入搬過這個測試。
  **UBSan 在每一支程式上都報它**（`applying non-zero offset 48 to null pointer`），而它是
  **既有的**（今天的任何改動之前就有）。修法是不從 null 造指標：**算術改用 `uintptr_t`**
  （整數沒有這條規則，而且它編出來就是舊形式的那個加法加比較——交錯量測六次、各取最小，
  `examples/bench_escape` 兩邊都是 0.92 s，差在雜訊裡）。兩個後端都修：LLVM 那邊是同一個
  缺陷的另一種寫法（`getelementptr **inbounds**` 從 null 出發是 poison，於是分支條件是
  poison）。**發現它的路徑值得記下來**：W14 第 9 項的代理實驗（把 `TY_ALIGN` 改成 32）讓
  `bench_escape` 在每個規模都 SIGSEGV，而**同一支變體在 `-fsanitize=address` 下跑得完**——
  ASan 看不到、UBSan 一指就中的那個組合，就是它。
  **修好它並沒有讓 (32,32) 那個變體跑起來**——那個崩潰是**另一個**原因，而它現在也有答案了：
  **slab 的基底只有 16 對齊**（`malloc` 的保證），所以 `TY_ALIGN` 大於 16 時，`valid_obj` 第一行的
  對齊前置測試 `((uintptr_t)p) & (TY_ALIGN - 1)` **丟掉每一個候選**，標記階段什麼都找不到
  （`live=0kB`）、清掃把還活著的 slab 整塊還回去，程式接著走進已回收的記憶體。`(32,16)` 與
  `(16,32)` 不崩，因為它們的 `TY_ALIGN` 還是 16——`malloc` 的 16 剛好就是那個測試要的。修法：
  `chunk_new` 改用 `aligned_alloc(TY_ALIGN, cap)`（`TY_CHUNK` 是 `TY_ALIGN` 的倍數，要求成立）。
  崩潰、診斷、兩次錯誤結論與量測都寫在 `scripts/w14-analysis.md` 的那一節。
- **W14 第 9 項（`TY_HDR` 與 `TY_ALIGN`）的第一個實驗：那兩個常數不是兩個常數。** 這一項要的是
  「收窄 8 或 16 位元組在真實基準上值多少」，而做這件事需要一個能單獨調這兩個數字的變體。
  試了：`TY_HDR` 16→32 與 `TY_ALIGN` 16→32 **各自單獨改**，`examples/bench_escape`（4000 萬次）
  與一支純配置迴圈（100 萬次）都跑得完、答案正確；**兩個一起改成 32 就 SIGSEGV**（`-O2` 與
  `-O0` 都一樣），而同一支變體跑 hello 與不觸發回收的配置迴圈是好的。所以那個「把區塊放大
  再量敏感度」的代理實驗**不可用**，而這正是這一項真正的形狀：兩個常數各自安全、**合起來
  不安全**——也就是 `scripts/w14-analysis.md` 第九之一節列的四個代價之外，還有第五個在
  「標頭大小 × 對齊」這個組合上。**因此「值多少」仍然沒有量到**；現在知道的是代理實驗不可用、
  以及要往哪裡找。下一次做這一項的人請從「為什麼 (32,32) 會崩而 (32,16) 與 (16,32) 不會」
  開始，那是一個可以用 `TEYRU_GCTRACE=1` 與 `valid_obj` 的計數回答的問題。
- **`t285_alloc_thread_stress` 在 arm64 上**：它是那次套件裡第二支失敗的，但**單獨跑不會失敗**
  ——同一個執行檔在 qemu 下連跑 **20 次全部 0 失敗**，輸出與 `.expected` 相同。所以它
  **不是**一個已確立的 arm64 缺陷，而是一個只在套件那樣的負載下出現過一次的東西；要說它
  是什麼，需要一個可重現的形狀，而現在沒有。**「套件那樣的負載」這條路後來試過了**：
  在 arm64 上把同一個執行檔在 qemu 下跑 **40 次**（八個忙迴圈，load ≈ 2.6），再在 **40 個忙迴圈
  加三個同時進行的 arm64 建置**下跑 **40 次**（load ≈ 3.4）——**80 次全部 0 失敗、輸出與
  `.expected` 相同**。所以光是負載也不足以重現它，與 HTTP 那一組的結論一樣。先前這一條把它與 `t245` 寫成「同一件事」
  （「收集器的根少了一半」），**那個說法是錯的**：`t245` 是多留（框架地圖沒生效），
  `t285` 還不知道是什麼。
- **一個沒能重現的觀察，寫在這裡只有一個理由：下一個看到它的人要知道那不是新的。** 2026-10-16
  一次**整套** `go test ./...` 的執行裡，四個 HTTP／WebSocket 案例同時失敗（`t227`、`t241`、
  `t255`，以及 `t241` 的另一行），症狀是**狀態列被印了兩次**——`deep -> HTTP/1.1 HTTP/1.1 500
  Internal Server Error`——以及一次 framing 錯位（`NumberFormatException: For input string:
  "HTTP/1.1"`，也就是讀到的下一個回應從中間開始）。那次執行在 load 9、同一台機器上有六個
  建置在跑。**之後再也沒有重現**：同一支編譯器把那三支直接跑 **900 次以上**、透過 `go test` 的
  driver 跑 **150 次**（`-count=50`）、把 stdout 接到管線再跑 **180 次**、六格建置矩陣、以及
  乾淨的 `main` 上整套 `TestPrograms`——**全部 0 次**；`TEYRU_GC_STRESS=1` 與
  `TEYRU_NO_FRAME_MAPS=1` 之下那幾支跑不完（每次配置都收集），所以那兩個開關對這一題沒有資訊。
  症狀不像任何一個已知的失敗模式：同一行裡前綴重複看起來像**某個字串被寫了兩次**，而不是連線、
  逾時或排程的形狀。所以它被記下來而不是被解釋掉——要動它的人需要的是一個可重現的形狀，
  而那還沒有人拿到。
  **之後又試了一次，這次刻意造出當時的條件**：背景同時跑六格建置矩陣（當時是「六個建置」）的
  情況下，把那三支各跑 **60 輪**（共 180 次），再在同樣的負載下用**分開比對 stdout 與 stderr**
  的方式跑 120 次——**0 次不符、0 次重複的狀態列**。所以「光是負載」不足以重現它；要動它的人
  需要的是別的線索，而這一段把「負載」這條路也劃掉了。
- **類別初始化失敗的兩半，與另一半為什麼還看不到（`TY_CLS_ERR`）**：JLS 12.4.2 說初始化區塊丟出時，
  那一**次**使用看到的是包起來的失敗（丟的是 `Error` 就原樣重丟，否則用
  `ExceptionInInitializerError` 包住並把原因放進 `cause`），而那之後**每一次**使用都以
  `NoClassDefFoundError`（訊息 `Could not initialize class X`）拒絕，不再重跑。兩半都實作了
  （`tyrt2.c` 的 `ty_clinit`：`TY_CLS_ERR` 在類別自己的監視器下發布，讀也在那裡；
  `ty_eiie`／`ty_ncdfe` 是兩個新的執行期建構子，`LinkageError`／`ExceptionInInitializerError`／
  `NoClassDefFoundError` 三個類別跟著補上，`Error` 也補了 Java 的 `(String, Throwable)` 與
  `(Throwable)` 兩個建構子）。
  **但只有前半在今天的樹上觀測得到，而這是 startup 順序的結果不是疏漏**：這個編譯器把**程式自己的
  類別**在 `main` 之前跑完（見上一條），所以一個初始化區塊會丟的程式類別在 startup 就死了、`main`
  根本沒跑到——因此「之後每次拒絕」需要的是**編譯器留給第一次使用**的類別，也就是標準庫的類別，
  而標準庫裡**沒有任何一個類別的初始化區塊會丟**（`grep -rn "static {" lib/*.teyru` 是空的）。
  觀測得到的那一半有測試：`tests/programs/t286_class_init_failure`（`.experr` 的第一行是 JDK 21
  對同一支程式印的那一行 `Exception in thread "main" java.lang.ExceptionInInitializerError`，
  而改動前它印的是原始的 `java.lang.RuntimeException: init failed`；JDK 差分也同意：
  `TestJDKDiff/t286` 是 translated 且 agreed）。後半那條路徑因此**沒有測試盯著**，而它會在
  任何一個「編譯器留給第一次使用、而初始化會失敗」的類別出現的那一天變成活的。

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
- **裝箱的識別性不靠逃逸分析，而是結構上不成立（`internal/codegen/escape.go`）**：計畫 W6.1 要求
  逃逸分析不得把身分會被 `==` 觀察到的箱子放到堆疊上，也不得複製它。今天之所以成立，不是因為分析
  保證了它，而是因為兩件事：自動裝箱產生的是**呼叫包裝類別自己的 `valueOf`**（快取就在那裡，不是
  每個使用現場自己配置），而 `findStackLocals`／`promotable` 只考慮**顯式 `new T(...)`**，並明確
  拒絕任何 `Special` 非空的類別——每個包裝類別都是 `Special = "box"`，`String` 亦然。所以可觀察的
  識別性由 `valueOf` 與快取那條路徑決定，而不是由一個會猜錯的分析決定。把這條契約釘住的是
  `t242_box_identity_across_call`：參考被帶出到另一個方法之後才比較（快取的 127、非快取的
  1000、`Boolean`、`Character`、以及經由陣列往返），期望值是 javac 21 的輸出——任何把箱子複製一份、
  讓每次使用各自新建、或弄壞快取的改動都會讓它變紅。**尚未實作的**是「分析證明箱子不逃逸因而
  可以在堆疊上」這個優化本身：它不存在，而上面兩條結構性事實讓它不需要存在。
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

- **字串的讀取語意是 UTF-16 code unit，儲存是 WTF-8（W5 的第二塊：讀取 API）**：`tystr`
  帶著兩個長度（`blen` 位元組、`ulen` code unit）與一個 ASCII 位元，`length`、`charAt`、
  `substring`、`indexOf`／`lastIndexOf`、`compareTo`、`hashCode`、`String.format` 的寬度與
  精度、`StringBuilder` 的每一個索引全部按 code unit 算，所以 `"中文".length()` 是 2、
  `"ab中c".indexOf("c")` 是 3、`"中".compareTo("文")` 是 **-5978**（code unit 的差，不是正負
  號）、`"中文".hashCode()` 是 646394。索引換算走**麵包屑**：字串自己的區塊裡每 64 個 code
  unit 一格（`TY_STR_BC_OFF`／`TY_STR_NBC`，`str_build_bc` 首次使用時建立、以 release store
  發布，兩個執行緒同時建也答同一個答案），ASCII 走 O(1) 快路徑。**這一段的代價**：同一台
  機器、同一份 `examples/`、交錯 A/B（短跑 40 對、長跑 24／23 對，每邊取最小 **CPU**
  時間，`/usr/bin/time -v`，進入時 load 17.7），四列全部在 10% 預算之內：
  `bench_string` 短跑 0.0200 → 0.0200 秒（1.000x）、長跑（16,000,000）1.740 → 1.730 秒
  （**0.994x**）；`bench_string_cjk` 短跑 0.0200 → 0.0200 秒（1.000x）、長跑（4,000,000）
  0.390 → **0.370** 秒（**0.949x，快 5%**），而且它的兩邊答案已經一致
  （`concat=2088890 walked=1000 sliced=4`，JDK 同一個數字），所以那一列現在印的是比值而不是
  `!(out)`。所以 **D1 的預設（WTF-8 + breadcrumb）成立，compact strings 不需要**。
  **這個數字要這樣讀**：前 9 對長跑樣本給的是 1.26x（0.390 → 0.490），第 23 對才把 after
  那邊的最小值收到 0.370——負載下的最小值需要**足夠多的對數**才會落定，9 對不夠，這也是這
  裡寫下兩者的原因；牆鐘在這種負載下沒有意義（同一支 1.9 秒 CPU 的程式量到 128–158 秒牆鐘），
  所以上面引的是 CPU 時間。
  **mid-pair 才是會錯的地方，三個都刻意處理了**：切開代理對的 `substring` 給的是 Java 給的
  那個孤立代理（`"\uD83D\uDE00".substring(0,1)` 是 `"\uD83D"`，`getBytes()` 是 **1** 個
  byte 的 `?`）；搜尋起點落在配對中間時，先逐 unit 比對那**一個**位置再看位元組，因為
  `indexOf("\uDE00", 1)` 在 Java 是 1；開頭是低代理或結尾是高代理的針走 unit 掃描，因為
  它的位元組不是乾草的位元組（`"😀".startsWith("\uD83D")` 是 true）。`compareTo` 因此把
  補充平面排在 U+E000–U+FFFF **之前**（`"\uFFFF".compareTo("\uD800\uDC00")` 是 10239），
  位元組序做不到這件事。`hashCode` 在 code unit 上算，**W6 的 HashMap 迭代順序可以直接
  開工**。`StringBuilder`／`StringBuffer` 的緩衝區是 code unit（不是位元組），所以
  `append(char)` 一次一個 unit、兩個半邊先後 append 就是那個字元、`reverse` 把配對整體翻轉
  再修回來；`String(char[])`／`String(char[],int,int)`／`String(int[],int,int)`／
  `valueOf(char[])`／`toCharArray`／`getChars`／`codePointAt`／`codePointBefore`／
  `codePointCount`／`offsetByCodePoints`／`regionMatches`／`chars()`／`codePoints()`／
  `subSequence` 都已提供（W7 的缺 API 清單）。位元組邊界只有**一個**寫出者
  （`ty_str_write`）：`getBytes`、`println`、`System.out` 都把孤立代理寫成 JDK 編碼器寫的
  一個 `?`。編譯期折疊與執行期必須同一套規則，所以 `util.StrConcat` 把折疊邊界上相遇的兩個
  半邊併成 four-byte 序列（`("\uD83D" + "\uDE00").equals("😀")` 為真，這正是 equals／
  switch／hash 靠的位元組等式），lexer 用 WTF-8 解 `char` 字面值（`'\uD83D'` 是那個 code
  unit 而不是 U+FFFD），`emit-java` 把孤立代理印成轉義而非 U+FFFD。
  **寫入那一半（W5 的第 4–6 項）在同一個分支上完成**：分類與大小寫映射是 **Unicode 15.0**
  （JDK 21 的版本），表的來源是標準自己的資料檔而不是 ASCII 範圍強行延伸——`internal/tools/
  genunicode/data/` 的三個檔（UnicodeData、SpecialCasing、PropList）由 `internal/tools/
  genunicode` 產生 `internal/runtime/src/tyrt_unicode.c`（兩級 byte 表 + 排序的稀疏表，
  `make unicode-tables` 重跑，`go test ./internal/tools/genunicode` 盯著入庫的檔案與資料一致）。
  `Character` 因此有 `isLetter`／`isDigit`／`isLetterOrDigit`／`isAlphabetic`／`isUpperCase`／
  `isLowerCase`／`isTitleCase`／`isWhitespace`／`isSpaceChar`／`isDefined`／`getType`（30 個
  類別常數照 JDK 編號）／`toUpperCase`／`toLowerCase`／`toTitleCase`／`digit`／`forDigit`／
  `getNumericValue` 的 **code point 形式**（int 參數；char 形式是 Teyru 寫的一行轉呼），加上
  `isSupplementaryCodePoint`／`isBmpCodePoint`／`isValidCodePoint`／`charCount`／`toCodePoint`／
  `highSurrogate`／`lowSurrogate`／`isSurrogate`／`isSurrogatePair`／`toString(int)`。字串那邊：
  `toUpperCase`／`toLowerCase` 走 code point 並套 **SpecialCasing**（`"ß".toUpperCase()` 是
  `SS`、`"İ".toLowerCase()` 是 `i` + U+0307）與 **Final_Sigma**（`"ΟΔΟΣ".toLowerCase()` 是
  `οδος`）；`strip`／`stripLeading`／`stripTrailing`／`isBlank` 是 `isWhitespace` 的集合
  （U+3000 剝得掉、U+00A0 剝不掉）；`Integer.parseInt`／`Long.parseLong`／`Byte`／`Short` 收
  JDK 收的每一個 Nd 數字（含全形與他種文字，但不含 BMP 以上的 OSMANYA 數字，因為 Java 逐
  unit 讀）；`Double.parseDouble` 仍然只收 ASCII 數字（Java 就是如此）但**會**依 `trim()` 略過
  前後空白。**這裡量出來的每一條都由 JDK 逐個碼點驗證**：`t251_unicode_tables` 走完
  1,114,112 個碼點，把 `getType`、十個判定、三個映射（含 `digit(36)`）與 `getNumericValue`
  折成計數與一個雜湊，與 JDK 的輸出逐位元組相同；`t250_string_utf16_builders` 與
  `t252_character_api` 是逐項的行為（builder 的 method 與代理對、分類與映射常數、strip 家族、
  解析、忽略大小寫的比較）。**這一段取代先前記的「仍然不做」**——那五項都做了。
  **還沒有做、也不假裝做了的**：Final_Sigma 的「字」用的是自己寫的邊界判定（字母／數字／記號
  相鄰、連接號與 `-`／`.`／`'` 相連、表意字自成一個字），它是**對著 JDK 量出來的**而不是
  UAX #29（JDK 的 BreakIterator 是自己的規則）；兩者在造出來的 8000 個字串上零分歧，但一個
  `.` 後面緊接組合記號再接字母這種形狀仍會不同（`AGENTS.md` 這一段就是它的記錄處）。語言的
  大小寫映射是 **locale-insensitive** 的（Java 的 `Locale.ROOT`）：土耳其／亞塞拜然／立陶宛
  的條件映射（`tr`／`az`／`lt`）沒有實作，因為沒有 locale。
  `StringBuilder`／`StringBuffer` 的 `offsetByCodePoints`／`appendCodePoint`／`codePoints()`
  沒有宣告（Java 的 AbstractStringBuilder 有；code point 的**讀取**那一組已補齊，見下一條）；
  `StringBuilder` 的 `setLength`／`setCharAt` 回傳 builder，Java 回傳 void（寬鬆的超集，先前
  就在）。**正則引擎的三個殘餘差異，都量過而不是未驗證**：①pattern 裡的 `\uXXXX`、`\a`
  這類跳脫序列沒有實作，這裡把它當成字面字元（`"A".matches("\\u0041")` 這裡是 false、JDK 是
  true；`"a".matches("\\a")` 這裡是 true、JDK 是 false，因為 Java 的 `\a` 是 BEL），所以
  `lib/21_regex.teyru` 開頭「不支援的都拒絕」那句話對這一族不成立——下一個動它的人要一起
  處理；②`.` 不吃的行終止符只有 `\n` 與 `\r`，Java 另外不吃 U+0085／U+2028／U+2029
  （`"\u0085".matches(".")` 這裡是 true、JDK 是 false）；③其餘不支援的語法仍是**拒絕**而不是
  別的答案（possessive、atomic group、lookaround、backreference、named group、`\p{...}`、
  `\b`）。
  測試：`t250_string_utf16_builders`（`t249` 已被 W8 的 t249_probe_boxed_compound_shapes 用掉）
  與 `t251_unicode_tables`、`t252_character_api`（期望輸出由
  OpenJDK 21.0.11+10 跑 `.java.ref` 產生）。
- **builder 的 code point 讀取、`capacity()`、越界的父類別與正則的 code point（已補齊，
  2026-10-16，四項，逐項對著 OpenJDK 21.0.11+10 差分）**：這四項原本記在上面那一條的「還沒有
  做」裡，現在都做了，每一項都有兩邊跑的程式（`.teyru` 與 `.java.ref`，期望輸出是 JDK 印的）。
  ①`StringBuilder`／`StringBuffer` 有了 Java 的 AbstractStringBuilder 的三個讀取
  `codePointAt`／`codePointBefore`／`codePointCount`（`ty_sb_code_point_*`，跑 builder 自己的
  unit 緩衝區而不是先 `toString()`），索引越界是 `StringIndexOutOfBoundsException`、範圍越界是
  父類別 `IndexOutOfBoundsException`，訊息逐字是 JDK 的（`Index 4 out of bounds for length 4`
  與 `Range [0, 5) out of bounds for length 4`）；測試 `t274_builder_codepoints`。
  ②`capacity()` 整個是 JDK 的答案：預設 16（原本 32）、`new StringBuilder(n)` 就是 n——0 就是
  0（原本下限 1）、`new StringBuilder(str)` 是 `str.length() + 16`、成長規則是 Java 的
  `newCapacity`（`cap * 2 + 2`，不夠就取要的），所以 `new StringBuilder(16).ensureCapacity(100)`
  是 100 而不是 128，而 0 也能長（`new StringBuilder(0).append("abc")` 是 3）；測試
  `t273_builder_capacity`。③`offsetByCodePoints` 與 `codePointCount` 越界時丟的改成**父類別**
  `IndexOutOfBoundsException`——先前記的「執行期沒有父類別的 handle」不是事實，handle 拿得到，
  成本是：`sema.Builtins` 多一個 `IOOBE` 欄位與一行 `get`、`emit.go` 的 handle 表一列、
  `llvm.go` 兩列（handle 表與 instantiate 清單）、執行期一個 `tyclass *TY_IOOBE` 與
  `ty_ioobe()`／`ty_ioobe_range()` 兩個 throw 函式、`tyrt.h` 的 extern 與兩行宣告。
  沒有別的代價：`IndexOutOfBoundsException` 本來就因為
  `StringIndexOutOfBoundsException` 的父指標而在每一支執行檔裡，所以沒有多出任何類別或 vtable。
  `offsetByCodePoints` 的訊息是 **null**（Java 是 `new IndexOutOfBoundsException()`，沒有訊息；
  `ty_make_ex` 因此把「null 的訊息」存成 NULL 而不是空字串——原本那個註解說 null 是
  InvocationTargetException 的「沒有文字」，而空字串不是 Java 的答案，只是沒有呼叫端用過那條路）；
  `codePointCount` 是 `Objects.checkFromToIndex` 的 `Range [from, to) out of bounds for length
  n`；測試 `t275_code_point_bounds`（用父類別 catch，這才是那兩個方法被寫出來的用法）。
  ④正則引擎的一個節點吃的是 **code point** 而不是 code unit（`.`、字元類別、pattern 裡的補充
  平面字面值），所以 `"😀a".matches(".a")` 是 true、`"😀a".matches("..")` 是 false，而
  `Matcher` 報的位移仍是 unit 索引（`Pattern.compile(".").matcher("😀a")` 的兩次 find 是
  `0-2` 與 `2-3`，與 JDK 相同）——引擎內部的位置從頭到尾都是 unit，改的只是每一步走多遠，
  所以索引語意沒有動到，這不是重寫：`lib/21_regex.teyru` 一個檔案裡約 40 行的改動（`CharSet`
  的範圍由 `char[]` 改 `int[]` 並按 code point 比對、`LitNode`／`AnyNode`／`ClassNode` 的推進改
  `Character.charCount`、parser 讀字面值與類別成員時走 `String.codePointAt`）；測試
  `t276_regex_codepoints`。
- **標準庫的快取、反射中繼資料的裁剪與平行編譯（W13 的第 1、2、3 項都已落地）**：一次建置原本把
  標準庫與程式放進**同一個編譯單元**、整份一起最佳化。實測（本機 linux/amd64、AMD Ryzen 7 5700X 16 核、
  clang 22.1.8）hello 產生的 C 是 6,232,818 位元組，其中程式本身只有 1,530 位元組——也就是說每一次建置
  都在重編同一個標準庫，而 `-flto` 是那個把整份看進去的步驟：附錄 A.1 的 `web.teyru` 在本機 `-O2`
  26.2 s、`--no-lto` 15.9 s、`-O1` 15.7 s、`-O0` 4.58 s。
  快取把「標準庫的語意檢查與產生的 C」變成每個程式共用的一次工作：`internal/driver` 的 `compileSplit`
  先產生程式的單元（它會說出程式是否到得了反射，那決定標準庫單元要不要帶成員表），再用
  `internal/cache` 的 key 找 `$XDG_CACHE_HOME/teyru/prelude/<key>/`（`TEYRU_CACHE_DIR` 可改位置、
  `TEYRU_NOCACHE=1` 關掉）。key 是（編譯器版本＋Go buildinfo 的 `vcs.revision`／`vcs.modified`＋
  **編譯器執行檔自己的 hash**、runtime 原始碼與標頭的 hash、`lib/*.teyru` 的 hash、目標、優化級別、
  後端、C 編譯器名）；`reflect=` 這個維度**在 W13.2 之後拿掉了**，理由見下面那一條：標準庫的單元
  不再隨程式是否反射分岔，所以一個 entry 服務每一個程式。
  `internal/codegen` 的 `EmitLibrary` 只吐標準庫的單元、`EmitProgram` 只吐程式的單元，兩者共用一個標頭，
  每個字串字面值以內容雜湊命名、由擁有它的那個單元定義（`==` 是參考比較，一文字只能有一個物件）。
  vtable 陣列留在**程式的**單元：`prune.go` 靠程式的分派點決定哪些槽留著，那是快取單元不能帶的決定；
  `pruneVtablesWith` 讀兩個單元回答 vtable 剪裁與 TLS 可達性，只改寫程式的單元。debug 建置另外用
  `-ffunction-sections -fdata-sections` 與 `-Wl,--gc-sections`（macOS 是 `-Wl,-dead_strip`）取代
  LTO 的死碼消除：標準庫的定義為了跨單元而全是外部連結，編譯器自己丟不掉。
  政策只有一條：`-O0`／`-O1` 走這條路，`-O2` 以上完全不變（單一單元＋LTO）。`teyru run` 因此預設 `-O0`
  （沒給 `-O*` 時），要整程式最佳化就明講 `teyru run -O2`。本機暖快取實測：hello `-O0` 0.69 s（舊 1.19 s）、
  `-O1` 1.26 s、`teyru run`（編譯加執行）0.73 s；`web.teyru` `-O0` 1.24 s（舊 4.58 s）、`-O1` 1.88 s
  （舊 15.7 s）；單核（`taskset -c 0`）舊 hello `-O2` 1.70 s／`-O0` 1.25 s。**release 沒有回退**：
  `-O2` 的 hello 1.67 s／66,808 B、`web.teyru` 26.2 s／5,235,704 B，且 `teyru emit` 的 C 對 hello、
  `web.teyru`、一支反射程式在重構前後**逐位元組相同**。
  代價要講清楚：①冷啟動——每個（優化級別、反射變體）第一次要把 5.1～10.0 MB 的標準庫編譯成物件
  （本機 `-O0` 2.48 s、`-O1` 10.4 s），之後才只付程式的編譯；②debug 執行檔變大（hello `-O0`
  106,856 → 139,752 位元組，+31%），因為跨單元的符號是外部的、vtable 也搬到了程式的單元；③在這台 16 核
  機器上單次建置的差距不大（`--no-lto` 的單一單元 `web.teyru` 1.27 s 對暖快取 1.24 s），快取的價值在慢
  機器與大量建置（測試套件每個程式都是 `-O0`／`-O1`，正好是這條路）。
  快取失效的論證：key 覆蓋上面每一項，任一根改了就是不同目錄。**編譯器自己的位元組是後來才補上的那一根**，
  而它補的是一個真實的漏洞：`vcs.revision` ＋ `vcs.modified` 說的是「這個 commit 的某棵髒樹」，不是「哪一棵」
  ——同一個 commit 的兩棵樹各自改了不同的東西，revision 與 dirty 完全相同、產生的 C 可以不同，於是其中一棵
  的建置會讀到另一棵吐出的 prelude。這不是假想的狀態：這個倉庫的常態就是每個工作項一個 worktree，而實測
  兩棵這樣的樹在補上之前**共用同一個 entry**（`101d2b09…`），補上之後是兩個（`internal/cache/cache_test.go`
  的 `TestTwoDirtyCompilersOfOneCommitAreDifferentCompilers` 與 `TestBuildNamesTheRunningCompiler`，兩個在
  沒有那一根時都是紅的）。**而在這台機器上，實測比「兩棵髒樹」更糟**：`go version -m` 對這裡建出來的二進位
  檔**一個 `vcs.*` 設定都沒有**（三棵樹、含 `-trimpath` 與 `-buildvcs=true` 都試過），所以 `cache.Build` 的
  revision 那一段是空的，Compiler 欄位就是 `0.4.1 go1.26.5-X:nodwarf5 linux amd64`——**每一棵樹的每一個
  建置都是同一個**。同一台機器上稍早建的那支 `teyru` **有** `vcs.revision=d7fde38`，所以蓋章不是永遠不會
  發生；一個要靠它才正確的 key 就是一個會安靜地錯的 key，而現在補的這一根不依賴 buildinfo。代價是**任何**
  改動編譯器原始碼的編輯都會讓 prelude 冷一次——包括改一個註解、
  或改測試檔（`go test` 的測試二進位檔就是那個「執行檔」）——所以這是保守的一根，而保守的方向是「多編一次」
  而不是「讀到別人的產物」。寫入是 `CreateTemp` ＋ `rename`，讀者只會
  看到完整檔，`Has()` 把 0 位元組當未命中；標頭、原始碼、物件共用一個 entry，不會混用不同建置的產物；
  兩個單元之間只有標頭宣告、`ty_prelude_install()`、`ty_prog_forname()` 與程式單元定義的 vtable 四個介面，
  而 driver 會檢查兩邊「哪些字面值由標準庫定義」必須完全相同，不同就以內部錯誤中止。`make cache`
  （`scripts/cache-check.sh`）把這些變成可執行的檢查，release workflow 會呼叫它。
  **W13.2「反射中繼資料只給實際需要的類別」已落地**（`internal/codegen/reflectset.go`）：成員表（`fds_*`／
  `mds_*`／每個方法一支 invoker）與 `Class.forName` 的類別表只為「反射真的到得了」的類別產生，其餘完全不寫。
  一張沒寫的表不是小一點的執行檔而是錯的答案——那個類別會答空的成員表——所以這份分析必須是**上近似**：
  形狀看不透就放大成「所有類別」，不是給一個小集合。兩個表都由**程式單元**持有（`enterTables`），標準庫的
  單元一張都不帶，所以快取的標準庫不再隨「程式是否反射」分岔：cache key 的 `reflect=` 維度因此拿掉，
  同一個 entry 服務所有程式。實測（本機 linux/amd64、clang 22.1.8）：`tests/programs/t146_reflect` 的執行檔
  `-O2` **9,694,296 → 1,626,232** 位元組、`-O0` **8,784,800 → 1,215,592**（hello `-O2` 80,000 不變）；
  t146 產生的 C 由 18,057,763 降到 12,618,776 位元組，其中 `mds_*` 由 663 張降到 9 張、`forname_all` 由
  671 個類別降到 2 個。
  分析怎麼做（`reflectset.go` 的檔頭有完整論證）：查詢點是會讀表的呼叫（`Class` 的成員存取子、`Field`／
  `Method`／`Constructor`／`Parameter`／`Annotation` 的每個方法、`Class.forName`），接收者可能的類別由一套
  來源規則算——類別字面值指名一個類別、`getClass()` 指到靜態型別與它底下所有子類別、`Class` 變數取所有被寫入的
  值、反射存取子（`getType`／`getReturnType`／`getParameterTypes`／`getSuperclass`／`getDeclaringClass`…）
  往它出來的類別追；**跟不下去的一律放大成所有類別**。`Class.forName` 的引數只認編譯期讀得出來的名字
  （字面值、`static final String`、兩者的串接）：讀不出來的名字等於排除不掉任何類別，類別表保持全部。
  標準庫自己的反射（`Validation`、`Gson`／`JsonBinding`、`ApplicationContext`、`RouteScanner`、`HttpServer`…）
  才是真正難的地方——它們反射的類別是程式交給它們的，自己的原始碼裡看不到。做法是把它們當「反射使用者」，
  取**靜態呼叫邊**（含建構呼叫）的閉包往上找誰會呼叫到它們，得到 31 個類別；程式自己的程式碼**提到**其中
  任何一個（型別、呼叫擁有者、字面值、`new`、轉型…），兩張表就都保持全部。
  **分派不是這裡的邊，兩個方向都不是**：一次分派需要的*實例*是由指名它那個類別的程式碼建出來的，而那是
  靜態呼叫，所以建它的類別（以及任何到得了那個類別的類別）本來就在上面那個閉包裡。改成分派的話整個標準庫
  都會進閉包——`lib/22_stream.teyru` 的 `closers.get(i).run()` 到得了每一個 `Runnable`，其中一個是網頁伺服器的
  `ConnectionTask`，於是 `String` 也會進閉包，每個程式都退回不剪。
  驗證是 `reflect_test.go` 的 `TestReflectReach`（產生的 C 帶哪些表、程式跑出來的答案）與
  `TestReflectReachFallsBack`（界算不出來時每張表都留著），加上 `go test ./...`。
  **W13.3「生成的 C 拆成多個編譯單元、按 CPU 數平行編譯」已落地**：`internal/driver/cc.go` 把「一次
  `cc` 帶著一整串來源、在一個行程裡一個檔一個檔編」換成「每個單元一次 `cc -c`、同時最多
  `runtime.NumCPU()` 個、再一次連結」，兩個路徑都走它。單元是：`-O2` 的**程式整個編譯單元**
  （`codegen.Emit`，標準庫在裡面）、執行期的七個檔案（能到 TLS 是八個）、`--native` 的來源；debug
  另外是程式自己的單元（`EmitProgram`），標準庫是快取裡的 `prelude.o`（冷快取時它自己先編一次，
  那是單一單元，本來就沒得平行）。**連結順序刻意保持原樣**（程式的單元、`prelude.o`、執行期），
  所以同一支程式同一級別的執行檔**逐位元組相同**。
  **LLVM 後端那條路也改了**：`compileLLVM` 的 `cc` 一樣是「一個行程編一整串來源」（模組＋執行期），
  現在同樣是每個單元一次 `-c`、同時跑、再一次連結；模組那個單元帶 `-x ir`（呼叫端可以把檔案命名成
  任何名字），而「模組不驗證」的診斷靠新的 `ccFailure` 把該單元自己的 stderr 帶回來才分得出來，
  所以那條訊息沒有變。hello 的 LLVM 執行檔（`-O0` 81,728 B、`-O2` 59,760 B）前後逐位元組相同；
  `scripts/backend-matrix.sh` 對 t146_reflect／t196_string_bytes／t84_sealed_switch／t133_arrow_blocks／
  t260_eval_order 跑六格：0 分歧、0 非指名建置失敗（三格是 LLVM 既有的 TY-INT-0100 指名拒絕）。
  **`-O2` 不拆生成的那一個單元**：webapp `-O2` 用會記錄起訖的 `cc` 包裝量到，63.9 s 裡 **LTO 連結
  50.6 s**、整個程式那個單元自己 **12.7 s**、其餘七個執行期單元 0.06～0.51 s（八個同時起跑）；
  拆那個單元只會動到那 12.7 s，而 LTO 連結要處理的 IR 一個位元組都不會少，所以政策不變
  （單一單元＋LTO）。hello `-O2` 同一個量法：2.39 s 裡整個程式那個單元 1.39 s、LTO 連結 0.27 s。
  **debug 變快**（本機 linux/amd64、Ryzen 7 5700X 16 執行緒、clang 22.1.8、暖快取、`/usr/bin/time -v`
  量 `teyru build`、前後**交錯**各三次；這台機器同時被別的 agent 壓到 load 10～30，所以中位數只當方向，
  不是精確值）：hello `-O0` 1.14→**0.76 s**、`-O1` 2.92→**2.13 s**；webapp `-O0` 1.69→**1.32 s**、
  `-O1` 4.09→**3.07 s**；t146 `-O0` 1.87→**1.54 s**、`-O1` 5.35→**3.42 s**。分階段（同一支 hello
  `-O0`，load 4～8）：`cc` 那一段 685→394 ms（同一台機器用 `/bin/true` 當 `cc` 量到的非編譯部分是
  68 ms），總牆鐘 1109→655 ms。
  **release 沒有回退**：九格（三支程式 × `-O0`／`-O1`／`-O2`）的執行檔**逐位元組相同**、大小一格沒變
  （hello 151,976／128,216／80,000、webapp 8,567,064／7,264,576／9,079,856、t146 8,784,800／
  7,425,104／9,694,296）。`-O2` 的牆鐘在 load 10～30 之下前後分不開（webapp 中位數 144.06 對
  134.69 s、t146 87.60 對 72.06 s，兩邊都跨過雜訊），而**結構上沒有回退的機制**：那一格裡 80% 是
  沒有改動的 LTO 連結，`-O2` 的執行檔也逐位元組相同。
  **有測試盯著單元數與平行度**（先前一個都沒有）：`internal/driver/cc_test.go` 用一支只記錄起訖、
  不編譯任何東西的假 C 編譯器量「同時幾個在跑」（`TestCompileUnitsParallel`，單元數取 4×CPU 以盯住
  上限），並在真的建置裡盯住「每個單元恰好一次 `-c`、連結恰好一次、連結拿到的是目的檔而不是來源」
  （`TestBuildCompilesEveryUnitSeparately`，`-O0` 與 `-O2` 各一次）。
  **重上之後的重量（本機 linux/amd64、16 核、`/usr/bin/time -v` 量 `teyru build`、三次取中位數，
  括號是當時的 load average）**：webapp `-O0` 暖 **1.85 s**（load 6.9；十秒目標達成）、`-O1` 暖 2.39 s；
  `-O2` **30.30 s** 對照不帶本支的 31.26 s、執行檔**逐位元組相同**（hello 67,240 B、webapp 5,399,568 B），
  release 的時間與大小都沒有回退。hello 的暖編譯是 **0.88～1.12 s**（load 3～7），而**同一個基底在更早
  一次用同一個方法量到 0.64 s**——同一個樹自己就差了 73%，所以**一秒那條線在這個方法下無法判定**，
  不是達成也不是未達成（快取抓取本身只花 14 ms，不是它）。
  **兩個解釋都被推翻，下一個人不要再用**：①連結假說——`prelude.o` 兩邊一樣大（5,324,976 對
  5,284,048 B），快取抓取兩邊都是 14 ms，物件沒有變大；②「重上之後樹變重」——同一個樹、同一個方法
  分別量到 0.64 s 與 1.11 s。剩下的成本在**程式單元的編譯＋連結**（暖建置 603～730 ms，總牆鐘
  880～1110 ms），那是兩邊一樣的量。
  **一個必須更正的數字**：先前寫的 12.7 MB 是 **webapp 的反射變體**物件，不是 hello 的；hello 的
  `prelude.o` 是 5.3 MB。照那個數字去縮小物件會縮錯對象。
  **要有人做才會有答案的下一步**：在 load < 2 下量**至少 10 次**，報最小值與四分位距——三次中位數
  在 load 3～7 之下解析不了一秒的線。重上之後**已經驗過的是行為**：冷快取與暖快取的執行檔對 hello、
  `examples/webapp`、字串探針、`t196_string_bytes`、`t146_reflect` 在 {C, LLVM} × {-O0, -O2} 的每一格
  都**逐位元組相同**且輸出、離開碼相同（20 格 0 壞，`-O0` 的 C 格 `cached=True`）；改 `lib/` 一個字元
  重編編譯器後 key 變成不同目錄、物件數 1→2、舊物件沒被動到，而新程式印的是新標準庫的字串；改回去
  後原本那一格被重用；截斷的物件、翻轉一個位元的物件、沒有 `manifest` 的 entry 都是未命中
  （`make cache` 第 8、9 節）。
- **棧上根集合的精確化：每一格的框架地圖（`tyrt.c`／`tyrt.h`／`codegen/frames.go`，預設開啟，
  `TEYRU_NO_FRAME_MAPS=1` 關掉）**：收集器的根除了陰影棧、靜態位址表、每個執行緒預先配置的
  SOE 之外，其餘過去一律是**保守掃描**：從停止點（或收集者自己的 `setjmp`）往上把每一個字當成
  可能是指標。代價是可量測的：**任何一個字只要看起來像物件的位址就留住它**，而 C 編譯器留在框架
  裡的字不歸我們管。最乾淨的例子是**含有 `try` 的方法**——產生器會把該方法**所有區域變數標成
  `volatile`**（longjmp 之後非 volatile 的區域變數值未定義），於是 C 編譯器**不得刪掉那個寫入**，
  一個已經死掉的 8 MB 陣列的指標就一直坐在框架裡到方法結束。實測：`tests/programs/t245_dropped_reference`
  （2M 個 int、有 `try`、丟掉之後做 30 萬次配置再 `System.gc()`），`System.liveBytes()/1MB`
  在舊收集器是 **8**，在地圖之後是 **0**——同一個程式、同一份期望輸出。
  **機制**：每個方法在進入時把 `tyframe`（`prev`、`slots`、`nslots`、`cap`、`fno`、`lo`/`hi`、
  `saved[10]`）串進 `ty_frames`；`framePass`（在產生的 C 上跑的文字掃描，因為指標型別的宣告散在
  產生器上百處）在**每一個指標型別的宣告之後**把它的位址寫進框架的 `_tyfs`，`_tyfs` 在序言裡
  **零初始化**（還沒執行到的宣告代表「這裡還沒有值」，而 0 就是那個意思）；`saved[]` 是**暫存器
  遮罩**——`ty_frame_enter_` 在自己的框架裡讀 ABI 的 callee-saved 暫存器，那些值就是呼叫者的；
  `lo` 由 helper 自己的框架位址推得（所以 `ty_frame_enter_` **必須 noinline**：LTO 一內聯，
  兩個界線就相等，地圖看起來像在描述一個空的範圍）；`fno` 是函式 C 名稱的 FNV 雜湊，產生 C 裡
  每個函式旁有註解寫出它，所以診斷報告能對回名字而 bin 裡不用放名字。收集器對**有地圖的框架**
  只讀地圖指名的字（外加暫存器遮罩），**保守掃描只跑地圖沒覆蓋的補集**——執行期自己的框架、
  平台層的等待框架、停止點濺出的暫存器、沒有地圖的產生框架，全都照舊被保守掃描，所以**執行期
  自己的 root 紀律（`TY_ROOT_PUSH`）與 `_Static_assert` 一個字都沒動**。
  **死掉的參考要主動放掉**：(1) **kill**——區塊內某個參考的**最後一次被提到的那個敘述之後**
  產生器寫 `VAR = NULL`（「提到」記在 `localName`，那是每個讀寫都會經過的地方，所以不會漏），
  只在**宣告它的區塊**決定，所以迴圈裡的 kill 不會被下一輪讀到；(2) **陳述式運算式的暫時變數**
  在該敘述結束時把它的字還回去（`_a` 這種）；(3) **在飛的值**（`f(g(), h())` 裡 g() 的結果）
  由 `frameWrap` 先放進框架自己的字，否則 C 編譯器把它放在哪裡不是地圖能指名的。
  **`setjmp` 的落地框架**：`ty_throw` 在 `longjmp` **之前**把 `ty_frames` 倒回 `ty_cur_catch->frames`
  （每個 arm 的地方都記），所以收集器走的是落地框架的地圖而不是已經被跳過、儲存空間即將被重用的
  那些框架——記錯方向不是多留一個物件，是**少一個 root**。
  **仍然保守、以及為什麼**：C 編譯器自己的字（它為跨呼叫的值選的溢位槽、它保存的暫存器、
  它為呼叫準備的傳出參數區）不在地圖裡，只被保守掃描覆蓋；執行期自己的 C 框架也是；沒有地圖的
  產生框架（`emitMethod` 以外的產生器）也是。方向是刻意的：**沒指名的字一律保守掃描，所以
  少 root 不可能發生，代價是多留**。**診斷**：`TEYRU_GCTRACE=1` 多印 `frames=/precise=/conservative=`
  （走了幾個框架、精確讀了幾個字、保守掃了幾個字）；`TEYRU_GC_VERIFY=1` 會去讀**有地圖的框架裡
  地圖沒指名的字**，把每一個指向存活物件的印出來並附函式雜湊——這是「有沒有漏 root」的量尺；
  `TEYRU_GC_NOEXCL=1` 把保守掃描退回整個範圍，`TEYRU_NO_FRAME_MAPS=1` 把整個機制關掉。
  本項落地時**尚未**完成：完整 `go test`／`TEYRU_GC_STRESS=1` 的整輪驗證、以及六個基準的前後數字
  （見下一個項目）。

- **地圖真的漏過四個字，以及釘住它們的檢查（`internal/codegen/frames_test.go` 的
  `TestEveryFrameWordIsMapped`）**：地圖漏一個字不是多留，是**少一個 root**——收集器對有地圖的
  框架只讀地圖指名的字，只有範圍的補集被保守掃描，所以沒被指名的參考就是一個「程式還拿著、
  收集器已經回收掉」的物件。用**真實產生的 C**（每個案例都含標準庫，四千多個有地圖的框架）
  逐字檢查之後，上面那句「沒指名的字一律保守掃描，所以少 root 不可能發生」在**有地圖的框架裡
  不成立**——沒指名的字只被保守掃描是對**沒有地圖的框架**說的；四個形狀真的漏了，都修了：
  (1) **`T* volatile x`**——`frameDeclAt` 把 `volatile` 讀成名字，於是**每一個含有 `try` 的方法
  的每一個區域變數**都沒被指名。實測：`int[] a = new int[16MB]`，只有那個 volatile 區域變數
  指著它，中途 `System.gc()`；修好前 `System.liveBytes()/1MB` 是 **0**（陣列被回收，程式之後
  還去讀它的元素），修好後是 **16**，`TEYRU_NO_FRAME_MAPS=1` 也是 16。
  (2) **宣告的初始化式裡面的宣告**（`T* x = (T*)({ U* y = ...; y; })`）：掃描讀到第一個宣告
  自己的 `;` 就跳過去，裡面的 `y` 從沒被看見。
  (3) **方法主體的第一個宣告**：掃描只在 `;`／`{`／`(`／`}`／換行的**後面**找宣告，而主體的第一個
  敘述前面沒有任何字元。
  (4) **放在框架裡的物件（逃逸分析提升到堆疊的那些）的參考欄位**：物件本身就是一個 struct，
  它的參考欄位是這一格的參考字，卻沒有任何「指標型別的宣告」把它們寫出來；而那個 struct 落在
  有地圖框架的範圍**內**，所以連保守掃描都不到。實測：把 2M 個 int 的陣列放進提升物件的欄位、
  中途 `System.gc()`；修好前 `liveBytes()/1MB` 是 **0**（陣列被回收），修好後是 **8**，
  `TEYRU_NO_FRAME_MAPS=1` 也是 8。修法不在文字掃描：`stackNew` 在物件宣告處留一個
  `/*ty-f: ... */` 記號（它知道類別的版面），`framePass` 在那裡給每個參考欄位一個格位——不能放
  進序言，序言看不到區域變數。
  **檢查**：`TestEveryFrameWordIsMapped` 自己讀產生的 C 找宣告與參考欄位（不呼叫 `frameDeclAt`，
  否則無法與它意見相左），要求每一個後面都有註冊，並在每次執行時斷言四個形狀都真的出現在
  案例裡；四個形狀各別用「把修好的地方改回去」證實會讓它變紅（訊息見檔案表頭）。仍然偏向
  多留的地方：提升物件死亡之後，它的欄位字要到框架結束才放手。

- **框架地圖的代價與收益（都量過，寫在這裡因為下一個看到 `make ci` 變慢的人需要這組數字）**：
  **收益是執行期的記憶體**——`t245_dropped_reference` 的 `System.liveBytes()/1MB` 從 **8 降到 0**
  ✓（一個已經死掉的 2M-int 陣列不再被框架留住 ✓），同一份原始碼在 `TEYRU_NO_FRAME_MAPS=1` 下仍是 8。
  **代價是編譯時間**：
  - 套件走的那條路徑（`-O1` → `debugOpt` 為真 → `compileSplit`，prelude 有快取物件）上，
    **真正被編的那個單元**（`t146_reflect` 的 split `program.c`）從 **321,122 B 變成 368,286 B
    （+14.7%）**；八支散佈整個套件的程式、`CC=gcc` 各三輪：**1.83 s/支（關）對 2.08 s/支（開）
    ＝ +14%**；雛形 b（見下）是 **1.82 s/支**。
  - `t146_reflect` 自己的方法 try 很多，是**離群**：那支的 gcc 從 **2.3 s → 3.7 s（+61%）**。
  - `teyru emit` 的**全程式輸出**（含被快取掉的 prelude）從 11,946,565 → 18,049,562 B（**+51%**，
    `t146_reflect`）、6,634,088 → 12,359,867 B（**+86%**，`t245_dropped_reference`，幾乎全是 prelude）。
  - 所以「地圖讓 gcc 腿超時」的正確版本是：**安靜時那條腿約 17～18 分**（由 `EvalOrder` 安靜量到的
    931 s 乘上上面量到的 +14%），**45 分的上限有充裕餘量**；一條在**爭用**下量到 `panic: test timed
    out after 45m0s` 的腿（2701 s）**不能用來推論 CI**，而那個 2701 s 裡每一支測試都通過。
  - **還有一個量過的替代方案（未落地）**：只對**含 `try` 的方法**插地圖（`!e.fnTry` 一個條件，
    因為 volatile 區域變數與 longjmp 落地框架正好就是那個判準）——產生的 C **+86% → +8%**、
    建置時間 **+14% → 0%**、而 `t245` 的 **8 → 0 完全保留**。它不改預設值，只把成本付在需要的地方。
  - **一個教訓（同一天裡三次同一個家族）**：**量成本之前先確認你量的是真正會跑的那條路徑**——
    這裡差點被**全程式的 `teyru emit` 輸出**誤導（它含**被快取掉**、每次建置並不重編的 prelude），
    而套件（`-O1` → `compileSplit`）與其他建置實際編的是 **split 單元**：**單元 +14.7%**、
    **腿 +14%** 才是這一節該引用的數字，全程式的 **+51%／+86%** 只在**沒有 prelude 快取**的路徑
    （`teyru emit`、`-O2` 的全程式路徑）上成立。

- **一個必須說清楚的實作陷阱（同一次工作裡踩到的三個，都修了）**：(1) LTO 會內聯
  `ty_frame_enter_`，於是它自己的框架就是呼叫者的，`lo == hi`、範圍空掉——**任何「靠自己是
  另一層框架」才成立的 helper 都必須 `noinline`**；(2) 「還沒執行到的註冊」留下的是**零**，
  收集器不能去解引用那個零（`tyrt.c` 的 `mark_frame`，實測 SIGSEGV、`si_addr=NULL`、
  用訊號處理器裡的 frame-pointer 鏈＋`addr2line` 定位到 `mark_frame` ← `gc_collect`
  ← `ty_alloc_slow` ← `ty_clinit`）；(3) 堆疊重用掉的框架記錄會讓收集器走進垃圾，所以
  `frame_map_ok` 先驗證字陣列落在該執行緒的堆疊範圍內，不成立就**不採信那筆記錄**（它的字
  照樣被保守掃描，所以不會少 root）。

- **兩個後端的語意一致性（W8.2 的清單在 `scripts/w8-inventory.md`）**：計畫寫的是「已經分叉過的
  語意規則要下沉為共享的 lowering」，而清單做出來的結論比那個具體：**規則本身已經共享了**
  （運算型別由檢查器寫在 AST 的 `Binary.OpType`／`Assign.OpType` 上，快取與 `valueOf` 在
  `lib/04_boxing.teyru`，WTF-8 的合併規則在 `util.StrConcat` 與執行期的 `ty_str_concat`），
  **重複的是 lowering**——而歷史上咬人的兩件事（浮點窄化飽和修了兩次、複合 `/=` 的提升 bug
  只在 LLVM 後端）都在 lowering 裡。清單把該共享的五件事按風險排好了：移位的遮罩（兩邊寫法
  不同）、null 與越界檢查的產生（26 處，最大的一份）、装箱／拆箱的 kind→accessor 表（兩份）、
  `promoteOf` 搬家（它已經是共享的，只是住在 C 後端的檔案裡），以及 `constDivOp`（常數除數的
  折疊**只有 C 後端有**，所以跨後端的整數除法時序比較量的是不同的程式）。**這一輪只交清單、
  沒有動程式碼**；`scripts/backend-matrix.sh` 是那個「兩個後端不能分叉」的測試。
- **LLVM 後端不帶框架地圖，於是跑保守的收集器（`llvm.go` 的 `EmitLLVM`，先前的指名拒絕已移除）**：
  地圖是預設之後，LLVM 後端是唯一會產生「收集器以為 roots 精確、後端卻從未說哪些字是參考」
  的程式的地方——它產自己的 IR，參考可能住在 SSA 暫存器或它自己選的 alloca 裡，失敗模式是
  幾天後才出現的 use-after-free。**處理方式不是拒絕，而是讓這個後端建出來的程式用保守的
  收集器**（`tyrt.h` 的 `ty_frame_map_ok` 對沒有地圖的框架回 false，於是那些字被保守掃描），
  代價是**多留而不是少留**：`t245_dropped_reference` 在這個後端是 8、在 C 後端是 0，而
  `scripts/backend-matrix-allow.txt` 就是為這一格存在的。先前這裡是**指名拒絕**
  （`TY-INT-0100`），理由是「沒有地圖的後端會拿到它從未描述過的精確 roots」——**那個看法是
  錯的**，而且拒絕每一支程式讓矩陣的一半無法測試。`internal/codegen/llvm.go` 的檔頭把這件事
  （含那句自我更正）寫下來了；`AGENTS.md` 這一段先前仍寫著「預設指名拒絕」，是落後的敘述。
  要讓這個後端也精確，只有兩條路：在 IR 裡把參考放進 alloca 並在每個安全點之前把 live 的
  SSA 值寫進去（本後端目前沒有 liveness 分析），或在該後端實作 stackmap。
  **尚未完成、要讀的人知道的事**：建構子的**捕獲參數**沒有被註冊（`funcParams` 只看
  `m.Params`），因此它們是保守掃描覆蓋的字，不是地圖指名的字。
  **下一步（還沒開始）**：地圖給的是「這個框架的哪些字此刻是參考」的精確清單；要再往前一步、
  讓收集器能移動或分代，需要的是**每個安全點各自的 live 位元圖**（同一個清單乘上 safepoint id，
  產生器在每個安全點更新框架記錄裡的那個 id），以及**寫入屏障**（移動需要知道誰指向誰，現在的
  收集器完全沒有屏障的概念）。兩者都不是這個改動的一部分，而這個改動把它們需要的前置條件
  ——精確的、每個字都可指名的 root 集合——擺好了。

- **JDK 21 的三條訊息與兩個裝箱建構子（已補，W6.4 與 W7 一起）**：`tests/programs/t281`
  到 `t284` 是**先以紅測試的形式進來**的（`teyru-lang/tests` 的 `ab31460`），編譯器那一半
  從來沒有寫過。測試的自查機制（`driver_test.go`）把「已列為 known failure 卻通過」當成
  失敗，所以條目必須跟著刪。**四個探針的 `.expected` 全部由本機的 JDK
  21.0.11+10 產出並在合入前重跑過**，不是手寫的。
  - **負陣列長度從編譯錯誤變成執行期例外**。`sema/check.go` 的 `TY-TYP-0067` 刪了（那是
    javac 不會擋的寫法）；執行期的 `ty_negarr` 現在**收長度**並把它放進訊息（`-1`），
    呼叫端多了一個參數（`ty_alloc_arr`／`ty_sb_init`／`ty_reflect_array_new`）。順序照
    JLS 15.10.2：每個寫出來的維度先由左至右求值，**再**由外而內逐一檢查——所以
    `new int[0][-1]` 仍然拒絕內層的 -1（外層是空的，JDK 也拒絕），而 `new int[-1][-1]`
    報外層那一個。**兩個後端都改了**（`emit_expr.go` 的 `newArrayDims` 與
    `llvm_call.go` 的同名前段），因為 C 後端原本只測它所配置的那一層。
  - **`ClassCastException` 帶上模組與載入器**。模組的判定是「名字以 `java.` 開頭」——
    **這不是一條關於 java.lang 的規則**：實測 JDK 21，`java.lang.Integer`、
    `java.lang.Comparable`、`java.lang.Runnable`、`java.lang.Thread`、
    `java.lang.StringBuilder` 全部印全名，**只有 `java.lang.Object` 與
    `java.lang.String` 印簡名**，而那是 JDK 自己的文字唯一對類名做的事。兩個在同一模組時
    併成一個子句（`A and B are in ...`），不同則用分號。
  - **`ArrayStoreException` 的訊息是放不進去的那個值的類別**（不是一句關於陣列的話），
    所以 `ty_arraystore` 從無參數變成收那個值；參考元素陣列的 store 檢查本來就在
    `tyrt.h` 的 `static inline` 裡，現在**產生器另外寫一次** null 測試（多一次已經載入過
    的值比較），因為執行期拿得到指標、拿不到「程式寫的那個運算式」。
  - **helpful `NullPointerException`（JEP 358）**。執行期新增 `ty_npe_msg(action, cause)`，
    兩個半句都是編譯期常數，執行期只負責組句子。**因果那一半是 javac 的 bytecode slot**，
    規則是量出來的而不是推出來的（`internal/codegen/npe.go` 的檔頭記著怎麼量的）：
    靜態方法從 0 算參數、實例方法從 1（slot 0 是接收者），`long`／`double` 佔**兩格**
    （實測 `static void f(long a, long b)` 的 b 在 slot 2），**離開宣告它的區塊就回收那格**
    ——所以 JDK 自己的輸出裡一連串兄弟 try 區塊的區域變數**都是 `<local1>`**。
    參數印成 `<parameterN>` 從 **1** 算起（slot 是 0），這兩件事不一致，是量出來的。
    **版面是每個 method 一份地快取**（`npeSlots` 依 method 分層），因為一個 method 的版面
    靠走它的本體算出來，共用一張 map 會讓後來的 method 蓋掉早先的那一份。
  - **兩個裝箱建構子**：`new Integer(int)` 與 `new Boolean(boolean)` 是 JDK 21 仍然保留的
    API（deprecated for removal），**而且永遠是新的箱子**——快取回答的是 `valueOf` 與自動
    裝箱，不是建構子，所以 `new Integer(127) == Integer.valueOf(127)` 是 `false`，
    連快取範圍內的值也是。`lib/04_boxing.teyru` 的 `fillCache`／`valueOf`／`Boolean.make`
    順便改成呼叫新宣告的建構子（那正是 JDK 自己寫那两个常數的方式）。
  - **`CharSequence` 補上了**（`lib/01_core.teyru`，`String` 宣告 `implements` 它）。它以前
    不存在，`t282` 裡的 `new CharSequence[2]` 是編譯期就被擋下來的。
  - **兩個 `.experr` 跟著 JDK 的文字改了**（`t80`／`t87` 的未捕捉 NPE）。舊的檔案記的是
    執行期**以前印的**訊息，不是 Java 印的訊息；兩者都在 JDK 21.0.11+10 上跑過確認。
  - **兩個後端的六格矩陣全綠**：`sh scripts/backend-matrix.sh` 的
    `{C+clang, C+gcc, LLVM} × {-O0, -O2}` 四支探針 0 分歧、全部符合 `.expected`。
  - **一個已知的取捨**：`ty_array_store_ref` 保留了自己的 null 測試（產生器那一次是額外的），
    所以一個沒有走產生器路徑的 store（原生輔助、反射路徑）仍會被它擋住——只是訊息退回
    舊的那句。反過來說，少寫一次就多一條可能漏掉的路徑。

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
