# 鞋款 BOM 管理系统

面向鞋业制造的 Web BOM 管理系统。前端使用 Vue 3 与 Vxe-Table，后端使用 Go/Gin，数据直接读写本机 PostgreSQL。

Web 表格支持单元格/整行编辑、键盘导航、物料编号校验、分类树、排序与分类筛选、合计行、冻结表头/底部及左侧列、多选删除和自定义列。Vxe 工具栏提供本地 XLSX/CSV 导入导出与打印；导入会在前端校验后通过现有事务接口整体替换 BOM。

## 当前 PostgreSQL 数据结构

服务使用同一个 PostgreSQL 数据库中的现有 schema 和按 Style No. 命名的表，不会连接或迁移 MariaDB，也不会在启动时自动创建、清空或改造现有 schema。

| Schema | 每个 Style No. 的表 |
| --- | --- |
| `style` | 鞋款表头，例如 `style.on_27q3_094w` |
| `footer` | 签核信息，例如 `footer.on_27q3_094w` |
| `upper`、`stitching`、`accessories`、`insole`、`lining`、`outsole`、`packing`、`reinforcement`、`sock_liner` | 各物料分类，例如 `upper.on_27q3_094w` |

物料表的字段按现有数据库结构使用：`id`、`style_no`、`no`、`parts`、`material_description`、`unit`、`remark`、`supplier`、`color` 和 `process`。`color`、`process` 为 JSONB 数组；页面展示并保存 8 组配色/工艺值。签核表使用 `approved_by`、`checked_by`、`prepared_by` 和 `approval_date`。

服务启动时会检查所需 schema 与现有 Style No. 表；如果结构不完整会明确报错，不会自动迁移。创建新鞋款时，服务在单个事务中按同一 schema 的现有表复制表结构并创建该鞋款各分类表；所有表创建和数据写入成功后才提交。删除鞋款会在单个事务中删除对应的所有鞋款表。

## 配置

将 `.env.example` 复制为 `.env`，并在本机填写 PostgreSQL 密码：

```dotenv
PG_HOST=127.0.0.1
PG_PORT=5432
PG_USER=postgres
PG_PASSWORD=请在本机填写数据库密码
PG_DATABASE=db
PG_SSLMODE=disable
PORT=8080
```

数据库密码只保存在本机 `.env` 或环境变量中，不要提交到版本控制。也可以通过 `PG_PASSWORD` 或 `PGPASSWORD` 环境变量提供密码。

## 运行

```bash
go mod download
go run .
```

访问 `http://127.0.0.1:8080`。

修改前端依赖或构建入口后运行：

```bash
npm install
npm run build
```

前端构建需要 Node.js 20.19.0 或更高版本。

## 一致性与 API

所有写操作在 PostgreSQL `SERIALIZABLE` 事务中执行，服务会对序列化冲突和死锁进行有限重试；事务失败会回滚，不会返回成功响应。跨客户端并发修改无法自动合并时，接口返回冲突，前端显示错误并保留数据库中的已提交状态。读取表头与 BOM 矩阵使用一致性快照。

| 接口 | 方法 | 用途 |
| --- | --- | --- |
| `/api/bom/list?q=...` | GET | 按 Style No. 前缀检索现有鞋款 |
| `/api/bom/header?bom_id=...` | GET | 读取鞋款表头与签核信息 |
| `/api/bom/matrix?bom_id=...` | GET | 读取 9 个物料分类及 8 组 JSONB 配色/工艺值 |
| `/api/bom/create` | POST | 在事务中创建鞋款及其分类表 |
| `/api/bom/delete?bom_id=...` | POST | 在事务中删除鞋款及其分类表 |
| `/api/bom/update-header?bom_id=...` | POST | 保存表头或签核字段；修改 Style No. 时同步重命名各 schema 表 |
| `/api/bom/update-row?bom_id=...` | POST | 保存物料行字段 |
| `/api/bom/update-cell?bom_id=...` | POST | 保存指定配色/工艺 JSONB 数组元素 |
| `/api/bom/add-row?bom_id=...` | POST | 新增物料行 |
| `/api/bom/delete-row?bom_id=...` | POST | 删除物料行 |
| `/api/bom/import?bom_id=...` | POST | 事务内整体替换当前鞋款 BOM 数据 |

历史 MariaDB 配置、容器、迁移脚本及其数据不再由本项目使用。
