# 来源与事实核验

## 在用的外部来源（2026-11-21，UTC+8）

正在运行的采集器 `pairscan`、录制服务 `booktape` 和它们的回补、核对脚本读取的全部外部来源。每个请求的参数、写到哪张表、精确的触发条件在 [dashboard/SPEC.md](dashboard/SPEC.md) 第 3 节和附录 A；这里给出地址、读什么、节奏和授权。除 Direxion 外都是匿名公开接口，不用密钥。`pairscan` 对每个主机每秒最多发一个请求。核对脚本 `dashboard/audit/sources.py` 手动运行时重读表中 `pairscan` 的大多数来源（约三百个请求，每个主机每秒最多 1 个，见 dashboard/README.md）。

| 来源 | 读什么 | 节奏 | 谁读 |
|---|---|---|---|
| Binance `GET https://fapi.binance.com/fapi/v1/klines` | 6 个合约的一分钟 K 线收盘价 | 每分钟结束后约 1 秒读最近 15 分钟、约 6 秒读最近 2 分钟；启动时补缺的历史 | pairscan |
| Binance `GET /fapi/v1/indexPriceKlines` | 收盘前一分钟的指数 K 线（铃锚点）；分红生效那一分钟的指数（核对） | 每次收盘后 1 分钟，缺的每 5 分钟；启动时补读（每秒一个） | pairscan |
| Binance `GET /fapi/v1/fundingRate` | 参考 ETF 分红时的特别资金费（核对） | 生效之后每 5 分钟，直到有结论 | pairscan |
| Binance `GET /fapi/v1/tradingSchedule` | 前后约一周的时段表（日历核对） | 每小时 | pairscan |
| Binance `GET /fapi/v1/exchangeInfo` | 价格刻度（定档数） | 启动时一次 | booktape |
| Binance WebSocket `wss://fstream.binance.com/public/stream`、`/market/stream` | `depth5@100ms`、`bookTicker`；`aggTrade`、`markPrice@1s` | 持续；心跳 5 秒 | booktape |
| Bitget `GET https://api.bitget.com/api/v3/market/candles`、`history-candles` | 一分钟 K 线；`type=index` 的指数 K 线（Binance 没有该合约时的铃锚点） | 同 Binance K 线；历史接口限流很严 | pairscan |
| Bitget `GET /api/v3/market/split-records` | 六只 ETF 合约的拆股记录 | 启动时、每天一次；有待定行为的载体窗口里每 5 分钟 | pairscan |
| Bitget `GET /api/v3/market/instruments?category=USDT-FUTURES` | 价格刻度（定档数） | 启动时一次 | booktape |
| Bitget WebSocket `wss://ws.bitget.com/v3/ws/public` | `books5`（SOXSUSDT 用 `books50` 全部 50 档，2026-11-22 前截到 10 档）、`books1`、`publicTrade`、`ticker`、`funding-rate` | 持续；心跳 5 秒 | booktape |
| Coinbase `GET https://api.exchange.coinbase.com/products/USDT-USD/ticker`、`/candles` | USD/USDT（F） | ticker 每 10 秒；K 线补分钟 | pairscan |
| ProShares 产品页 `https://www.proshares.com/our-etfs/leveraged-and-inverse/<代码>` | TQQQ / SQQQ 的 NAV（`price-nav`、`price-asOfDate`） | 收盘起每 5 分钟每只一个请求，直到确认；之后每小时复核 | pairscan |
| ProShares 历史 NAV 文件 `https://accounts.profunds.com/etfdata/ByFund/<代码>-historical_nav.csv` | 缺 NAV 的过去 Session | 启动时，每只一个请求 | pairscan |
| ProShares `https://www.proshares.com/api/distributionsummary?fund=<代码>&year=<年>`、分配日程页 `https://www.proshares.com/resources/distributions/distribution-schedule` | TQQQ / SQQQ 的分红金额与预计除息日 | 启动时、每天一次；载体窗口里每 5 分钟 | pairscan |
| Nasdaq `GET https://api.nasdaq.com/api/quote/<代码>/dividends?assetclass=etf` | QQQ、SOXX、TQQQ、SQQQ 宣布的分红 | 同上 | pairscan |
| Direxion 网站数据接口（AWS AppSync GraphQL）`https://p6vg2dlkpverjbr2afvpbqupky.appsync-api.us-east-1.amazonaws.com/graphql` | `GetThirtyDayPrice`：SOXL / SOXS 的 NAV；`GetKurtosysFunds` 的 `Distributions`：SOXL / SOXS 的分红 | NAV：收盘起每 5 分钟每只一个请求，直到两腿都确认（第 4 条授权“直到 NAV 稳定”；确认后到下一次收盘不再读）；启动时补缺的近期交易日。分红：每天一次；除息前一天从收盘起每 5 分钟、只问还没有金额的那只，读到就停，最晚到除息日之后那个交易日结束 | pairscan（`audit/sources.py` 核对时也读） |
| Bitget 下载中心 `POST https://www.bitget.com/v1/statistics/public/download/getPublicDataV2` 与文件 `https://img.bitgetimg.com/online/trades/UMCBL/<合约>/…zip`；REST `GET https://api.bitget.com/api/v2/mix/market/fills-history` | 历史成交（`perp_second` 的成交列）；审计时逐笔对照 | 手动回补时；审计 `--rest` 每秒最多 2 个 | backfill、audit |
| Binance `https://data.binance.vision/` 日 aggTrades（核对 `.CHECKSUM`）；REST `GET https://fapi.binance.com/fapi/v1/aggTrades` | 历史成交 | 手动回补时 | backfill |
| NYSE Hours & Calendars `https://www.nyse.com/trade/hours-calendars` | 2026–2028 休市日与提前收盘日 | 已抄进 `dashboard/internal/scan/calendar.go`，运行时不读；临时停市按 NYSE / Nasdaq 公告手工写进配置 | 运维 |
| iShares 分配日程 PDF（2026-11-18 版）、Direxion 与 ProShares 的历年除息规律 | 配置 `corporate_actions` 里预先写的除息日 | 每个季度由运维更新 | 运维 |

**Direxion 的授权**（只此一项需要授权，原话在 `experiments/anchor_v4_20261120/OWNER.md`）：第 4 条，2026-11-20 01:35，用户选择“授权用 Direxion 接口”：“只读，收盘后每 5 分钟一次、每只基金一次请求，直到 NAV 稳定；密钥只放在 dashboard/.env，不写进代码”；第 11 条，2026-11-21 14:40，“授权读 Direxion 分红数据”：“每天一次；除息前一天从收盘起每 5 分钟一次、每只基金一次请求，读到金额就停”。范围只限 SOXL、SOXS 的只读查询；不解、不绕过网页的 Cloudflare 人机验证。密钥是网站公开脚本里的，只放在 `dashboard/.env` 的 `DIREXION_SITE_API_KEY`。使用条款没能核实，Direxion 可能随时更换密钥或限制访问；密钥被拒时采集器停止请求它，SOXL / SOXS 停在铃锚点。

## 历史核验记录

以下各节按日期解释，是当时研究或设计的依据，不是现行来源；其中 Yahoo、Nasdaq.com 收盘价、综合行情带、Cboe 等收盘价渠道已在 2026-11-20 退出（PRD 3.1）。下面各节提到的 `experiments/<目录>`，不在本目录的已于 2026-11-20 移到 `/home/ubuntu/code/etf-pair-archives/experiments-before-20261120-082031/<目录>`（清单 `MANIFEST.json`）。

核验日期：2026-11-17（UTC+8）。当时 PRD 用到的事实（PRD 现在引用其中的 S1、S2）；历史接口调用与费率快照见 [历史来源](docs/research/SOURCES_20261117.md)。页面可读不代表本账户已具备实时行情权限或交易能力。

| ID | 来源 | 支持的事实与限制 |
|---|---|---|
| S1 | [Direxion SOXL/SOXS factsheet](https://www.direxion.com/uploads/SOXL-SOXS-Fact-Sheet.pdf) | 每日 ±300% 目标，基准 ICESEMIT；市场收盘与 NAV 分列，不承诺多日累计 ±3 倍。 |
| S2 | [NYSE Hours & Calendars](https://www.nyse.com/trade/hours-calendars) | 常规收盘、盘前/盘后、交易日及提前收盘日；实现需读取正确日历并处理 DST。 |
| S3 | [Massive: official closing price](https://massive.com/knowledge-base/article/how-do-i-get-the-official-closing-price-for-a-stock) | 官方收盘需同时识别上市交易所和收盘成交条件；日线 close 可能不同；历史/实时权限取决于订阅。仅核验文档，未授权或验证付费行情接入。 |
| S4 | [Bitget Traditional Asset Perpetual Futures](https://www.bitget.com/support/articles/12560603845668) | USDT 报价结算；闭市 internal mode 使用自身盘口与 EMA，不能当独立 ETF 公允价格。 |
| S5 | [Bitget UTA introduction](https://www.bitget.com/docs/uta/uta-intro)；[Order management](https://www.bitget.com/docs/catalog/trading/order-management) | 账户与订单接口的产品边界；适配设计须逐接口核实模式、IOC/post-only、reduce-only、数量及错误语义，不由研究脚本推断。 |
| S6 | [Binance USD-M new order](https://developers.binance.com/en/docs/catalog/core-trading-derivatives-trading-usd-s-m-futures/api/rest-api/trade) | 订单、client ID、IOC/GTX、持仓模式与 reduce-only 限制；不能套用 Bitget 字段。未提交订单或更改账户。 |
| S7 | [Binance local order book](https://developers.binance.com/en/docs/products/derivatives-trading-usds-futures/websocket-market-streams/How-to-manage-a-local-order-book-correctly) | REST 快照与 WS 增量的衔接、更新序号、断序重建；公开成交采集不等于完成可执行 L2。 |
| S9 | Tardis.dev：目录 `https://api.tardis.dev/v1/exchanges/binance-futures`、`…/bitget-futures`，免费日数据 `https://datasets.tardis.dev/v1/<交易所>/quotes/2026/11/01/<合约>.csv.gz`，[订阅说明](https://docs.tardis.dev/faq/billing-and-subscriptions)（2026-11-23 读，原件在 `experiments/mid_history_20261123/raw/`） | 12 个合约从上线起的最优价逐次变化（`quotes`）和 5 / 25 档快照，可算录制以前每秒的中价；只能订阅、不卖单段，价格不公开；每月 1 日免费。只做了调查、没有采用（用户第 20 条问起，`experiments/mid_history_20261123/REPORT.md`） |
| S8 | [pandas rolling](https://pandas.pydata.org/docs/reference/api/pandas.DataFrame.rolling.html)；[EWM](https://pandas.pydata.org/docs/reference/api/pandas.DataFrame.ewm.html) | 历史研究使用的滚动均值与 EMA 定义；新代码以 PRD 的明确因果公式和手算测试为准。此项沿用已保存的研究核验，未据此宣称生产实现。 |

## 本地证据

- `experiments/anchor_review_20261117/`：Yahoo 实际返回与到达时间；同日 SOXS 的日线更正及末分钟差别。Yahoo 不是官方交易所；query1/query2 不构成独立多源核验。
- `experiments/paper_20261117_fullsize/{finished.json,config.json,paper.sqlite}`：纸面结束状态、假设与逐笔记录。
- `experiments/flow_20261116/finished.json`：公开成交研究已结束。
- `research/rollover.py`：当前锚点权重的均线研究与现金重算；这里只读核对，没有转换成生产策略。
- `strategy/pair.go`、`tape/`：当时的纯算式和匿名行情代码，2026-11-20 已删除（存档见 [DATA.md](DATA.md) 第 3 节）。

## SPEC 阶段必须落实的输入

1. 两场所当前合约交易状态、乘数、规则、真实账户费率、保证金模式/档位和私有流权限。本次没有重新读取账户，不把历史费率当当前值。
2. 两个独立收盘发布渠道的实际访问方式、权限、延迟和字段语义。Yahoo chart 可用于历史研究，但没有已核验的实时发布 SLA。（已由 2026-11-20 的锚点规则取代：铃锚点后 NAV，PRD 3.1。）
3. 实时 USD/USDT 来源、精度与可用时段；ICESEMIT 的绝对点位仅为可选展示。
4. 多源冲突和收盘更正的真实样本（随收盘价锚点一起退出）、夜盘/周末的连续有效输入、实际深度下的 TT / MT 纸面验收。

以上未完成项不能写成“接口都已验证可实盘”。数据权限不足时先交付离线/只读能力，生产启用条件须如实列明。

## 11-17 Binance 比较实验补充

- [Binance public-data](https://github.com/binance/binance-public-data)：本次实际下载 `data.binance.vision/data/futures/um/daily/aggTrades/` 与 `bookDepth/`，SOXLUSDT/SOXSUSDT，11-08 至 11-16 UTC。后者是累计百分比深度，不是 L2；相应 bookTicker 历史路径返回 404。原件和 SHA-256 在 `experiments/binance_policy_20261117/manifest.json`。
- 实测 Binance 公共 `fapi/v1/exchangeInfo`、`depth?limit=100`、`klines`、`fundingRate`，成交流 `fstream.binance.com/market/stream`；查询参数和原始响应落盘。当前规则两腿数量步长 0.01、价格步长 0.01、最小名义 5 U，未据此推断历史所有时刻规则或实际账户费率。
- Yahoo chart 日线与 Nasdaq `api.nasdaq.com/api/quote/{symbol}/historical?assetclass=etf` 实际响应交叉核对；SOXS 11-16 日线两者均为 59.18。这是两份供应商返回的一致性证据，不是官方收盘竞价来源已验证。
- Kraken 公共 `api.kraken.com/0/public/Ticker?pair=USDTUSD` 用于纸面 USD/USDT；Yahoo USDT-USD 已完成五分钟条用于历史。均保存采集时间。

## 11-17 架构设计（SPEC）补充

为编写 SPEC（根目录的 SPEC.md 已于 2026-11-24 按用户要求删除，最后的副本在 `experiments/decisions_20261123/prd_rewrite_20261124/before/`）另行核验了两所的接口契约、收盘价渠道、USD/USDT 来源和交易日历，读取时间 2026-11-17 17:38–17:53（UTC+8）。逐条来源和待验收项在 SPEC 第 15 节与 4.8 节（更细的清单在 1.0 版的备份 `experiments/spec_review_20261117/SPEC_v1.0_20261117.md` 里）；原始响应、官方文档副本和抓取脚本在 `experiments/spec_review_20261117/{binance,bitget,anchor}/`。全部是匿名公开 GET，没有使用密钥、没有连接私有接口、没有下单。

- 上面“SPEC 阶段必须落实的输入”四项的现状：第 1 项的公开部分（规则、步长、资金费、Bitget 档位）已核验，账户费率、Binance 档位、账户模式与私有流权限仍待用只读密钥验收（账户费率后来定为不核实、用用户给的值，OWNER.md 第 14、25 条）；第 2 项找到了匿名可读的渠道并记录了它们的局限（语义无文档、使用条款限制、Yahoo 收盘后改价），带成交条件的官方收盘来源只读了文档，是否订阅待用户决定；第 3 项按用户意见只用 Coinbase 的 USDT-USD，只有单次快照的实测；第 4 项没有新增证据。
- 设计没有把任何待验收项写成已验证。
- 上表 S6 指向的 Binance 现行逐端点页面匿名读不到；SPEC 的订单参数依据是官方旧版文档页和官方 Go SDK 文档（SPEC 第 15 节 B1）。

## 11-17 19:51 后：双场所费率和历史档案

- 用户确认 Binance 两合约 taker 2.5 bps；不是私有接口核验。
- [Bitget account fee rate](https://www.bitget.com/docs/catalog/account/assets-balance)：19:51:09 UTC+8，对 SOXLUSDT / SOXSUSDT 各发一次账户 GET，均返回 taker `0.000065`、maker `-0.00001`。只在原外部私有文件读取签名信息，没有复制凭据或发出交易；原始费用响应在 `experiments/venue_policy_20261117/*_account_fee.json`。
- [Bitget 官方下载中心](https://www.bitget.com/data-download)：网页公开脚本使用 `/v1/statistics/public/download/getPublicDataV2`；本次按原接口参数取得实际 ZIP 链接，成交 CSV、500 档深度 XLSX 均成功读取。可用日档截至 11-15；不是假定全部日期已发布。网页、下载清单、原件和哈希保留。
- [Bitget market data](https://www.bitget.com/docs/catalog/market/market-data)：实际核验 `orderbook`、`candles`、`history-fund-rate` 及两个合约规则；当前两腿数量步长 0.01，最小名义 5 U。资金费 mark 分钟查询实测起点排除、终点包含，精确取结算分钟开盘；金额属于估计，非账户已结流水。
- 两所新的纸面以公开行情运行；共同锚点与 FX 沿用原研究，未把研究的 Kraken/Yahoo FX 变成生产 SPEC 的 Coinbase 配置。


## 2026-11-19 有限时实盘接口核验

- [Bitget Order Management](https://www.bitget.com/docs/catalog/trading/order-management)：双腿逐笔限价IOC、reduceOnly、clientOid未知结果查单、order-info累计值和fills实际成交时间。账户只读GET验证了现有真实订单字段，正手续费为支出、负数为返佣。
- [Account Settings](https://www.bitget.com/docs/catalog/account/account-settings)：只改SOXLUSDT/SOXSUSDT为crossed 10倍；不改全账户持仓模式。
- [Financial Records](https://www.bitget.com/legacy-docs/uta/account/Get-Financial-Records)：amount为资金金额、positionAmount非现金替代、ts为流水生成时间。逐笔fills.createdTime与公开fundInterval=8/nextUpdate核对资金费归属；未知归属不批准普通保本退出。
- 账户费率和产品参数本次重新GET，两腿taker均0.65bps，数量步长0.01；SOXL价格tick0.01、SOXS0.001。证据在 `experiments/live_bitget_20261119/`，不保存凭据。
- 01:32订单GET确认零成交cancelled IOC的`feeDetail`可为双空字段；这是本次真实响应事实，文档未承诺。01:35恢复后再次出现零成交取消，已能对账后重试。原三份28笔fills现金/费用独立合计+0.08773562230U，交易所读回两合约空仓无挂单。原始响应与核对在`incident_0126/`。

## 2026-11-19 Python实时试验

- [Order Book Channel](https://www.bitget.com/docs/uta/websocket/public/Order-Book-Channel)、[Quick Start](https://www.bitget.com/docs/uta/quick-start)、[Update Preview](https://www.bitget.com/docs/uta/update-preview)：books1/5/50、快照和增量/pseq、公开WS地址及ping/pong；本次已读。books50生产与demo切换时间不同，代码同时处理snapshot/update，不假定只有快照。
- [Order Management](https://www.bitget.com/docs/catalog/trading/order-management)：每UID下单10次/秒，未知发送结果按clientOid查询。Python连接池计量请求开始/写出/返回，写出不代表撮合。
- [Account Assets](https://www.bitget.com/docs/catalog/account/assets-balance)：资金流水最大30天窗口，分29天请求并保留检查点/流水ID去重。
- 45秒真实公开WS只读预检两次通过，当前账户规则重新读取；数据路径与费用来源见试验文档。Python基准只用本地HTTP替身，不能代表真实网络速度。


## 2026-11-19 资金流水分页核验

- [Assets & Balance / Get Financial Records](https://www.bitget.com/docs/catalog/account/assets-balance)：单页最多100，cursor分页。文档声明list数组；本次只读查询事故窗口实际返回两页各100条、尾页`list:null,cursor:null`。空尾页行为按实测记录，不声称文档保证。证据：`experiments/live_bitget_20261119/repair_1100/funding_pages.json`。
- [Order Management](https://www.bitget.com/docs/catalog/trading/order-management)、[Depth Channel](https://www.bitget.com/docs/uta/websocket/public/Order-Book-Channel)：复核IOC/reduceOnly/clientOid及books1/5快照、books50增量，保持已有接口使用；没有双IOC原子成交保证。

## 2026-11-24 盘口冲突与整本盘口的调研

证据和报告在 `experiments/decisions_20261123/book_conflict/`（`REPORT.md`；用户 OWNER.md 第 33、35 条）。只读：公开文档、录制服务的原始文件，以及一次 5 分钟的公开行情连接（不登录、不下单）。

- [Bitget UTA Depth Channel](https://www.bitget.com/docs/uta/websocket/public/Order-Book-Channel)：`books1` 1 ms、`books5` 10 ms、`books50` 20 ms、`books` 50 ms；`seq` / `pseq` 的定义。SBE 行情 [BBO](https://www.bitget.com/docs/uta/websocket/sbe/sbe-bbo)、[Level 50](https://www.bitget.com/docs/uta/websocket/sbe/sbe-depth50) 和 [RPI 深度](https://www.bitget.com/docs/uta/websocket/public/RPI-OrderBook-Channel)（只读了文档，没有试）。四页 2026-11-24 15:03（UTC+8）取回的 HTML 在 `sources/bitget_*_20261124.html`。
- Bitget 公开 WebSocket `wss://ws.bitget.com/v3/ws/public`：2026-11-24 14:33:14–14:38:14（UTC+8）订阅 SOXLUSDT、SOXSUSDT 的 `books`、`books1`、`books5`、`books50`，原始帧在 `probe/`（每行“接收毫秒、制表符、原文”）。
- [Binance 文档全文](https://developers.binance.com/en/docs/llms-full.txt)（2026-11-24 14:37 读取，8,465,366 字节，sha256 `9e633d0c…`）：USDⓈ-M 合约“How to manage a local order book correctly”的九个步骤、增量深度流 250 / 500 / 100 ms、`@bookTicker` 实时；节选在 `sources/binance_llms_full_extract_20261124.txt`。
- 开源框架的做法（hftbacktest 的 Fusing Depth Data、NautilusTrader、Tardis 的数据说明）：地址和读取时间在报告第 6 节；后两者只读了检索摘要。
