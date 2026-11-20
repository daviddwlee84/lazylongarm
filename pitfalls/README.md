# Pitfalls

過去踩過或調查時發現的坑，以**症狀與錯誤原文**索引。正常操作見 [`docs/`](../docs/index.md)，
未來工作見 [`TODO.md`](../TODO.md)。

| 記錄 | 症狀關鍵字 | 狀態 |
| --- | --- | --- |
| [Longarm upstream 的行為與文件差異](longarm-upstream.md) | aka.ms/longarm, Entra, weavejam, install.sh HUB_URL, bootstrap as root, worker add 24h, XDG | 已確認（v0.1.42） |
| [上傳頻寬慢時 worker add 推送 binary 逾時](slow-uplink-binary-push.md) | context deadline exceeded, Install longarmd + longarm, slow uplink, 重複複製 | 已修正（Unreleased，`--fetch`） |

新增記錄時保留日期、版本、錯誤原文、觀察證據與未驗證事項；不要放 join code、私鑰或完整設定。
