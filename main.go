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
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/joho/godotenv"
)

const (
	defaultDatabase = "db"
	colorwayCount   = 8
	maxImportRows   = 5000
)

var (
	styleNumberPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,63}$`)
	tableNamePattern   = regexp.MustCompile(`^[a-z0-9_-]{1,63}$`)
	categories         = []string{
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
	ItemID      int      `json:"item_id"`
	Category    string   `json:"category"`
	No          *int     `json:"no"`
	Parts       string   `json:"parts"`
	Description string   `json:"material_description"`
	Unit        string   `json:"unit"`
	Remark      string   `json:"remark"`
	Supplier    string   `json:"supplier"`
	Color       []string `json:"color"`
	Process     []string `json:"process"`
}

type importedItem struct {
	Category    string   `json:"category"`
	No          *int     `json:"no"`
	Parts       string   `json:"parts"`
	Description string   `json:"material_description"`
	Unit        string   `json:"unit"`
	Remark      string   `json:"remark"`
	Supplier    string   `json:"supplier"`
	Color       []string `json:"color"`
	Process     []string `json:"process"`
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

func createClonedTable(ctx context.Context, tx *sql.Tx, schema, table, template string) error {
	target := qualifiedTable(schema, table)
	source := qualifiedTable(schema, template)
	if _, err := tx.ExecContext(ctx, "CREATE TABLE "+target+" (LIKE "+source+" INCLUDING CONSTRAINTS INCLUDING INDEXES)"); err != nil {
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
		c.JSON(http.StatusOK, gin.H{
			"categories": categories,
			"columns":    colorwayLabels(),
			"data":       items,
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

	router.POST("/api/bom/add-row", func(c *gin.Context) {
		var request struct {
			Category string `json:"category"`
		}
		if err := c.ShouldBindJSON(&request); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if !validateCategory(request.Category) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "无效的物料分类"})
			return
		}
		styleNo := requestedStyle(c)
		table, err := requireStyleTable(c.Request.Context(), db, styleNo)
		if err != nil {
			writeStyleError(c, err)
			return
		}
		itemColor, _ := encodeStringArray(nil)
		itemProcess, _ := encodeStringArray(nil)
		var itemID int
		err = withSerializableTransaction(c.Request.Context(), db, func(tx *sql.Tx) error {
			if _, err := requireStyleTable(c.Request.Context(), tx, styleNo); err != nil {
				return err
			}
			var nextNo int
			if err := tx.QueryRowContext(c.Request.Context(),
				"SELECT COALESCE(MAX(no), 0) + 1 FROM "+qualifiedTable(request.Category, table)+
					" WHERE style_no = $1", styleNo).Scan(&nextNo); err != nil {
				return err
			}
			return tx.QueryRowContext(c.Request.Context(), `
				INSERT INTO `+qualifiedTable(request.Category, table)+` (
					style_no, no, parts, material_description, unit, remark, supplier, color, process
				) VALUES ($1, $2, '', '', '', '', '', $3::jsonb, $4::jsonb)
				RETURNING id
			`, styleNo, nextNo, string(itemColor), string(itemProcess)).Scan(&itemID)
		})
		if err != nil {
			writeMutationError(c, err, "新增物料行失败")
			return
		}
		c.JSON(http.StatusCreated, gin.H{"success": true, "item_id": itemID})
	})

	router.POST("/api/bom/delete-row", func(c *gin.Context) {
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
			result, err := tx.ExecContext(c.Request.Context(),
				"DELETE FROM "+qualifiedTable(request.Category, table)+" WHERE id = $1 AND style_no = $2",
				request.ItemID, styleNo)
			if err != nil {
				return err
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
			writeMutationError(c, err, "删除物料行失败")
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
			if err := updateHeaderInTx(c.Request.Context(), tx, table, styleNo, request.Header); err != nil {
				return err
			}
			for _, category := range categories {
				if _, err := tx.ExecContext(c.Request.Context(),
					"DELETE FROM "+qualifiedTable(category, table)+" WHERE style_no = $1", styleNo); err != nil {
					return fmt.Errorf("替换 %s 物料失败: %w", category, err)
				}
			}
			for _, item := range request.Rows {
				color, err := encodeStringArray(item.Color)
				if err != nil {
					return err
				}
				process, err := encodeStringArray(item.Process)
				if err != nil {
					return err
				}
				_, err = tx.ExecContext(c.Request.Context(), `
					INSERT INTO `+qualifiedTable(item.Category, table)+` (
						style_no, no, parts, material_description, unit, remark, supplier, color, process
					) VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9::jsonb)
				`, styleNo, item.No, item.Parts, item.Description, item.Unit,
					item.Remark, item.Supplier, string(color), string(process))
				if err != nil {
					return fmt.Errorf("导入 %s 物料行失败: %w", item.Category, err)
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
			SELECT id, no, COALESCE(parts, ''), COALESCE(material_description, ''),
				COALESCE(unit, ''), COALESCE(remark, ''), COALESCE(supplier, ''),
				COALESCE(color, '[]'::jsonb), COALESCE(process, '[]'::jsonb)
			FROM `+qualifiedTable(category, table)+`
			WHERE style_no = $1
			ORDER BY id
		`, styleNo)
		if err != nil {
			return nil, fmt.Errorf("读取 %s 分类失败: %w", category, err)
		}
		for rows.Next() {
			var item bomItem
			var itemNo sql.NullInt64
			var colorsRaw, processRaw []byte
			if err := rows.Scan(&item.ItemID, &itemNo, &item.Parts, &item.Description,
				&item.Unit, &item.Remark, &item.Supplier, &colorsRaw, &processRaw); err != nil {
				rows.Close()
				return nil, fmt.Errorf("读取 %s 物料失败: %w", category, err)
			}
			if itemNo.Valid {
				itemNoValue := int(itemNo.Int64)
				item.No = &itemNoValue
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
