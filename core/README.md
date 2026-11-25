# core

ETF Pair Arbitrage 的交易进程（Go）：读 Bitget 上 SOXLUSDT / SOXSUSDT 两个永续合约的盘口，按锚点算价差、均线和 σ，非盘中两条腿同时偏离足够远时用 IOC 限价单同时开仓，回到平仓线时平仓，每个美股常规收盘前全部平掉。

- 规则在 [../PRD.md](../PRD.md)，设计在 [SPEC.md](SPEC.md)（模块、接口、表、实施步骤）。
- **现状：SPEC 第 9 节的第 0–6.4 步已实现并提交**（各步都经独立审查；第 6.5 步纸面联调进行中）。下面按模块说明：
  - 两条腿的盘口：每条腿 `cex.book_connections` 条 Bitget 公共连接（默认 3），各订阅深度（两条腿都是 `books5`）和 `books1`，按撮合序号合并，判冲突和有效期（`Books()` 一次读出两条腿，`DepthUsable`、`DepthOK`、`HasPrice` 都在这同一份上回答）；
  - 汇率 F（Coinbase，每 5 秒一次）、美股日历与 Session、与 Binance 时段表的每小时核对；
  - 锚点：铃锚点（Binance 指数 × F）→ NAV 稳定 1 小时后换成 NAV（Direxion），分红窗口与 A − Div，拆股条目的不开仓窗口；每个事实写一行 `anchor_fact`，重启时读回接着用；
  - 合约规则和结算资金费率（Bitget 公开接口）。账户设置、持仓、资金费流水的读取按 `Venue` 接口实现。
- **第 5.2 步 Bitget 的账户侧已实现**（`cex/bitget/rest.go`、`private.go`）：签名 REST（账户设置、持仓、按编号查订单、历史订单、成交、资金费流水、下单）和私有 WebSocket（登录，`order` / `fill` / `position` 三个频道，只留这一对合约和带本程序编号前缀的订单）。实盘模式下 `TradingReady` 还要求私有流在线。下单方法只在门控测试里对交易所调用过（下面的 `TestLiveOrderLatency`，第 54 条授权的 80 张成交不了的 IOC；`TestLiveWSOrderRoundTrip`，第 56 条的两次最小仓位开平）；只读的账户实测是门控的：`BITGET_ACCOUNT_READ_TEST=1 BITGET_ACCOUNT_ENV=<密钥文件> go test -run TestLiveBitgetAccountRead ./cex/bitget`（它的 HTTP 传输拒绝一切非 GET 请求）。
- **延迟设计 L1–L4 和 WebSocket 下单已实现**（`core/SPEC.md` 5.7；用户第 53 条）：
  - L1：每条腿开 `cex.book_connections` 条公共连接（默认 3，1–5），各订阅深度和 `books1`，都进同一本盘口，同一个序号先到的那一帧生效；一条腿的连接全断才算断。状态行里每条连接有三个数：帧数、“先到”的占比、帧延迟的中位数。
  - L2：Bitget 客户端用自己的 HTTP 连接池（空闲连接留 5 分钟，Go 默认 90 秒就关），实盘模式下 `Client.Run` 启动时先建连接、之后每 30 秒发一个公共的服务器时间请求，下单时连接是热的。
  - L3：私有连接和下单连接按 ping 选：拨号后每隔 1 秒发一个 ping、共 3 个（紧挨着发的 ping 在慢连接上也很快，预测不了之后的延迟），中位数高于 15 毫秒就再拨一条，最多 3 条，留最快的，所以建立这条连接要 3–9 秒；之后每次心跳都计时，状态行里有 `private_ping_ms`、`trade_ping_ms`、`rest_warm_ms`。
  - L4：每张单多记三个时刻，`cex_order` 新增三列：`accepted_ms`（交易所网关收到，交易所时钟）、`exchange_created_ms`（交易所建单）、`final_ms`（最终状态到达本机）。
  - WebSocket 下单（用户第 56 条：“下单改成WSS并进行验证”）：`cex.order_transport` **默认 `ws`**（`rest` 仍可选）——订单走一条只用来下单的已登录连接（`cex/bitget/trade.go`），每张单一帧、请求号就是订单编号；没有应答、连接断开、不带请求号的错误帧一律按“结果未知”处理，不重发；已按未知放弃等待的单迟到的应答（受理或拒绝）只打 Warn，不影响其余在途单；发单那一刻这条连接没连上，这张单改走 REST（打 Warn）。真实订单上量过：80 张成交不了的 IOC（`TestLiveOrderLatency`），WSS 发出到建单中位 4.5 毫秒、REST 13.7。只减仓的拼写也在真实订单上验证过：帧里照频道参数表写 `YES` / `NO`（REST 是 `yes` / `no`），两个合约各一次真实开平（`TestLiveWSOrderRoundTrip`，下面）都被交易所当作只减仓接受并平掉（`../experiments/core_step5/latency/WS_ROUND_TRIP_20261125.md`）。
- **第 4 步 `inventory` 已实现**：账本由一轮的订单快照整份重算（逐笔更新、只收到最终状态、重启重放得到同一个账本），额度 `L × (M + min(0, R))`、在途订单按额度价预留、零头只记录不处理、与交易所持仓的核对、资金费的归属与入账、改本金等旧的一轮结清才生效。
- **第 5.1 步 `cex` 的订单表和纸面交易所也已实现**：
  - 订单表（`cex/orders.go`）：`PlaceOrders` 先把 `cex_order` 行同步写库再发单（写不了时，含非只减仓单的一组整组不发，只减仓的照发）；回报、推送、查询按“累计成交量只增不减、最终状态之后只补成交量和手续费”合并；受理后 1 秒没有结果、超时或断线的单是“结果未知”，每秒按编号查询、不重发；交易所答“没有这张单”满 30 秒且成交记录里也没有，才标 `NotFound`；每次变化写 `cex_order`，成交照到达的写 `cex_fill`（重复的由表的主键 `exec_id` 去掉）；手续费只对报它时的那个成交量算“已知”，成交量变大而新的手续费还没到时回到“未知”继续查；拒单和因价格超限被撤的单在记录上带错误种类（`ErrorKind`，只在内存里，`cex_order` 没有这一列）；重启时读回的订单进表，没有最终状态的变成未知继续查；`RecoverOrders(cycleID)` 只在有未结束的一轮时调用，从交易所认回这一轮里本地没记下的单（两个合约都读到才开始认；别的轮次的单一律不动，它们在库里的行是完整的），`SetOrderLot` 把账本定下的仓位号存进 `cex_order.lot_id`。
  - 纸面交易所（`cex/paper.go`）：纸面模式（`exec.live: false`）下 `main` 用它包住 Bitget 客户端。行情、合约规则、结算费率照常读真实的公开接口；账户侧全在本地——订单过了这条腿的 `sim_order_latency` 后，在那一刻这条腿的盘口上按限价逐档成交（IOC，只减仓单不超过纸面持仓），手续费 = 成交额 × `taker_fee_bps`；纸面持仓、纸面资金费流水；每次撮合写一行 `decision`（`kind = paper_match`，带这一轮和仓位的编号、发单时和成交时的两份盘口；不进 `event`，那里只放报警）；两张单同时撮合时事件按撮合的先后逐张送出；纸面资金费流水在真实交易所公布结算费率之后入账，读取时不按时间过滤。**真实交易所收不到任何订单和账户请求。**
- **第 3 步 `strategy` 也已实现**：纯计算，不联网、不读时钟、不持有状态、不打日志。有效档位（`EffectiveLevels`）、价差（`Spread`）、按每条腿美元价存的均线和 σ（`ComputeStats`、`Stats.Mu` / `Sigma`，同一份数据可按任意一组锚点表达）、开仓的检查顺序与数量（`EvaluateEntry`，不开仓的原因是成组的 `Reason` 常量）、平仓线与数量（`EvaluateExit`）、成交不齐时补齐还是回吐（`EvaluateGap`）、强制平仓的限价（`ForcedOrders`）。测试用 PRD 附录 A 的四个算例和手算的数逐数核对。它现在还没有调用方：等 `inventory`（第 4 步）和 `exec`（第 6 步）。
- 进程能启动、连上行情、读出锚点、读回上次的账本和订单并恢复未结束的一轮，按 PRD 开仓、平仓、补齐 / 回吐、收盘前强制平仓，处理结果未知、持仓不符、停止信号和人工接管。纸面模式（`exec.live: false`）只撮合在本地；实盘模式（`exec.live: true`）还没有运行过，需要用户另行授权。

## 构建与检查

```bash
cd core
go build ./... && go vet ./... && go test -race ./...
```

数据库测试要一个临时库，没有设 `ETFPAIR_TRADE_TEST_DSN` 时跳过（输出里写明是跳过）：

```bash
scripts/dev_db.sh start      # 私有 PostgreSQL 18，127.0.0.1:5441，数据在 ../experiments/core_dev_db/
ETFPAIR_TRADE_TEST_DSN=postgres://etfpair_trade@127.0.0.1:5441/etfpair go test -race -count=1 ./...
scripts/dev_db.sh stop
```

这些测试会清空 `trade` 里除 `process_run` 以外的表，所以拒绝连 5432 端口（正式库）。其中 `TestStopSignalDuringStartupIsHonoured` 会把真实的二进制启动几百毫秒，它会去连 Bitget、Binance、Coinbase 的公开接口（只读，随即退出）。

连真实来源的只读测试各有一个环境变量开关，默认跳过；读到的内容存到 `../experiments/core_step2/`（不入库）：

```bash
BITGET_PUBLIC_TEST=1   go test -count=1 -v -run TestLiveBitgetPublic   ./cex/bitget/    # 公共流录 60 秒，两条腿都要有有效盘口；原始帧落盘
BINANCE_PUBLIC_TEST=1  go test -count=1 -v -run TestLiveBinancePublic  ./cex/binance/   # 上一次收盘前一分钟的指数 K 线、时段表与日历的核对
COINBASE_PUBLIC_TEST=1 go test -count=1 -v -run TestLiveCoinbasePublic ./cex/coinbase/  # ticker、分钟 K 线
DIREXION_READ_TEST=1 DIREXION_SITE_API_KEY=<密钥> go test -count=1 -v -run TestLiveDirexionRead ./cex/direxion/   # 要用户授权的密钥，各发一个 NAV 和分红请求
```

四个都只发公开或只读的请求，不带交易所密钥，不下单。

延迟的只读实测（结果在 `../experiments/core_step5/latency/`）：

```bash
BITGET_LATENCY_TEST=1 go test -count=1 -v -timeout 20m -run TestLiveBookConnections ./cex/bitget/   # 每条腿 3 条公共连接录 5 分钟，逐帧比较先后
BITGET_LATENCY_TEST=1 BITGET_ACCOUNT_ENV=<密钥文件> go test -count=1 -v -timeout 20m -run 'TestLiveRESTWarmth|TestLivePrivateConnectionChoice|TestLiveTradeConnection' ./cex/bitget/   # 空闲 120 秒后的第一个请求；私有连接按 ping 选与不选；下单连接只登录和 ping
```

这些只发 GET 和 WebSocket 的连接、登录、订阅、ping。下面两个会**下真实订单**，要同时给 `BITGET_ORDER_LATENCY_TEST=1` 和 `BITGET_ORDER_LATENCY_AUTHORISED=owner-authorised`，没有用户授权不要跑；两者共用这两个开关，所以 `-run` 要写全名，一次只跑一个：

```bash
# 两种通道各 N 张成交不了的 IOC，比较延迟（第 54 条）
BITGET_ORDER_LATENCY_TEST=1 BITGET_ORDER_LATENCY_AUTHORISED=owner-authorised BITGET_ACCOUNT_ENV=<密钥文件> BITGET_ORDER_LATENCY_SYMBOL=SOXLUSDT go test -count=1 -v -run '^TestLiveOrderLatency$' ./cex/bitget/
# WSS 下单的一次往返（第 56 条）：开一个最小仓位再平掉，验证只减仓单
BITGET_ORDER_LATENCY_TEST=1 BITGET_ORDER_LATENCY_AUTHORISED=owner-authorised BITGET_ACCOUNT_ENV=<密钥文件> BITGET_ORDER_LATENCY_SYMBOL=SOXLUSDT go test -count=1 -v -timeout 5m -run '^TestLiveWSOrderRoundTrip$' ./cex/bitget/
```

`TestLiveWSOrderRoundTrip` 在一个合约上（`SOXLUSDT` 默认，或 `SOXSUSDT`）：先确认这个合约 REST 持仓为空、账户是单向持仓、下单连接和私有流在线、买一卖一不到 2 秒，否则什么都不发；开仓是一张 WSS 的 IOC 限价买单，数量刚过最小名义（约 5.5 USDT，超过最小名义 3 倍就不发），价格为卖一上方 0.3%；平仓是一张 WSS 的只减仓 IOC 限价卖单（帧里写 `YES`），数量就是成交量，价格为买一下方 0.3%；交易所拒绝或没有平掉时记下它的答复（代码和信息），依次再试 REST 只减仓、REST 普通卖单（单向持仓下卖出同样数量即平多，只按 REST 读到的持仓和订单记账都还剩的数量发），每次用当时的买一重新定价，平掉为止；最后用 REST 读持仓，不为空就报错并写出剩下多少。一次最多 6 张、间隔 1 秒以上，订单编号以 `lat` 开头。原始记录存到 `../experiments/core_step5/latency/ws_round_trip_<合约>_<UTC 时间>.json`：每张单发出的帧或请求体（不含请求头、登录和签名）、下单连接收到的每一帧、这些订单的 `order` / `fill` 推送和这个合约的 `position` 推送、所有 REST 请求和应答（保温请求除外），都带本机时间；日志写出哪一种平仓成功、每张单发出 → 应答、发出 → 建单、发出 → 最终推送、成交价对发单时的最优价、交易所报的手续费对订单金额的 0.65 bps。`TestWSRoundTripRehearsal` 用同一份代码对着本地模拟的交易所把各种答复（`YES` 被拒、被接受但不当只减仓、回一个不带请求号的错误帧、全部只减仓都被拒、第一张只成交一半、开仓没成交、开始时不空仓等）都跑一遍。

表结构（`store/schema.sql`）里新加的表和列写在文件末尾，按需添加，进程启动时自动加上；改了已有的列（类型、约束）时，开发库要删掉重建，否则进程会把写不进去的行丢掉并打 Error。开发库里只有测试数据：

```bash
scripts/dev_db.sh stop && rm -rf ../experiments/core_dev_db && scripts/dev_db.sh start
```

## 配置

- `config.yaml`：公开参数，从 `config.example.yaml` 复制后修改；每个键的含义、单位和影响写在示例的注释里。`mean_model`、`mean_minutes`、`sigma_minutes`、`d_in`、`d_out`、`taker_fee_bps`、`capital`、`leverage`、`sim_order_latency` 没有默认值，必须写。未知的键会被拒绝。
- 密钥文件（JSON，形状见 `.env.example`），路径由 `-env` 给出。`store.dsn` 是写入角色 `etfpair_trade` 的连接串，必填；`direxion.site_api_key` 为空时只用铃锚点。密钥和连接串不进日志，也不进数据库。
  - 纸面模式（`exec.live: false`）：不给 `-env` 时读当前目录的 `.env`。`bitget` 一段不解析，可以没有，写成什么都不影响；纸面服务的文件建议只放 `store`（和可选的 `direxion`）。
  - 实盘（`exec.live: true`）：必须在命令行显式给 `-env`，指向服务的凭据文件，不会去读默认的 `.env`；`bitget` 的三项必须齐全。
- `config.yaml`、`.env`、`logs/` 和二进制不入库（`.gitignore`）。

检查一份配置（只加载和校验，打印一行后退出，不连数据库）：

```bash
go run . -check-config -config config.example.yaml -env <密钥文件>
```

## 运行（纸面）

```bash
go run . -config config.yaml -env .env
```

日志写到 `logs/etf_core_<启动时刻>.log` 和标准输出，时间是 UTC+8。`SIGINT` / `SIGTERM` 停止，`SIGUSR1` 停新仓；启动过程中收到的信号不会丢，进程启动完成后照常处理（停止信号：写下停止时刻后以退出码 0 退出）。数据库要事先建好库、两个角色和 schema `trade`（开发库由 `scripts/dev_db.sh` 建；正式库的建法在第 6.5 步，要用户授权），表由进程启动时自己建。

启动后日志里依次能看到：`cex: session`（当前 Session 的起止，UTC+8）、`cex: market rule`、两条腿的 `cex: book live`、`cex: bell anchor read` 和 `cex: anchor changed`，之后每 30 秒一行 `cex: status`（两条腿的深度年龄和冲突、F 及其年龄、锚点）。没配 `direxion.site_api_key` 时会有一条 Error `cex: issuer not readable`，这个 Session 一直用铃锚点。

真正的纸面交易和实盘（`exec.live: true`）都要等第 6 步的执行循环；实盘还要用户另行授权。

## 目录

| 路径 | 内容 |
|---|---|
| `main.go` | 启动流程：配置 → 日志 → store → 各 manager → 信号 |
| `config/` | 读 `config.yaml` 和密钥文件，各段校验 |
| `logger/` | `GlobalLogger`（slog，按 50 MB 换文件） |
| `store/` | 表结构 `schema.sql`、行类型、写入（`Record` / `WriteSync`）、读回（`LoadState`）、测试用的 `Fake` |
| `cex/` | 事实层：`types.go`（类型和 `Venue`、三个来源接口）、`manager.go`（装配、盘口循环、读接口）、`books.go`（每条腿的盘口合并）、`fx.go`、`calendar.go`、`anchors.go`、`reference.go`（规则、账户设置、持仓、资金费）、`orders.go`（订单表：发单、合并、结果未知的查询、重启认单）、`paper.go`（纸面交易所）、`store.go`（`anchor_fact`、`cex_order`、`cex_fill`、`event` 行）；`testdata/` 是从 11-24 的盘口探测录制里截的 2.5 秒原始帧 |
| `cex/bitget/` | Bitget：公共 WebSocket（拨号、每 5 秒 `ping`、读超时、退避重连）、帧解码、合约规则和结算资金费率的公开 REST；账户侧：签名 REST（`rest.go`）和私有 WebSocket（`private.go`）；按 ping 选连接（`ws.go`）、保温的 HTTP 连接和 `Run`（`client.go`）、WebSocket 下单连接（`trade.go`） |
| `cex/binance/`、`cex/coinbase/`、`cex/direxion/` | 三个只读来源：指数 K 线与时段表；USDT-USD 行情与分钟 K 线；NAV 与分红 |
| `strategy/` | 纯计算：`types.go`（参数、`Direction`、`Reason`、各输入和计划）、`levels.go`（有效档位、原始盘口上的逐档计算、限价）、`spread.go`、`stats.go`（均线和 σ）、`entry.go`、`exit.go`（按线平仓、强制平仓）、`gap.go`（补齐还是回吐） |
| `inventory/` | 库存：`ledger.go`（由订单重算的账本）、`quota.go`、`reconcile.go`、`manager.go`（恢复、资金费、关轮）、`store.go` |
| `exec/` | 执行循环：`manager.go`（Run、信号、状态行）、`step.go`（每一拍的判断顺序、开仓阻挡）、`bars.go`（分钟 bar、均线重新开始、换锚）、`orders.go`（开仓、按线平仓、强制平仓、结清、关轮）、`store.go`（落库行）。6.1 已实现；补齐 / 回吐（6.2）、结果未知与停止的其余部分（6.3）、重启恢复（6.4）待做 |
| `scripts/dev_db.sh` | 开发和测试用的私有数据库：`start` / `stop` / `status` |

## 纸面长跑（第 6.5 步）

- 正式库的角色和 schema：`core/scripts/install_trade_db.sh`（幂等；密码写进 `core/.env` 的 `store.dsn` 和 `core/.env.reader`，不打印）。
- 配置：`core/config.yaml`（不提交），`exec.live: false`。
- 启动（在仓库根目录）：`cd core && go build -buildvcs=false -o ../experiments/core_paper/etf_core . && cd ../experiments/core_paper && (setsid nohup ./etf_core -config ../../core/config.yaml -env ../../core/.env > stdout.log 2>&1 < /dev/null & echo $! > etf_core.pid)`。日志在 `experiments/core_paper/logs/etf_core_<UTC+8 启动时刻>.log`。
- 停止：`kill -TERM $(cat experiments/core_paper/etf_core.pid)`——停止信号会先减仓、结清再退出（SPEC 5.4）。
- 纸面模式只连公共行情，不连私有流和下单连接，交易所收不到任何订单。
