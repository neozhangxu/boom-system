package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/joho/godotenv"
)

const (
	defaultDatabase  = "db"
	colorwayCount    = 8
	maxImportRows    = 5000
	maxCustomColumns = 100
)

var (
	styleNumberPattern    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,63}$`)
	tableNamePattern      = regexp.MustCompile(`^[a-z0-9_-]{1,63}$`)
	customColumnIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	builtInColumnPattern  = regexp.MustCompile(`^(color|process)_[0-7]$`)
	categories            = []string{
		"upper",
		"stitching",
		"accessories",
		"insole",
		"lining",
		"outsole",
		"packing",
		"reinforcement",
		"sock_liner",
	}
	schemas     = append([]string{"style", "footer"}, categories...)
	dropSchemas = append(append([]string(nil), categories...), "footer", "style")
)

type dbQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type bomSummary struct {
	ID        string `json:"id"`
	StyleNo   string `json:"style_no"`
	StyleName string `json:"style_name"`
}

type styleHeader struct {
	StyleNo       string `json:"style_no"`
	StyleName     string `json:"style_name"`
	SampleOrderNo string `json:"sample_order_no"`
	SampleType    string `json:"sample_type"`
	Season        string `json:"season"`
	Gender        string `json:"gender"`
	LastNo        string `json:"last_no"`
	BottomNo      string `json:"bottom_no"`
	ShoeFillerNo  string `json:"shoe_filler_no"`
	ShoeType      string `json:"shoe_type"`
	CreateDate    string `json:"create_date"`
	ApprovedBy    string `json:"approved_by"`
	CheckedBy     string `json:"checked_by"`
	PreparedBy    string `json:"prepared_by"`
	ApprovalDate  string `json:"approval_date"`
}

type bomItem struct {
	ItemID       int               `json:"item_id"`
	Category     string            `json:"category"`
	ParentID     *int              `json:"parent_id"`
	SortOrder    int               `json:"sort_order"`
	No           *int              `json:"no"`
	Parts        string            `json:"parts"`
	Description  string            `json:"material_description"`
	Unit         string            `json:"unit"`
	Remark       string            `json:"remark"`
	Supplier     string            `json:"supplier"`
	Color        []string          `json:"color"`
	Process      []string          `json:"process"`
	CustomFields map[string]string `json:"custom_fields"`
}

type customColumn struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Position int    `json:"position"`
}

type importedItem struct {
	Category     string            `json:"category"`
	ParentRow    *int              `json:"parent_row"`
	No           *int              `json:"no"`
	Parts        string            `json:"parts"`
	Description  string            `json:"material_description"`
	Unit         string            `json:"unit"`
	Remark       string            `json:"remark"`
	Supplier     string            `json:"supplier"`
	Color        []string          `json:"color"`
	Process      []string          `json:"process"`
	CustomFields map[string]string `json:"custom_fields"`
}

type rowInsertItem struct {
	ParentIndex  *int              `json:"parent_index"`
	No           *int              `json:"no"`
	Parts        string            `json:"parts"`
	Description  string            `json:"material_description"`
	Unit         string            `json:"unit"`
	Remark       string            `json:"remark"`
	Supplier     string            `json:"supplier"`
	Color        []string          `json:"color"`
	Process      []string          `json:"process"`
	CustomFields map[string]string `json:"custom_fields"`
}

func quoteIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func qualifiedTable(schema, table string) string {
	return quoteIdentifier(schema) + "." + quoteIdentifier(table)
}

func tableForStyle(styleNo string) (string, error) {
	styleNo = strings.TrimSpace(styleNo)
	if !styleNumberPattern.MatchString(styleNo) {
		return "", fmt.Errorf("Style No. 必须为 1–63 位英文字母、数字、下划线或连字符")
	}
	return strings.ToLower(styleNo), nil
}

func validateCategory(category string) bool {
	for _, allowed := range categories {
		if allowed == category {
			return true
		}
	}
	return false
}

func validateCustomColumns(columns []customColumn) error {
	seen := make(map[string]bool, len(columns))
	positions := make(map[int]bool, len(columns))
	for _, column := range columns {
		if !customColumnIDPattern.MatchString(column.ID) {
			return errors.New("自定义列编号无效")
		}
		title := strings.TrimSpace(column.Title)
		if title == "" || len([]rune(title)) > 80 {
			return errors.New("自定义列标题不能为空且最多 80 个字符")
		}
		if column.Position < 7 {
			return errors.New("自定义列不能放置在固定基础列之前")
		}
		if seen[column.ID] {
			return errors.New("自定义列编号重复")
		}
		if positions[column.Position] {
			return errors.New("自定义列位置重复")
		}
		seen[column.ID] = true
		positions[column.Position] = true
	}
	if len(columns) > maxCustomColumns {
		return fmt.Errorf("自定义列数量不能超过 %d", maxCustomColumns)
	}
	return nil
}

func readCustomColumns(ctx context.Context, queryer dbQueryer, table string) ([]customColumn, error) {
	var raw []byte
	if err := queryer.QueryRowContext(ctx,
		"SELECT COALESCE(custom_columns, '[]'::jsonb) FROM "+qualifiedTable("style", table)+" LIMIT 1",
	).Scan(&raw); err != nil {
		return nil, fmt.Errorf("读取自定义列失败: %w", err)
	}
	var columns []customColumn
	if err := json.Unmarshal(raw, &columns); err != nil {
		return nil, fmt.Errorf("解析自定义列失败: %w", err)
	}
	if columns == nil {
		columns = []customColumn{}
	}
	if err := validateCustomColumns(columns); err != nil {
		return nil, fmt.Errorf("数据库自定义列结构无效: %w", err)
	}
	return columns, nil
}

func readHiddenColumns(ctx context.Context, queryer dbQueryer, table string) ([]string, error) {
	var raw []byte
	if err := queryer.QueryRowContext(ctx,
		"SELECT COALESCE(hidden_columns, '[]'::jsonb) FROM "+qualifiedTable("style", table)+" LIMIT 1",
	).Scan(&raw); err != nil {
		return nil, fmt.Errorf("读取隐藏列失败: %w", err)
	}
	var columns []string
	if err := json.Unmarshal(raw, &columns); err != nil {
		return nil, fmt.Errorf("解析隐藏列失败: %w", err)
	}
	seen := make(map[string]bool, len(columns))
	for _, column := range columns {
		if !builtInColumnPattern.MatchString(column) || seen[column] {
			return nil, errors.New("数据库隐藏列结构无效")
		}
		seen[column] = true
	}
	return columns, nil
}

func writeHiddenColumns(ctx context.Context, tx *sql.Tx, table, styleNo string, columns []string) error {
	encoded, err := json.Marshal(columns)
	if err != nil {
		return fmt.Errorf("编码隐藏列失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE "+qualifiedTable("style", table)+" SET hidden_columns = $1::jsonb WHERE style_no = $2",
		string(encoded), styleNo); err != nil {
		return fmt.Errorf("保存隐藏列失败: %w", err)
	}
	return nil
}

func builtInColumnPosition(field string, hidden []string) (int, bool) {
	position := 7
	for index := 0; index < colorwayCount; index++ {
		for _, candidate := range []string{fmt.Sprintf("color_%d", index), fmt.Sprintf("process_%d", index)} {
			if slices.Contains(hidden, candidate) {
				continue
			}
			if candidate == field {
				return position, true
			}
			position++
		}
	}
	return 0, false
}

func writeCustomColumns(ctx context.Context, tx *sql.Tx, table, styleNo string, columns []customColumn) error {
	if err := validateCustomColumns(columns); err != nil {
		return err
	}
	encoded, err := json.Marshal(columns)
	if err != nil {
		return fmt.Errorf("编码自定义列失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE "+qualifiedTable("style", table)+" SET custom_columns = $1::jsonb WHERE style_no = $2",
		string(encoded), styleNo); err != nil {
		return fmt.Errorf("保存自定义列失败: %w", err)
	}
	return nil
}

func requireStyleTable(ctx context.Context, queryer dbQueryer, styleNo string) (string, error) {
	table, err := tableForStyle(styleNo)
	if err != nil {
		return "", err
	}
	var exists bool
	err = queryer.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.tables
			WHERE table_schema = 'style' AND table_name = $1 AND table_type = 'BASE TABLE'
		)
	`, table).Scan(&exists)
	if err != nil {
		return "", fmt.Errorf("检查 Style No. 数据表失败: %w", err)
	}
	if !exists {
		return "", sql.ErrNoRows
	}
	var storedStyleNo string
	if err := queryer.QueryRowContext(ctx,
		"SELECT style_no FROM "+qualifiedTable("style", table)+" LIMIT 1",
	).Scan(&storedStyleNo); err != nil {
		return "", err
	}
	if !strings.EqualFold(storedStyleNo, styleNo) {
		return "", fmt.Errorf("Style No. 与现有数据表名称不匹配")
	}
	return table, nil
}

func relationExists(ctx context.Context, queryer dbQueryer, schema, table string) (bool, error) {
	var exists bool
	err := queryer.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.tables
			WHERE table_schema = $1 AND table_name = $2 AND table_type = 'BASE TABLE'
		)
	`, schema, table).Scan(&exists)
	return exists, err
}

func decodeStringArray(raw []byte, expectedLength int) ([]string, error) {
	values := make([]string, expectedLength)
	if len(raw) == 0 || string(raw) == "null" {
		return values, nil
	}
	var stored []string
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, fmt.Errorf("读取 JSONB 配色/工艺数组失败: %w", err)
	}
	copy(values, stored)
	return values, nil
}

func encodeStringArray(values []string) ([]byte, error) {
	if len(values) < colorwayCount {
		values = append(values, make([]string, colorwayCount-len(values))...)
	}
	if len(values) > colorwayCount {
		values = values[:colorwayCount]
	}
	return json.Marshal(values)
}

func retryableTransactionError(err error) bool {
	var stateError interface{ SQLState() string }
	if errors.As(err, &stateError) {
		return stateError.SQLState() == "40001" || stateError.SQLState() == "40P01"
	}
	return false
}

func withSerializableTransaction(ctx context.Context, db *sql.DB, operation func(*sql.Tx) error) error {
	const maxAttempts = 4
	for attempt := 0; attempt < maxAttempts; attempt++ {
		tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
		if err != nil {
			return fmt.Errorf("开启 PostgreSQL 串行化事务失败: %w", err)
		}
		if err = operation(tx); err != nil {
			_ = tx.Rollback()
		} else {
			err = tx.Commit()
		}
		if !retryableTransactionError(err) || attempt == maxAttempts-1 {
			return err
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return errors.New("PostgreSQL 串行化事务重试失败")
}

func resequenceSiblings(ctx context.Context, tx *sql.Tx, table, styleNo string, parentID *int) error {
	_, err := tx.ExecContext(ctx, `
		WITH ordered AS (
			SELECT id, row_number() OVER (ORDER BY sort_order, id)::integer - 1 AS next_order,
				row_number() OVER (ORDER BY sort_order, id)::integer AS next_no
			FROM `+table+`
			WHERE style_no = $1 AND parent_id IS NOT DISTINCT FROM $2
		)
		UPDATE `+table+` AS item
		SET sort_order = ordered.next_order,
			no = CASE WHEN item.parent_id IS NULL THEN ordered.next_no ELSE item.no END
		FROM ordered
		WHERE item.id = ordered.id AND item.style_no = $1
	`, styleNo, parentID)
	if err != nil {
		return fmt.Errorf("重排物料行失败: %w", err)
	}
	return nil
}

func insertRowsInTx(
	ctx context.Context,
	tx *sql.Tx,
	table, styleNo, category string,
	parentID, beforeID *int,
	items []rowInsertItem,
) ([]int, error) {
	if len(items) == 0 || len(items) > maxImportRows {
		return nil, errors.New("新增物料行数无效")
	}
	tableName := qualifiedTable(category, table)
	if parentID != nil {
		var exists bool
		if err := tx.QueryRowContext(ctx, `
			SELECT EXISTS (SELECT 1 FROM `+tableName+` WHERE id = $1 AND style_no = $2)
		`, *parentID, styleNo).Scan(&exists); err != nil {
			return nil, fmt.Errorf("检查父级物料失败: %w", err)
		}
		if !exists {
			return nil, sql.ErrNoRows
		}
	}

	insertOrder := 0
	if beforeID != nil {
		var beforeParentID sql.NullInt64
		if err := tx.QueryRowContext(ctx, `
			SELECT sort_order, parent_id FROM `+tableName+`
			WHERE id = $1 AND style_no = $2 FOR UPDATE
		`, *beforeID, styleNo).Scan(&insertOrder, &beforeParentID); err != nil {
			return nil, err
		}
		if (parentID == nil && beforeParentID.Valid) ||
			(parentID != nil && (!beforeParentID.Valid || int(beforeParentID.Int64) != *parentID)) {
			return nil, errors.New("插入位置与父级物料不匹配")
		}
	} else if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(sort_order) + 1, 0) FROM `+tableName+`
		WHERE style_no = $1 AND parent_id IS NOT DISTINCT FROM $2
	`, styleNo, parentID).Scan(&insertOrder); err != nil {
		return nil, fmt.Errorf("读取物料排序失败: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE `+tableName+` SET sort_order = sort_order + $1
		WHERE style_no = $2 AND parent_id IS NOT DISTINCT FROM $3 AND sort_order >= $4
	`, len(items), styleNo, parentID, insertOrder); err != nil {
		return nil, fmt.Errorf("为新增物料腾出排序位置失败: %w", err)
	}

	createdIDs := make([]int, len(items))
	nextOrder := map[int]int{}
	for index, item := range items {
		currentParent := parentID
		if index > 0 {
			if item.ParentIndex == nil || *item.ParentIndex < 0 || *item.ParentIndex >= index {
				return nil, errors.New("复制的父子行顺序无效")
			}
			currentParent = &createdIDs[*item.ParentIndex]
		} else if item.ParentIndex != nil {
			return nil, errors.New("新增物料的根行不能指定内部父级")
		}

		parentKey := 0
		if currentParent != nil {
			parentKey = *currentParent
		}
		order, initialized := nextOrder[parentKey]
		if !initialized {
			if index == 0 {
				order = insertOrder
			} else if err := tx.QueryRowContext(ctx, `
				SELECT COALESCE(MAX(sort_order) + 1, 0) FROM `+tableName+`
				WHERE style_no = $1 AND parent_id = $2
			`, styleNo, *currentParent).Scan(&order); err != nil {
				return nil, fmt.Errorf("读取子行排序失败: %w", err)
			}
		}
		nextOrder[parentKey] = order + 1

		color, err := encodeStringArray(item.Color)
		if err != nil {
			return nil, err
		}
		process, err := encodeStringArray(item.Process)
		if err != nil {
			return nil, err
		}
		customFields := item.CustomFields
		if customFields == nil {
			customFields = map[string]string{}
		}
		customFieldsRaw, err := json.Marshal(customFields)
		if err != nil {
			return nil, fmt.Errorf("编码自定义物料值失败: %w", err)
		}
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO `+tableName+` (
				style_no, no, parent_id, sort_order, parts, material_description,
				unit, remark, supplier, color, process, custom_fields
			) VALUES ($1, NULL, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10::jsonb, $11::jsonb)
			RETURNING id
		`, styleNo, currentParent, order, item.Parts, item.Description, item.Unit,
			item.Remark, item.Supplier, string(color), string(process), string(customFieldsRaw)).Scan(&createdIDs[index]); err != nil {
			return nil, fmt.Errorf("新增物料行失败: %w", err)
		}
	}
	if err := resequenceSiblings(ctx, tx, tableName, styleNo, parentID); err != nil {
		return nil, err
	}
	return createdIDs, nil
}

func createClonedTable(ctx context.Context, tx *sql.Tx, schema, table, template string) error {
	target := qualifiedTable(schema, table)
	source := qualifiedTable(schema, template)
	if _, err := tx.ExecContext(ctx, "CREATE TABLE "+target+" (LIKE "+source+
		" INCLUDING DEFAULTS INCLUDING CONSTRAINTS INCLUDING INDEXES)"); err != nil {
		return fmt.Errorf("复制 %s 表结构失败: %w", schema, err)
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT column_name
		FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = $2
			AND column_default LIKE 'nextval(%'
		ORDER BY ordinal_position
	`, schema, template)
	if err != nil {
		return fmt.Errorf("读取 %s 序列字段失败: %w", schema, err)
	}
	var sequenceColumns []string
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			rows.Close()
			return fmt.Errorf("读取 %s 序列字段失败: %w", schema, err)
		}
		sequenceColumns = append(sequenceColumns, column)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("读取 %s 序列字段失败: %w", schema, err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("关闭 %s 序列字段查询失败: %w", schema, err)
	}

	for _, column := range sequenceColumns {
		sum := sha256.Sum256([]byte(schema + "." + table + "." + column))
		sequence := "seq_" + fmt.Sprintf("%x", sum[:10])
		sequenceTable := qualifiedTable(schema, sequence)
		if _, err := tx.ExecContext(ctx, "CREATE SEQUENCE "+sequenceTable); err != nil {
			return fmt.Errorf("创建 %s 自增序列失败: %w", schema, err)
		}
		regclass := `"` + schema + `"."` + sequence + `"`
		if _, err := tx.ExecContext(ctx, "ALTER TABLE "+target+" ALTER COLUMN "+quoteIdentifier(column)+
			" SET DEFAULT nextval('"+regclass+"'::regclass)"); err != nil {
			return fmt.Errorf("绑定 %s 自增序列失败: %w", schema, err)
		}
		if _, err := tx.ExecContext(ctx, "ALTER SEQUENCE "+sequenceTable+" OWNED BY "+
			target+"."+quoteIdentifier(column)); err != nil {
			return fmt.Errorf("关联 %s 自增序列失败: %w", schema, err)
		}
	}
	if validateCategory(schema) {
		if _, err := tx.ExecContext(ctx, "ALTER TABLE "+target+
			" ADD CONSTRAINT "+quoteIdentifier("fk_bom_parent")+
			" FOREIGN KEY (parent_id) REFERENCES "+target+" (id) ON DELETE CASCADE"); err != nil {
			return fmt.Errorf("创建 %s 物料父子关系约束失败: %w", schema, err)
		}
	}
	return nil
}

func templateTable(ctx context.Context, tx *sql.Tx, schema, excluded string) (string, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT table_name
		FROM information_schema.tables
		WHERE table_schema = $1 AND table_type = 'BASE TABLE' AND table_name <> $2
		ORDER BY table_name
		LIMIT 1
	`, schema, excluded)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return "", err
		}
		return "", fmt.Errorf("PostgreSQL schema %s 中没有可复制的现有表", schema)
	}
	var name string
	if err := rows.Scan(&name); err != nil {
		return "", err
	}
	if !tableNamePattern.MatchString(name) {
		return "", fmt.Errorf("schema %s 中存在不支持的表名", schema)
	}
	return name, nil
}

func renameStyleRelations(ctx context.Context, tx *sql.Tx, oldTable, newTable string) error {
	for _, schema := range schemas {
		if _, err := tx.ExecContext(ctx, "ALTER TABLE "+qualifiedTable(schema, oldTable)+
			" RENAME TO "+quoteIdentifier(newTable)); err != nil {
			return fmt.Errorf("重命名 %s.%s 失败: %w", schema, oldTable, err)
		}
	}
	return nil
}

func ensureStyleTables(ctx context.Context, tx *sql.Tx, table string) error {
	for _, schema := range schemas {
		template, err := templateTable(ctx, tx, schema, table)
		if err != nil {
			return err
		}
		if err := createClonedTable(ctx, tx, schema, table, template); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, "ALTER TABLE "+qualifiedTable("footer", table)+
		" ADD CONSTRAINT "+quoteIdentifier("fk_footer")+" FOREIGN KEY (style_no) REFERENCES "+
		qualifiedTable("style", table)+" (style_no) ON UPDATE CASCADE ON DELETE CASCADE"); err != nil {
		return fmt.Errorf("创建签核记录外键失败: %w", err)
	}
	return nil
}

func main() {
	_ = godotenv.Load()

	host := envOr("PG_HOST", "127.0.0.1")
	port := envOr("PG_PORT", "5432")
	user := envOr("PG_USER", "postgres")
	password := os.Getenv("PG_PASSWORD")
	if password == "" {
		password = os.Getenv("PGPASSWORD")
	}
	databaseName := envOr("PG_DATABASE", defaultDatabase)
	if password == "" {
		log.Fatal("未设置 PostgreSQL 密码，请设置 PG_PASSWORD 或 PGPASSWORD")
	}

	dsn := url.URL{
		Scheme: "postgres",
		Host:   net.JoinHostPort(host, port),
		User:   url.UserPassword(user, password),
		Path:   "/" + databaseName,
	}
	query := url.Values{}
	query.Set("sslmode", envOr("PG_SSLMODE", "disable"))
	dsn.RawQuery = query.Encode()

	db, err := sql.Open("pgx", dsn.String())
	if err != nil {
		log.Fatalf("打开 PostgreSQL 连接失败: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(30 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		log.Fatalf("连接 PostgreSQL %s/%s 失败: %v", host, databaseName, err)
	}
	if err := verifyExistingSchema(ctx, db); err != nil {
		log.Fatalf("现有 PostgreSQL 表结构不符合预期: %v", err)
	}

	router := gin.Default()
	router.Use(func(c *gin.Context) {
		c.Header("Cache-Control", "no-store, no-cache, must-revalidate")
		c.Header("Pragma", "no-cache")
		c.Header("Expires", "0")
		c.Next()
	})
	router.Static("/static", "./static")
	router.StaticFile("/", "./static/index.html")

	router.GET("/api/bom/list", func(c *gin.Context) {
		rows, err := db.QueryContext(c.Request.Context(), `
			SELECT table_name FROM information_schema.tables
			WHERE table_schema = 'style' AND table_type = 'BASE TABLE'
			ORDER BY table_name
		`)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "读取 Style No. 列表失败: " + err.Error()})
			return
		}
		var tables []string
		for rows.Next() {
			var table string
			if err := rows.Scan(&table); err != nil {
				rows.Close()
				c.JSON(http.StatusInternalServerError, gin.H{"error": "读取 Style No. 表名失败: " + err.Error()})
				return
			}
			if tableNamePattern.MatchString(table) {
				tables = append(tables, table)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "读取 Style No. 列表失败: " + err.Error()})
			return
		}
		if err := rows.Close(); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "关闭 Style No. 查询失败: " + err.Error()})
			return
		}

		prefix := strings.ToLower(strings.TrimSpace(c.Query("q")))
		items := make([]bomSummary, 0, len(tables))
		for _, table := range tables {
			var item bomSummary
			err := db.QueryRowContext(c.Request.Context(),
				"SELECT style_no, COALESCE(style_name, '') FROM "+qualifiedTable("style", table)+" LIMIT 1",
			).Scan(&item.StyleNo, &item.StyleName)
			if err == sql.ErrNoRows {
				continue
			}
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "读取鞋款信息失败: " + err.Error()})
				return
			}
			if !strings.HasPrefix(strings.ToLower(item.StyleNo), prefix) {
				continue
			}
			item.ID = item.StyleNo
			items = append(items, item)
		}
		sort.Slice(items, func(i, j int) bool {
			return strings.ToLower(items[i].StyleNo) > strings.ToLower(items[j].StyleNo)
		})
		c.JSON(http.StatusOK, gin.H{"items": items, "has_more": false})
	})

	router.GET("/api/bom/header", func(c *gin.Context) {
		table, err := requireStyleTable(c.Request.Context(), db, requestedStyle(c))
		if err != nil {
			writeStyleError(c, err)
			return
		}
		tx, err := db.BeginTx(c.Request.Context(), &sql.TxOptions{
			Isolation: sql.LevelRepeatableRead,
			ReadOnly:  true,
		})
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "读取表头失败: " + err.Error()})
			return
		}
		defer tx.Rollback()
		header, err := readHeader(c.Request.Context(), tx, table)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "读取表头失败: " + err.Error()})
			return
		}
		c.JSON(http.StatusOK, header)
	})

	router.GET("/api/bom/matrix", func(c *gin.Context) {
		styleNo := requestedStyle(c)
		table, err := requireStyleTable(c.Request.Context(), db, styleNo)
		if err != nil {
			writeStyleError(c, err)
			return
		}
		tx, err := db.BeginTx(c.Request.Context(), &sql.TxOptions{
			Isolation: sql.LevelRepeatableRead,
			ReadOnly:  true,
		})
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "读取 BOM 矩阵失败: " + err.Error()})
			return
		}
		defer tx.Rollback()
		items, err := readMatrix(c.Request.Context(), tx, styleNo, table)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "读取 BOM 矩阵失败: " + err.Error()})
			return
		}
		customColumns, err := readCustomColumns(c.Request.Context(), tx, table)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "读取 BOM 自定义列失败: " + err.Error()})
			return
		}
		hiddenColumns, err := readHiddenColumns(c.Request.Context(), tx, table)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "读取 BOM 隐藏列失败: " + err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"categories":     categories,
			"columns":        colorwayLabels(),
			"custom_columns": customColumns,
			"hidden_columns": hiddenColumns,
			"data":           items,
		})
	})

	router.POST("/api/bom/create", func(c *gin.Context) {
		var request struct {
			Header styleHeader `json:"header"`
		}
		if err := c.ShouldBindJSON(&request); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		request.Header.StyleNo = strings.TrimSpace(request.Header.StyleNo)
		table, err := tableForStyle(request.Header.StyleNo)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if request.Header.StyleName == "" {
			request.Header.StyleName = request.Header.StyleNo
		}
		err = withSerializableTransaction(c.Request.Context(), db, func(tx *sql.Tx) error {
			for _, schema := range schemas {
				exists, err := relationExists(c.Request.Context(), tx, schema, table)
				if err != nil {
					return err
				}
				if exists {
					return fmt.Errorf("目标 Style No. 的 %s 数据表已存在", schema)
				}
			}
			if err := ensureStyleTables(c.Request.Context(), tx, table); err != nil {
				return err
			}
			_, err := tx.ExecContext(c.Request.Context(), `
				INSERT INTO `+qualifiedTable("style", table)+` (
					style_no, style_name, sample_order_no, sample_type, season, gender,
					last_no, bottom_no, shoe_filler_no, shoe_type, create_date
				) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NULLIF($11, '')::date)
			`,
				request.Header.StyleNo, request.Header.StyleName, request.Header.SampleOrderNo,
				request.Header.SampleType, request.Header.Season, request.Header.Gender,
				request.Header.LastNo, request.Header.BottomNo, request.Header.ShoeFillerNo,
				request.Header.ShoeType, request.Header.CreateDate,
			)
			if err != nil {
				return fmt.Errorf("新增 Style No. 主记录失败: %w", err)
			}
			_, err = tx.ExecContext(c.Request.Context(), `
				INSERT INTO `+qualifiedTable("footer", table)+` (
					style_no, approved_by, checked_by, prepared_by, approval_date
				) VALUES ($1, $2, $3, $4, NULLIF($5, '')::date)
			`,
				request.Header.StyleNo, request.Header.ApprovedBy, request.Header.CheckedBy,
				request.Header.PreparedBy, request.Header.ApprovalDate,
			)
			if err != nil {
				return fmt.Errorf("新增签核记录失败: %w", err)
			}
			return nil
		})
		if err != nil {
			writeMutationError(c, err, "新增鞋款失败")
			return
		}
		c.JSON(http.StatusCreated, gin.H{"success": true, "style_no": request.Header.StyleNo})
	})

	router.POST("/api/bom/delete", func(c *gin.Context) {
		styleNo := requestedStyle(c)
		table, err := requireStyleTable(c.Request.Context(), db, styleNo)
		if err != nil {
			writeStyleError(c, err)
			return
		}
		err = withSerializableTransaction(c.Request.Context(), db, func(tx *sql.Tx) error {
			if _, err := requireStyleTable(c.Request.Context(), tx, styleNo); err != nil {
				return err
			}
			for _, schema := range dropSchemas {
				if _, err := tx.ExecContext(c.Request.Context(),
					"DROP TABLE "+qualifiedTable(schema, table)); err != nil {
					return fmt.Errorf("删除 %s 鞋款数据表失败: %w", schema, err)
				}
			}
			return nil
		})
		if err != nil {
			writeMutationError(c, err, "删除鞋款失败")
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true})
	})

	router.POST("/api/bom/row-insert", func(c *gin.Context) {
		var request struct {
			Category     string          `json:"category"`
			ParentID     *int            `json:"parent_id"`
			BeforeItemID *int            `json:"before_item_id"`
			Items        []rowInsertItem `json:"items"`
		}
		if err := c.ShouldBindJSON(&request); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if !validateCategory(request.Category) || len(request.Items) == 0 ||
			len(request.Items) > maxImportRows ||
			(request.ParentID != nil && *request.ParentID <= 0) ||
			(request.BeforeItemID != nil && *request.BeforeItemID <= 0) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "物料分类、插入位置或行数据无效"})
			return
		}
		styleNo := requestedStyle(c)
		table, err := requireStyleTable(c.Request.Context(), db, styleNo)
		if err != nil {
			writeStyleError(c, err)
			return
		}
		var createdIDs []int
		err = withSerializableTransaction(c.Request.Context(), db, func(tx *sql.Tx) error {
			if _, err := requireStyleTable(c.Request.Context(), tx, styleNo); err != nil {
				return err
			}
			customColumns, err := readCustomColumns(c.Request.Context(), tx, table)
			if err != nil {
				return err
			}
			allowedColumns := make(map[string]bool, len(customColumns))
			for _, column := range customColumns {
				allowedColumns[column.ID] = true
			}
			for _, item := range request.Items {
				for columnID := range item.CustomFields {
					if !allowedColumns[columnID] {
						return errors.New("新增行包含未知的自定义列")
					}
				}
			}
			createdIDs, err = insertRowsInTx(c.Request.Context(), tx, table, styleNo,
				request.Category, request.ParentID, request.BeforeItemID, request.Items)
			return err
		})
		if err != nil {
			writeMutationError(c, err, "新增或粘贴物料失败")
			return
		}
		c.JSON(http.StatusCreated, gin.H{"success": true, "item_ids": createdIDs})
	})

	router.POST("/api/bom/row-delete", func(c *gin.Context) {
		var request struct {
			Category string `json:"category"`
			ItemID   int    `json:"item_id"`
		}
		if err := c.ShouldBindJSON(&request); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if !validateCategory(request.Category) || request.ItemID <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "物料分类或编号无效"})
			return
		}
		styleNo := requestedStyle(c)
		table, err := requireStyleTable(c.Request.Context(), db, styleNo)
		if err != nil {
			writeStyleError(c, err)
			return
		}
		err = withSerializableTransaction(c.Request.Context(), db, func(tx *sql.Tx) error {
			if _, err := requireStyleTable(c.Request.Context(), tx, styleNo); err != nil {
				return err
			}
			tableName := qualifiedTable(request.Category, table)
			var parent sql.NullInt64
			if err := tx.QueryRowContext(c.Request.Context(), `
				DELETE FROM `+tableName+` WHERE id = $1 AND style_no = $2 RETURNING parent_id
			`, request.ItemID, styleNo).Scan(&parent); err != nil {
				return err
			}
			var parentID *int
			if parent.Valid {
				value := int(parent.Int64)
				parentID = &value
			}
			return resequenceSiblings(c.Request.Context(), tx, tableName, styleNo, parentID)
		})
		if err != nil {
			writeMutationError(c, err, "删除物料行失败")
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true})
	})

	router.POST("/api/bom/update-header", func(c *gin.Context) {
		styleNo := requestedStyle(c)
		var request struct {
			Field string `json:"field"`
			Value string `json:"value"`
		}
		if err := c.ShouldBindJSON(&request); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		headerColumns := map[string]string{
			"style_name": "style_name", "sample_order_no": "sample_order_no",
			"sample_type": "sample_type", "season": "season", "gender": "gender",
			"last_no": "last_no", "bottom_no": "bottom_no", "shoe_filler_no": "shoe_filler_no",
			"shoe_type": "shoe_type", "stage": "shoe_type", "create_date": "create_date",
		}
		footerColumns := map[string]string{
			"approved_by": "approved_by", "checked_by": "checked_by",
			"reviewed_by": "checked_by", "prepared_by": "prepared_by",
			"approval_date": "approval_date",
		}
		if request.Field != "style_no" && headerColumns[request.Field] == "" && footerColumns[request.Field] == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "不支持修改的表头字段"})
			return
		}
		if request.Field == "style_no" {
			request.Value = strings.TrimSpace(request.Value)
			if _, err := tableForStyle(request.Value); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
		}
		err := withSerializableTransaction(c.Request.Context(), db, func(tx *sql.Tx) error {
			oldTable, err := requireStyleTable(c.Request.Context(), tx, styleNo)
			if err != nil {
				return err
			}
			if request.Field == "style_no" {
				newTable, _ := tableForStyle(request.Value)
				if oldTable != newTable {
					for _, schema := range schemas {
						exists, err := relationExists(c.Request.Context(), tx, schema, newTable)
						if err != nil {
							return err
						}
						if exists {
							return fmt.Errorf("目标 Style No. 已有 %s 数据表", schema)
						}
					}
					if err := renameStyleRelations(c.Request.Context(), tx, oldTable, newTable); err != nil {
						return err
					}
				}
				if _, err := tx.ExecContext(c.Request.Context(),
					"UPDATE "+qualifiedTable("style", newTable)+" SET style_no = $1 WHERE style_no = $2",
					request.Value, styleNo); err != nil {
					return fmt.Errorf("更新 Style No. 失败: %w", err)
				}
				for _, category := range categories {
					if _, err := tx.ExecContext(c.Request.Context(),
						"UPDATE "+qualifiedTable(category, newTable)+" SET style_no = $1 WHERE style_no = $2",
						request.Value, styleNo); err != nil {
						return fmt.Errorf("更新 %s 数据的 Style No. 失败: %w", category, err)
					}
				}
				if _, err := tx.ExecContext(c.Request.Context(),
					"UPDATE "+qualifiedTable("footer", newTable)+" SET style_no = $1 WHERE style_no = $2",
					request.Value, styleNo); err != nil {
					return fmt.Errorf("更新签核记录的 Style No. 失败: %w", err)
				}
				return nil
			}
			if column := headerColumns[request.Field]; column != "" {
				var result sql.Result
				if column == "create_date" {
					result, err = tx.ExecContext(c.Request.Context(),
						"UPDATE "+qualifiedTable("style", oldTable)+" SET create_date = NULLIF($1, '')::date WHERE style_no = $2",
						strings.TrimSpace(request.Value), styleNo)
				} else {
					result, err = tx.ExecContext(c.Request.Context(),
						"UPDATE "+qualifiedTable("style", oldTable)+" SET "+quoteIdentifier(column)+" = $1 WHERE style_no = $2",
						strings.TrimSpace(request.Value), styleNo)
				}
				if err != nil {
					return fmt.Errorf("更新鞋款表头失败: %w", err)
				}
				affected, err := result.RowsAffected()
				if err != nil {
					return err
				}
				if affected == 0 {
					return sql.ErrNoRows
				}
				return nil
			}
			column := footerColumns[request.Field]
			value := any(strings.TrimSpace(request.Value))
			insertValue := "$2"
			if column == "approval_date" {
				insertValue = "NULLIF($2, '')::date"
			}
			_, err = tx.ExecContext(c.Request.Context(),
				"INSERT INTO "+qualifiedTable("footer", oldTable)+" (style_no, "+quoteIdentifier(column)+
					") VALUES ($1, "+insertValue+") ON CONFLICT (style_no) DO UPDATE SET "+
					quoteIdentifier(column)+" = EXCLUDED."+quoteIdentifier(column),
				styleNo, value)
			if err != nil {
				return fmt.Errorf("更新签核信息失败: %w", err)
			}
			return nil
		})
		if err != nil {
			writeMutationError(c, err, "保存表头失败")
			return
		}
		response := gin.H{"success": true}
		if request.Field == "style_no" {
			response["style_no"] = request.Value
		}
		c.JSON(http.StatusOK, response)
	})

	router.POST("/api/bom/update-row", func(c *gin.Context) {
		var request struct {
			Category string `json:"category"`
			ItemID   int    `json:"item_id"`
			Field    string `json:"field"`
			Value    string `json:"value"`
		}
		if err := c.ShouldBindJSON(&request); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if !validateCategory(request.Category) || request.ItemID <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "物料分类或编号无效"})
			return
		}
		column := map[string]string{
			"parts": "parts", "material_description": "material_description",
			"unit": "unit", "remark": "remark", "supplier": "supplier",
		}[request.Field]
		var value any = request.Value
		if request.Field == "no" {
			column = "no"
			if request.Value == "" {
				value = nil
			} else {
				number, err := strconv.Atoi(request.Value)
				if err != nil {
					c.JSON(http.StatusBadRequest, gin.H{"error": "物料编号必须为整数"})
					return
				}
				value = number
			}
		}
		if column == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "不支持修改的物料字段"})
			return
		}
		table, err := requireStyleTable(c.Request.Context(), db, requestedStyle(c))
		if err != nil {
			writeStyleError(c, err)
			return
		}
		err = withSerializableTransaction(c.Request.Context(), db, func(tx *sql.Tx) error {
			if _, err := requireStyleTable(c.Request.Context(), tx, requestedStyle(c)); err != nil {
				return err
			}
			result, err := tx.ExecContext(c.Request.Context(),
				"UPDATE "+qualifiedTable(request.Category, table)+" SET "+quoteIdentifier(column)+
					" = $1 WHERE id = $2 AND style_no = $3",
				value, request.ItemID, requestedStyle(c))
			if err != nil {
				return fmt.Errorf("更新物料失败: %w", err)
			}
			affected, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if affected == 0 {
				return sql.ErrNoRows
			}
			return nil
		})
		if err != nil {
			writeMutationError(c, err, "更新物料失败")
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true})
	})

	router.POST("/api/bom/update-cell", func(c *gin.Context) {
		var request struct {
			Category  string `json:"category"`
			ItemID    int    `json:"item_id"`
			FieldType string `json:"field_type"`
			Index     int    `json:"index"`
			Value     string `json:"value"`
		}
		if err := c.ShouldBindJSON(&request); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if !validateCategory(request.Category) || request.ItemID <= 0 ||
			(request.FieldType != "color" && request.FieldType != "process") ||
			request.Index < 0 || request.Index >= colorwayCount {
			c.JSON(http.StatusBadRequest, gin.H{"error": "配色/工艺单元格参数无效"})
			return
		}
		styleNo := requestedStyle(c)
		table, err := requireStyleTable(c.Request.Context(), db, styleNo)
		if err != nil {
			writeStyleError(c, err)
			return
		}
		err = withSerializableTransaction(c.Request.Context(), db, func(tx *sql.Tx) error {
			if _, err := requireStyleTable(c.Request.Context(), tx, styleNo); err != nil {
				return err
			}
			tableName := qualifiedTable(request.Category, table)
			var raw []byte
			if err := tx.QueryRowContext(c.Request.Context(),
				"SELECT COALESCE("+quoteIdentifier(request.FieldType)+", '[]'::jsonb) FROM "+
					tableName+" WHERE id = $1 AND style_no = $2 FOR UPDATE",
				request.ItemID, styleNo).Scan(&raw); err != nil {
				return err
			}
			values, err := decodeStringArray(raw, colorwayCount)
			if err != nil {
				return err
			}
			values[request.Index] = request.Value
			encoded, err := encodeStringArray(values)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(c.Request.Context(),
				"UPDATE "+tableName+" SET "+quoteIdentifier(request.FieldType)+" = $1::jsonb WHERE id = $2 AND style_no = $3",
				string(encoded), request.ItemID, styleNo); err != nil {
				return fmt.Errorf("保存 JSONB 配色/工艺值失败: %w", err)
			}
			return nil
		})
		if err != nil {
			writeMutationError(c, err, "保存配色/工艺失败")
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true})
	})

	router.POST("/api/bom/update-custom-cell", func(c *gin.Context) {
		var request struct {
			Category string `json:"category"`
			ItemID   int    `json:"item_id"`
			ColumnID string `json:"column_id"`
			Value    string `json:"value"`
		}
		if err := c.ShouldBindJSON(&request); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if !validateCategory(request.Category) || request.ItemID <= 0 ||
			!customColumnIDPattern.MatchString(request.ColumnID) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "自定义列单元格参数无效"})
			return
		}
		styleNo := requestedStyle(c)
		table, err := requireStyleTable(c.Request.Context(), db, styleNo)
		if err != nil {
			writeStyleError(c, err)
			return
		}
		err = withSerializableTransaction(c.Request.Context(), db, func(tx *sql.Tx) error {
			if _, err := requireStyleTable(c.Request.Context(), tx, styleNo); err != nil {
				return err
			}
			columns, err := readCustomColumns(c.Request.Context(), tx, table)
			if err != nil {
				return err
			}
			if !slices.ContainsFunc(columns, func(column customColumn) bool {
				return column.ID == request.ColumnID
			}) {
				return sql.ErrNoRows
			}
			result, err := tx.ExecContext(c.Request.Context(), `
				UPDATE `+qualifiedTable(request.Category, table)+`
				SET custom_fields = jsonb_set(
					COALESCE(custom_fields, '{}'::jsonb), ARRAY[$1]::text[], to_jsonb($2::text), true
				)
				WHERE id = $3 AND style_no = $4
			`, request.ColumnID, request.Value, request.ItemID, styleNo)
			if err != nil {
				return fmt.Errorf("保存自定义列单元格失败: %w", err)
			}
			affected, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if affected == 0 {
				return sql.ErrNoRows
			}
			return nil
		})
		if err != nil {
			writeMutationError(c, err, "保存自定义列单元格失败")
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true})
	})

	router.POST("/api/bom/custom-column", func(c *gin.Context) {
		var request struct {
			Action   string       `json:"action"`
			Column   customColumn `json:"column"`
			ColumnID string       `json:"column_id"`
			Title    string       `json:"title"`
			Values   []struct {
				Category string `json:"category"`
				ItemID   int    `json:"item_id"`
				Value    string `json:"value"`
			} `json:"values"`
		}
		if err := c.ShouldBindJSON(&request); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if request.Action != "add" && request.Action != "rename" && request.Action != "delete" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "自定义列操作无效"})
			return
		}
		if len(request.Values) > maxImportRows {
			c.JSON(http.StatusBadRequest, gin.H{"error": "复制列数据超过上限"})
			return
		}
		if request.Action == "add" {
			request.Column.ID = strings.ToLower(request.Column.ID)
			request.Column.Title = strings.TrimSpace(request.Column.Title)
			if err := validateCustomColumns([]customColumn{request.Column}); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
		} else if request.Action == "rename" && !customColumnIDPattern.MatchString(request.ColumnID) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "自定义列编号无效"})
			return
		} else if request.Action == "delete" &&
			!customColumnIDPattern.MatchString(request.ColumnID) &&
			!builtInColumnPattern.MatchString(request.ColumnID) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "列编号无效"})
			return
		}
		if request.Action == "rename" {
			request.Title = strings.TrimSpace(request.Title)
			if request.Title == "" || len([]rune(request.Title)) > 80 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "列名不能为空且最多 80 个字符"})
				return
			}
		}
		for _, value := range request.Values {
			if !validateCategory(value.Category) || value.ItemID <= 0 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "复制列包含无效的物料行"})
				return
			}
		}

		styleNo := requestedStyle(c)
		table, err := requireStyleTable(c.Request.Context(), db, styleNo)
		if err != nil {
			writeStyleError(c, err)
			return
		}
		err = withSerializableTransaction(c.Request.Context(), db, func(tx *sql.Tx) error {
			if _, err := requireStyleTable(c.Request.Context(), tx, styleNo); err != nil {
				return err
			}
			var raw []byte
			if err := tx.QueryRowContext(c.Request.Context(),
				"SELECT COALESCE(custom_columns, '[]'::jsonb) FROM "+qualifiedTable("style", table)+
					" LIMIT 1 FOR UPDATE").Scan(&raw); err != nil {
				return err
			}
			var columns []customColumn
			if err := json.Unmarshal(raw, &columns); err != nil {
				return fmt.Errorf("解析自定义列失败: %w", err)
			}
			if columns == nil {
				columns = []customColumn{}
			}
			if err := validateCustomColumns(columns); err != nil {
				return err
			}
			hiddenColumns, err := readHiddenColumns(c.Request.Context(), tx, table)
			if err != nil {
				return err
			}
			switch request.Action {
			case "add":
				if len(columns) >= maxCustomColumns {
					return fmt.Errorf("自定义列数量不能超过 %d", maxCustomColumns)
				}
				if slices.ContainsFunc(columns, func(column customColumn) bool {
					return column.ID == request.Column.ID
				}) {
					return errors.New("自定义列编号已存在")
				}
				position := request.Column.Position
				if position < 7 || position > 7+2*colorwayCount-len(hiddenColumns)+len(columns) {
					return errors.New("自定义列插入位置无效")
				}
				for index := range columns {
					if columns[index].Position >= position {
						columns[index].Position++
					}
				}
				columns = append(columns, request.Column)
				sort.Slice(columns, func(i, j int) bool { return columns[i].Position < columns[j].Position })
				for _, value := range request.Values {
					result, err := tx.ExecContext(c.Request.Context(), `
						UPDATE `+qualifiedTable(value.Category, table)+`
						SET custom_fields = jsonb_set(
							COALESCE(custom_fields, '{}'::jsonb), ARRAY[$1]::text[], to_jsonb($2::text), true
						)
						WHERE id = $3 AND style_no = $4
					`, request.Column.ID, value.Value, value.ItemID, styleNo)
					if err != nil {
						return fmt.Errorf("复制自定义列数据失败: %w", err)
					}
					affected, err := result.RowsAffected()
					if err != nil {
						return err
					}
					if affected == 0 {
						return sql.ErrNoRows
					}
				}
			case "rename":
				found := false
				for index := range columns {
					if columns[index].ID == request.ColumnID {
						columns[index].Title = request.Title
						found = true
						break
					}
				}
				if !found {
					return sql.ErrNoRows
				}
			case "delete":
				position := -1
				for index, column := range columns {
					if column.ID == request.ColumnID {
						position = column.Position
						columns = append(columns[:index], columns[index+1:]...)
						break
					}
				}
				if position >= 0 {
					for index := range columns {
						if columns[index].Position > position {
							columns[index].Position--
						}
					}
					for _, category := range categories {
						if _, err := tx.ExecContext(c.Request.Context(),
							"UPDATE "+qualifiedTable(category, table)+
								" SET custom_fields = COALESCE(custom_fields, '{}'::jsonb) - $1 WHERE style_no = $2",
							request.ColumnID, styleNo); err != nil {
							return fmt.Errorf("删除 %s 自定义列数据失败: %w", category, err)
						}
					}
				} else {
					position, ok := builtInColumnPosition(request.ColumnID, hiddenColumns)
					if !ok {
						return sql.ErrNoRows
					}
					hiddenColumns = append(hiddenColumns, request.ColumnID)
					for index := range columns {
						if columns[index].Position > position {
							columns[index].Position--
						}
					}
					if err := writeHiddenColumns(c.Request.Context(), tx, table, styleNo, hiddenColumns); err != nil {
						return err
					}
				}
			}
			return writeCustomColumns(c.Request.Context(), tx, table, styleNo, columns)
		})
		if err != nil {
			writeMutationError(c, err, "自定义列操作失败")
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true})
	})

	router.POST("/api/bom/import", func(c *gin.Context) {
		styleNo := requestedStyle(c)
		var request struct {
			Header styleHeader    `json:"header"`
			Rows   []importedItem `json:"rows"`
		}
		if err := c.ShouldBindJSON(&request); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if len(request.Rows) > maxImportRows {
			c.JSON(http.StatusBadRequest, gin.H{"error": "导入行数超过上限"})
			return
		}
		if request.Header.StyleNo != "" && !strings.EqualFold(strings.TrimSpace(request.Header.StyleNo), styleNo) {
			c.JSON(http.StatusConflict, gin.H{"error": "导入文件的 Style No. 与当前鞋款不一致"})
			return
		}
		for _, item := range request.Rows {
			if !validateCategory(item.Category) {
				c.JSON(http.StatusBadRequest, gin.H{"error": "导入文件包含未知物料分类"})
				return
			}
		}
		table, err := requireStyleTable(c.Request.Context(), db, styleNo)
		if err != nil {
			writeStyleError(c, err)
			return
		}
		err = withSerializableTransaction(c.Request.Context(), db, func(tx *sql.Tx) error {
			if _, err := requireStyleTable(c.Request.Context(), tx, styleNo); err != nil {
				return err
			}
			customColumns, err := readCustomColumns(c.Request.Context(), tx, table)
			if err != nil {
				return err
			}
			allowedCustomColumns := make(map[string]bool, len(customColumns))
			for _, column := range customColumns {
				allowedCustomColumns[column.ID] = true
			}
			if err := updateHeaderInTx(c.Request.Context(), tx, table, styleNo, request.Header); err != nil {
				return err
			}
			for _, category := range categories {
				if _, err := tx.ExecContext(c.Request.Context(),
					"DELETE FROM "+qualifiedTable(category, table)+" WHERE style_no = $1", styleNo); err != nil {
					return fmt.Errorf("替换 %s 物料失败: %w", category, err)
				}
			}
			itemIDs := make([]int, len(request.Rows))
			nextOrders := make(map[string]map[int]int)
			for index, item := range request.Rows {
				for columnID := range item.CustomFields {
					if !allowedCustomColumns[columnID] {
						return fmt.Errorf("导入第 %d 行包含未知的自定义列", index+1)
					}
				}
				color, err := encodeStringArray(item.Color)
				if err != nil {
					return err
				}
				process, err := encodeStringArray(item.Process)
				if err != nil {
					return err
				}
				customFields := item.CustomFields
				if customFields == nil {
					customFields = map[string]string{}
				}
				customFieldsRaw, err := json.Marshal(customFields)
				if err != nil {
					return fmt.Errorf("编码导入自定义列失败: %w", err)
				}
				var parentID *int
				if item.ParentRow != nil {
					if *item.ParentRow < 1 || *item.ParentRow > index {
						return fmt.Errorf("导入第 %d 行的父级行号无效", index+1)
					}
					parentIDValue := itemIDs[*item.ParentRow-1]
					parentID = &parentIDValue
					if request.Rows[*item.ParentRow-1].Category != item.Category {
						return fmt.Errorf("导入第 %d 行与父级行的分类不一致", index+1)
					}
				}
				categoryOrders := nextOrders[item.Category]
				if categoryOrders == nil {
					categoryOrders = make(map[int]int)
					nextOrders[item.Category] = categoryOrders
				}
				parentKey := 0
				if parentID != nil {
					parentKey = *parentID
				}
				order := categoryOrders[parentKey]
				categoryOrders[parentKey] = order + 1
				err = tx.QueryRowContext(c.Request.Context(), `
					INSERT INTO `+qualifiedTable(item.Category, table)+` (
						style_no, no, parent_id, sort_order, parts, material_description,
						unit, remark, supplier, color, process, custom_fields
					) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb, $11::jsonb, $12::jsonb)
					RETURNING id
				`, styleNo, item.No, parentID, order, item.Parts, item.Description, item.Unit,
					item.Remark, item.Supplier, string(color), string(process), string(customFieldsRaw)).Scan(&itemIDs[index])
				if err != nil {
					return fmt.Errorf("导入 %s 物料行失败: %w", item.Category, err)
				}
			}
			for _, category := range categories {
				tableName := qualifiedTable(category, table)
				if _, err := tx.ExecContext(c.Request.Context(), `
					WITH ordered AS (
						SELECT id, row_number() OVER (PARTITION BY parent_id ORDER BY sort_order, id)::integer - 1 AS next_order
						FROM `+tableName+` WHERE style_no = $1
					)
					UPDATE `+tableName+` AS item SET sort_order = ordered.next_order
					FROM ordered WHERE item.id = ordered.id
				`, styleNo); err != nil {
					return fmt.Errorf("重排导入的 %s 物料失败: %w", category, err)
				}
			}
			return nil
		})
		if err != nil {
			writeMutationError(c, err, "导入 BOM 失败")
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "rows": len(request.Rows)})
	})

	portHTTP := envOr("PORT", "8080")
	log.Printf("BOM 服务已连接 PostgreSQL %s/%s", host, databaseName)
	if err := router.Run(":" + portHTTP); err != nil {
		log.Fatalf("启动 HTTP 服务失败: %v", err)
	}
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func verifyExistingSchema(ctx context.Context, db *sql.DB) error {
	for _, schema := range schemas {
		var exists bool
		if err := db.QueryRowContext(ctx,
			"SELECT EXISTS (SELECT 1 FROM information_schema.schemata WHERE schema_name = $1)",
			schema).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("缺少现有 schema %s；服务不会自动创建或迁移数据库结构", schema)
		}
	}
	var styleTableCount int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.tables
		WHERE table_schema = 'style' AND table_type = 'BASE TABLE'
	`).Scan(&styleTableCount); err != nil {
		return err
	}
	if styleTableCount == 0 {
		return errors.New("style schema 中没有现有鞋款数据表")
	}
	rows, err := db.QueryContext(ctx, `
		SELECT table_name FROM information_schema.tables
		WHERE table_schema = 'style' AND table_type = 'BASE TABLE'
	`)
	if err != nil {
		return fmt.Errorf("读取现有 Style No. 表失败: %w", err)
	}
	var styleTables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			rows.Close()
			return fmt.Errorf("读取现有 Style No. 表失败: %w", err)
		}
		styleTables = append(styleTables, table)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("读取现有 Style No. 表失败: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("关闭 Style No. 表查询失败: %w", err)
	}
	for _, table := range styleTables {
		var customColumnsCount int
		if err := db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM information_schema.columns
			WHERE table_schema = 'style' AND table_name = $1
				AND column_name IN ('custom_columns', 'hidden_columns') AND data_type = 'jsonb'
		`, table).Scan(&customColumnsCount); err != nil {
			return fmt.Errorf("检查 style.%s 的自定义列字段失败: %w", table, err)
		}
		if customColumnsCount != 2 {
			return fmt.Errorf("缺少 style.%s.custom_columns/hidden_columns；请先手动执行 migrations/002_custom_columns.sql", table)
		}
		for _, category := range categories {
			var columnCount int
			if err := db.QueryRowContext(ctx, `
				SELECT COUNT(*) FROM information_schema.columns
				WHERE table_schema = $1 AND table_name = $2
					AND (
						column_name IN ('parent_id', 'sort_order')
						OR (column_name = 'custom_fields' AND data_type = 'jsonb')
					)
			`, category, table).Scan(&columnCount); err != nil {
				return fmt.Errorf("检查 %s.%s 的层级字段失败: %w", category, table, err)
			}
			if columnCount != 3 {
				return fmt.Errorf("缺少 %s.%s 的 parent_id/sort_order/custom_fields；请先手动执行 migrations/001_bom_hierarchy.sql 和 migrations/002_custom_columns.sql", category, table)
			}
			var hasParentConstraint bool
			if err := db.QueryRowContext(ctx, `
				SELECT EXISTS (
					SELECT 1 FROM pg_constraint c
					JOIN pg_class t ON t.oid = c.conrelid
					JOIN pg_namespace n ON n.oid = t.relnamespace
					WHERE n.nspname = $1 AND t.relname = $2
						AND c.conname = 'fk_bom_parent' AND c.contype = 'f'
				)
			`, category, table).Scan(&hasParentConstraint); err != nil {
				return fmt.Errorf("检查 %s.%s 的父子关系约束失败: %w", category, table, err)
			}
			if !hasParentConstraint {
				return fmt.Errorf("缺少 %s.%s 的 fk_bom_parent；请先手动执行 migrations/001_bom_hierarchy.sql", category, table)
			}
		}
	}
	return nil
}

func requestedStyle(c *gin.Context) string {
	if styleNo := strings.TrimSpace(c.Query("style_no")); styleNo != "" {
		return styleNo
	}
	return strings.TrimSpace(c.Query("bom_id"))
}

func writeStyleError(c *gin.Context, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "未找到指定的 Style No."})
		return
	}
	if _, validationErr := tableForStyle(requestedStyle(c)); validationErr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": validationErr.Error()})
		return
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
}

func writeMutationError(c *gin.Context, err error, prefix string) {
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": prefix + ": 未找到对应记录"})
		return
	}
	var stateError interface{ SQLState() string }
	if errors.As(err, &stateError) {
		switch stateError.SQLState() {
		case "23505":
			c.JSON(http.StatusConflict, gin.H{"error": prefix + ": 数据重复，未保存任何更改"})
			return
		case "40001", "40P01":
			c.JSON(http.StatusConflict, gin.H{"error": prefix + ": 并发修改冲突，请重试"})
			return
		}
	}
	message := prefix + ": " + err.Error()
	if strings.Contains(err.Error(), "目标 Style No.") || strings.Contains(err.Error(), "没有可复制的现有表") {
		c.JSON(http.StatusConflict, gin.H{"error": message})
		return
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": message})
}

func readHeader(ctx context.Context, tx *sql.Tx, table string) (styleHeader, error) {
	var header styleHeader
	err := tx.QueryRowContext(ctx, `
		SELECT style_no, COALESCE(style_name, ''), COALESCE(sample_order_no, ''),
			COALESCE(sample_type, ''), COALESCE(season, ''), COALESCE(gender, ''),
			COALESCE(last_no, ''), COALESCE(bottom_no, ''), COALESCE(shoe_filler_no, ''),
			COALESCE(shoe_type, ''), COALESCE(create_date::text, '')
		FROM `+qualifiedTable("style", table)+` LIMIT 1
	`).Scan(&header.StyleNo, &header.StyleName, &header.SampleOrderNo, &header.SampleType,
		&header.Season, &header.Gender, &header.LastNo, &header.BottomNo,
		&header.ShoeFillerNo, &header.ShoeType, &header.CreateDate)
	if err != nil {
		return header, err
	}
	err = tx.QueryRowContext(ctx, `
		SELECT COALESCE(approved_by, ''), COALESCE(checked_by, ''),
			COALESCE(prepared_by, ''), COALESCE(approval_date::text, '')
		FROM `+qualifiedTable("footer", table)+` LIMIT 1
	`).Scan(&header.ApprovedBy, &header.CheckedBy, &header.PreparedBy, &header.ApprovalDate)
	if err == sql.ErrNoRows {
		return header, nil
	}
	return header, err
}

func readMatrix(ctx context.Context, tx *sql.Tx, styleNo, table string) ([]bomItem, error) {
	var items []bomItem
	for _, category := range categories {
		rows, err := tx.QueryContext(ctx, `
			SELECT id, parent_id, sort_order, no, COALESCE(parts, ''), COALESCE(material_description, ''),
				COALESCE(unit, ''), COALESCE(remark, ''), COALESCE(supplier, ''),
				COALESCE(color, '[]'::jsonb), COALESCE(process, '[]'::jsonb),
				COALESCE(custom_fields, '{}'::jsonb)
			FROM `+qualifiedTable(category, table)+`
			WHERE style_no = $1
			ORDER BY parent_id NULLS FIRST, sort_order, id
		`, styleNo)
		if err != nil {
			return nil, fmt.Errorf("读取 %s 分类失败: %w", category, err)
		}
		for rows.Next() {
			var item bomItem
			var itemNo, parentID sql.NullInt64
			var colorsRaw, processRaw, customFieldsRaw []byte
			if err := rows.Scan(&item.ItemID, &parentID, &item.SortOrder, &itemNo, &item.Parts, &item.Description,
				&item.Unit, &item.Remark, &item.Supplier, &colorsRaw, &processRaw, &customFieldsRaw); err != nil {
				rows.Close()
				return nil, fmt.Errorf("读取 %s 物料失败: %w", category, err)
			}
			if itemNo.Valid {
				itemNoValue := int(itemNo.Int64)
				item.No = &itemNoValue
			}
			if parentID.Valid {
				parentIDValue := int(parentID.Int64)
				item.ParentID = &parentIDValue
			}
			item.Category = category
			item.Color, err = decodeStringArray(colorsRaw, colorwayCount)
			if err != nil {
				rows.Close()
				return nil, err
			}
			item.Process, err = decodeStringArray(processRaw, colorwayCount)
			if err != nil {
				rows.Close()
				return nil, err
			}
			if err := json.Unmarshal(customFieldsRaw, &item.CustomFields); err != nil {
				rows.Close()
				return nil, fmt.Errorf("读取 %s 自定义列数据失败: %w", category, err)
			}
			if item.CustomFields == nil {
				item.CustomFields = map[string]string{}
			}
			items = append(items, item)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("读取 %s 分类失败: %w", category, err)
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf("关闭 %s 查询失败: %w", category, err)
		}
	}
	return items, nil
}

func colorwayLabels() []string {
	labels := make([]string, colorwayCount)
	for i := range labels {
		labels[i] = fmt.Sprintf("配色 %d", i+1)
	}
	return labels
}

func updateHeaderInTx(ctx context.Context, tx *sql.Tx, table, styleNo string, header styleHeader) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE `+qualifiedTable("style", table)+` SET
			style_name = COALESCE(NULLIF($1, ''), style_name),
			sample_order_no = $2, sample_type = $3, season = $4, gender = $5,
			last_no = $6, bottom_no = $7, shoe_filler_no = $8, shoe_type = $9,
			create_date = NULLIF($10, '')::date
		WHERE style_no = $11
	`, header.StyleName, header.SampleOrderNo, header.SampleType, header.Season,
		header.Gender, header.LastNo, header.BottomNo, header.ShoeFillerNo,
		header.ShoeType, header.CreateDate, styleNo)
	if err != nil {
		return fmt.Errorf("更新鞋款表头失败: %w", err)
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO `+qualifiedTable("footer", table)+` (
			style_no, approved_by, checked_by, prepared_by, approval_date
		) VALUES ($1, $2, $3, $4, NULLIF($5, '')::date)
		ON CONFLICT (style_no) DO UPDATE SET
			approved_by = EXCLUDED.approved_by,
			checked_by = EXCLUDED.checked_by,
			prepared_by = EXCLUDED.prepared_by,
			approval_date = EXCLUDED.approval_date
	`, styleNo, header.ApprovedBy, header.CheckedBy, header.PreparedBy, header.ApprovalDate)
	if err != nil {
		return fmt.Errorf("更新签核记录失败: %w", err)
	}
	return nil
}
