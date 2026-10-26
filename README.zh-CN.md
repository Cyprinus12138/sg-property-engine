# sg-property-engine

[English](README.md) · **简体中文**

[![CI](https://github.com/Cyprinus12138/sg-property-engine/actions/workflows/ci.yml/badge.svg)](https://github.com/Cyprinus12138/sg-property-engine/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/Cyprinus12138/sg-property-engine)](https://goreportcard.com/report/github.com/Cyprinus12138/sg-property-engine)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

定时采集新加坡住宅房产公开数据，存入 Postgres + PostGIS，并通过统一的 API 提供地理空间查询。

本项目从 [ProProp](https://proprop.algonavis.com/) 中独立出来。ProProp 是已投入生产使用的新加坡 AI 房产顾问，这个引擎通过每晚更新的 PostGIS 数据库，为 agent 的市场、楼盘和地理查询工具提供数据。

## 它能做什么

- **采集官方数据**：组屋转售交易和楼栋信息（data.gov.sg）；URA 私宅交易、租赁、待推出项目和开发商销售数据（URA Data Service）；地铁与轻轨出口、学校、巴士站和 OneMap 主题兴趣点（data.gov.sg、OneMap、LTA DataMall）；以及 CEA 房产经纪人登记和交易记录。
- **幂等采集**：交易记录和兴趣点（POI）记录通过 `source_hash` 去重；POI 同步会插入或更新记录，并删除上游已移除的记录。其他表使用自然键或在事务中替换快照。重复拉取不会产生重复记录。对于不支持按变更时间筛选的上游 API，引擎会重新获取可用时间范围内的数据。
- **补充派生数据**：根据 URA 交易记录及仅出现在待推出项目数据中的新盘，构建 `projects` 主表；按需通过 OneMap 进行地理编码，必要时使用 Google Places 作为备选，并为每个楼盘生成 `nearby_mrt`，包含最近车站的线路代码、距离和步行时间（分钟）。
- **通过 SQL 进行空间查询**：列表接口共用地理筛选条件，包括*某个参考位置附近*（对坐标点、楼盘、兴趣点或组屋楼栋使用 `ST_DWithin`）和 *N 米内有某类兴趣点*（空间 `EXISTS` 查询）。两者可以组合，例如一次请求即可查询“第 15 区内、距离地铁出口不超过 500 米的公寓”。
- **自动调度**：进程内置 cron 调度器，按数据源使用 Postgres 咨询锁（advisory lock），防止多个副本重复运行同一任务，也避免手动触发与定时任务重叠。`sync_runs` 记录运行状态，并限制进度写入频率；数据源已有任务运行时返回 `409`。
- **支持 MCP**：同一套查询也作为只读的模型上下文协议（Model Context Protocol）工具提供，可通过 HTTP `/mcp` 或 stdio `sgpe mcp` 访问，让 AI 助手直接查询数据库，回答“碧山地铁站附近四房式组屋的转售价格”等问题。
- **内置运维页面**：同一可执行文件内的 `/ui/` 页面显示各数据源的状态和进度，支持发起增量同步或历史数据回填、查看运行历史，以及预览数据表。页面由三个内嵌静态文件组成，无需 Node 工具链。

## 快速开始

同一条启动命令支持三种运行方式，均会等待 `/healthz` 返回响应。

```bash
scripts/stack.sh up                 # docker compose: PostGIS + engine image (default)
scripts/stack.sh up docker          # plain `docker run`, no compose needed
scripts/stack.sh up local           # Go binary on the host (Go 1.25+)
```

`make up` / `make up MODE=local` / `make dev`（本地前台运行）均调用同一个脚本。`scripts/stack.sh status`、`logs` 和 `down` 适用于所有运行方式；`down --purge` 还会删除数据。

数据库初始为空，默认不会在启动时触发历史数据回填，但定时增量同步仍会运行。可以手动发起首次组屋数据回填并查看进度：

```bash
scripts/stack.sh up --no-build --backfill hdb      # or: curl -X POST 'http://localhost:8081/sync/run?source=hdb&mode=backfill'
curl 'http://localhost:8081/sync/status'
```

也可以打开 <http://localhost:8081/ui/>（`/` 会重定向到此处），查看进度、发起任务或浏览数据表。

然后查询：

```bash
# HDB resale within 800 m of Bishan MRT, nearest first
curl 'http://localhost:8081/hdb/transactions?near_poi_type=mrt_exit&near_poi_name=BISHAN&radius_m=800&sort=distance_asc'

# condos in D15 that have an MRT exit within 500 m
curl 'http://localhost:8081/projects?district=15&property_type=Condominium&has_poi_type=mrt_exit&has_poi_within_m=500'

# median PSF by town, last 12 months
curl 'http://localhost:8081/hdb/stats?group_by=town&metric=median_psf&min_month=2025-10-01'

# nearest stations to an arbitrary point
curl 'http://localhost:8081/geo/nearest-stations?lat=1.3521&lng=103.8198'
```

也可以直接运行命令行程序：

```bash
go run ./cmd/sgpe serve                                   # no DB configured → starts a local PostGIS via Docker
go run ./cmd/sgpe sync --source hdb --mode backfill       # one job in the foreground, then exit
go run ./cmd/sgpe --config prod.yaml migrate
go run ./cmd/sgpe config                                  # print the effective config, secrets masked
```

每日增量同步由调度器自动执行，时间安排见 [docs/DATA_ENGINE.md](docs/DATA_ENGINE.md) 中的 cron 表。如需在新部署启动时回填所有数据源，可设置 `server.bootstrap_on_boot: true`。

## 引擎运维

`sgpe serve` 在 `/ui/` 提供运维页面。下图展示了本地数据库同步组屋、地铁和兴趣点数据后的页面：

<table>
<tr><td width="50%" valign="top"><a href="docs/images/ops-jobs.png"><img src="docs/images/ops-jobs.png" width="430" alt="Jobs：每个数据源一行，可手动触发"></a><br><sub><b>Jobs。</b>每个数据源的状态、阶段和最近一次运行，可触发增量同步或历史数据回填。</sub></td><td width="50%" valign="top"><a href="docs/images/ops-tables.png"><img src="docs/images/ops-tables.png" width="430" alt="Tables：数据表及原始记录预览"></a><br><sub><b>Tables。</b>每张表的行数、大小和最近写入时间，以及原始记录预览。</sub></td></tr>
</table>

本项目的原始数据引擎运行于 ProProp。以下是其生产环境运维后台及数据规模：

<table>
<tr><td width="33%" valign="top"><a href="docs/images/proprop-jobs.png"><img src="docs/images/proprop-jobs.png" width="270" alt="ProProp 后台的 Jobs"></a><br><sub><b>Jobs</b>，每个同步数据源一行。</sub></td><td width="33%" valign="top"><a href="docs/images/proprop-runs.png"><img src="docs/images/proprop-runs.png" width="270" alt="运行历史"></a><br><sub><b>运行历史</b>，含模式、耗时和行数。</sub></td><td width="33%" valign="top"><a href="docs/images/proprop-tables.png"><img src="docs/images/proprop-tables.png" width="270" alt="生产规模的 Tables"></a><br><sub><b>Tables：</b>24.3 万条组屋转售记录、45.6 万条 URA 租赁记录、120 万条 CEA 交易记录。</sub></td></tr>
</table>

## 配置

配置按以下三层加载，后加载的配置覆盖前面的值：

1. 内置默认值。
2. YAML 文件：依次查找 `--config FILE` 指定的文件、`$SGPE_CONFIG` 指定的文件，以及当前目录下的 `./sgpe.yaml`（如存在）。可复制 [sgpe.example.yaml](sgpe.example.yaml) 作为起点，其中列出了所有配置项、默认值及对应的环境变量。未知配置项会导致启动报错，避免拼写错误导致某个数据源被意外禁用。
3. 环境变量，包括 `.env` 文件中的变量（参见 [.env.example](.env.example)）。空值视为未设置。

**未配置数据库时**，`serve`、`sync` 和 `migrate` 会通过 Docker CLI 启动或复用本地 PostGIS 容器，并连接该数据库。容器名为 `sgpe-postgis`，绑定 `127.0.0.1:55432`，数据保存在 `sgpe-pgdata` 卷中，回填的数据在重启后仍会保留。此功能用于方便本地开发；正式部署应设置 `database.url`（或 `SGPE_DB_URL`）。设置 `database.auto_docker.enabled: false` 后，缺少数据库 URL 将直接报错。如果已存在并非由 sgpe 创建的同名容器，程序会报告该情况，不会修改该容器。

各运行方式如何使用配置：

| 方式 | 配置文件 | 数据库 |
|---|---|---|
| `local` | 上述完整分层 | `database.url`，否则自动启动的容器 |
| `docker` | `./sgpe.yaml` 或 `--config`，只读挂载 | `SGPE_DB_URL`，否则与 `local` 相同的 `sgpe-postgis` 容器 |
| `compose` | 与 `docker` 模式相同的挂载方式 | `SGPE_DB_URL`，否则自带的 `postgres` 服务 |

使用容器运行时，脚本会以当前用户身份启动引擎，因此仍可读取权限为 `0600` 的 `sgpe.yaml`。容器内监听端口固定，可通过 `--port` 指定映射到宿主机的端口。

### 凭据

上游服务凭据均为可选项；缺少凭据时，对应数据源会被禁用，并在启动日志中说明。凭据可写入 `.env` 或 YAML 文件的 `credentials:` 配置下。注册方式见 [.env.example](.env.example)。

| 变量 | YAML 键 | 启用 |
|---|---|---|
| `URA_ACCESS_KEY` | `ura_access_key` | `ura_txn`、`ura_rental`、`ura_pipeline`、`ura_developer_sales` |
| `ONEMAP_EMAIL` / `ONEMAP_PASSWORD` | `onemap_email` / `onemap_password` | OneMap 主题兴趣点（幼儿园、小贩中心、公园、医院）。不配置也能做地理编码搜索。 |
| `LTA_ACCOUNT_KEY` | `lta_account_key` | 巴士站 |
| `GOOGLE_MAPS_API_KEY` | `google_maps_api_key` | OneMap 查不到时的备用地理编码查询 |

本仓库不附带或再分发数据；引擎使用你注册的账号从各发布方获取数据。部署前请阅读 [docs/DATA_TERMS.md](docs/DATA_TERMS.md)。

## API

所有路由均未设置身份验证，包括 `POST /sync/run` 和运维页面。请仅在私有网络中开放端口，或通过提供身份验证的代理访问。Compose 文件和启动脚本默认将端口绑定到所有网络接口。

服务使用同一监听端口，以 JSON 接收和返回数据。列表接口返回 `{rows, total, limit, offset}`；统计接口返回 `{buckets: [{key, value, count}]}`。

| 方法 | 路径 | 用途 |
|---|---|---|
| GET | `/healthz` | 检查数据库连接 |
| GET | `/sync/status` | 每个数据源最近一次运行，以及已知数据源 |
| POST | `/sync/run?source=&mode=incremental\|backfill[&arg=]` | 触发任务（返回 202 和 `run_id`；正在运行则 409） |
| GET | `/sync/runs[?source=]` | 运行历史，新的在前（分页） |
| GET | `/sync/runs/{id}` | 单条 `sync_runs` 记录，含阶段和进度 |
| GET | `/db/tables` | 每张表的行数、磁盘大小和最近写入时间 |
| GET | `/db/tables/{table}/rows` | 指定表的原始记录，每页 100 行（最多 200）；地理字段为 WKT，bytea 为十六进制 |
| GET | `/ui/` | 运维页面（内嵌静态文件） |
| POST | `/mcp` | 基于 Streamable HTTP 的 MCP；见 [MCP](#mcp) |
| GET | `/hdb/transactions` | 筛选条件：town、flat_type、block、street、min/max_month、min/max_price、min/max_area、storey、sort 以及地理筛选 |
| GET | `/hdb/stats` | `group_by=month\|town\|flat_type`，`metric=median_psf\|median_price\|avg_price\|count`，支持相同的筛选条件 |
| GET | `/ura/transactions` | 筛选条件：project、district、property_type、tenure、market_segment、min/max_date、min/max_price、min/max_psf 以及地理筛选 |
| GET | `/ura/rentals` | 筛选条件：project、district、property_type、min/max_date 以及地理筛选 |
| GET | `/ura/rentals/trend` | 每月租金的 P25 / 中位数 / P75；筛选条件：district、property_type、bedroom_num、min/max_date |
| GET | `/ura/pipeline` | 待推出和在建的楼盘；筛选条件：district、status |
| GET | `/ura/developer-sales` | 每月开发商销售快照；筛选条件：project、district、market_segment、developer、min/max_period |
| GET | `/projects` | 筛选条件：name（前缀）、district、property_type 以及地理筛选 |
| GET | `/projects/{name}` | 单个楼盘（不区分大小写和标点），含 `nearby_mrt` |
| GET | `/pois` | 筛选条件：type、subtype、name（前缀）以及参考位置附近的地理筛选 |
| GET | `/pois/types` | 已同步的兴趣点类型及数量 |
| GET | `/geo/nearest-stations?lat=&lng=[&limit=][&within_m=]` | 某个点最近的地铁和轻轨站 |
| GET | `/search/properties?q=` | 在楼盘和组屋楼栋名称中进行自动补全 |
| GET | `/explore/hottest` | 按近 90 天 URA 成交量排名的楼盘，含 PSF 走势 |
| GET | `/explore/district_median?district=` | 某个区的价格 / PSF 中位数，可选面积区间 |
| GET | `/cea/lookup?registration_no=` | CEA 经纪人登记信息查询 |
| GET | `/cea/practice-stats?reg_no=&mode=sale\|rent` | 指定经纪人近 12 个月的业务汇总 |

**地理筛选**（适用于返回记录含位置信息的列表接口）：

| 参数 | 含义 |
|---|---|
| `near_lat` + `near_lng` | 以原始坐标为参考位置 |
| `near_project` | 以楼盘中心点为参考位置 |
| `near_poi_type` + `near_poi_name` | 以指定名称的兴趣点为参考位置，例如 `mrt_exit` + `BISHAN` |
| `near_block` + `near_street` | 以组屋楼栋为参考位置 |
| `radius_m` | 默认 1000，最大 5000；结果行增加 `distance_m`；`sort=distance_asc` |
| `has_poi_type` + `has_poi_within_m` [+ `has_poi_subtype`] | 只返回指定范围内有匹配兴趣点的记录（默认 500 米） |

URA 发布的名称使用全大写字母。响应保留原始 `name` 作为查询键，同时提供各单词首字母大写的 `display_name` 用于展示。

## MCP

`sgpe serve` 同时在 `/mcp` 上通过 Streamable HTTP 将查询 API 暴露为 MCP 工具。对于通过启动命令连接的客户端，`sgpe mcp` 可通过 stdio 提供相同工具；该命令无需直接连接数据库，只需访问正在运行的引擎。

```bash
# Claude Code, over HTTP
claude mcp add --transport http sgpe http://localhost:8081/mcp

# Any stdio client: local binary, or inside the docker-mode container
sgpe mcp --url http://localhost:8081
docker exec -i sgpe-engine /sgpe mcp
```

使用 JSON 配置的客户端可参考：

```json
{ "mcpServers": { "sgpe": { "command": "/path/to/sgpe", "args": ["mcp", "--url", "http://localhost:8081"] } } }
```

| 工具 | 查询内容 |
|---|---|
| `search_properties` | 根据部分名称查找对应楼盘或组屋楼栋 |
| `get_project`、`find_projects` | 楼盘信息和最近车站；按区、类型、位置查询楼盘 |
| `hdb_resale_transactions`、`hdb_resale_stats` | 组屋转售记录；按月、镇或房型统计的每平方英尺价格（PSF）中位数、价格中位数和成交量 |
| `private_transactions`、`private_rentals`、`private_rental_trend` | URA 买卖和租赁；每月租金分位数 |
| `development_pipeline`、`developer_sales` | 待推出楼盘；每月新盘销售 |
| `nearest_mrt`、`find_pois`、`poi_types` | 某地附近的车站和其他兴趣点 |
| `hottest_projects`、`district_median` | 90 天成交量排名；各区的价格和 PSF 中位数 |
| `cea_salesperson`、`cea_practice_stats` | CEA 登记查询；指定经纪人近 12 个月的业务情况 |
| `data_freshness` | 每个数据源最近一次同步的时间，避免将“未查到记录”误解为“没有此类数据” |

MCP 工具仅提供只读查询；同步任务通过 API 或运维页面发起。为方便模型调用，工具具备以下行为：

- **由服务器解析地点。** `near` 接受楼盘名称、地铁站（`poi_type: mrt_exit`）、组屋楼栋与街道，或坐标。服务器将其解析为坐标，并在结果中返回解析后的 `anchor`。如果名称无法匹配，会返回错误，不会退化为全岛查询。
- **在代码中规范化输入。** “Bishan”、“4-room”、“D9”、“condo”和“2024-03”会分别转换为存储格式（`BISHAN`、`4 ROOM`、`09`、`Condominium`、`2024-03-01`）。超出范围的区号、未知市场分区或新加坡以外的坐标会被拒绝，并返回具体原因。
- **限制单页结果数量。** 默认每页 20 条，最多 100 条，并提供 `total` 供客户端分页。

`/mcp` 拒绝跨源浏览器请求；对于回环连接，还会拒绝 `Host` 不是 localhost 的请求，以防止 DNS 重绑定攻击。与其他 API 一样，该端点未设置身份验证，详见 [API](#api) 开头的说明。

### Go 客户端

```go
import "github.com/Cyprinus12138/sg-property-engine/client"

c := client.New("http://localhost:8081")
page, err := c.HDBTransactions(ctx, url.Values{"town": {"BISHAN"}, "limit": {"20"}})
proj, err := c.Project(ctx, "The Sail @ Marina Bay")   // proj.NearbyMRT …
```

响应在进程内缓存五分钟（可配置）；404 映射为 `client.ErrNotFound`。

## 架构

```
  data.gov.sg ──┐  ingest/hdb, cea, cea_transactions, pois(schools, mrt exits)
  URA API     ──┤  ingest/ura_txn, ura_rental, ura_pipeline, ura_developer_sales
  OneMap      ──┤  ingest/geo (+ resolver), pois (themes)          scheduler
  LTA DataMall──┤  ingest/pois (bus stops)                      ┌──(cron + advisory locks)
  Google Maps ──┘  ingest/resolver (fallback only)              │
                          │                                     │
                          ▼                                     ▼
                   store/ (pgx)  ───────────────────────►  Postgres + PostGIS
                                                                 ▲
                   httpapi/ (query + ops)  ◄── client/ ──── your service
```

设计说明、表结构和每个数据源的增量策略见 [docs/DATA_ENGINE.md](docs/DATA_ENGINE.md)（英文）。

## 开发

```bash
make test        # unit tests (race detector), no services needed
make smoke       # end-to-end over a throwaway PostGIS container
make vet
```

`make smoke` 会自动启动临时 PostGIS 容器、设置 `TEST_DATABASE_URL`，运行 `-tags=smoke` 测试集，并在测试结束后删除容器，无需手动设置该变量。如果希望使用已有的 PostGIS 实例，可自行设置 `TEST_DATABASE_URL` 并直接运行测试：

```bash
TEST_DATABASE_URL=postgres://postgres:test@localhost:55434/postgres?sslmode=disable \
  go test -tags=smoke -count=1 -p 1 ./...
```

冒烟测试会为每个测试用例克隆已完成迁移的模板数据库，使测试彼此隔离，并加快首次运行后的执行速度。上游客户端通过 `sources/sourcestest` 将请求重定向至 `httptest` 测试服务器，无需访问外部网络。数据库结构变更统一通过 `migrations/` 下的迁移文件完成，不在启动代码中直接执行 `ALTER`。

## 许可

[MIT](LICENSE)。Copyright 2026 Mr.Cyprinus (https://github.com/Cyprinus12138)。

此许可证仅适用于代码。引擎获取的数据仍受各发布方的条款约束，详见 [docs/DATA_TERMS.md](docs/DATA_TERMS.md)。