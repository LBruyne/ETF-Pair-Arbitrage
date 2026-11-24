Responses the parser tests read, saved by the anchor v4.1 research (experiments/anchor_v4_20261120/v41/raw/index.tsv),
public GET requests with a generic User-Agent. Retrieval time UTC, URL, sha256 of the response as saved:

proshares_distributions_TQQQ_2026.json      2026-11-20T08:40:26Z  https://www.proshares.com/api/distributionsummary/?fund=TQQQ&year=2026 (redirected to distributionsummary?fund=...)  90ce3d70a4fc14b4c2149d713235736f8e3901136c614aec7365d81c34d87e17
proshares_distribution-schedule_excerpt.html 2026-11-20T08:40:19Z  https://www.proshares.com/resources/distributions/distribution-schedule  361c5ddf1bee63dff3e2f19eaa7097fc0647854569c3e96af4dc3d7df68d4ad8
                                            (the "2026 Quarterly Dates" part of the page only)
nasdaq_dividends_TQQQ.json                  2026-11-20T08:44:29Z  https://api.nasdaq.com/api/quote/TQQQ/dividends?assetclass=etf  b37b271035dc4731c374a3f588e5e99e3623639c836e5d0e101023fb775a91d2
nasdaq_dividends_SOXL.json                  2026-11-20T08:44:34Z  https://api.nasdaq.com/api/quote/SOXL/dividends?assetclass=etf  b89695b4db2c5ddd1a7ff40f2e2872a52096a694174195396b0fcc9097d47071
bitget_split-records.json                   2026-11-20T08:34:33Z  https://api.bitget.com/api/v3/market/split-records  f8e9708a4fe9d7a794ac96fceedf59fc4a12e1e0a6d4b46ffa50078ebc739566
binance_tradingSchedule.json                2026-11-20T08:32:19Z  https://fapi.binance.com/fapi/v1/tradingSchedule  0da6faf9ba95cd6358bb50ea32b8b3db8e7955935c1c5713e13d347804802bd0
binance_fundingRate_QQQUSDT.json            2026-11-20T08:49:07Z  https://fapi.binance.com/fapi/v1/fundingRate?symbol=QQQUSDT&startTime=1789344000000&endTime=1790380800000&limit=1000  c91582fd870f9e8cf6f9bacec81efc7d24384298726c2ebafbcbd185365be947

Saved by the second fix round of anchor v4.1 (experiments/anchor_v4_20261120/v41/impl/raw/direxion/index.tsv): POST of the query
getKurtosysFunds(Ticker) { Ticker Distributions { ... } TimeStamp TradeDate } to Direxion's site API (OWNER.md item 11), the key in
the X-Api-Key header only (not in these files):

direxion_getKurtosysFunds_SOXL.json         2026-11-21T06:40:11Z  https://p6vg2dlkpverjbr2afvpbqupky.appsync-api.us-east-1.amazonaws.com/graphql  36a18fb963151642499d3d580f436bd8bcc1054210ccbd2f37e429ced0334766
direxion_getKurtosysFunds_SOXS.json         2026-11-21T06:40:13Z  https://p6vg2dlkpverjbr2afvpbqupky.appsync-api.us-east-1.amazonaws.com/graphql  31459eabea809b244756c96ce98dc711131f775ff054e01bf8027e56375cf457
