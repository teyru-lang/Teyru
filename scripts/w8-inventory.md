# W8.2 的清單：兩個後端各自實作了什麼

這是一份**內部紀錄**（與 `scripts/lib-bytes-audit.md`、`scripts/w14-analysis.md` 同性質），
不是使用者文件。計畫的 W8.2 說：

> 已經分叉過的語意規則——數值提升與窄化、複合賦值的隱含轉換、移位、整除與取餘、字串拼接、
> 装箱與拆箱、null 與越界檢查——在 sema 之後下沉為共享的 lowering（生成顯式的轉換與檢查節點）。
> 兩個後端只做機械翻譯，刪除各自的重複實現。

這一輪做的是**清單**（計畫自己把「先做清單」放在最前面），而清單的結論比預期具體：
**規則本身已經共享了，重複的是 lowering，而咬人的正是 lowering。**

## 方法

兩個後端各自讀一遍，找出計畫點名的每一條規則在哪裡、兩邊是不是同一個演算法。
「規則」指**決定**（這個運算的運算型別是什麼、要不要丟例外），「lowering」指**怎麼產生**
（把哪個呼叫寫進 C、把哪個指令寫進 IR）。行號是 `main @ a47f5bf` 的。

## 清單

| 規則 | C 後端（`emit_expr.go` 等） | LLVM 後端（`llvm_expr.go` 等） | 裁決 |
|---|---|---|---|
| 數值提升與窄化 | `promoteOf`（1486）、`coerce`（80）、`narrowTarget`（1283） | `coerce`（510）、`coerceFrom`（518） | **規則已共享**：運算型別由檢查器寫在 AST 上（`Binary.OpType`／`Assign.OpType`，`ast.go:549,555`），`promoteOf` 也**只有一份**——但它住在 C 後端的檔案裡，而 LLVM 後端也用它（`grep promoteOf` 是 4 對 1）。lowering 各一份。 |
| 複合賦值的隱含轉換 | `compoundOp`（1607）、`assignBoxed`（1581）、`boxedUpdate`（1528） | `compoundDiv`（1570）、`boxedIncDec`（948） | **規則已共享**（`opTypeOf(a, t)` 讀的是檢查器算好的 `OpType`）。**當年 `/=`、`%=` 的 bug 就在 LLVM 這一邊的 lowering**——它自己算了一次轉換，而 C 那一邊讀 `OpType`。 |
| 移位 | `shiftCount`（1906）遮罩成 `& 31`／`& 63`，並把遮罩後的值轉回有號 | `shift`（1190）：`and width-1` 之後 `shl`／`ashr`／`lshr` | **重複的規則**：遮罩寬度與「轉回有號」這件事兩邊各寫一次，而且兩邊的寫法不同（C 是 `(int32_t)((uint32_t)x & 31)`，LLVM 是一個 `and` 指令）。這是清單上第一件該共享的。 |
| 整除與取餘 | 呼叫執行期的 `ty_div_int`／`ty_rem_int`（1622、2038、2054、2334），另有 `constDivOp`（2071）把常數除數折成乘法與移位 | `divide`（1152）、`remainder`（1169）：同樣呼叫那兩個執行期 helper，**沒有** `constDivOp` | **行為已共享**（同一個執行期 helper），**最佳化只有一邊有**：C 的常數除數折疊在 LLVM 上不存在。那不是語意分叉，是效能分叉——而它讓兩邊產生的程式差很多，所以任何跨後端的時序比較都量到不同的東西。 |
| 字串拼接 | `concat`（2130）、`concatFrom`（2136）、`concatParts`（2150） | `concat`（1215）、`concatParts`（1232） | **規則已共享**：WTF-8 的合併規則在 `util.StrConcat`（編譯期折疊）與執行期 `ty_str_concat`；兩邊的 lowering 各一份，但都只是把運算元接起來交給那兩個地方。 |
| 装箱與拆箱 | `boxCall`（177）、`unboxCall`（247）、`unboxAccessor`（279）、`boxKindOf`（1515） | `boxValue`（540）、`unboxValue`（561）、`unboxValueMsg`（567）、`asBox`（931） | **規則已共享**（快取與 `valueOf` 在 `lib/04_boxing.teyru`，檢查器決定哪裡要裝箱）；lowering 各一份，而且**各有一份「哪個 kind 用哪個 accessor」的表**。 |
| null 與越界檢查 | `boundCheck`（1196）把檢查與取值寫成一個敘述運算式、`npeCall`（`npe.go`）產生訊息 | `nullCheck`（438）、`boundsCheck`（1443）、`fieldAddr*`（350、357） | **兩邊各一份 lowering**，而且是最大的一份：`ty_npe`／`npeCall`／`nullCheck` 在三個檔案裡共 26 處（`grep -c`：C 10、LLVM 11、`llvm_call.go` 5）。訊息本身（JEP 358 的兩個半句）由 `npe.go` 的規則產生，那一半是共享的。 |

## 結論

**「規則下沉」這件事已經發生過了**：運算型別、要不要丟例外、快取怎麼用、WTF-8 怎麼併——這些
決定都在檢查器、`util` 或 `lib/*.teyru` 裡，兩個後端只是照著產生程式碼。計畫寫的
「兩個後端分別實現 Java 語意」在**決定**這一層已經不成立。

**重複的是 lowering**，而**歷史上咬人的兩件事都發生在 lowering 裡**（`AGENTS.md` §10 記著：
浮點窄化飽和修了兩次、複合 `/=` 的提升 bug 只出現在 LLVM 後端）。所以 W8.2 真正有用的版本
不是「把規則再往下搬一層」，而是**把 lower 的輔助函式共享**——尤其是：

1. **移位遮罩**（兩邊各一份、寫法不同、而 C 的寫法要處理有號轉回）；
2. **null／越界檢查的產生**（最大的一份，26 處）；
3. **装箱／拆箱的 kind→accessor 表**（兩份）；
4. **`promoteOf` 搬家**（它已經是共享的，只是住錯檔案）；
5. **`constDivOp` 兩邊都有或兩邊都沒有**（現在只有 C 有，所以跨後端的時序比較在整數除法上
   量的是不同的程式）。

前四項是「讓分叉不可能發生」，第五項是「讓量測可比」。

## 沒做的、沒看的

- **沒有動任何程式碼**：這一輪只交清單（計畫的 W8.2 第 2 步是「先做清單」，而第 3 步才是實作）。
- **沒有量**「共享之後每個後端少幾行」——那是實作那一步的事。
- **只讀了 `emit*.go` 與 `llvm*.go` 的對應函式**，沒有逐一比對兩個 lowering 在每一個型別組合
  上的行為（那需要一份跨後端的差異測試，而 `scripts/backend-matrix.sh` 就是那個測試的雛形：
  它對每支程式比六格的輸出）。
- **沒有看 `emit_synth.go`／`llvm_synth.go` 與 `native.go`**，那兩邊也有各自產生的程式碼。
