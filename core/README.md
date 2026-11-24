# core

ETF Pair Arbitrage 的交易进程（Go）：读 Bitget 上 SOXLUSDT / SOXSUSDT 两个永续合约的盘口，按锚点算价差、均线和 σ，非盘中两条腿同时偏离足够远时用 IOC 限价单同时开仓，回到平仓线时平仓，每个美股常规收盘前全部平掉。

- 规则在 [../PRD.md](../PRD.md)，设计在 [SPEC.md](SPEC.md)（模块、接口、表、实施步骤）。
- **现状：只有 SPEC 第 9 节的第 0、1 步**——脚手架（配置、日志、启动流程、各模块的空骨架）和 `store`（`trade` schema 的 13 张表、写入和读回）。进程现在能启动、写一行 `process_run`、等停止信号后退出；不连交易所，不读行情，不发任何订单。

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

这些测试会清空 `trade` 里除 `process_run` 以外的表，所以拒绝连 5432 端口（正式库）。

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

真正的纸面交易要等第 2–6 步实现；实盘（`exec.live: true`）要用户另行授权。

## 目录

| 路径 | 内容 |
|---|---|
| `main.go` | 启动流程：配置 → 日志 → store → 各 manager → 信号 |
| `config/` | 读 `config.yaml` 和密钥文件，各段校验 |
| `logger/` | `GlobalLogger`（slog，按 50 MB 换文件） |
| `store/` | 表结构 `schema.sql`、行类型、写入（`Record` / `WriteSync`）、读回（`LoadState`）、测试用的 `Fake` |
| `cex/`、`strategy/`、`inventory/`、`exec/` | 各模块的配置和空骨架，后续步骤在这里实现 |
| `scripts/dev_db.sh` | 开发和测试用的私有数据库：`start` / `stop` / `status` |
