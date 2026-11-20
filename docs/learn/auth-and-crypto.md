# 學習筆記：這套 fleet 的加密與身分驗證是怎麼運作的

> 撰寫：2026-11-20，Longarm v0.1.42。範例用一個假想的 fleet：Hub `hub.example.com`、worker `worker-1`、admin client `laptop`。
> 標「依文件」的內容出自 Longarm 官方文件或原始碼；標「推論」的是根據行為推出來的。

## 一句話版本

**Hub 是一個私人的憑證機構（CA）**。每台機器加入時，自己在本機產生私鑰，再用一次性的
join code 換到一張由 Hub 簽發的憑證。之後機器之間一律用 **mTLS**（雙方都出示憑證）互相確認
身分並加密。**人**登入 Web Panel 則是另一套：密碼加 TOTP（兩步驟驗證）。而
**lazylongarm 管理這些機器靠的是 SSH**，又是第三套信任。

```
            你（人）
   ┌──────────┼─────────────────────────┐
   │ SSH key  │ 密碼 + TOTP              │ 你的 admin 憑證（mTLS）
   ▼          ▼                         ▼
 各台機器   Web Panel（只在 127.0.0.1）   longarm CLI ──mTLS──► Hub ◄──mTLS── 各 worker
 (setup/    經 SSH tunnel 存取                    └──── mTLS（可直連，或經 Hub relay）────► worker
  修復用)
```

## 1. 基本名詞（先懂這幾個就夠）

| 名詞 | 白話 | 在這套系統裡 |
|---|---|---|
| 公鑰／私鑰 | 一對鑰匙：私鑰自己藏好，公鑰可以公開；私鑰簽的東西，任何人都能用公鑰驗證 | 每台機器、Hub 的 CA、你的 SSH 都各有一對 |
| 憑證（certificate） | 「某人擔保：這把公鑰屬於某某」的簽名文件，有效期限、用途（角色）都寫在裡面 | Hub 簽給每台機器的「通行證」 |
| CA（憑證機構） | 負責簽發憑證的那個人；大家信任它，就等於信任它簽出來的憑證 | Hub 自己就是 CA（`/var/lib/longarm-hub/ca/`） |
| fingerprint（指紋） | 一把鑰匙或一張憑證的 SHA-256 摘要，短到可以人工比對 | Hub CA 的指紋，例如 `3f9a…c2e1` |
| TLS | HTTPS 用的加密協定：先驗證對方身分，再建立加密通道 | 所有 Longarm 連線的基礎 |
| mTLS（mutual TLS） | 一般 HTTPS 只有伺服器出示憑證；mTLS 是**雙方都出示**，互相驗證 | Hub、client、worker 之間全部用 mTLS |
| TOFU | Trust On First Use：第一次連線時「先信任再說」，之後記住 | SSH 的 host key、`--ca-fingerprint` 就是為了把 TOFU 變成「先比對再信任」 |

## 2. 一台機器怎麼加入 fleet（enrollment）

以 `worker-1` 為例，對照 `lazylongarm worker add` 的輸出：

1. **Admin 產生一次性 join code**（`longarm admin join-code --role worker --name worker-1 --ttl 15m`）。
   - 依文件：code 是 6 個字元、單次使用、有期限（這裡設 15 分鐘）。Hub 的資料庫裡只存它的雜湊值。
   - code 只是「入場券」，本身不是憑證，也不能拿來執行任何指令。
2. **worker 在本機產生私鑰**（依文件：「The key is generated here; the hub signs a certificate」）。
   私鑰**從頭到尾不會離開 worker**。
3. **worker 先驗證 Hub 是不是真的 Hub**：用 `--ca-fingerprint 3f9a…` 比對 Hub 出示的
   CA 憑證指紋，對不上就拒絕。輸出裡的 `hub CA verified (3f9a…)` 就是這一步。
   這可以防止有人冒充 Hub 騙走 join code（中間人攻擊）。
4. **用 join code 換憑證**：worker 送出公鑰加 code，Hub 驗證 code 後，用 CA 私鑰簽一張憑證給它，
   上面寫著名字（worker-1）、角色（worker）、有效期限。
5. **等待核准**：`auto_approve = false`，所以新 worker 先是 `pending`；lazylongarm 以 admin 身分立刻
   `longarm admin approve worker-1`。這一步是第二道關卡：就算 code 外洩，陌生機器也要有人核准。
6. 之後 worker 用這張憑證和 Hub 建立 mTLS 連線，開始回報狀態、接 job。

憑證期限（`hub.toml` 的 `[certs]`）：worker 90 天、client／admin 30 天，到期前會自動續簽。
`whoami` 裡的 `renew_before: 864000s` 就是到期前 10 天開始續簽。

**lazylongarm 額外做的保護**：join code 經 **ssh stdin** 傳給 worker（`read -r code`），不會出現在
Review 畫面、log、設定檔，也不會留在遠端的 shell history 裡。

## 3. 連線時發生什麼事（mTLS）

```
client (laptop)                           worker (worker-1)
  出示：laptop 的憑證（admin, 由 Hub CA 簽） 出示：worker-1 的憑證（worker, 由 Hub CA 簽）
  檢查：對方憑證是 Hub CA 簽的嗎？名字對嗎？   檢查：對方憑證是 Hub CA 簽的嗎？角色有權限嗎？
            └──────────── 雙方都通過 → 建立加密通道 ────────────┘
```

- **路徑可以是直連**（例如 tailnet 上的 `tcp://100.x.y.z:7701`，或 QUIC 打洞成功的
  `longnet/direct`），**也可以經 Hub relay**（打不了洞時）。
- **經過 relay 時，Hub 讀不到內容**：client 和 worker 之間的 mTLS 是端到端的，relay 只是轉送加密後的 bytes。
  這就是「TLS 裡面再包一層 TLS」（TLS-in-TLS）：外層是到 relay 的 TLS，內層是端到端的 mTLS。
  它保護了隱私，但這種「TLS 裡面再包 TLS」的流量型態也比較容易被深度封包檢測辨識出來；
  所以不要把 Hub 和需要低調的服務放在同一個 IP 上。
- **Hub 掌握的是「誰可以加入」**，不是「每次連線的內容」。Hub 掛掉時，已經在跑的 job 不受影響
  （依文件：控制面和資料面分離）。

## 4. 人怎麼登入 Web Panel（密碼 + TOTP）

Panel 是給人用的網頁，用的不是機器憑證，而是帳號、密碼加 **TOTP**：

| 因素 | 你「知道」或「擁有」的東西 | 存在哪 |
|---|---|---|
| 密碼 | 你知道的 | Hub 資料庫只存雜湊（推論：一般做法，不存明文） |
| TOTP code | 你擁有的（手機上的 authenticator app） | 種子（secret）存在 Hub 和你的手機 |

**TOTP 的原理（RFC 6238）**：
1. 建立帳號時，Hub 隨機產生一個 secret（形如 `JBSWY3DPEHPK3PXP…` 的一串 Base32），用 QR code（`otpauth://` URI）交給你的 app。
2. 之後 Hub 和 app **各自**用「secret + 目前時間（每 30 秒一格）」算 HMAC，取 6 位數。
   兩邊算出一樣的數字，就代表你手上真的有那個 secret。
3. 所以 6 位數每 30 秒換一次，**secret 本身則固定不變**（直到你重設帳號）。
   - secret 外洩 = 別人也能算出你的 6 位數 → 要重設（`hub panel-user NAME --reset`）。
   - 某一次的 6 位數外洩影響很小，30 秒後就失效。
4. 手機和 Hub 的時間要大致準確，誤差太大就會一直驗證失敗。

**Panel 為什麼只綁 127.0.0.1**：網路上的人連不到它，你要先通過 SSH（你的 SSH key）建立 tunnel 才能存取。
所以 Panel 其實受「SSH key + 密碼 + TOTP」三層保護。

## 5. lazylongarm 自己用到的信任（SSH 與軟體來源）

| 環節 | 怎麼確保沒被冒充 |
|---|---|
| 連到 VM 的 SSH | 第一次連線要確認 host key。建議先用 `az vm run-command`，透過 Azure 自己的通道（不走 SSH）拿到 fingerprint，再和 ssh 顯示的 `SHA256:…` 比對：「先比對再信任」，不是盲目 TOFU |
| 登入 VM | SSH key（`~/.ssh/id_ed25519`），密碼登入是關的 |
| sudo | 背景先試 `sudo -n`；需要密碼時把終端交給你輸入，lazylongarm 不經手 |
| apt 套件 | 下載 Longarm 的 apt 簽章金鑰後，先比對 fingerprint `CA72074C…5307` 再信任；apt 再用它驗證 repository 的簽章 |
| worker 的 binary | 從 GitHub release 下載，並用同一個 release 的 `SHA256SUMS` 驗證（還沒做 minisign 簽章驗證，見 TODO） |
| 從 Hub 讀 CA fingerprint | 經過已驗證的 SSH 讀 `longarm-hub ca show`，所以這個 fingerprint 是可信的 |

這條**信任鏈**是：Azure 帳號 → VM 的 host key → SSH → Hub CA fingerprint → 每一張機器憑證。
任何一環沒驗證，後面的保證就會打折扣。

## 6. 哪些東西最需要保護（被偷了會怎樣）

| 秘密 | 放在哪 | 被偷了 | 對策 |
|---|---|---|---|
| **Hub CA 私鑰** | Hub VM `/var/lib/longarm-hub/ca/ca.key`（只有 longarm-hub 使用者可讀） | **最嚴重**：能簽出任何角色的憑證 → 在所有 worker 上以 longarmd 的身分執行指令 | Hub 放在只跑 Hub 的專用 VM、`hub backup` 加密後離線保存 |
| 你的 admin 憑證和私鑰 | Mac `~/.config/longarm/tls/` | 對方能以 admin 身分管理 fleet，直到憑證被撤銷或過期（30 天） | `longarm admin revoke`；日常可以另開非 admin 的 client |
| worker 的私鑰 | 各 worker 的 `~/.longarm/tls/` | 能冒充那台 worker（接到別人的 job） | 撤銷那台 worker、重新加入 |
| join code | 只在加入的那 15 分鐘內存在 | 有效期內可以拿去加入，但 `auto_approve = false` 還要 admin 核准 | 短期限、單次使用、經 stdin 傳 |
| TOTP secret | Hub 資料庫加你的手機 | 加上密碼就能登入 Panel | 不要貼到聊天或 log；外洩就重設 |
| Panel 密碼 | 你的腦袋或密碼管理器 | 加上 TOTP 和 SSH 才能登入 | 用密碼管理器 |
| SSH 私鑰 | Mac `~/.ssh/` | 能登入各機器（這是 lazylongarm 管理的通道） | 加 passphrase、用 agent |

**worker 上的 job 以誰的身分執行**：以 longarmd 的服務帳號執行（一般使用者帳號，
不是 root）。所以 fleet 被攻破的影響範圍，是那個帳號能碰到的一切：SSH key、
git 憑證、資料。這也是「體驗期只加不敏感機器」的原因。macOS worker 沒有 per-user isolation。

## 7. 洩露或遺失時怎麼重設（靠的是哪一台的權限）

有**兩條獨立的管理路徑**，彼此可以互相救援：

| 路徑 | 需要什麼 | 能做什麼 |
|---|---|---|
| **A. admin 憑證** | 你的 Mac 上的 `~/.config/longarm`（`mac (admin)`） | `longarm admin approve/revoke/join-code`、`longarm worker remove`：日常管理 |
| **B. Hub 主機的 SSH 加 sudo** | `~/.ssh/id_ed25519` 加 VM 的 sudo（Azure 帳號還能用 `az vm run-command` 當最後手段） | `longarm-hub …`：建立或重設 Panel 帳號、**產生新的 admin join code**、讀 CA、備份。這是緊急時的 break-glass 路徑 |

| 外洩或遺失的東西 | 怎麼處理 | 用哪條路徑 |
|---|---|---|
| TOTP secret 或 Panel 密碼 | `lazylongarm hub panel-user NAME --reset`（新密碼加新 secret，舊的立刻失效），也可以在 dashboard 按 `U` 選 reset | B |
| 某台 worker 的私鑰 | `longarm admin revoke <worker>`（依文件：worker 在一次 heartbeat 內就會拒絕舊憑證），再 `lazylongarm worker add <host> --reenroll` 重新加入 | A，重新加入時加上 SSH |
| 你的 Mac（admin 憑證）遺失或外洩 | 在 Hub 主機產生新的 admin code（`longarm-hub join-code --role admin`），用另一台機器加入成為新的 admin，再 `longarm admin revoke mac` | **B**：不需要舊的 admin 憑證，所以 Mac 丟了也救得回來 |
| join code | 15 分鐘後失效、只能用一次，而且還要 admin 核准；`longarm admin join-codes` 可以列出最近的 code（不顯示 code 本身） | — |
| **Hub CA 私鑰** | 最糟的情況：所有憑證都不能再信任，要換一把新 CA，所有機器重新加入（`hub setup` 加上每台 `worker add --reenroll`） | B 加上每台的 SSH |
| SSH 私鑰 | 不屬於 Longarm 的範圍：換一把 key，更新各機器的 `authorized_keys`；Azure VM 可以用 `az vm user update` 重設 | Azure 帳號 |

由此可見：**Hub 主機的 SSH／sudo 加上 Azure 帳號，才是真正的最高權限**；admin 憑證是日常方便用的。
所以要保護好 `~/.ssh` 和 Azure 帳號（開 MFA），並跑一次 `lazylongarm hub backup`，
把備份加密後離線保存，萬一 VM 壞掉才能還原 CA。

## 8. 常見問題

- **mTLS 已經加密了，為什麼還要 SSH？** 用途不同：mTLS 是 Longarm 機器之間的日常通道；
  SSH 是 lazylongarm 用來**安裝和修復**機器的管理通道（Longarm 本身沒有遠端安裝功能）。
- **Tailscale（WireGuard）和 mTLS 重複嗎？** 這次 worker 直連走的是 tailnet，所以有兩層加密
  （外層 WireGuard、內層 mTLS），多一層無妨；Longarm 不依賴 Tailscale，換成普通 TCP 也一樣安全。
- **Hub 看得到我的程式碼或 job 輸出嗎？** 經 relay 時看不到（端到端 mTLS）；但 Hub 知道有哪些機器、
  哪些 job（metadata），而 Hub 的管理者可以簽憑證，所以「信任 Hub 的主人」仍然是前提。
- **自簽 CA 安全嗎？** 安全與否不在於 CA 是不是公開機構，而在於大家有沒有正確比對 fingerprint、
  CA 私鑰有沒有保護好。公開 CA 適合「陌生人互相信任」；私人 fleet 用自己的 CA 反而更可控。

## 延伸閱讀

- TOTP：[RFC 6238](https://www.rfc-editor.org/rfc/rfc6238)；HOTP：[RFC 4226](https://www.rfc-editor.org/rfc/rfc4226)
- TLS 1.3：[RFC 8446](https://www.rfc-editor.org/rfc/rfc8446)（client 憑證就是 mTLS）
- Longarm：官方 `docs/design.md`、`docs/security.md`、`docs/usage.md` 的 enrollment／relay 章節
