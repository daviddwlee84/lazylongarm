# 上傳頻寬慢時 `worker add` 推送 binary 逾時（lazylongarm v0.2.0，2026-11-21）

## 症狀

`lazylongarm worker add worker-1` 的第 1 步「Install longarmd + longarm into ~/.local/bin」
連續失敗兩次：

```
SSH command on worker-1: context deadline exceeded
```

重跑時第一個 binary（`longarmd`）明明已經完整送到，還是又被複製一次。

## 原因

- controller 到 worker 的上傳只有約 70–150 KB/s，而每個 binary 約 40 MB；
  `connection.PushFile` 對整個複製設了固定的 `Timeout: 10 * time.Minute`，
  慢但持續有進度的連線也會被砍。
- 第 1 步的 Check 只回答「兩個都對」或「不對」；Apply 不分青紅皂白把兩個都重送，
  所以只完成一半的組合會重複複製已經相同的那個。
- 同一台 worker 自己從 GitHub 下載約 800 KB/s，瓶頸在 controller 的上傳，不在 worker。

## 處理（Unreleased）

- `--fetch auto`（預設）：worker 自己用 curl（沒有就 wget）下載 release 資產，
  用 controller 從 `SHA256SUMS` 讀到的 sha256 驗證後才 rename；下載失敗
  （連不上、10 秒連線逾時、20 秒內低於 32 KiB/s、checksum 不符）就退回由 controller 推送，
  並在輸出說明原因。`--fetch push` 維持舊行為（worker 不需要 GitHub），`--fetch remote` 不退回。
- 推送改成「60 秒內沒有新位元組到達才算失敗」（接收端每 5 秒回報檔案大小），
  外加依檔案大小計算的總期限（2 分鐘 + 以 16 KiB/s 計的時間）。
- Apply 逐一比對 sha256，已相同的 binary 不再重送。

## 當時的手動解法

在 worker 上直接 `curl -fLO` 下載同版本的兩個資產，用 controller 快取裡
（`$XDG_CACHE_HOME/lazylongarm/releases/<tag>/SHA256SUMS`）驗證過的 sha256 比對，
放到 `~/.local/bin` 並 `chmod 0755`；重跑 `worker add` 時第 1 步回報
「already has longarmd/longarm vX」並繼續。

## 未驗證

- 沒有在真實慢速連線上重跑新版本；stall 偵測與 fallback 只在測試（fake ssh／curl）中驗證。
- BusyBox wget 只支援 `-T`（讀取逾時），沒有低速中止。
- 舊版在 ssh 連線中途斷掉時，接收端的 `cat` 可能讀到 EOF 而把截斷的檔案 rename 到位；
  新版先比對大小與 sha256，這點未在真實斷線下重現。
