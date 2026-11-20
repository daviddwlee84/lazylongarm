# Longarm upstream 的行為與文件差異（v0.1.42，main@ef4643bc，2026-11-20）

| 症狀 | 原因 | lazylongarm 的處理 |
|---|---|---|
| `aka.ms/longarm` 的登入頁只出現「Microsoft (work or school)」 | 這是 Microsoft 內部 Hub（ACI，公司 Entra tenant）| 不用；自架 Hub，或公開 binary 預設的 `longarm.weavejam.com` |
| `longarm login` 不帶參數時跑到 weavejam.com | 公開 release 建置時寫入 `LONGARM_DEFAULT_HUB` | 一律用 join code + `--ca-fingerprint` 加入自己的 Hub |
| 透過 SSH 抓 Hub 的 `install.sh` 給 worker 用，worker 卻下載失敗 | `HUB_URL` 取自 `public_url`，沒設時取 request 的 Host header（例如 127.0.0.1:7480）| 不用 install.sh；從 GitHub 下載並驗證 binary，再用 scp 推過去 |
| `longarm-hub` service 起不來，state dir 裡有 root 擁有的檔案 | 用 root 執行了 `bootstrap`／`ca init` | 用 `runuser -u longarm-hub --` 執行 |
| deb 安裝 `longarm` 後 join 失敗，但套件安裝回報成功 | `postinst.sh` 在 `longarmd join` 失敗時仍 exit 0 | 不走 postinst 路線；一律用 `worker ls --json` 確認 |
| join code 一天後還有效 | `longarm worker add` 預設 `--ttl 24h` | 一律帶 `--ttl 15m` |
| 設了 `XDG_CONFIG_HOME` 沒效果 | client config dir 寫死 `~/.config/longarm`（issue #160）| 每個 Hub 可以設 `config_dir`（`LONGARM_CONFIG_DIR`）|
| `longarm version --json` 的 features 沒有 `speedtest` | `cliFeatures` 漏列 | 不靠 feature 偵測 speedtest |
| 文件寫 `longarm_<v>_<arch>.deb`，release 上找不到 | 實際檔名是 `longarm-linux-amd64.deb` | 用 bare binary 資產名稱 |
| `[tailnet]` 沒設，卻嘗試建立 Tailscale 節點 | provider 預設值看 OAuth secret／headscale 是否存在 | hub.toml 明確寫 `provider = "none"` |
| `longarm worker add` 回 `NotFound: no user "mac" (longarm admin user add mac)` | bootstrap 的 admin code 產生的 admin 憑證帶有 `user: "mac"`，但 Hub 的使用者表（multi-tenancy）裡沒有這個 user；owned worker 需要真的 user 紀錄 | 是 admin 就改用 `longarm admin join-code --role worker --name N` + `longarm admin approve N`（`--mint admin`，預設）|
