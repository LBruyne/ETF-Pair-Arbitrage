# 数据清单

核验：2026-11-21 20:40，2026-11-22 02:31–03:00 复核（UTC+8），正式库只经只读角色查询，文件按目录实测；大小除 `perp_second` 用库给的字节数（10⁹）外，都是 `du -h` 的值（GiB / MiB）。这里只写每份数据由谁写、从哪里来、从何时起、存在哪、多大，以及已删除数据的存档。表的列和口径在 [dashboard/SPEC.md](dashboard/SPEC.md) 第 4 节和附录 A，外部来源在 [SOURCES.md](SOURCES.md)，数据核验的结论在 `experiments/verify_20261121/{perp_second,anchors,board}/REPORT.md` 和 `experiments/verify_20261122/data/REPORT.md`；回测能用哪些数据、缺口和补充在第 5 节。

目前在写数据的进程：看板的采集器 `pairscan`（systemd `etf-pair-pairscan.service`）、录制服务 `booktape`（`etf-pair-booktape.service`）；另一会话的只读采集 `research.offhours.collect`（第 2 节末），以及另一会话 2026-11-23 的 Bitget EMA/σ 实验（`live_pair.ema_live` 真实下单、`live_pair.ema_paper` 纸面、`research.ema_receipts`、`research.ema_watch`，都写 `experiments/bitget_ema_wss_20261123/`，见 [实验说明](docs/research/BITGET_EMA_WSS_20261123.md)）。本项目的生产交易程序（PRD / SPEC）尚不存在；上述实验的订单与账户记录只在它自己的目录里。所有数据都没有清理规则，不会被自动删除。

## 1. 正式库 `etfpair`（PostgreSQL 127.0.0.1:5432，schema `etfpair`）

写入角色 `etfpair_writer`，Grafana 与核对脚本用只读角色 `etfpair_reader`；密码在 `dashboard/.env`。

| 表 | 写入方 | 来源 | 范围（UTC+8） | 规模 |
|---|---|---|---|---|
| `perp_second` | `booktape`（实时）；`dashboard/backfill/perp_second.py`（历史成交）、`perp_second_recordings.py`（临时录制） | 两所 WebSocket 推送；Binance `data.binance.vision` 日 aggTrades 与 REST aggTrades，Bitget 下载中心成交归档与 REST fills-history | 成交列：每个合约从 2026-07-16（`history_start`）之后的第一笔成交起，Binance SOXSUSDT 07-16 17:05、SOXXUSDT 11-18 17:30 上市起。盘口列：SOXL / SOXS 2026-11-19 19:15 起（临时录制：Binance 1 档、Bitget 5 档；11-20 12:00:00–15:10:44 没有），全部 12 个合约 11-20 18:43:06 起（录制服务：每侧 5 档，Bitget SOXSUSDT 10 档，2026-11-22 12:16:42 起 50 档） | 约 1.22 亿行，20.3 GB（堆 10.5 GB、主键索引 9.7 GB）；每天 1,036,800 行 |
| `perp_second_minute` | `pairscan`（启动时和每分钟刷新最近 65 分钟；补数后由 `dashboard/backfill/perp_second.py` 在同一事务里刷新） | 由 `perp_second` 汇总：每个合约每分钟一行（有成交、有盘口的秒数，分钟末的成交价、中价及其时刻） | 2026-07-16 起；2026-11-23 11:44–11:48 一次补全 | 2,068,593 行、347 MB（2026-11-23 12:35）；每天 17,280 行 |
| `perp_minute` | `pairscan` | Binance `klines`；Bitget `candles` / `history-candles` | 2026-07-16 起回补，2026-11-18 起实时（每分钟）；更早的 253,403 行（约 11 %：Binance QQQ 自 05-12、SOXL 自 05-15、TQQQ / SQQQ 自 06-29，Bitget SOXL 自 05-15）是试装时留下的，不属于任何 Session | 12 个合约，约 230 万行（2026-11-22），329 MiB |
| `fx_minute` | `pairscan` | Coinbase `USDT-USD`：`candles`（历史）、`ticker`（每 10 秒） | `candle` 到 2026-11-18 15:13，`ticker` 11-18 15:14 起 | 275,003 行，39 MB |
| `etf_bell` | `pairscan` | Binance `indexPriceKlines`；Binance 没有该合约时 Bitget `history-candles` `type=index` | Session 2026-07-16 起，每次收盘后约 1–2 分钟 | 546 行（Binance 458；Bitget 88，即 SOXX 到 Session 11-17） |
| `etf_nav_obs`、`etf_nav` | `pairscan`；`dashboard/backfill/nav_wayback.py`（一次性） | Direxion 网站数据接口（`api`）、Wayback 存档页（`wayback`）；ProShares 历史 NAV 文件（`history-csv`）、产品页（`page`） | SOXL / SOXS：`wayback` 2026-09-03 至 10-09，`api` 10-12 起（07-16 至 09-02 没有来源）；TQQQ / SQQQ：`history-csv` 07-16 至 11-18，`page` 11-19 起 | `etf_nav` 292 行 |
| `etf_action` | `pairscan` | 配置 `corporate_actions`、ProShares、Nasdaq、Direxion 分配数据、Bitget 拆股记录 | 除息 / 除权日 2026-09-15 至 2027-03-24 | 24 行 |
| `etf_action_check` | `pairscan` | Binance 指数 K 线与资金费、`etf_nav`、`perp_minute` | 已生效的行为 | 11 行 |
| `etf_session`、`etf_phase` | `pairscan`，每次启动和每次收盘后整表重写 | `calendar.go`（NYSE 2026-05 至 2028 的休市日、提前收盘日）加配置 `calendar_overrides` | 2026-07-16 至未来 7 天（现在到 Session 11-27） | 95 / 400 行 |
| `calendar_check` | `pairscan`，每小时 | Binance `tradingSchedule` | 只有最新一行 | 1 行 |
| `combo`、`combo_market`、`fee_level` | `pairscan` 启动时 | `dashboard/config.yaml` | — | 2 / 4 / 4 行 |

看板（3.3，2026-11-23 13:54 发布；SQL 是 3.2 的，同日 11:39 起）的数字出自 dashboard/SPEC.md 第 4 节的函数（`combo_range`、`combo_prices`、`combo_last`、`perp_coverage`、`combo_sessions`、`action_start`、`combo_actions`，以及它们内部用的函数）和上表几张表的直接查询；函数不存数据，唯一存数据的派生表是 `perp_second_minute`。3.1 的函数（`combo_series`、`combo_points` 等）已于 2026-11-23 15:24 删除（用户第 24 条）。

## 2. 文件

| 位置 | 写入方 | 内容 | 范围（UTC+8） | 规模 |
|---|---|---|---|---|
| `experiments/book_tape/<venue>/<YYYYMMDD>/` | `booktape` | 每个合约每个推送通道的原始帧，原样（Bitget `books50` 到 2026-11-22 12:16 截到每侧 10 档，之后全部 50 档），每小时一个 gzip 文件；`events.jsonl` 记连接、断开和交易所的控制帧。格式见 dashboard/SPEC.md 附录 A，读取用 `research/booktape.py` | 2026-11-20 18:43 起 | 3.3 GiB（到 11-22 02:50）；美股交易日约 3.0 GiB / 天（常规时段 7 小时占约 2/3），周末约 1.1 GB / 天 |
| `dashboard/run/pairscan.log`、`booktape.log` | 两个服务 | 运行日志（UTC+8），追加写，含每次重启 | `pairscan.log` 2026-11-18 15:14 起，`booktape.log` 2026-11-20 18:43 起 | 各约 0.3 MB；没有轮转，`booktape.log` 每天约 0.23 MB |
| `experiments/book_tape_interim_20261119/` | 临时录制（已删除的 `cmd/flow-record` 的程序，副本在目录里） | `raw/`：Bitget `books5` 与 `publicTrade`、Binance `bookTicker` 与 `aggTrade`，只有 SOXL / SOXS；`tape/`：转换成录制服务目录格式的同一份数据 | 2026-11-19 19:15 至 11-20 12:00 | 27 GB |
| `experiments/book_tape_interim_20261120/` | 同上 | 同上 | 2026-11-20 15:10 至 18:44 | 3.4 GB |
| `experiments/perp_second_history/` | `dashboard/backfill/perp_second.py archives` | 下载的成交归档原件（`raw/`）和 `manifest.csv`（地址、字节数、sha256、读取时刻） | 2026-11-20 起的下载；更早的在存档（下一行） | 474 MB，888 条 |
| `experiments/funding_history_20261122/` | lead，2026-11-22 一次性读取 | 已结算资金费：`funding.csv`（交易所给的结算时刻、费率、Binance 的类型和标记价）和原件 `raw/` | Binance 6 个合约 2026-07-16 起；Bitget SOX 三只 08-22 起、QQQ 三只 08-24 08:00 起（接口只给 90 天，更早的已不可得），都到 11-22 00:00 | 940 KB；Bitget 要在 2027-02-15 前重取（README） |
| `/home/ubuntu/code/etf-pair-archives/experiments-before-20261120-082031/` | 2026-11-20 从 `experiments/` 移出 | 42 项旧实验，含 `perp_second_history/rows/`（载入 `perp_second` 的 1,411 个行文件；`experiments/perp_second_load_20261120/rows_archive/` 是指向它们的符号链接）、`soxl_bitget_study_20261119/`（SOXL、SOXS、SOXX 的成交库）、`live_bitget_20261119/`（已结束的 Python 试验的账本）、`official_close_20261118/`、`official_close_20261119/`（已删除的收盘价来源的调研证据）、`go_removed_20261120/`；清单 `MANIFEST.json` | — | 97 GB |
| `experiments/bitget_offhours_v1_20261120/prospective/` | 另一项研究的只读采集 `research.offhours.collect` | 见该研究的说明 | 运行到 2026-11-24 03:54 或 12 GB | — |

测试用的临时 PostgreSQL 集群都停着，只在开发时启动（看板 3.1–3.3 的临时库 `experiments/real_spread_20261122/pg`，127.0.0.1:5462，已于 2026-11-23 15:25 删除，用户第 24 条；要用时按 `experiments/real_spread_20261122/scripts/copy_db.sh` 重建）：`experiments/nav_anchor_20261120/pg`（库测试的 `ETFPAIR_TEST_DSN`，13 GB）、`booktape_python_v3_20261120/pg`、`booktape_v3_impl_20261120/pg`、`booktape_review_20261120/pg`、`book_tape_smoke/pg`。

## 3. 已删除的数据与存档

| 删了什么 | 何时（UTC+8） | 存档 | 存档里有什么 |
|---|---|---|---|
| 表 `etf_close_obs`、`etf_anchor`，`etf_session.usable_ms` 列（看板 2.8，锚点改为铃锚点后 NAV） | 2026-11-20 19:45 | `experiments/anchor_v4_20261120/deploy/archive_20261120T114549Z.dump`（pg_dump 自定义格式）和同名 `.schema.sql` | `etf_close_obs` 3,235 行（nasdaq 1,518、yahoo 1,531、tape 180、proshares 6）、`etf_anchor` 1,516 行（Session 2025-11-18 至 2026-11-19），以及当时的 `etf_session`、`etf_phase` |
| 表 `perp_tick`（看板 3.0，删除 5 秒 REST 采样） | 2026-11-21 19:32:49 | `experiments/board_second_20261120/deploy/archive_20261121T112943Z.dump`（sha256 `70523b3a…`）和同名 `.schema.sql` | 442,048 行，2026-11-19 15:51:20 至 11-21 19:29:40 每 5 秒：REST 成交价和含 RPI 的买一 / 卖一 |
| Go 代码 `pilot/`、`cmd/pair-live`、`tape/`、`cmd/flow-record`、`strategy/` | 2026-11-20 | 上表存档目录的 `go_removed_20261120/removed_go_sources.tar.gz`（说明在同目录 `README.md`） | 删除时的源码 |

存档只能读，不会自动放回。查看：`pg_restore -l <文件>`；放回 `perp_tick` 的命令在 dashboard/SPEC.md 第 9 节末（回退）。读这些表的研究脚本已于 2026-11-22 移到各自的实验目录，只能改读存档，清单在 [RESEARCH.md](RESEARCH.md) 第 13 节。

## 4. 增长与磁盘

- 原始文件美股交易日约 3.0 GiB、周末约 1.1 GB。`perp_second` 在 2026-11-21 19:59 前约每天 0.89 GB（含每行插入后再更新一次留下的旧版本）；20:06 起这张表每天自动清理一次旧版本（20:08:24 第一次），19:59 → 11-22 02:46 只增长 38 MB（约 0.135 GB / 天：堆在复用清理空出的空间，增长全在索引）；空间用完之后按行宽（5 档 373–407 字节、10 档 625 字节、SOXS 的 50 档约 2.5 KB、主键约 80 字节）稳态约 0.6 GB / 天，下一次清理（约 11-22 19:15）之后和一周后复测。
- 根分区 3.5 TiB，剩 904 GiB（2026-11-22 02:45）。按每天 3.8 GB 的上限约 238 天、按实测一周平均约 3.1 GB / 天约 290 天写满（分区上别的项目也在写，这个天数只算本项目）；没有清理规则，由运维决定何时归档或删除。

## 5. 回测数据的缺口（2026-11-22 盘点，11-23 补记）

详见 `experiments/verify_20261122/backtest_data/REPORT.md`。

- **可成交的多档盘口**只有录制以来的：12 个合约 2026-11-20 18:43:06 起（第一个完整的录制 Session 是 Session 11-20，2026-11-24 05:00 结束）；SOXL / SOXS 另有 11-19 19:15 起的临时录制（Binance 只有 1 档，11-20 12:00:00–15:10:44 没有）。更早的补不了：Binance 没有这些合约的 `bookTicker` 归档，`bookDepth` 没有价位；Bitget 下载中心的 `depth500` 含 RPI、约 20–30 秒一张，只能做成本和容量的诊断。
- **盘口年龄**：读超时按连接算，安静的合约（周末的 SOXX）盘口可以合法地 1 分钟不变；判断过期按连接（断线事件），不按单个合约的 `second_ms − book_ms`。
- **资金费**：已补（上表 `funding_history_20261122`）；Bitget 07-16 至 08-21 / 08-24 的已不可得，回测里记“未知”，不记 0。预测资金费只有录制以来的（Bitget `funding-rate`、Binance `markPrice@1s`）。
- **得知时刻**：成交价、分钟价、FX、铃锚点、日历、分红拆股的数值从 07-16 起完整；历史上的 NAV 发布时刻和分红宣布时刻没有记录，只能从现在起记（采集器在记），更早的按规则假设并标注。SOXL / SOXS 07-16 至 09-02 没有 NAV（整段用铃锚点）。
- **成交列**：2026-11-23 11:42 的 `load` 之后缺的 447 秒已全部补上——Binance UTC 11-22 的 41 秒于 11-23 16:17（插入 9 行、补 32 格），Bitget 11-22 的 406 秒于 11-24 14:07（插入 19 行、补 387 格），到 11-24 13:07 为止缺成交价的秒数为 0（dashboard/SPEC.md 第 9 节末）；之后每天录制服务重连留下的几秒，用 `experiments/board33_20261123/maint/archives.sh` 照此补。Bitget 11-21 有 130 秒录到的每秒最后一笔早于归档（04:50、05:13–05:16、18:12，价差中位 9.6 bps、最大 72.8 bps），按只补空列的规则保留录到的值（dashboard/SPEC.md 第 9 节末），回测用到这几秒时要知道它们与归档不同。
- **其他**：Binance SOXL、SOXS、TQQQ、SQQQ、QQQ 10-27 08:02:45–08:30:18 的每秒成交已于 2026-11-23 11:42 从官方日 `trades` 文件补上（8,221 行）；合约规则 07-16 以来没变（由成交价和四次快照确认）；订单延迟和 IOC 回报只能在纸面和小额实盘时测。
