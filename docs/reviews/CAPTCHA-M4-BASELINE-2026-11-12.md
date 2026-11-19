# CAPTCHA M4 Qualification Gate B — 明顯自動化攻擊基線（2026-11-12）

> **狀態：Gate B 評測報告，不是生產批准。**
> 基線 OID：`b5506989075c82927ead14cda0e71a8f9f401803`（Gate A 完成後的乾淨 HEAD；
> 分支 `fix/release-preparation-2026-11-02`）。
> 本次工作區是 dirty，但差異只有 **新增** 的 M4 工具、evidence 與本報告、
> `Makefile` 的一個 dev target，以及 `docs/DESIGN.md` §15/§19 的結構與變更記錄；
> `internal/generator`、`internal/admin`、`internal/dispatch`、
> `cmd/server`、`web/` **沒有任何修改**。
>
> 本報告逐項區分 **PASS / FAIL / BLOCKED / NOT_RUN**；所有未實測項不得讀成通過。
> 7 個題型全部維持 `experimental:true` / `production_ready:false`，權重與預設站點設定
> **沒有**被本輪修改。本文的數字**不是**抗 AI / 抗 OCR 證明；正好相反：它證明一組
> 確定性程式能從公開題面直接解出這些題。

---

## 0. 結論摘要

| 題型 | 成功／總數 | 成功率 | 95% CI（Wilson） | p95 CPU | p95 wall | Gate B 判定 | 狀態 | 建議行動 |
|---|---:|---:|---|---:|---:|---|---|---|
| `tile-rotation-v1` | 1000 / 1000 | 100.0% | [99.62%, 100%] | 4.64 ms | 1.00 ms | **FAIL** | KEEP_EXPERIMENTAL | REDESIGN |
| `symbol-path-v1` | 991 / 1000 | 99.1% | [98.30%, 99.53%] | 0 ms¹ | 4.15 ms | **FAIL** | KEEP_EXPERIMENTAL | REDESIGN |
| `crowd-find-v1` | 845 / 1000 | 84.5% | [82.13%, 86.61%] | 0 ms¹ | 6.24 ms | **FAIL** | KEEP_EXPERIMENTAL | REDESIGN |
| `visual-rank-v1` | 1000 / 1000 | 100.0% | [99.62%, 100%] | 13.66 ms | 2.58 ms | **FAIL** | KEEP_EXPERIMENTAL | REDESIGN |
| `progressive-target-v1` | 1000 / 1000 | 100.0% | [99.62%, 100%] | 22.76 ms | 6.26 ms | **FAIL** | KEEP_EXPERIMENTAL | REDESIGN |
| `shell-track-v1` | 1000 / 1000 | 100.0% | [99.62%, 100%] | 57.21 ms | 16.74 ms | **FAIL** | KEEP_EXPERIMENTAL | REDESIGN |
| `silhouette-assembly-v1` | 1000 / 1000 | 100.0% | [99.62%, 100%] | 6.93 ms | 1.43 ms | **FAIL** | KEEP_EXPERIMENTAL | REDESIGN |
| **合計** | **6836 / 7000** | **97.66%** | [97.28%, 97.99%] | — | — | **7 FAIL** | — | — |

¹ 參考主機（Windows）的 process CPU 計數器粒度約 15.6 ms，單題小於此值時 CPU delta 常為 0。
`p95 wall` 是單執行緒 solver 的實際每題耗時，兩者都遠低於 1 秒門檻；量測口徑見 §1.3。

**判定**：

- `FAIL`：成功率 ≥ 80% **且** p95 ≤ 1 CPU 秒。7 個題型全部符合，且 `crowd-find-v1`
  的 95% CI 下界（82.13%）也高於 80%，不是「點估計剛好過線」。
- `FAIL/REDESIGN`（20%–80% 或顯著高於隨機猜測）：本輪沒有題型落在這一格，因為全部
  都已被第一條 FAIL 線覆蓋；但所有題型同時也「顯著高於隨機猜測」（見附表 A）。
- `PASS`：0 個題型。任何「PASS」都只能表述為「通過本輪已定義 baseline」，不能表述成抗 AI。
- `BLOCKED` / `NOT_RUN`（7 個 solver 本身）：0 個；7 個 baseline 都已實作並實際跑完
  1,000 個獨立 challenge。
- 工程面的 `-race` 與 Windows 全量測試有 BLOCKED 項，與題型判定分開列在 §6。

**最重要的兩條結論**：

1. 這 7 個題型的公開 payload／公開 PNG 已經足以讓一支**不含 AI、不讀 verifier、
   不讀資料庫、不用私有 seed** 的確定性程式在毫秒級解題。`tile-rotation-v1` 甚至
   不需要讀圖片像素——payload 裡的 `initial_masks` 就是整個棋盤狀態。
2. `symbol-path-v1` 有一個**與安全無關的渲染缺陷**：`target_len=8` 時目標條總寬
   8×40+7×4=348 px，超過圖片寬 320 px，兩端樣本卡被圖片邊界裁掉；payload 的
   `target_h=48` 也與實際繪製的 40 px 卡片不一致。這應在 redesign 前先修正。

---

## 1. 基線、方法與邊界

### 1.1 攻擊面：只使用普通瀏覽器能取得的內容

每個 challenge 的 solver 輸入只有一份 JSON，即 `/api/challenge` 回給瀏覽器的
`question` 物件：

- `SanitizePublic` 白名單欄位（幾何、`cell_ids`、公開提示等）；
- `generator` / `generator_version`；
- `media` / `image_mime` / `image_base64`（PNG 以 base64 內聯，與 `questionDTO` 一致）。

solver **沒有**拿到、也沒有任何程式路徑可以拿到：

- `pool.verifier_blob` / `PrivateVerifier` / 任何 HMAC 密鑰；
- 資料庫裡的答案、pool 行、challenge 行；
- 生成器的熵流或私有 seed；
- 管理面 `/api/admin/*`；
- 在線 API、在線 AI 模型、OCR 服務。

**驗證分數的邊界**：harness 在 solver 回傳**之後**才用生產的 `g.Verify(q.Verifier, answer)`
判分。這是伺服器本來就會做的判定，不是 solver 讀 verifier；`solver` 套件不 import
`internal/generator`（有測試 `TestSolverDoesNotImportGenerator` 釘住），公開 DTO 也沒有
`verifier` / `private` / `answer` / `seed` 欄位（有測試 `TestPublicDTOOnlyContainsPublicFields`）。

Mac key 由 `crypto/rand` 在記憶體中生成，challenge 不落庫、不寫資產檔；跑完即消失。

### 1.2 Challenge 生成

- 每型 **1,000 個獨立 challenge**，共 7,000 個。
- 熵源：`crypto/rand.Reader`，每個 challenge 重新抽取；沒有固定 seed、沒有重播。
- 參數：`difficulty=3`、`lang=zh-CN`（多數題型忽略 lang；保持與生產 `Params` 一致）。
- 7,000 次生成全部第一次就成功（`generation_attempts=1`），沒有 generation failure。
- **調參與正式測量的界線**：solver 的閾值先在 40/60/100/150/200/300 題的 dev run
  上調整，凍結後才跑正式 1,000。正式 1,000 的 challenge 是新的隨機樣本，但這**不是**
  預先註冊的 holdout；渲染是確定性的，閾值看過早期樣本。這一點如實記錄，不宣稱
  「完全獨立的 holdout」。

### 1.3 量測口徑

| 指標 | 口徑 |
|---|---|
| 成功率 | `solved / total`；`solved` 只能由生產 `Verify` 回 true |
| 95% CI | Wilson score interval（`internal/m4baseline/stats.go`） |
| CPU 時間 | process user CPU delta（`runtime/metrics` `/cpu/classes/user:cpu-seconds`）；Windows 計數器粒度約 15.6 ms，單題小值會被量化成 0 |
| wall 時間 | 單執行緒 solver 的 per-challenge wall clock；這是實際每題成本，也是 CPU 時間的保守上界（solver 沒有平行化） |
| p95 判定依據 | summary 的 `p95_timing_basis` 以 CPU **總量**是否有訊號標記；本輪 `symbol-path`/`crowd-find` 的 per-challenge CPU p95 被量化成 0，但仍標 `cpu`。因此表中同時列 wall；判定在 CPU 0 與 wall <1s 兩種讀法下都成立 |
| RSS | 每題 solver 回傳後的 process working set（Windows PSAPI `WorkingSetSize`）；是整個 process（Go runtime + generator + harness + solver），**不是** solver 增量 |
| 平均 CPU／成功 | 所有 per-challenge CPU delta 之和 ÷ solved；因量化，另列 wall 口徑 |
| 資產大小 | 每個 challenge 的 PNG 位元組數（median / p95 / min / max） |
| 解析失敗率 | 公開 DTO 或 base64/PNG 解不開的比率（不是「答錯」） |
| 放棄率 | 解析成功但 solver 沒有給出唯一答案的比率；**不能**把放棄寫成「題型抗機器」 |
| 答錯率 | 有給出格式合法的答案、但 `Verify` 為 false 的比率 |
| 隨機猜測 | 依公開答案空間取**對攻擊者有利**的上界；見附表 A |

### 1.4 參考主機完整規格

| 項 | 值 |
|---|---|
| CPU | Intel(R) Core(TM) i7-14650HX |
| 核心／執行緒 | 16 cores / 24 logical processors |
| 標稱時脈 | 2200 MHz（WMI MaxClockSpeed） |
| RAM | 34,053,414,912 B ≈ 31.71 GiB |
| OS | Microsoft Windows 11 专业版，版本 10.0.26200，Build 26200 |
| Go | go1.27.3，windows/amd64，`GOMAXPROCS=24` |
| 量測時間 | 2026-11-12T14:07:08Z – 14:09:49Z（161.2 s） |

**這不是目標機（1 vCPU / 1GB）的數字**。CPU/RSS 只對這台參考主機成立；換機器必須重跑，
不得把這裡的毫秒數或 RSS 當成 VPS 的穩態承諾。

---

## 2. 逐題 baseline 與結果

### 2.1 `tile-rotation-v1` — FAIL

**攻擊方法（只用公開 payload）**：payload 直接給了 9 格的 `initial_masks`（4-bit
N/E/S/W 邊掩碼）。攻擊器把每格的 4 個不同旋轉做列優先回溯：邊界不得出邊、相鄰邊必須
成對、最後 9 格必須連通。生成器保證唯一解，搜尋找到唯一合法棋盤即輸出 `v1:<9 hex>`。
**答案完全由 `initial_masks` 決定，不需要讀圖片像素。**

**結果**：1000/1000，答錯 0，放棄 0，解析失敗 0；median CPU 0 ms、p95 CPU 4.64 ms，
median wall 0 ms、p95 wall 1.00 ms；平均 CPU／成功 0.507 ms（wall 0.236 ms）；
RSS median 15.6 MiB / p95 16.3 MiB；資產 median 1,078 B / p95 1,142 B（min 976 / max 1,190）；
隨機猜測上界 1/64（生成器要求 candidate space ≥64；實際通常遠大於 64）。

**為什麼成功**：`initial_masks` 就是整個棋盤狀態。題目的「推理」是 9 格旋轉的約束
滿足，成本和一般數獨求解器同級；圖片只是把同一個狀態畫出來。這不是圖像辨識的勝利，
而是**公開結構化欄位直接泄漏了題目狀態**。

**最小 redesign**：移除公開 DTO 的 `initial_masks`，讓客戶端只能從 PNG 取得初始棋盤
（widget 需要改成以圖片為底板、在上面疊加互動）；同時加入誘餌邊、遮擋與非對稱噪聲，
使「從圖片讀出 4-bit mask」不再是一眼可做的事。若 image-only 版本仍被同一 baseline
解到 ≥20%，此題型應考慮 REMOVE，而不是繼續加動畫延遲。

### 2.2 `symbol-path-v1` — FAIL

**攻擊方法（只用公開 payload + 公開 PNG）**：目標條與 6×6 網格使用同一套 8 個字形；
**同一字形在網格裡的繪製是逐像素相同的**。攻擊器先把 36 個格子按 24×24 筆跡密度
聚類（同一字形距離 ≈ 0），得到 6–8 個字形類；再把目標條上的樣本卡按密度匹配到
這些類。`target_len=8` 時兩端樣本卡被圖片裁切，攻擊器利用「目標序列是 8 個字形的
一個排列」把未出現的類補到被裁切的位置，最後在網格上做類序列 DFS。

**結果**：991/1000（99.1%），答錯 0，放棄 9（0.9%，`匹配路径不唯一`：候選序列找到多於一條
匹配路徑時 solver 不猜），
解析失敗 0；median CPU 0 ms、p95 CPU 0 ms（量化），median wall 2.07 ms、p95 wall 4.15 ms；
平均 CPU／成功 0.211 ms（wall 2.283 ms）；RSS median 16.2 MiB / p95 16.5 MiB；
資產 median 4,597 B / p95 4,890 B（min 4,003 / max 5,126）；隨機猜測 1/4,008
（6×6 上長度 6 的自避路徑最小數量；長度 7/8 的分母更大）。

**與安全無關的缺陷（必須修）**：

- 目標條在 `target_len=8` 時總寬 348 px > 圖片寬 320 px，第一張與最後一張樣本卡
  被圖片邊界裁掉（`docs/reviews/evidence` 的 challenge 影像可重現）。這讓題目對
  **人類**也可能不可讀；不是安全特性。
- payload 的 `target_h=48` 與實際繪製的卡片高度 40 px 不一致（渲染器用
  `targetCell=40`）。widget 的目標條幾何會與圖片有 8 px 的偏差。

**最小 redesign**：先修目標條幾何（縮小卡片/間距或加寬圖片，讓 L=8 完整可見）；
再讓目標樣本與網格字形在**視覺風格**上不同（不同字重/尺度/遮擋/局部噪聲），
使「同一字形模板」不再可直接重用；加入誘餌路徑。若只修裁切、不改模板重用，
此題仍會被同一攻擊器解到 ~100%。

### 2.3 `crowd-find-v1` — FAIL

**攻擊方法（只用公開 payload + 公開 PNG）**：頂部樣本與 36 個格子是同一套程序化
角色，只是樣本以 58 px、格子以 42 px 繪製；六個特徵（頭輪廓、眼睛、天線、嘴、
身體紋理、配件）都由固定像素偏移的離散圖元組成。攻擊器用公開卡片幾何算出
`cx/cy/hy/hr/by`，在固定偏移窗口對每個特徵的候選模板做二值差，得到 7 元組
（head, eyes, mouth, antenna, hat, texture, accessory）；再以模板得分作決定性
tie-break。

**結果**：845/1000（84.5%），答錯 155（15.5%），放棄 0，解析失敗 0；
median CPU 0 ms、p95 CPU 0 ms（量化），median wall 3.39 ms、p95 wall 6.24 ms；
平均 CPU／成功 0.744 ms（wall 4.452 ms）；RSS median 16.5 MiB / p95 17.0 MiB；
資產 median 4,829 B / p95 5,300 B（min 3,845 / max 5,738）；隨機猜測 1/36。

**為什麼成功**：同一個 sprite 在兩個尺度上重用，特徵是高對比、無遮擋、無姿態變化；
手工模板分類器就足以恢復。**這不是「AI 太強」**，而是題目的視覺空間太小且太乾淨。

**設計歧義（redesign 時要處理）**：帽子（`sig5=1`）會蓋住天線；此時兩個角色可能
只在被遮住的天線型別上不同，公開畫面無法區分。solver 的 tie-break 在這些情況會
答錯；人類也可能無法分辨。這是題目設計的**可見性/歧義**問題，不是安全強度。
84.5% 是這支 baseline 的實測精確答案率；真實自動化風險至少是這個量級。

**最小 redesign**：每個 instance 加入姿態/旋轉/遮擋/光照變化，不要讓樣本與格子
共用同一張可逐像素比對的 sprite；讓干擾項的差異落在語意特徵而不是固定像素位置。
若 redesign 後仍 ≥20%，考慮 REMOVE。

### 2.4 `visual-rank-v1` — FAIL

**攻擊方法（只用公開 payload + 公開 PNG）**：payload 給了 5 張卡的包圍盒；卡片內
的折線畫在可見的 5×4 節點柵格上（節點間距 30 px，等於 card_h/4）。攻擊器在每條
柵格邊的中段取暗像素（避開節點圓點與起終點標記），得到一張二值邊圖；若邊圖不是
一條簡單路徑就放棄；否則線段數 = 邊數，按邊數排序輸出 `v1:A.B.C.D.E`。

**結果**：1000/1000，答錯 0，放棄 0；median CPU 0 ms、p95 CPU 13.66 ms，
median wall 0.53 ms、p95 wall 2.58 ms；平均 CPU／成功 1.989 ms（wall 0.849 ms）；
RSS median 17.2 MiB / p95 18.3 MiB；資產 median 1,707 B / p95 1,822 B
（min 1,474 / max 1,875）；隨機猜測 1/5! = 1/120。

**為什麼成功**：所有線段都是軸對齊、5 px 粗、無遮擋，節點間距可見且固定；線段數
就是邊圖的邊數。這是一個「數線段」的幾何題，不是需要視覺泛化的題。

**最小 redesign**：不要畫可見的節點柵格/節點圓點；改用曲線、不等長、可變粗細、
部分遮擋與交叉的筆畫，讓線段數不能由單一柵格邊取樣直接得到。若 redesign 後仍
≥20%，考慮 REMOVE。

### 2.5 `progressive-target-v1` — FAIL

**攻擊方法（只用公開 payload + 公開 PNG）**：三輪各有一張 3×3 board 與頂部樣本，
樣本與格子共用同一套 8 個字形（樣本 42 px、格子 34 px）。攻擊器逐輪把樣本與 9 格
做 24×24 筆跡密度模板匹配，取最近格，輸出 `v1:a.b.c`。

**結果**：1000/1000，答錯 0，放棄 0；median CPU 0 ms、p95 CPU 22.76 ms，
median wall 1.89 ms、p95 wall 6.26 ms；平均 CPU／成功 4.809 ms（wall 2.508 ms）；
RSS median 17.2 MiB / p95 18.8 MiB；資產 median 4,387 B / p95 4,715 B
（min 3,617 / max 5,009）；隨機猜測 1/9³ = 1/729。

**為什麼成功**：三輪只是把同一個模板匹配重複三次；每輪的樣本與格子字形完全同源，
沒有姿態/遮擋/噪聲變化。

**最小 redesign**：讓樣本與格子使用不同的視覺渲染（或引入旋轉/遮擋/局部噪聲），
使模板不能直接重用；三輪之間加入不同的視覺干擾。若 redesign 後仍 ≥20%，考慮 REMOVE。

### 2.6 `shell-track-v1` — FAIL

**攻擊方法（只用公開 payload + 公開 PNG）**：payload 給 33 幀雪碧圖、4 個容器點擊區、
`frames_per_round=11`，**不給**交換序列。攻擊器：

1. 每輪第 0 幀在容器上方找十字標記的 x 質心，得到目標初始槽位；
2. 每次交換的「運動幀」與上一張「靜止幀」做逐列差分；兩個源槽位的杯子被搬走，
   差分列的左右邊界是這兩個槽位的外沿；
3. 由差分 bbox 的中心與寬度反推交換對（六個容器對在 center/width 上可區分）；
4. 只更新「目標若在 a 則移到 b、在 b 則移到 a」，不需要識別哪個杯子裝了什麼。

**結果**：1000/1000，答錯 0，放棄 0；median CPU 0 ms、p95 CPU 57.21 ms，
median wall 6.09 ms、p95 wall 16.74 ms；平均 CPU／成功 15.035 ms（wall 8.476 ms）；
RSS median 19.4 MiB / p95 20.9 MiB；資產 median 11,982 B / p95 12,225 B
（min 11,450 / max 12,363）；隨機猜測 1/4³ = 1/64。

**為什麼成功**：容器外觀完全相同、背景乾淨、交換是確定性的剛體平移；差分影像
直接編碼了「哪兩個槽位在交換」。payload 裡沒有私有交換序列，但**公開幀本身**
就足以恢復它。

**最小 redesign**：加入遮擋、誘餌運動、非剛體變形、變速/不規則時序，或讓容器在
交換過程中部分離開畫面；目標是讓「幀差分 → 交換對」不再是一條直接管線。若無法
在可讀性與機器成本之間取得平衡，考慮 REMOVE。

### 2.7 `silhouette-assembly-v1` — FAIL

**攻擊方法（只用公開 payload + 公開 PNG）**：5 個部件與 5 個槽位都是 3×3 柵格上的
程序化 polyomino；槽位顯示的是同一形狀的隨機旋轉輪廓。攻擊器：

1. 從部件盒取 3×3 佔用格（`cell=18`，中心採樣）；
2. 從槽位盒取 3×3 佔用格（`cell=20`、`inset=2`，中心採樣）；
3. 枚舉 piece×slot×0/90/180/270，求唯一完整 assignment；
4. 輸出 `v1:<slot>:<pieceID>:<rot>,...`。

**結果**：1000/1000，答錯 0，放棄 0；median CPU 0 ms、p95 CPU 6.93 ms，
median wall 0.52 ms、p95 wall 1.43 ms；平均 CPU／成功 1.212 ms（wall 0.565 ms）；
RSS median 18.3 MiB / p95 19.7 MiB；資產 median 1,707 B / p95 1,767 B
（min 1,549 / max 1,821）；隨機猜測 1/5! = 1/120（旋轉可由可見槽位輪廓確定，
對攻擊者最有利的口徑是只猜 piece 排列）。

**為什麼成功**：形狀目錄只有 9 個 3×3 polyomino，且繪製為乾淨的實心/斜紋方格；
3×3 佔用格可以從像素直接讀出，旋轉枚舉只有 4 個桶。

**最小 redesign**：改用曲線/不規則輪廓、部分遮擋、非柵格放置與重疊部件；讓
「提取 3×3 佔用格」不再是一眼可做的事。若 redesign 後仍 ≥20%，考慮 REMOVE。

---

## 3. 跨題結論

1. **7/7 FAIL，整體 6,836/7,000 = 97.66%**（95% CI [97.28%, 97.99%]）。這不是
   「某一個題型剛好過線」；除 `crowd-find-v1` 外全部 ≥99%，`crowd-find-v1` 的
   CI 下界也高於 80%。
2. 共同的失效形狀：
   - 公開 payload 直接含結構化題目狀態（`tile-rotation` 的 `initial_masks` 最明顯）；
   - 同一套模板/字形在樣本與答案區域重用（`symbol-path`、`progressive-target`、
     `crowd-find`）；
   - 乾淨、高對比、無遮擋、軸對齊的幾何（`visual-rank`、`silhouette`）；
   - 動畫的每一幀都是公開的，且交換對可被差分恢復（`shell-track`）。
3. **這不是抗 AI/抗 OCR 測量**。本輪沒有使用在線 AI、在線模型、OCR 服務、管理面、
   verifier、資料庫或私有 seed。它測的是「一支普通的確定性程式，只讀公開題面，
   能多快、多準地解題」。因此：
   - 不能把任何 FAIL 讀成「AI 可以」或「AI 不可以」；
   - 不能把任何 `crowd-find` 的 15.5% 答錯讀成「抗機器」；
   - 不能把本報告的數字外推到其他 12 個未測題型或未來的 redesign。
4. **歷史 OCR 數字（低/中/高檔 96%/31%/3%）不屬於本輪**；它們是單一工具、固定配置、
   固定樣本的嚴格整串通過率，不能拿來和本輪的 1,000 題/型、公開 DTO baseline 相加或
   互相替代。
5. **`pipe-connect-v1` 不在 Gate B 範圍**。它的 legacy verifier 限制（無法表達逐格
   edge mask）仍是 DESIGN/ADR 的 BLOCKED 項；本輪沒有改它，也不對它下判定。
6. **`crowd-find-v1` 的設計歧義**：帽子遮住天線時，兩個角色可能只在不可見特徵上
   不同。這是題目可見性問題，應在 redesign 中一併處理（要麼讓被遮住的特徵不參與
   簽名唯一性，要麼讓它可見）。不能把這種歧義當成安全強度。

---

## 4. 處置與 redesign 建議

### 4.1 本輪不變更任何題型

依任務要求，**沒有**修改任何題型、`production_ready`、權重、難度或預設站點設定：

- `experimental:true` 維持；`production_ready:false` 維持；
- high/critical 權重維持 0；預設站點權重維持 0；
- 沒有加入動畫延遲、沒有用「延長等待」假裝增加安全性；
- `internal/generator`、`internal/admin`、`internal/dispatch`、`cmd/server`、`web/`
  沒有修改（`git status` 只顯示新增的 M4 工具/evidence/報告與 Makefile 的 dev target）。

### 4.2 逐題處置

| 題型 | Gate B | 狀態 | 建議 | 最小 redesign（先做這個，再重跑同一 baseline） |
|---|---|---|---|---|
| `tile-rotation-v1` | FAIL | KEEP_EXPERIMENTAL | REDESIGN | 移除公開 `initial_masks`；以 PNG 為棋盤底板；加誘餌邊/遮擋/非對稱噪聲。若 image-only 仍 ≥20%，考慮 REMOVE。 |
| `symbol-path-v1` | FAIL | KEEP_EXPERIMENTAL | REDESIGN | 先修 `target_len=8` 目標條裁切與 `target_h` 幾何；再讓樣本/網格字形視覺不同源（風格/尺度/遮擋/噪聲）；加誘餌路徑。 |
| `crowd-find-v1` | FAIL | KEEP_EXPERIMENTAL | REDESIGN | 每個 instance 加姿態/旋轉/遮擋/光照變化；不要重用同一 sprite；處理帽子遮住天線的可見性歧義。若仍 ≥20%，考慮 REMOVE。 |
| `visual-rank-v1` | FAIL | KEEP_EXPERIMENTAL | REDESIGN | 移除可見節點柵格/圓點；改用曲線、不等長、可變粗細、遮擋/交叉筆畫。若仍 ≥20%，考慮 REMOVE。 |
| `progressive-target-v1` | FAIL | KEEP_EXPERIMENTAL | REDESIGN | 樣本與格子使用不同渲染；三輪加入不同噪聲/遮擋；不要讓同一模板直接重用。若仍 ≥20%，考慮 REMOVE。 |
| `shell-track-v1` | FAIL | KEEP_EXPERIMENTAL | REDESIGN | 加遮擋、誘餌運動、非剛體變形、變速/不規則時序；讓「幀差分 → 交換對」不再直接。若無法平衡可讀性與機器成本，考慮 REMOVE。 |
| `silhouette-assembly-v1` | FAIL | KEEP_EXPERIMENTAL | REDESIGN | 曲線/不規則輪廓、部分遮擋、非柵格放置與重疊部件。若仍 ≥20%，考慮 REMOVE。 |

**REMOVE 的判準（下一輪）**：對 redesign 後的版本重跑同一組 7 個 baseline（fresh
1,000/型、相同門檻）：

- 成功率 ≥80% 且 p95 ≤1 CPU 秒 ⇒ 直接 FAIL，優先 REMOVE；
- 成功率 20%–80%，或顯著高於隨機猜測 ⇒ FAIL/REDESIGN，繼續改；
- 成功率 <20% 且不顯著高於隨機猜測 ⇒ 只能寫「通過本輪已定義 baseline」，
  **不是**抗 AI/抗 OCR 認證。

本輪不建議直接 REMOVE 任何題型：7 個都還有可操作的視覺硬化空間；但 `tile-rotation`
與 `shell-track` 的「結構化狀態/公開幀直接可解」是概念級風險，若 redesign 後仍
≥20%，應優先考慮移除。

---

## 5. 證據與重現

### 5.1 產物

| 檔案 | 大小 | 內容 |
|---|---:|---|
| `docs/reviews/evidence/m4-summary.json` | 9,483 B | 7 題型的聚合指標、主機規格、判定、criteria、隨機猜測口徑 |
| `docs/reviews/evidence/m4-results.csv` | 500,699 B | 7,000 列 per-challenge 記錄；欄位見下 |

**SHA-256**（2026-11-12 正式跑完後）：

```
d9aaf3c390714e898fdadd592fcde45187f2c7b9b89010bdb20ce2d2f072748f  docs/reviews/evidence/m4-results.csv
1f48ce71ca55fd6de5244bee00c8a73dc638e4c72f1c9cc27306f4cec951d185  docs/reviews/evidence/m4-summary.json
```

**CSV 欄位**：`generator,index,solved,parse_failed,gave_up,solver_cpu_ms,solver_wall_ms,
rss_bytes,asset_bytes,payload_bytes,generation_attempts,note`。**沒有**答案、verifier、
seed、payload 原文或圖片；`note` 只含失敗族與特徵向量/距離這類診斷。因此它可以依
現有倉庫規則提交；它不是 `eval-out/` 那類含 ground truth 的目錄。若未來倉庫決定
不提交聚合 evidence，將整包移到倉庫外並保留上述 SHA-256 即可。

### 5.2 重現指令

```bash
# 1) 建置 dev-only 攻擊器（不進 cmd/server 依賴圖）
go build -trimpath -o bin/epocha-m4baseline ./cmd/m4baseline

# 2) 正式基線：7 題型 × 1,000 個獨立 challenge
bin/epocha-m4baseline --challenges 1000 --out-dir docs/reviews/evidence
```

Makefile 也提供 `make m4-baseline`（`M4_CHALLENGES` 可覆蓋 1000；預設輸出
`tmp/m4-baseline-<UTC>`，不會覆蓋本報告的 `docs/reviews/evidence`；要重建正式
evidence 用 `M4_OUT=docs/reviews/evidence`）。注意本倉庫
Makefile 固定 `GOTOOLCHAIN=local`；本參考主機的系統 `go` 是 1.24.2，而模組要求
1.27.3，因此本次正式跑是用 Go 1.27.3（`GOTOOLCHAIN=auto` 從模組快取取得）直接
`go build`，不是用 `make`。在已安裝 Go 1.27.3 的 Linux/ARM64 目標機上，
`make m4-baseline` 是預期入口。

### 5.3 工程檢查（與題型判定分開）

| 檢查 | 結果 | 說明 |
|---|---|---|
| `gofmt -l internal/m4baseline cmd/m4baseline` | PASS | 無輸出 |
| `go vet ./internal/m4baseline/... ./cmd/m4baseline` | PASS | 無輸出 |
| `go test -count=1 ./internal/m4baseline/... ./cmd/m4baseline` | PASS | 含 solver 合成案例、統計、公開 DTO 無私有欄位、solver 不 import generator、server 不 link m4baseline |
| `go test -race -count=1 ./...` | **BLOCKED** | 本 Windows 主機 `CGO_ENABLED=0` 且無 C 編譯器；`-race requires cgo`。沒有把 race 記成通過 |
| `go test -count=1 ./...`（全倉庫） | **BLOCKED/FAIL（既有平台限制）** | `internal/store/lock.go` 使用 `syscall.Flock`，Windows 上不存在；`cmd/server`、`cmd/{devseed,ops}`、`internal/{store,api,admin,config,pool}` 因此在 Windows build failed。這是既有的 Linux-only 依賴，不是本輪 M4 改動造成；`internal/generator` 與新增套件在 Windows 上 PASS。輸出另外包含被 `.gitignore` 的 `tmp/`、`release-artifacts/` 本地產物，與本輪無關 |
| `GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -o tmp/epocha-linux-arm64 ./cmd/server` | PASS | 產生 18,048,708 B 的靜態 linux/arm64 ELF（未 strip；production 用 `-s -w`） |
| `GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -o tmp/m4baseline-linux-arm64 ./cmd/m4baseline` | PASS | 5,625,524 B 的靜態 linux/arm64 dev tool；Linux 的 `host_linux.go`/`rss_linux.go` 可編譯 |
| `go list -deps ./cmd/server` 含 `internal/m4baseline` | **0** | 攻擊器不在 runtime binary 依賴圖；`internal/generator` 仍在（預期） |

---

## 6. 未執行與阻塞（不得讀成通過）

| 項 | 狀態 | 原因 |
|---|---|---|
| 在線 AI / 在線模型 / 外部 vision API | NOT_RUN | 任務禁止；本報告不聲稱抗 AI |
| 在線 OCR / 打碼平台 | NOT_RUN | 同上；歷史 tesseract 數字不屬於本輪 |
| 人工可讀性（每型人工各過 5 次） | NOT_RUN | 沒有真人受試者 |
| 輔助技術（螢幕閱讀器/開關設備） | NOT_RUN | 沒有對應設備與流程 |
| 真機移動端 | NOT_RUN | 本輪沒有真實裝置 |
| GIF disposal / Chrome/Firefox/WebKit 動畫一致性 | NOT_RUN | 沒有做瀏覽器動畫 E2E |
| 生產流量 / VPS 1 vCPU 穩態 | NOT_RUN | 本輪是開發機上的離線評測 |
| 其餘 12 個題型的 M4 | NOT_RUN | Gate B 只覆蓋 Wave 1/2 的 7 個題型 |
| `-race` | **BLOCKED** | 本機缺 cgo/C 編譯器 |
| Windows 全量 build/test | **BLOCKED** | 既有 `syscall.Flock` 的 Linux-only 依賴 |
| 嚴格 holdout 獨立性 | **BLOCKED** | 正式 1,000 是新的隨機樣本，但 solver 閾值看過早期 dev 樣本；不是預先註冊的 holdout |

**沒有**任何「PASS：某項未測」的混合表達。上表的 NOT_RUN/BLOCKED 不會被計入
7 個題型的 Gate B 判定；7 個題型的判定只由 §2 的實測數字決定。

---

## 附表 A：隨機猜測口徑

| 題型 | 公開答案空間（對攻擊者有利的上界） | 單次 p0 | 三次至少中一次 |
|---|---|---:|---:|
| `tile-rotation-v1` | 1/64（generator 要求 candidate space ≥64；實際通常更大） | 1.5625% | 4.61% |
| `symbol-path-v1` | 1/4,008（6×6 上長度 6 的自避路徑；L=7/8 分母更大） | 0.02495% | 0.0748% |
| `crowd-find-v1` | 1/36 | 2.778% | 8.10% |
| `visual-rank-v1` | 1/120（5!） | 0.833% | 2.48% |
| `progressive-target-v1` | 1/729（9³） | 0.137% | 0.411% |
| `shell-track-v1` | 1/64（4³） | 1.5625% | 4.61% |
| `silhouette-assembly-v1` | 1/120（5! piece 排列；旋轉由可見槽位輪廓決定） | 0.833% | 2.48% |

**「顯著高於隨機猜測」的定義**：Wilson 95% 下界 > 上表的 p0，且成功數 ≥10。
7 個題型全部滿足；但本輪的判定主要由「≥80% 且 p95 ≤1s」這條更強的 FAIL 線決定。

## 附表 B：逐題完整數字

| 題型 | solved | wrong | give-up | parse fail | mean CPU/success | mean wall/success | RSS med / p95 (MiB) | asset med / p95 (B) | asset min / max (B) | payload (B) |
|---|---:|---:|---:|---:|---:|---:|---|---|---:|---:|
| `tile-rotation-v1` | 1000 | 0 | 0 | 0 | 0.507 ms | 0.236 ms | 15.6 / 16.3 | 1,078 / 1,142 | 976 / 1,190 | 474 |
| `symbol-path-v1` | 991 | 0 | 9 | 0 | 0.211 ms | 2.283 ms | 16.2 / 16.5 | 4,597 / 4,890 | 4,003 / 5,126 | 730 |
| `crowd-find-v1` | 845 | 155 | 0 | 0 | 0.744 ms | 4.452 ms | 16.5 / 17.0 | 4,829 / 5,300 | 3,845 / 5,738 | 678 |
| `visual-rank-v1` | 1000 | 0 | 0 | 0 | 1.989 ms | 0.849 ms | 17.2 / 18.3 | 1,707 / 1,822 | 1,474 / 1,875 | 516 |
| `progressive-target-v1` | 1000 | 0 | 0 | 0 | 4.809 ms | 2.508 ms | 17.2 / 18.8 | 4,387 / 4,715 | 3,617 / 5,009 | 525 |
| `shell-track-v1` | 1000 | 0 | 0 | 0 | 15.035 ms | 8.476 ms | 19.4 / 20.9 | 11,982 / 12,225 | 11,450 / 12,363 | 654 |
| `silhouette-assembly-v1` | 1000 | 0 | 0 | 0 | 1.212 ms | 0.565 ms | 18.3 / 19.7 | 1,707 / 1,767 | 1,549 / 1,821 | 753 |

RSS 是整個 process 的 working set，包含 Go runtime、generator 與 harness；不是
solver 增量，也不是目標機（1GB）的承諾。payload 位元組數在每個題型內是常數
（題面內容在 PNG 裡），因此只列一個值。

---

**最終一句話**：本輪的 7 個題型全部在「公開 DTO + 公開 PNG + 毫秒級確定性程式」
下被解到 84.5%–100%；它們必須維持 experimental / production_ready:false，並在
redesign 後用同一 baseline 重跑。這份報告**不產生**任何 production promotion，
也不把 solver 的失敗或成功寫成抗 AI 證明。
