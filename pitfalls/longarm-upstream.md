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
| 第一次 `admin user add` 之後，新使用者看得到、也能用我們的 worker | user separation 打開後，fleet worker（沒有 owner）對所有使用者開放 | `user invite` 在還有 fleet worker 時拒絕執行；先 `worker add --reenroll --owner <你>` |
| worker 改成某個 user 的 owned worker 後，admin 憑證看不到也用不了它 | admin 對別人的 owned worker 沒有豁免；舊的 admin 憑證被當成同名的 user | `worker add --owner` 要求本機憑證的 user 就是 owner；先 `user self` |
| `admin join-code --role client --user U` 被拒絕 | user-bound code 只給該 user 的第一台裝置；之後的裝置要 `longarm login` 再到 `/link` 核准 | `peer add` 在 U 已有裝置時直接說明替代步驟 |
| `longarm admin user set U --panel` 回 `FailedPrecondition: U has machines already` | admin 幫 user 建的 panel login 只能用在還沒有機器的新 user（避免 admin 冒充 user） | 第一台裝置時就建（`user self` 預設 `--panel`）；事後只能在 hub 主機用 `lazylongarm hub panel-user U` 建同名的 admin 登入，核准的裝置會拿到 admin 憑證 |
| `longarm login` 印出的 `http://<hub>:7480/link` 打不開 | hub 沒設 `public_url` 時，網址是 gRPC 的 host 加上 HTTP listener 的 port；panel 只綁 loopback 時這個網址從外面連不到 | 用 `lazylongarm hub panel` 打開 panel，自己到 `/link` |
| user separation 打開後，`hub setup` 時建的 admin panel login 在 Nodes 頁看不到任何機器 | 名稱不是某個 user 的 admin 登入，會被當成同名的（空的）user | 用跟 user 同名的 admin 登入（`hub panel-user U`），或 user 自己的 panel login |
| `--reenroll` 之後 worker 一直 offline，`longarmd doctor` 顯示舊的 node | longarmd 只會重新載入同一個 node 的憑證；新的 node ID 要重啟才生效 | re-enroll 一律重啟 service |
| 設了 `https = ":443"` 後 longarm-hub 綁不到 port（依 unit 檔推斷）| 套件的 `longarm-hub.service` 以 `User=longarm-hub`、`NoNewPrivileges=yes` 執行，沒有 `CAP_NET_BIND_SERVICE` | `hub panel-access public` 寫入 drop-in `/etc/systemd/system/longarm-hub.service.d/lazylongarm-https.conf`（`AmbientCapabilities=CAP_NET_BIND_SERVICE`），切回 loopback 時移除 |
