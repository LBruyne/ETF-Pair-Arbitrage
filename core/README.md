# core

ETF Pair Arbitrage 的交易进程（Go）：读 Bitget 上 SOXLUSDT / SOXSUSDT 两个永续合约的盘口，按锚点算价差、均线和 σ，非盘中两条腿同时偏离足够远时用 IOC 限价单同时开仓，回到平仓线时平仓，每个美股常规收盘前全部平掉。

- 规则在 [../PRD.md](../PRD.md)，设计在 [SPEC.md](SPEC.md)（模块、接口、表、实施步骤）。
- **现状：SPEC 第 9 节的第 0、1、2 步**——脚手架、`store`，以及 `cex` 的行情与参考数据：
  - 两条腿的盘口：每条腿一条 Bitget 公共连接，订阅深度（`books5` / `books50`）和 `books1`，按撮合序号合并，判冲突和有效期（`Book`、`DepthUsable`、`HasPrice`）；
  - 汇率 F（Coinbase，每 5 秒一次）、美股日历与 Session、与 Binance 时段表的每小时核对；
  - 锚点：铃锚点（Binance 指数 × F）→ NAV 稳定 1 小时后换成 NAV（Direxion），分红窗口与 A − Div，拆股条目的不开仓窗口；每个事实写一行 `anchor_fact`，重启时读回接着用；
  - 合约规则和结算资金费率（Bitget 公开接口）。账户设置、持仓、资金费流水的读取已按 `Venue` 接口写好并用假实现测过，但 Bitget 的账户侧（签名 REST、私有流、下单）要到第 5.2 步才有：现在这三项读取会各打一条 Warn 然后每 5 秒静默重试。
- **第 3 步 `strategy` 也已实现**：纯计算，不联网、不读时钟、不持有状态、不打日志。有效档位（`EffectiveLevels`）、价差（`Spread`）、按每条腿美元价存的均线和 σ（`ComputeStats`、`Stats.Mu` / `Sigma`，同一份数据可按任意一组锚点表达）、开仓的检查顺序与数量（`EvaluateEntry`，不开仓的原因是成组的 `Reason` 常量）、平仓线与数量（`EvaluateExit`）、成交不齐时补齐还是回吐（`EvaluateGap`）、强制平仓的限价（`ForcedOrders`）。测试用 PRD 附录 A 的四个算例和手算的数逐数核对。它现在还没有调用方：等 `inventory`（第 4 步）和 `exec`（第 6 步）。
- 进程现在能启动、连上行情、读出当前 Session 的铃锚点、每 30 秒打一行 `cex: status`、收到停止信号后退出。**没有库存和下单，策略的计算还没有接进循环**（第 4–6 步），任何模式下都不发订单。

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

表结构（`store/schema.sql`）改了之后，开发库要删掉重建：已有的表不会自动多出新列，进程会把写不进去的行丢掉并打 Error。开发库里只有测试数据：

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

真正的纸面交易要等第 3–6 步实现；实盘（`exec.live: true`）要用户另行授权。

## 目录

| 路径 | 内容 |
|---|---|
| `main.go` | 启动流程：配置 → 日志 → store → 各 manager → 信号 |
| `config/` | 读 `config.yaml` 和密钥文件，各段校验 |
| `logger/` | `GlobalLogger`（slog，按 50 MB 换文件） |
| `store/` | 表结构 `schema.sql`、行类型、写入（`Record` / `WriteSync`）、读回（`LoadState`）、测试用的 `Fake` |
| `cex/` | 事实层：`types.go`（类型和 `Venue`、三个来源接口）、`manager.go`（装配、盘口循环、读接口）、`books.go`（每条腿的盘口合并）、`fx.go`、`calendar.go`、`anchors.go`、`reference.go`（规则、账户设置、持仓、资金费）、`store.go`（`anchor_fact`、`event` 行）；`testdata/` 是从 11-24 的盘口探测录制里截的 2.5 秒原始帧 |
| `cex/bitget/` | Bitget：公共 WebSocket（拨号、每 5 秒 `ping`、读超时、退避重连）、帧解码、合约规则和结算资金费率的公开 REST；账户侧的方法现在返回“未实现” |
| `cex/binance/`、`cex/coinbase/`、`cex/direxion/` | 三个只读来源：指数 K 线与时段表；USDT-USD 行情与分钟 K 线；NAV 与分红 |
| `strategy/` | 纯计算：`types.go`（参数、`Direction`、`Reason`、各输入和计划）、`levels.go`（有效档位、原始盘口上的逐档计算、限价）、`spread.go`、`stats.go`（均线和 σ）、`entry.go`、`exit.go`（按线平仓、强制平仓）、`gap.go`（补齐还是回吐） |
| `inventory/`、`exec/` | 各模块的配置和空骨架，后续步骤在这里实现 |
| `scripts/dev_db.sh` | 开发和测试用的私有数据库：`start` / `stop` / `status` |
