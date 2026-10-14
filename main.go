package main

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	_ "github.com/go-sql-driver/mysql"
	"github.com/joho/godotenv"
)

var childTableNames = []string{
	"bom_parts",
	"bom_item",
	"bom_colorway",
	"bom_item_color_detail",
}

var childTablePattern = regexp.MustCompile(`\b(bom_item_color_detail|bom_colorway|bom_parts|bom_item)\b`)
var styleNoPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

const unassignedStyleTable = "_UNASSIGNED_"

type database struct {
	*sql.DB
	defaultSchema string
	styleTable    string
}

type transaction struct {
	*sql.Tx
	db *database
}

const (
	defaultBOMListLimit = 50
	maxBOMListLimit     = 100
)

func qualifySQL(query string) string {
	return childTablePattern.ReplaceAllStringFunc(query, func(table string) string {
		return "`" + table + "`.`" + table + "`"
	})
}

func qualifySQLForStyle(query, styleTable string) string {
	return childTablePattern.ReplaceAllStringFunc(query, func(table string) string {
		if styleTable != "" {
			switch table {
			case "bom_item", "bom_colorway", "bom_item_color_detail":
				return fmt.Sprintf("`%s`.`%s`", schemaForTable("", table), styleTable)
			}
		}
		return "`" + table + "`.`" + table + "`"
	})
}

func (db *database) schemaForTable(table string) string {
	return schemaForTable(db.defaultSchema, table)
}

func (db *database) tableForTable(table string) string {
	if db.styleTable != "" {
		switch table {
		case "bom_item", "bom_colorway", "bom_item_color_detail":
			return db.styleTable
		}
	}
	return table
}

func (db *database) constraintName(name string) string {
	if db.styleTable == "" {
		return name
	}
	hash := sha256.Sum256([]byte(db.styleTable))
	return fmt.Sprintf("%s_%x", name, hash[:8])
}

func (db *database) forStyle(styleNo string) *database {
	return &database{DB: db.DB, defaultSchema: db.defaultSchema, styleTable: styleTableName(styleNo)}
}

func (db *database) Exec(query string, args ...any) (sql.Result, error) {
	return db.DB.Exec(qualifySQLForStyle(query, db.styleTable), args...)
}

func (db *database) Query(query string, args ...any) (*sql.Rows, error) {
	return db.DB.Query(qualifySQLForStyle(query, db.styleTable), args...)
}

func (db *database) QueryRow(query string, args ...any) *sql.Row {
	return db.DB.QueryRow(qualifySQLForStyle(query, db.styleTable), args...)
}

func (db *database) Begin() (*transaction, error) {
	tx, err := db.DB.Begin()
	if err != nil {
		return nil, err
	}
	return &transaction{Tx: tx, db: db}, nil
}

func (tx *transaction) Exec(query string, args ...any) (sql.Result, error) {
	return tx.Tx.Exec(qualifySQLForStyle(query, tx.db.styleTable), args...)
}

func (tx *transaction) Query(query string, args ...any) (*sql.Rows, error) {
	return tx.Tx.Query(qualifySQLForStyle(query, tx.db.styleTable), args...)
}

func (tx *transaction) QueryRow(query string, args ...any) *sql.Row {
	return tx.Tx.QueryRow(qualifySQLForStyle(query, tx.db.styleTable), args...)
}

func schemaForTable(defaultSchema, table string) string {
	for _, childTable := range childTableNames {
		if table == childTable {
			return childTable
		}
	}
	return defaultSchema
}

func migrateTablesToOwnSchemas(db *database) error {
	sourceSchema := db.defaultSchema
	type tableState struct {
		name         string
		sourceExists bool
		targetExists bool
	}
	states := make([]tableState, 0, len(childTableNames))
	for _, table := range childTableNames {
		var sourceCount, targetCount int
		if err := db.DB.QueryRow(`
			SELECT COUNT(*) FROM information_schema.tables
			WHERE table_schema = ? AND table_name = ?
		`, sourceSchema, table).Scan(&sourceCount); err != nil {
			return err
		}
		if err := db.DB.QueryRow(`
			SELECT COUNT(*) FROM information_schema.tables
			WHERE table_schema = ? AND table_name = ?
		`, table, table).Scan(&targetCount); err != nil {
			return err
		}
		if sourceCount > 0 && targetCount > 0 {
			return fmt.Errorf("源数据库 %s 和目标数据库 %s 同时存在表 %s，已停止以避免覆盖数据", sourceSchema, table, table)
		}
		states = append(states, tableState{
			name:         table,
			sourceExists: sourceCount > 0,
			targetExists: targetCount > 0,
		})
	}

	for _, state := range states {
		if state.sourceExists && !state.targetExists {
			if _, err := db.DB.Exec("CREATE DATABASE IF NOT EXISTS `" + state.name + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
				return fmt.Errorf("创建数据库 %s 失败: %w", state.name, err)
			}
		}
	}

	renames := make([]string, 0, len(states))
	for _, state := range states {
		if state.sourceExists && !state.targetExists {
			renames = append(renames, fmt.Sprintf("`%s`.`%s` TO `%s`.`%s`",
				sourceSchema, state.name, state.name, state.name))
		}
	}
	if len(renames) > 0 {
		if _, err := db.DB.Exec("RENAME TABLE " + strings.Join(renames, ", ")); err != nil {
			return fmt.Errorf("迁移实体数据库中的旧表失败: %w", err)
		}
	}
	return nil
}

func tableExistsInSchema(db *database, schema, table string) (bool, error) {
	var exists bool
	err := db.DB.QueryRow(`
		SELECT EXISTS(
			SELECT 1 FROM information_schema.tables
			WHERE table_schema = ? AND table_name = ?
		)
	`, schema, table).Scan(&exists)
	return exists, err
}

func sharedBOMTablesPresent(db *database) (bool, error) {
	tables := []string{"bom_item", "bom_colorway", "bom_item_color_detail"}
	existsCount := 0
	for _, table := range tables {
		exists, err := tableExistsInSchema(db, table, table)
		if err != nil {
			return false, err
		}
		if exists {
			existsCount++
		}
	}
	if existsCount != 0 && existsCount != len(tables) {
		return false, fmt.Errorf("共享 BOM 迁移表不完整，需检查 bom_item、bom_colorway 和 bom_item_color_detail")
	}
	return existsCount == len(tables), nil
}

func styleTableExists(db *database, styleDB *database, logicalTable string) (bool, error) {
	return tableExistsInSchema(db, styleDB.schemaForTable(logicalTable), styleDB.tableForTable(logicalTable))
}

func ensureStyleTables(db *database, styleNo string, templateTable string) (*database, error) {
	styleDB, _, err := ensureStyleTablesWithCreated(db, styleNo, templateTable)
	return styleDB, err
}

func ensureStyleTablesWithCreated(db *database, styleNo string, templateTable string) (*database, []string, error) {
	styleDB := db.forStyle(styleNo)
	createdTables := make([]string, 0, 3)
	for _, logicalTable := range []string{"bom_item", "bom_colorway", "bom_item_color_detail"} {
		exists, err := styleTableExists(db, styleDB, logicalTable)
		if err != nil {
			return styleDB, createdTables, err
		}
		if exists {
			continue
		}
		templateDB := db.forStyle(templateTable)
		templateExists, err := styleTableExists(db, templateDB, logicalTable)
		if err != nil {
			return styleDB, createdTables, err
		}
		sourceTable := templateDB.tableForTable(logicalTable)
		if !templateExists {
			sourceTable = logicalTable
			templateExists, err = tableExistsInSchema(db, schemaForTable(db.defaultSchema, logicalTable), sourceTable)
			if err != nil {
				return styleDB, createdTables, err
			}
		}
		if !templateExists {
			return styleDB, createdTables, fmt.Errorf("缺少创建 Style No. 数据表的模板 %s.%s", schemaForTable(db.defaultSchema, logicalTable), sourceTable)
		}
		if _, err := db.DB.Exec(fmt.Sprintf("CREATE TABLE `%s`.`%s` LIKE `%s`.`%s`",
			styleDB.schemaForTable(logicalTable), styleDB.tableForTable(logicalTable),
			schemaForTable(db.defaultSchema, logicalTable), sourceTable)); err != nil {
			return styleDB, createdTables, fmt.Errorf("创建 Style No. 数据表 %s.%s 失败: %w",
				styleDB.schemaForTable(logicalTable), styleDB.tableForTable(logicalTable), err)
		}
		createdTables = append(createdTables, logicalTable)
	}
	if err := ensureColorwayProcessColumn(styleDB); err != nil {
		return styleDB, createdTables, fmt.Errorf("迁移配色列类型字段失败: %w", err)
	}
	for _, relation := range []struct {
		table, column, parentTable, constraint string
	}{
		{"bom_item_color_detail", "bom_item_id", "bom_item", "fk_bom_detail_item"},
		{"bom_item_color_detail", "colorway_id", "bom_colorway", "fk_bom_detail_colorway"},
	} {
		if err := ensureSingleColumnForeignKey(styleDB, relation.table, relation.column, relation.parentTable, relation.constraint); err != nil {
			return styleDB, createdTables, fmt.Errorf("创建 Style No. 数据表外键 %s.%s 失败: %w", styleDB.tableForTable(relation.table), relation.column, err)
		}
	}
	return styleDB, createdTables, nil
}

func ensureColorwayProcessColumn(db *database) error {
	exists, err := columnExists(db, "bom_colorway", "has_process")
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	_, err = db.DB.Exec(fmt.Sprintf(
		"ALTER TABLE `%s`.`%s` ADD COLUMN `has_process` TINYINT(1) NOT NULL DEFAULT 1",
		db.schemaForTable("bom_colorway"), db.tableForTable("bom_colorway"),
	))
	return err
}

func dropCreatedStyleTables(db *database, styleDB *database, tables []string) error {
	var firstErr error
	for i := len(tables) - 1; i >= 0; i-- {
		table := tables[i]
		if _, err := db.DB.Exec(fmt.Sprintf("DROP TABLE `%s`.`%s`",
			styleDB.schemaForTable(table), styleDB.tableForTable(table))); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("清理新增失败的 Style No. 数据表 %s.%s 失败: %w",
				styleDB.schemaForTable(table), styleDB.tableForTable(table), err)
		}
	}
	return firstErr
}

func renameStyleTablesWithinSchemas(db *database, styleDB *database, sourceNames, targetNames map[string]string) error {
	renames := make([]string, 0, len(targetNames))
	for _, logicalTable := range []string{"bom_item", "bom_colorway", "bom_item_color_detail"} {
		targetName, ok := targetNames[logicalTable]
		if !ok {
			return fmt.Errorf("缺少 %s 的目标表名", logicalTable)
		}
		sourceName, ok := sourceNames[logicalTable]
		if !ok {
			sourceName = styleDB.tableForTable(logicalTable)
		}
		renames = append(renames, fmt.Sprintf("`%s`.`%s` TO `%s`.`%s`",
			styleDB.schemaForTable(logicalTable), sourceName,
			styleDB.schemaForTable(logicalTable), targetName))
	}
	if _, err := db.DB.Exec("RENAME TABLE " + strings.Join(renames, ", ")); err != nil {
		return fmt.Errorf("重命名 Style No. 数据表失败: %w", err)
	}
	return nil
}

func temporaryStyleTableNames(token string) map[string]string {
	return map[string]string{
		"bom_item":              "__deleted_" + token + "_item",
		"bom_colorway":          "__deleted_" + token + "_colorway",
		"bom_item_color_detail": "__deleted_" + token + "_detail",
	}
}

func ensureStyleTablesForHeader(db *database, headerID int) (*database, error) {
	styleNo, err := styleForHeader(db, headerID)
	if err != nil {
		return nil, err
	}
	styleDB := db.forStyle(styleNo)
	for _, table := range []string{"bom_item", "bom_colorway", "bom_item_color_detail"} {
		exists, err := styleTableExists(db, styleDB, table)
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, fmt.Errorf("Style No. 数据表 %s.%s 不存在", styleDB.schemaForTable(table), styleDB.tableForTable(table))
		}
	}
	if err := ensureColorwayProcessColumn(styleDB); err != nil {
		return nil, fmt.Errorf("迁移 Style No. 配色列类型字段失败: %w", err)
	}
	return styleDB, nil
}

func ensureStyleTablesForImport(db *database, styleNo string) (*database, error) {
	if err := validateStyleNo(styleNo); err != nil {
		return nil, err
	}
	var existingStyleNo string
	err := db.QueryRow("SELECT style_no FROM bom_header WHERE LOWER(style_no) = LOWER(?)", styleNo).Scan(&existingStyleNo)
	headerExists := err == nil
	if err == nil {
		styleNo = existingStyleNo
	} else if err != sql.ErrNoRows {
		return nil, fmt.Errorf("检查 Style No. 主记录失败: %w", err)
	}
	styleDB := db.forStyle(styleNo)
	allExist := true
	anyExists := false
	for _, table := range []string{"bom_item", "bom_colorway", "bom_item_color_detail"} {
		exists, err := styleTableExists(db, styleDB, table)
		if err != nil {
			return nil, err
		}
		allExist = allExist && exists
		anyExists = anyExists || exists
	}
	if allExist {
		if err := validateStyleTableData(db, styleDB.styleTable); err != nil {
			return nil, err
		}
		return ensureStyleTables(db, styleNo, unassignedStyleTable)
	}
	if anyExists {
		return nil, fmt.Errorf("Style No. %s 的数据表不完整，拒绝导入以避免覆盖数据", styleDB.styleTable)
	}
	if headerExists {
		return nil, fmt.Errorf("Style No. %s 的数据表不存在，拒绝覆盖现有鞋款数据", styleDB.styleTable)
	}
	templateTable, err := findStyleTableTemplate(db, styleDB.styleTable)
	if err != nil {
		return nil, err
	}
	return ensureStyleTables(db, styleNo, templateTable)
}

func findStyleTableTemplate(db *database, excludedTable string) (string, error) {
	rows, err := db.DB.Query("SELECT COALESCE(style_no, '') FROM bom_header ORDER BY id")
	if err != nil {
		return "", fmt.Errorf("查找 Style No. 数据表模板失败: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var styleNo string
		if err := rows.Scan(&styleNo); err != nil {
			return "", fmt.Errorf("读取 Style No. 数据表模板失败: %w", err)
		}
		templateDB := db.forStyle(styleNo)
		if strings.EqualFold(templateDB.styleTable, excludedTable) {
			continue
		}
		allExist := true
		for _, table := range []string{"bom_item", "bom_colorway", "bom_item_color_detail"} {
			exists, err := styleTableExists(db, templateDB, table)
			if err != nil {
				return "", err
			}
			allExist = allExist && exists
		}
		if allExist {
			return templateDB.styleTable, nil
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("读取 Style No. 数据表模板失败: %w", err)
	}
	return "", nil
}

func renameStyleTables(db *database, oldStyleNo, newStyleNo string) error {
	oldDB := db.forStyle(oldStyleNo)
	newDB := db.forStyle(newStyleNo)
	if oldDB.styleTable == "" || newDB.styleTable == "" {
		return fmt.Errorf("Style No. 不符合表名规则")
	}
	if oldDB.styleTable == newDB.styleTable {
		return nil
	}
	renames := make([]string, 0, 3)
	for _, logicalTable := range []string{"bom_item", "bom_colorway", "bom_item_color_detail"} {
		oldExists, err := styleTableExists(db, oldDB, logicalTable)
		if err != nil {
			return err
		}
		if !oldExists {
			return fmt.Errorf("原 Style No. 数据表 %s.%s 不存在", oldDB.schemaForTable(logicalTable), oldDB.tableForTable(logicalTable))
		}
		newExists, err := styleTableExists(db, newDB, logicalTable)
		if err != nil {
			return err
		}
		if newExists {
			return fmt.Errorf("目标 Style No. 数据表 %s.%s 已存在", newDB.schemaForTable(logicalTable), newDB.tableForTable(logicalTable))
		}
		renames = append(renames, fmt.Sprintf("`%s`.`%s` TO `%s`.`%s`",
			oldDB.schemaForTable(logicalTable), oldDB.tableForTable(logicalTable),
			newDB.schemaForTable(logicalTable), newDB.tableForTable(logicalTable)))
	}
	if _, err := db.DB.Exec("RENAME TABLE " + strings.Join(renames, ", ")); err != nil {
		return fmt.Errorf("同步重命名 Style No. 数据表失败: %w", err)
	}
	return nil
}

func moveHeaderBetweenStyleTables(db *database, headerID int, sourceStyleNo, targetStyleNo string) error {
	sourceDB := db.forStyle(sourceStyleNo)
	targetDB := db.forStyle(targetStyleNo)
	if sourceDB.styleTable == targetDB.styleTable {
		return nil
	}
	if targetStyleNo != "" {
		for _, table := range []string{"bom_item", "bom_colorway", "bom_item_color_detail"} {
			exists, err := styleTableExists(db, targetDB, table)
			if err != nil {
				return err
			}
			if exists {
				return fmt.Errorf("目标 Style No. 数据表 %s.%s 已存在", targetDB.schemaForTable(table), targetDB.tableForTable(table))
			}
		}
	}
	if _, err := ensureStyleTables(db, targetStyleNo, sourceDB.styleTable); err != nil {
		return err
	}
	if err := validateStyleTableData(db, targetDB.styleTable); err != nil {
		return err
	}
	var targetItems, targetColors int
	if err := db.DB.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM `%s`.`%s` WHERE header_id = ?",
		targetDB.schemaForTable("bom_item"), targetDB.tableForTable("bom_item")), headerID).Scan(&targetItems); err != nil {
		return err
	}
	if err := db.DB.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM `%s`.`%s` WHERE header_id = ?",
		targetDB.schemaForTable("bom_colorway"), targetDB.tableForTable("bom_colorway")), headerID).Scan(&targetColors); err != nil {
		return err
	}
	if targetItems != 0 || targetColors != 0 {
		return fmt.Errorf("目标 Style No. 数据表已包含该鞋款数据")
	}
	tx, err := db.DB.Begin()
	if err != nil {
		return fmt.Errorf("开始迁移鞋款表数据失败: %w", err)
	}
	rollback := func(cause error) error {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && rollbackErr != sql.ErrTxDone {
			return fmt.Errorf("%w; 回滚失败: %v", cause, rollbackErr)
		}
		return cause
	}
	for _, table := range []string{"bom_item", "bom_colorway"} {
		query := fmt.Sprintf("INSERT INTO `%s`.`%s` SELECT * FROM `%s`.`%s` WHERE header_id = ?",
			targetDB.schemaForTable(table), targetDB.tableForTable(table),
			sourceDB.schemaForTable(table), sourceDB.tableForTable(table))
		if _, err := tx.Exec(query, headerID); err != nil {
			return rollback(fmt.Errorf("迁移 %s 数据失败: %w", table, err))
		}
	}
	detailQuery := fmt.Sprintf(
		"INSERT INTO `%s`.`%s` SELECT d.* FROM `%s`.`%s` d "+
			"JOIN `%s`.`%s` i ON i.id = d.bom_item_id WHERE i.header_id = ?",
		targetDB.schemaForTable("bom_item_color_detail"), targetDB.tableForTable("bom_item_color_detail"),
		sourceDB.schemaForTable("bom_item_color_detail"), sourceDB.tableForTable("bom_item_color_detail"),
		sourceDB.schemaForTable("bom_item"), sourceDB.tableForTable("bom_item"),
	)
	if _, err := tx.Exec(detailQuery, headerID); err != nil {
		return rollback(fmt.Errorf("迁移物料配色明细失败: %w", err))
	}
	for _, table := range []string{"bom_item", "bom_colorway"} {
		var sourceCount, targetCount int
		if err := tx.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM `%s`.`%s` WHERE header_id = ?",
			sourceDB.schemaForTable(table), sourceDB.tableForTable(table)), headerID).Scan(&sourceCount); err != nil {
			return rollback(err)
		}
		if err := tx.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM `%s`.`%s` WHERE header_id = ?",
			targetDB.schemaForTable(table), targetDB.tableForTable(table)), headerID).Scan(&targetCount); err != nil {
			return rollback(err)
		}
		if sourceCount != targetCount {
			return rollback(fmt.Errorf("迁移 %s 数据数量不一致: source=%d target=%d", table, sourceCount, targetCount))
		}
	}
	if err := assertBOMTableCopyMatches(tx, sourceDB, targetDB, headerID); err != nil {
		return rollback(err)
	}
	for _, statement := range []string{
		fmt.Sprintf("DELETE d FROM `%s`.`%s` d JOIN `%s`.`%s` i ON i.id = d.bom_item_id WHERE i.header_id = ?",
			sourceDB.schemaForTable("bom_item_color_detail"), sourceDB.tableForTable("bom_item_color_detail"),
			sourceDB.schemaForTable("bom_item"), sourceDB.tableForTable("bom_item")),
		fmt.Sprintf("DELETE FROM `%s`.`%s` WHERE header_id = ?", sourceDB.schemaForTable("bom_item"), sourceDB.tableForTable("bom_item")),
		fmt.Sprintf("DELETE FROM `%s`.`%s` WHERE header_id = ?", sourceDB.schemaForTable("bom_colorway"), sourceDB.tableForTable("bom_colorway")),
	} {
		if _, err := tx.Exec(statement, headerID); err != nil {
			return rollback(fmt.Errorf("清理原 Style No. 表数据失败: %w", err))
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交鞋款表数据迁移失败: %w", err)
	}
	return nil
}

func assertBOMTableCopyMatches(queryer interface {
	QueryRow(string, ...any) *sql.Row
}, sourceDB, targetDB *database, headerID int) error {
	checks := []struct {
		name, query string
		args        []any
	}{
		{
			"bom_item",
			fmt.Sprintf(
				"SELECT COUNT(*) FROM `%s`.`%s` s LEFT JOIN `%s`.`%s` t ON t.id = s.id "+
					"WHERE s.header_id = ? AND (t.id IS NULL OR NOT "+
					"(s.header_id <=> t.header_id AND s.part_name <=> t.part_name AND s.item_seq <=> t.item_seq "+
					"AND s.display_order <=> t.display_order AND s.material_desc <=> t.material_desc AND s.unit <=> t.unit))",
				sourceDB.schemaForTable("bom_item"), sourceDB.tableForTable("bom_item"),
				targetDB.schemaForTable("bom_item"), targetDB.tableForTable("bom_item"),
			),
			[]any{headerID},
		},
		{
			"bom_colorway",
			fmt.Sprintf(
				"SELECT COUNT(*) FROM `%s`.`%s` s LEFT JOIN `%s`.`%s` t ON t.id = s.id "+
					"WHERE s.header_id = ? AND (t.id IS NULL OR NOT "+
					"(s.header_id <=> t.header_id AND s.color_code <=> t.color_code AND s.display_order <=> t.display_order))",
				sourceDB.schemaForTable("bom_colorway"), sourceDB.tableForTable("bom_colorway"),
				targetDB.schemaForTable("bom_colorway"), targetDB.tableForTable("bom_colorway"),
			),
			[]any{headerID},
		},
		{
			"bom_item_color_detail",
			fmt.Sprintf(
				"SELECT COUNT(*) FROM `%s`.`%s` s "+
					"JOIN `%s`.`%s` i ON i.id = s.bom_item_id AND i.header_id = ? "+
					"LEFT JOIN `%s`.`%s` t ON t.bom_item_id = s.bom_item_id AND t.colorway_id = s.colorway_id "+
					"WHERE t.bom_item_id IS NULL OR NOT "+
					"(s.material_color_spec <=> t.material_color_spec AND s.process_desc <=> t.process_desc)",
				sourceDB.schemaForTable("bom_item_color_detail"), sourceDB.tableForTable("bom_item_color_detail"),
				sourceDB.schemaForTable("bom_item"), sourceDB.tableForTable("bom_item"),
				targetDB.schemaForTable("bom_item_color_detail"), targetDB.tableForTable("bom_item_color_detail"),
			),
			[]any{headerID},
		},
	}
	for _, check := range checks {
		var differences int
		if err := queryer.QueryRow(check.query, check.args...).Scan(&differences); err != nil {
			return fmt.Errorf("比较迁移前后的 %s 数据失败: %w", check.name, err)
		}
		if differences > 0 {
			return fmt.Errorf("%s 有 %d 条记录在迁移后字段内容不一致", check.name, differences)
		}
	}
	return nil
}

func migrateSharedBOMTablesByStyle(db *database) error {
	var styles []struct {
		headerID int
		styleNo  string
	}
	rows, err := db.DB.Query("SELECT id, COALESCE(style_no, '') FROM bom_header ORDER BY id")
	if err != nil {
		return fmt.Errorf("读取鞋款 Style No. 失败: %w", err)
	}
	for rows.Next() {
		var style struct {
			headerID int
			styleNo  string
		}
		if err := rows.Scan(&style.headerID, &style.styleNo); err != nil {
			rows.Close()
			return fmt.Errorf("读取鞋款 Style No. 失败: %w", err)
		}
		if style.styleNo != "" {
			if err := validateStyleNo(style.styleNo); err != nil {
				rows.Close()
				return err
			}
		}
		styles = append(styles, style)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("读取鞋款 Style No. 失败: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("关闭鞋款 Style No. 查询失败: %w", err)
	}

	seenTables := make(map[string]int)
	for _, style := range styles {
		tableName := styleTableName(style.styleNo)
		key := styleTableKey(style.styleNo)
		if style.styleNo != "" {
			if previousID, exists := seenTables[key]; exists {
				return fmt.Errorf("鞋款 %d 和 %d 的 Style No. 对应同一数据表名 %s", previousID, style.headerID, tableName)
			}
			seenTables[key] = style.headerID
		}
		styleDB := db.forStyle(style.styleNo)
		anyExists, allExist := false, true
		for _, logicalTable := range []string{"bom_item", "bom_colorway", "bom_item_color_detail"} {
			exists, err := styleTableExists(db, styleDB, logicalTable)
			if err != nil {
				return err
			}
			anyExists = anyExists || exists
			allExist = allExist && exists
		}
		if anyExists && !allExist {
			return fmt.Errorf("Style No. 数据表 %s 不完整，停止迁移以避免覆盖", tableName)
		}
		if allExist {
			if err := validateStyleTableData(db, styleDB.styleTable); err != nil {
				return err
			}
		} else if _, err := ensureStyleTables(db, style.styleNo, ""); err != nil {
			return err
		}
		if err := ensureColorwayProcessColumn(styleDB); err != nil {
			return fmt.Errorf("迁移 Style No. %s 的配色列类型字段失败: %w", tableName, err)
		}

		tx, err := db.DB.Begin()
		if err != nil {
			return fmt.Errorf("开始迁移 Style No. %s 数据失败: %w", tableName, err)
		}
		for _, logicalTable := range []string{"bom_item", "bom_colorway"} {
			query := fmt.Sprintf("INSERT IGNORE INTO `%s`.`%s` SELECT * FROM `%s`.`%s` WHERE header_id = ?",
				styleDB.schemaForTable(logicalTable), styleDB.tableForTable(logicalTable),
				styleDB.schemaForTable(logicalTable), logicalTable)
			if _, err := tx.Exec(query, style.headerID); err != nil {
				tx.Rollback()
				return fmt.Errorf("迁移 Style No. %s 的 %s 数据失败: %w", tableName, logicalTable, err)
			}
		}
		detailQuery := fmt.Sprintf(
			"INSERT IGNORE INTO `%s`.`%s` "+
				"SELECT d.* FROM `%s`.`%s` d "+
				"JOIN `%s`.`%s` i ON i.id = d.bom_item_id WHERE i.header_id = ?",
			styleDB.schemaForTable("bom_item_color_detail"), styleDB.tableForTable("bom_item_color_detail"),
			styleDB.schemaForTable("bom_item_color_detail"), "bom_item_color_detail",
			styleDB.schemaForTable("bom_item"), "bom_item",
		)
		if _, err := tx.Exec(detailQuery, style.headerID); err != nil {
			tx.Rollback()
			return fmt.Errorf("迁移 Style No. %s 的物料配色明细失败: %w", tableName, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("提交 Style No. %s 数据迁移失败: %w", tableName, err)
		}
		if err := assertBOMTableCopyMatches(db.DB, db, styleDB, style.headerID); err != nil {
			return fmt.Errorf("校验 Style No. %s 的迁移数据失败: %w", tableName, err)
		}

		for _, logicalTable := range []string{"bom_item", "bom_colorway"} {
			var sourceCount, targetCount int
			if err := db.DB.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM `%s`.`%s` WHERE header_id = ?",
				styleDB.schemaForTable(logicalTable), logicalTable),
				style.headerID).Scan(&sourceCount); err != nil {
				return fmt.Errorf("校验 Style No. %s 的 %s 源记录失败: %w", tableName, logicalTable, err)
			}
			if err := db.DB.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM `%s`.`%s` WHERE header_id = ?",
				styleDB.schemaForTable(logicalTable), styleDB.tableForTable(logicalTable)),
				style.headerID).Scan(&targetCount); err != nil {
				return fmt.Errorf("校验 Style No. %s 的 %s 目标记录失败: %w", tableName, logicalTable, err)
			}
			if sourceCount != targetCount {
				return fmt.Errorf("Style No. %s 的 %s 迁移记录数不一致: source=%d target=%d", tableName, logicalTable, sourceCount, targetCount)
			}
		}
		var sourceDetails, targetDetails int
		if err := db.DB.QueryRow(
			"SELECT COUNT(*) FROM `bom_item_color_detail`.`bom_item_color_detail` d "+
				"JOIN `bom_item`.`bom_item` i ON i.id = d.bom_item_id WHERE i.header_id = ?",
			style.headerID,
		).Scan(&sourceDetails); err != nil {
			return fmt.Errorf("校验 Style No. %s 的明细源记录失败: %w", tableName, err)
		}
		if err := db.DB.QueryRow(fmt.Sprintf(
			"SELECT COUNT(*) FROM `%s`.`%s` d JOIN `%s`.`%s` i ON i.id = d.bom_item_id WHERE i.header_id = ?",
			styleDB.schemaForTable("bom_item_color_detail"), styleDB.tableForTable("bom_item_color_detail"),
			styleDB.schemaForTable("bom_item"), styleDB.tableForTable("bom_item"),
		), style.headerID).Scan(&targetDetails); err != nil {
			return fmt.Errorf("校验 Style No. %s 的明细目标记录失败: %w", tableName, err)
		}
		if sourceDetails != targetDetails {
			return fmt.Errorf("Style No. %s 的明细迁移记录数不一致: source=%d target=%d", tableName, sourceDetails, targetDetails)
		}
	}
	for _, style := range styles {
		if err := validateStyleTableData(db, styleTableName(style.styleNo)); err != nil {
			return err
		}
	}

	if _, err := db.DB.Exec(
		"DROP TABLE `bom_item_color_detail`.`bom_item_color_detail`, " +
			"`bom_item`.`bom_item`, `bom_colorway`.`bom_colorway`",
	); err != nil {
		return fmt.Errorf("清理共享迁移表失败: %w", err)
	}
	for _, style := range styles {
		styleDB := db.forStyle(style.styleNo)
		for _, relation := range []struct {
			table, column, parentTable, constraint string
		}{
			{"bom_item_color_detail", "bom_item_id", "bom_item", "fk_bom_detail_item"},
			{"bom_item_color_detail", "colorway_id", "bom_colorway", "fk_bom_detail_colorway"},
		} {
			if err := ensureSingleColumnForeignKey(styleDB, relation.table, relation.column, relation.parentTable, relation.constraint); err != nil {
				return fmt.Errorf("Style No. 数据表 %s 外键迁移失败: %w", styleDB.styleTable, err)
			}
		}
	}
	return nil
}

func migrateStyleSchemasToNamedTables(db *database) error {
	rows, err := db.DB.Query("SELECT id, COALESCE(style_no, '') FROM bom_header ORDER BY id")
	if err != nil {
		return fmt.Errorf("读取待迁移 Style No. 失败: %w", err)
	}
	type styleHeader struct {
		id      int
		styleNo string
	}
	var headers []styleHeader
	for rows.Next() {
		var header styleHeader
		if err := rows.Scan(&header.id, &header.styleNo); err != nil {
			rows.Close()
			return fmt.Errorf("读取待迁移 Style No. 失败: %w", err)
		}
		headers = append(headers, header)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("读取待迁移 Style No. 失败: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("关闭待迁移 Style No. 查询失败: %w", err)
	}

	for _, header := range headers {
		if header.styleNo != "" {
			if err := validateStyleNo(header.styleNo); err != nil {
				return err
			}
		}
		oldSchema := strings.ToLower(strings.TrimSpace(header.styleNo))
		if oldSchema == "" {
			oldSchema = unassignedStyleTable
		}
		styleDB := db.forStyle(header.styleNo)
		sourceCount, targetCount := 0, 0
		for _, table := range []string{"bom_item", "bom_colorway", "bom_item_color_detail"} {
			exists, err := tableExistsInSchema(db, oldSchema, table)
			if err != nil {
				return err
			}
			if exists {
				sourceCount++
			}
			exists, err = styleTableExists(db, styleDB, table)
			if err != nil {
				return err
			}
			if exists {
				targetCount++
			}
		}
		if sourceCount == 0 {
			continue
		}
		if sourceCount != 3 {
			return fmt.Errorf("旧版 Style No. 数据库 %s 的 BOM 表不完整，拒绝自动迁移", oldSchema)
		}
		if targetCount != 0 {
			return fmt.Errorf("旧版 Style No. 数据库 %s 与目标同名表同时存在，拒绝覆盖", oldSchema)
		}
		renames := make([]string, 0, 3)
		for _, logicalTable := range []string{"bom_item", "bom_colorway", "bom_item_color_detail"} {
			renames = append(renames, fmt.Sprintf("`%s`.`%s` TO `%s`.`%s`",
				oldSchema, logicalTable, styleDB.schemaForTable(logicalTable), styleDB.tableForTable(logicalTable)))
		}
		if _, err := db.DB.Exec("RENAME TABLE " + strings.Join(renames, ", ")); err != nil {
			return fmt.Errorf("迁移旧版 Style No. 数据库 %s 的数据表失败: %w", oldSchema, err)
		}
		for _, relation := range []struct {
			table, column, parentTable, constraint string
		}{
			{"bom_item_color_detail", "bom_item_id", "bom_item", "fk_bom_detail_item"},
			{"bom_item_color_detail", "colorway_id", "bom_colorway", "fk_bom_detail_colorway"},
		} {
			if err := ensureSingleColumnForeignKey(styleDB, relation.table, relation.column, relation.parentTable, relation.constraint); err != nil {
				return fmt.Errorf("迁移 Style No. %s 的外键 %s.%s 失败: %w", styleDB.styleTable, relation.table, relation.column, err)
			}
		}
		if err := validateStyleTableData(db, styleDB.styleTable); err != nil {
			return err
		}
	}
	return nil
}

func validateStyleTables(db *database) error {
	rows, err := db.Query("SELECT id, COALESCE(style_no, '') FROM bom_header ORDER BY id")
	if err != nil {
		return fmt.Errorf("读取鞋款 Style No. 失败: %w", err)
	}
	defer rows.Close()
	seenTables := make(map[string]int)
	for rows.Next() {
		var headerID int
		var styleNo string
		if err := rows.Scan(&headerID, &styleNo); err != nil {
			return fmt.Errorf("读取鞋款 Style No. 失败: %w", err)
		}
		if styleNo != "" {
			if err := validateStyleNo(styleNo); err != nil {
				return err
			}
		}
		tableName := styleTableName(styleNo)
		key := styleTableKey(styleNo)
		if styleNo != "" {
			if existingID, exists := seenTables[key]; exists {
				return fmt.Errorf("鞋款 %d 和 %d 的 Style No. 对应同一数据表名 %s", existingID, headerID, tableName)
			}
			seenTables[key] = headerID
		}
		styleDB, err := ensureStyleTablesForHeader(db, headerID)
		if err != nil {
			return err
		}
		if err := validateStyleTableData(db, styleDB.styleTable); err != nil {
			return err
		}
		for _, relation := range []struct {
			table, column, parentTable, constraint string
		}{
			{"bom_item_color_detail", "bom_item_id", "bom_item", "fk_bom_detail_item"},
			{"bom_item_color_detail", "colorway_id", "bom_colorway", "fk_bom_detail_colorway"},
		} {
			if err := ensureSingleColumnForeignKey(styleDB, relation.table, relation.column, relation.parentTable, relation.constraint); err != nil {
				return fmt.Errorf("校验 Style No. 数据表 %s 外键失败: %w", styleDB.styleTable, err)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("读取鞋款 Style No. 失败: %w", err)
	}
	return nil
}

func validateStyleTableData(db *database, tableName string) error {
	expectedStyleNo := strings.ToLower(tableName)
	if strings.EqualFold(tableName, unassignedStyleTable) {
		expectedStyleNo = ""
	}
	for _, table := range []string{"bom_item", "bom_colorway"} {
		var invalidRows int
		query := fmt.Sprintf(
			"SELECT COUNT(*) FROM `%s`.`%s` t LEFT JOIN bom_header h ON h.id = t.header_id "+
				"WHERE h.id IS NULL OR LOWER(COALESCE(h.style_no, '')) <> ?",
			schemaForTable(db.defaultSchema, table), tableName,
		)
		if err := db.DB.QueryRow(query, expectedStyleNo).Scan(&invalidRows); err != nil {
			return fmt.Errorf("校验 Style No. 数据表 %s.%s 归属失败: %w", schemaForTable(db.defaultSchema, table), tableName, err)
		}
		if invalidRows > 0 {
			return fmt.Errorf("Style No. 数据表 %s.%s 存在 %d 条未归属或归属错误的数据", schemaForTable(db.defaultSchema, table), tableName, invalidRows)
		}
	}
	var crossStyleDetails int
	detailQuery := fmt.Sprintf(
		"SELECT COUNT(*) FROM `%s`.`%s` d "+
			"JOIN `%s`.`%s` i ON i.id = d.bom_item_id "+
			"JOIN `%s`.`%s` cw ON cw.id = d.colorway_id "+
			"WHERE i.header_id <> cw.header_id",
		schemaForTable(db.defaultSchema, "bom_item_color_detail"), tableName,
		schemaForTable(db.defaultSchema, "bom_item"), tableName,
		schemaForTable(db.defaultSchema, "bom_colorway"), tableName,
	)
	if err := db.DB.QueryRow(detailQuery).Scan(&crossStyleDetails); err != nil {
		return fmt.Errorf("校验 Style No. 数据表 %s 明细归属失败: %w", tableName, err)
	}
	if crossStyleDetails > 0 {
		return fmt.Errorf("Style No. 数据表 %s 存在 %d 条跨鞋款物料配色明细", tableName, crossStyleDetails)
	}
	return nil
}

func resolveHeaderID(db *database, requestedID string) (int, error) {
	if requestedID == "" {
		requestedID = "1"
	}
	headerID, err := strconv.Atoi(requestedID)
	if err != nil || headerID <= 0 {
		return 0, fmt.Errorf("无效的 BOM 主记录编号")
	}
	var exists int
	if err := db.QueryRow("SELECT id FROM bom_header WHERE id = ?", headerID).Scan(&exists); err != nil {
		return 0, err
	}
	return exists, nil
}

func styleTableName(styleNo string) string {
	styleNo = strings.TrimSpace(styleNo)
	if styleNo == "" {
		return unassignedStyleTable
	}
	if !validStyleTableName(styleNo) {
		return ""
	}
	return styleNo
}

func styleTableKey(styleNo string) string {
	return strings.ToLower(styleTableName(styleNo))
}

func validateStyleNo(styleNo string) error {
	styleNo = strings.TrimSpace(styleNo)
	if !validStyleTableName(styleNo) {
		return fmt.Errorf("Style No. 必须为 1–64 位英文字母、数字、下划线或连字符，且不能与系统表名冲突")
	}
	return nil
}

func validStyleTableName(styleNo string) bool {
	if !styleNoPattern.MatchString(styleNo) {
		return false
	}
	switch strings.ToLower(styleNo) {
	case strings.ToLower(unassignedStyleTable), "bom_parts", "bom_item", "bom_colorway", "bom_item_color_detail":
		return false
	default:
		return true
	}
}

func styleForHeader(db *database, headerID int) (string, error) {
	var styleNo string
	err := db.QueryRow("SELECT COALESCE(style_no, '') FROM bom_header WHERE id = ?", headerID).Scan(&styleNo)
	if err != nil {
		return "", err
	}
	tableName := styleTableName(styleNo)
	if tableName == "" {
		return "", fmt.Errorf("鞋款 Style No. 不符合数据表命名规则")
	}
	return tableName, nil
}

func scopedDatabase(db *database, headerID int) (*database, error) {
	styleNo, err := styleForHeader(db, headerID)
	if err != nil {
		return nil, err
	}
	return db.forStyle(styleNo), nil
}

func ensureUniqueIndex(db *database, table, index, columns, duplicateQuery string) error {
	wantedColumns := splitIndexColumns(columns)
	exists, err := hasIndex(db, table, wantedColumns, true, true)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	if duplicateQuery != "" {
		var duplicates int
		if err := db.QueryRow(duplicateQuery).Scan(&duplicates); err != nil {
			return err
		}
		if duplicates > 0 {
			return fmt.Errorf("表 %s 存在重复关联数据，无法创建唯一索引 %s", table, index)
		}
	}
	_, err = db.DB.Exec(fmt.Sprintf("ALTER TABLE `%s`.`%s` ADD UNIQUE INDEX `%s` (%s)",
		db.schemaForTable(table), db.tableForTable(table), index, columns))
	return err
}

func ensureIndex(db *database, table, index, columns string) error {
	exists, err := hasIndex(db, table, splitIndexColumns(columns), false, false)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	_, err = db.DB.Exec(fmt.Sprintf("ALTER TABLE `%s`.`%s` ADD INDEX `%s` (%s)",
		db.schemaForTable(table), db.tableForTable(table), index, columns))
	return err
}

func dropIndex(db *database, table, index string) error {
	var exists bool
	err := db.DB.QueryRow(`
		SELECT EXISTS(
			SELECT 1
			FROM information_schema.statistics
			WHERE table_schema = ? AND table_name = ? AND index_name = ?
		)
	`, db.schemaForTable(table), db.tableForTable(table), index).Scan(&exists)
	if err != nil || !exists {
		return err
	}
	_, err = db.DB.Exec(fmt.Sprintf("ALTER TABLE `%s`.`%s` DROP INDEX `%s`",
		db.schemaForTable(table), db.tableForTable(table), index))
	return err
}

type indexColumn struct {
	name       string
	prefixSize sql.NullInt64
}

func splitIndexColumns(columns string) []string {
	parts := strings.Split(columns, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func indexHasColumns(actual []indexColumn, wanted []string, requireUnique, actualUnique, exact bool) bool {
	if len(actual) < len(wanted) || (exact && len(actual) != len(wanted)) {
		return false
	}
	for i, column := range wanted {
		if actual[i].name != column || actual[i].prefixSize.Valid {
			return false
		}
	}
	return !requireUnique || actualUnique
}

func hasIndex(db *database, table string, wanted []string, unique, exact bool) (bool, error) {
	rows, err := db.DB.Query(`
		SELECT index_name, column_name, sub_part, non_unique
		FROM information_schema.statistics
		WHERE table_schema = ? AND table_name = ?
		ORDER BY index_name, seq_in_index
	`, db.schemaForTable(table), db.tableForTable(table))
	if err != nil {
		return false, err
	}
	defer rows.Close()

	var currentName string
	var currentColumns []indexColumn
	var currentUnique bool
	for rows.Next() {
		var name, column string
		var prefixSize sql.NullInt64
		var nonUnique int
		if err := rows.Scan(&name, &column, &prefixSize, &nonUnique); err != nil {
			return false, err
		}
		if currentName != "" && currentName != name {
			if indexHasColumns(currentColumns, wanted, unique, currentUnique, exact) {
				return true, nil
			}
			currentColumns = currentColumns[:0]
		}
		currentName = name
		currentUnique = nonUnique == 0
		currentColumns = append(currentColumns, indexColumn{name: column, prefixSize: prefixSize})
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	if indexHasColumns(currentColumns, wanted, unique, currentUnique, exact) {
		return true, nil
	}
	return false, nil
}

func escapeLikePrefix(value string) string {
	replacer := strings.NewReplacer("=", "==", "%", "=%", "_", "=_")
	return replacer.Replace(value) + "%"
}

func columnExists(db *database, table, column string) (bool, error) {
	var count int
	err := db.QueryRow(`
		SELECT COUNT(*)
		FROM information_schema.columns
		WHERE table_schema = ? AND table_name = ? AND column_name = ?
	`, db.schemaForTable(table), db.tableForTable(table), column).Scan(&count)
	return count > 0, err
}

func migratePartsIntoItems(db *database) error {
	var partsTableExists int
	if err := db.DB.QueryRow(`
		SELECT COUNT(*)
		FROM information_schema.tables
		WHERE table_schema = ? AND table_name = 'bom_parts'
	`, schemaForTable(db.defaultSchema, "bom_parts")).Scan(&partsTableExists); err != nil {
		return err
	}
	partIDExists, err := columnExists(db, "bom_item", "part_id")
	if err != nil {
		return err
	}
	partNameExists, err := columnExists(db, "bom_item", "part_name")
	if err != nil {
		return err
	}
	if partsTableExists > 0 && !partIDExists && !partNameExists {
		return fmt.Errorf("存在旧 bom_parts 表，但 bom_item 缺少 part_id 和 part_name，无法安全迁移部件名称")
	}
	if !partNameExists {
		if _, err := db.Exec("ALTER TABLE bom_item ADD COLUMN part_name TEXT NULL"); err != nil {
			return fmt.Errorf("添加物料部件名称字段失败: %w", err)
		}
	}
	if partIDExists && partsTableExists > 0 {
		var invalidItems int
		if err := db.QueryRow(`
			SELECT COUNT(*)
			FROM bom_item i
			LEFT JOIN bom_parts p ON p.id = i.part_id
			WHERE p.id IS NULL OR i.header_id <> p.header_id
		`).Scan(&invalidItems); err != nil {
			return fmt.Errorf("检查物料与部件关联失败: %w", err)
		}
		if invalidItems > 0 {
			return fmt.Errorf("数据库存在孤立或跨鞋款的部件关联，请先修复 bom_item.part_id/header_id")
		}
		if _, err := db.Exec(`
			UPDATE bom_item i
			JOIN bom_parts p ON p.id = i.part_id AND p.header_id = i.header_id
			SET i.part_name = p.part_name
		`); err != nil {
			return fmt.Errorf("迁移部件名称至物料表失败: %w", err)
		}

		rows, err := db.DB.Query(`
			SELECT DISTINCT constraint_name
			FROM information_schema.key_column_usage
			WHERE table_schema = ? AND table_name = 'bom_item'
				AND column_name = 'part_id' AND referenced_table_name IS NOT NULL
		`, schemaForTable(db.defaultSchema, "bom_item"))
		if err != nil {
			return fmt.Errorf("查询旧部件外键失败: %w", err)
		}
		var constraints []string
		for rows.Next() {
			var constraint string
			if err := rows.Scan(&constraint); err != nil {
				rows.Close()
				return fmt.Errorf("读取旧部件外键失败: %w", err)
			}
			constraints = append(constraints, constraint)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("读取旧部件外键失败: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("关闭旧部件外键查询失败: %w", err)
		}
		for _, constraint := range constraints {
			if _, err := db.DB.Exec(fmt.Sprintf("ALTER TABLE `%s`.`bom_item` DROP FOREIGN KEY `%s`",
				schemaForTable(db.defaultSchema, "bom_item"), constraint)); err != nil {
				return fmt.Errorf("删除旧部件外键 %s 失败: %w", constraint, err)
			}
		}
		if _, err := db.Exec("ALTER TABLE bom_item DROP COLUMN part_id"); err != nil {
			return fmt.Errorf("删除旧物料部件关联字段失败: %w", err)
		}
	} else if partIDExists {
		return fmt.Errorf("物料表仍有 part_id，但未找到 bom_parts 表，无法安全迁移部件名称")
	}

	if partsTableExists > 0 {
		if _, err := db.DB.Exec(fmt.Sprintf("DROP TABLE `%s`.`bom_parts`", schemaForTable(db.defaultSchema, "bom_parts"))); err != nil {
			return fmt.Errorf("删除已迁移的旧部件表失败: %w", err)
		}
	}
	return nil
}

func ensureSingleColumnForeignKey(db *database, table, column, parentTable, constraint string) error {
	tableSchema := db.schemaForTable(table)
	parentSchema := db.schemaForTable(parentTable)
	tableName := db.tableForTable(table)
	parentTableName := db.tableForTable(parentTable)
	constraint = db.constraintName(constraint)
	var exists bool
	err := db.DB.QueryRow(`
		SELECT COUNT(*) = 1 AND (
			SELECT COUNT(*) FROM information_schema.key_column_usage
			WHERE table_schema = ? AND table_name = ? AND constraint_name = ?
		) = 1
		FROM information_schema.key_column_usage
		WHERE table_schema = ? AND table_name = ? AND constraint_name = ?
			AND column_name = ? AND referenced_table_schema = ?
			AND referenced_table_name = ? AND referenced_column_name = 'id'
	`, tableSchema, tableName, constraint, tableSchema, tableName, constraint, column, parentSchema, parentTableName).Scan(&exists)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}

	rows, err := db.DB.Query(`
		SELECT DISTINCT constraint_name
		FROM information_schema.key_column_usage
		WHERE table_schema = ? AND table_name = ?
			AND column_name = ? AND referenced_table_name = ?
	`, tableSchema, tableName, column, parentTableName)
	if err != nil {
		return err
	}
	var oldConstraints []string
	for rows.Next() {
		var oldConstraint string
		if err := rows.Scan(&oldConstraint); err != nil {
			rows.Close()
			return err
		}
		oldConstraints = append(oldConstraints, oldConstraint)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	var namedConstraintExists int
	if err := db.DB.QueryRow(`
		SELECT COUNT(*) FROM information_schema.referential_constraints
		WHERE constraint_schema = ? AND table_name = ? AND constraint_name = ?
	`, tableSchema, tableName, constraint).Scan(&namedConstraintExists); err != nil {
		return err
	}
	if namedConstraintExists > 0 {
		oldConstraints = append(oldConstraints, constraint)
	}
	droppedConstraints := make(map[string]struct{}, len(oldConstraints))
	for _, oldConstraint := range oldConstraints {
		if _, alreadyDropped := droppedConstraints[oldConstraint]; alreadyDropped {
			continue
		}
		if _, err := db.DB.Exec(fmt.Sprintf("ALTER TABLE `%s`.`%s` DROP FOREIGN KEY `%s`",
			tableSchema, tableName, oldConstraint)); err != nil {
			return err
		}
		droppedConstraints[oldConstraint] = struct{}{}
	}

	_, err = db.DB.Exec(fmt.Sprintf(
		"ALTER TABLE `%s`.`%s` ADD CONSTRAINT `%s` FOREIGN KEY (`%s`) REFERENCES `%s`.`%s` (`id`) ON DELETE CASCADE ON UPDATE RESTRICT",
		tableSchema, tableName, constraint, column, parentSchema, parentTableName,
	))
	return err
}

func migrateDetailHeaderID(db *database) error {
	headerIDExists, err := columnExists(db, "bom_item_color_detail", "header_id")
	if err != nil {
		return err
	}
	if headerIDExists {
		var mismatchedDetails int
		if err := db.QueryRow(`
			SELECT COUNT(*)
			FROM bom_item_color_detail d
			LEFT JOIN bom_item i ON i.id = d.bom_item_id
			LEFT JOIN bom_colorway cw ON cw.id = d.colorway_id
			WHERE i.id IS NULL OR cw.id IS NULL OR i.header_id <> cw.header_id
		`).Scan(&mismatchedDetails); err != nil {
			return fmt.Errorf("检查配色明细关联失败: %w", err)
		}
		if mismatchedDetails > 0 {
			return fmt.Errorf("数据库存在跨鞋款或孤立的配色明细，请先修复 bom_item_color_detail 关联")
		}
		if _, err := db.Exec(`
			UPDATE bom_item_color_detail d
			JOIN bom_item i ON i.id = d.bom_item_id
			JOIN bom_colorway cw ON cw.id = d.colorway_id AND cw.header_id = i.header_id
			SET d.header_id = i.header_id
			WHERE d.header_id IS NULL
		`); err != nil {
			return fmt.Errorf("回填配色明细主记录外键失败: %w", err)
		}
		var invalidDetails int
		if err := db.QueryRow(`
			SELECT COUNT(*)
			FROM bom_item_color_detail d
			LEFT JOIN bom_item i ON i.id = d.bom_item_id
			LEFT JOIN bom_colorway cw ON cw.id = d.colorway_id
			WHERE d.header_id IS NULL OR i.id IS NULL OR cw.id IS NULL
				OR i.header_id <> cw.header_id OR d.header_id <> i.header_id
		`).Scan(&invalidDetails); err != nil {
			return fmt.Errorf("检查配色明细所属鞋款失败: %w", err)
		}
		if invalidDetails > 0 {
			return fmt.Errorf("数据库存在跨鞋款、孤立或无效的配色明细，请先修复 bom_item_color_detail 关联")
		}
	} else {
		var invalidDetails int
		if err := db.QueryRow(`
			SELECT COUNT(*)
			FROM bom_item_color_detail d
			LEFT JOIN bom_item i ON i.id = d.bom_item_id
			LEFT JOIN bom_colorway cw ON cw.id = d.colorway_id
			WHERE i.id IS NULL OR cw.id IS NULL OR i.header_id <> cw.header_id
		`).Scan(&invalidDetails); err != nil {
			return fmt.Errorf("检查配色明细关联失败: %w", err)
		}
		if invalidDetails > 0 {
			return fmt.Errorf("数据库存在跨鞋款或孤立的配色明细，请先修复 bom_item_color_detail 关联")
		}
	}

	if headerIDExists {
		rows, err := db.DB.Query(`
			SELECT DISTINCT constraint_name
			FROM information_schema.key_column_usage
			WHERE table_schema = ? AND table_name = 'bom_item_color_detail'
				AND column_name IN ('header_id', 'bom_item_id', 'colorway_id')
				AND referenced_table_name IN ('bom_item', 'bom_colorway')
		`, schemaForTable(db.defaultSchema, "bom_item_color_detail"))
		if err != nil {
			return fmt.Errorf("查询配色明细旧外键失败: %w", err)
		}
		var constraints []string
		for rows.Next() {
			var constraint string
			if err := rows.Scan(&constraint); err != nil {
				rows.Close()
				return fmt.Errorf("读取配色明细旧外键失败: %w", err)
			}
			constraints = append(constraints, constraint)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("读取配色明细旧外键失败: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("关闭配色明细旧外键查询失败: %w", err)
		}
		for _, constraint := range constraints {
			if _, err := db.DB.Exec(fmt.Sprintf("ALTER TABLE `%s`.`bom_item_color_detail` DROP FOREIGN KEY `%s`",
				schemaForTable(db.defaultSchema, "bom_item_color_detail"), constraint)); err != nil {
				return fmt.Errorf("删除配色明细旧外键 %s 失败: %w", constraint, err)
			}
		}
	}
	for _, index := range []string{
		"idx_bom_detail_header_item_color",
		"idx_bom_detail_colorway_header",
	} {
		if err := dropIndex(db, "bom_item_color_detail", index); err != nil {
			return fmt.Errorf("删除旧配色明细索引 %s 失败: %w", index, err)
		}
	}
	if headerIDExists {
		if _, err := db.Exec("ALTER TABLE bom_item_color_detail DROP COLUMN header_id"); err != nil {
			return fmt.Errorf("删除配色明细冗余主记录字段失败: %w", err)
		}
	}
	return nil
}

func setItemDisplayOrder(tx *transaction, itemIDs []int) error {
	lastPosition := make(map[int]int, len(itemIDs))
	orderedIDs := make([]int, 0, len(itemIDs))
	for i, itemID := range itemIDs {
		if _, exists := lastPosition[itemID]; !exists {
			orderedIDs = append(orderedIDs, itemID)
		}
		lastPosition[itemID] = i + 1
	}

	const batchSize = 250
	for start := 0; start < len(orderedIDs); start += batchSize {
		end := start + batchSize
		if end > len(orderedIDs) {
			end = len(orderedIDs)
		}

		batchIDs := orderedIDs[start:end]
		displayOrders := make([]int, len(batchIDs))
		for i, itemID := range batchIDs {
			displayOrders[i] = lastPosition[itemID]
		}
		query, args := buildDisplayOrderUpdate(batchIDs, displayOrders)
		if _, err := tx.Exec(query, args...); err != nil {
			return err
		}
	}
	return nil
}

func buildDisplayOrderUpdate(itemIDs, displayOrders []int) (string, []any) {
	var query strings.Builder
	query.WriteString("UPDATE bom_item SET display_order = CASE id")

	args := make([]any, 0, len(itemIDs)*3)
	for i, itemID := range itemIDs {
		query.WriteString(" WHEN ? THEN ?")
		args = append(args, itemID, displayOrders[i])
	}
	query.WriteString(" ELSE display_order END WHERE id IN (")
	for i, itemID := range itemIDs {
		if i > 0 {
			query.WriteString(", ")
		}
		query.WriteString("?")
		args = append(args, itemID)
	}
	query.WriteString(")")
	return query.String(), args
}

func main() {
	// 加载 .env 文件
	err := godotenv.Load()
	if err != nil {
		log.Println("未找到 .env 文件，将尝试读取系统环境变量")
	}

	dbUser := os.Getenv("DB_USER")
	dbPassword := os.Getenv("DB_PASSWORD")
	dbHost := os.Getenv("DB_HOST")
	dbPort := os.Getenv("DB_PORT")
	dbName := os.Getenv("DB_NAME")
	serverPort := os.Getenv("SERVER_PORT")

	if serverPort == "" {
		serverPort = ":8080"
	}

	dsn := fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?charset=utf8mb4&parseTime=True&loc=Local", dbUser, dbPassword, dbHost, dbPort, dbName)

	rawDB, err := sql.Open("mysql", dsn)
	if err != nil {
		log.Fatalf("数据库连接初始化失败: %v", err)
	}

	if err = rawDB.Ping(); err != nil {
		log.Fatalf("数据库连接测试失败: %v", err)
	}
	db := &database{DB: rawDB, defaultSchema: dbName}
	fmt.Printf("=== 成功连通 1Panel MariaDB 数据库 (%s) ===\n", dbName)

	if err := migrateTablesToOwnSchemas(db); err != nil {
		log.Fatalf("数据库分表迁移失败: %v", err)
	}
	sharedTablesPresent, err := sharedBOMTablesPresent(db)
	if err != nil {
		log.Fatalf("检查共享 BOM 表失败: %v", err)
	}

	if sharedTablesPresent {
		for _, migration := range []struct {
			table string
			query string
		}{
			{"bom_colorway", "ALTER TABLE bom_colorway ADD COLUMN IF NOT EXISTS display_order INT NOT NULL DEFAULT 0"},
			{"bom_colorway", "ALTER TABLE bom_colorway ADD COLUMN IF NOT EXISTS has_process TINYINT(1) NOT NULL DEFAULT 1"},
			{"bom_item", "ALTER TABLE bom_item ADD COLUMN IF NOT EXISTS display_order INT NOT NULL DEFAULT 0"},
		} {
			existed, err := columnExists(db, migration.table, "display_order")
			if err != nil {
				log.Fatalf("检查数据库排序字段失败 (%s): %v", migration.table, err)
			}
			if _, err := db.Exec(migration.query); err != nil {
				log.Fatalf("数据库排序字段迁移失败 (%s): %v", migration.table, err)
			}
			if !existed {
				if _, err := db.Exec("UPDATE " + migration.table + " SET display_order = id"); err != nil {
					log.Fatalf("数据库排序初始化失败 (%s): %v", migration.table, err)
				}
			}
		}
	}
	for _, query := range []string{
		"ALTER TABLE bom_header ADD COLUMN IF NOT EXISTS customer VARCHAR(200) NOT NULL DEFAULT ''",
		"ALTER TABLE bom_header ADD COLUMN IF NOT EXISTS brand VARCHAR(200) NOT NULL DEFAULT ''",
		"ALTER TABLE bom_header ADD COLUMN IF NOT EXISTS company_name VARCHAR(200) NOT NULL DEFAULT ''",
		"ALTER TABLE bom_header ADD COLUMN IF NOT EXISTS document_title VARCHAR(200) NOT NULL DEFAULT ''",
		"ALTER TABLE bom_header ADD COLUMN IF NOT EXISTS request_finish_date VARCHAR(50) NULL",
		"ALTER TABLE bom_header ADD COLUMN IF NOT EXISTS pattern_master VARCHAR(200) NULL",
		"ALTER TABLE bom_header ADD COLUMN IF NOT EXISTS tooling_master VARCHAR(200) NULL",
		"ALTER TABLE bom_header ADD COLUMN IF NOT EXISTS shoe_filler_no VARCHAR(100) NULL",
		"ALTER TABLE bom_header ADD COLUMN IF NOT EXISTS approved_by VARCHAR(200) NOT NULL DEFAULT ''",
		"ALTER TABLE bom_header ADD COLUMN IF NOT EXISTS reviewed_by VARCHAR(200) NOT NULL DEFAULT ''",
		"ALTER TABLE bom_header ADD COLUMN IF NOT EXISTS prepared_by VARCHAR(200) NOT NULL DEFAULT ''",
	} {
		if _, err := db.Exec(query); err != nil {
			log.Fatalf("数据库表头字段迁移失败: %v", err)
		}
	}
	stageExists, err := columnExists(db, "bom_header", "stage")
	if err != nil {
		log.Fatalf("检查 Stage 数据库字段失败: %v", err)
	}
	legacyShoetypeExists, err := columnExists(db, "bom_header", "shoetype")
	if err != nil {
		log.Fatalf("检查旧 Shoetype 数据库字段失败: %v", err)
	}
	if !stageExists {
		if _, err := db.Exec("ALTER TABLE bom_header ADD COLUMN stage TEXT NULL"); err != nil {
			log.Fatalf("添加 Stage 数据库字段失败: %v", err)
		}
		if legacyShoetypeExists {
			if _, err := db.Exec("UPDATE bom_header SET stage = shoetype WHERE shoetype IS NOT NULL"); err != nil {
				log.Fatalf("迁移 Shoetype 数据至 Stage 字段失败: %v", err)
			}
		}
	}
	if _, err := db.Exec("INSERT INTO bom_header (id) VALUES (1) ON DUPLICATE KEY UPDATE id = VALUES(id)"); err != nil {
		log.Fatalf("初始化默认 BOM 主记录失败: %v", err)
	}
	if _, err := db.Exec("UPDATE bom_header SET style_no = TRIM(style_no) WHERE style_no IS NOT NULL AND style_no <> TRIM(style_no)"); err != nil {
		log.Fatalf("规范化 Style No. 空白字符失败: %v", err)
	}
	if _, err := db.Exec("UPDATE bom_header SET style_no = NULL WHERE style_no = ''"); err != nil {
		log.Fatalf("规范化空 Style No. 失败: %v", err)
	}
	if err := ensureUniqueIndex(db, "bom_header", "uq_bom_header_style_no", "style_no",
		"SELECT COUNT(*) FROM (SELECT style_no FROM bom_header WHERE style_no IS NOT NULL GROUP BY style_no HAVING COUNT(*) > 1) duplicate_rows"); err != nil {
		log.Fatalf("数据库 Style No. 唯一索引迁移失败: %v", err)
	}
	if !sharedTablesPresent {
		if err := migrateStyleSchemasToNamedTables(db); err != nil {
			log.Fatalf("迁移旧版按 Style No. 分库的数据失败: %v", err)
		}
	}
	if sharedTablesPresent {
		for _, table := range []string{"bom_item", "bom_colorway"} {
			if _, err := db.Exec("UPDATE " + table + " SET header_id = 1 WHERE header_id IS NULL"); err != nil {
				log.Fatalf("迁移旧版 BOM 外键编号失败 (%s): %v", table, err)
			}
		}
		var legacyPartsExists int
		if err := db.DB.QueryRow(`
		SELECT COUNT(*)
		FROM information_schema.tables
		WHERE table_schema = ? AND table_name = 'bom_parts'
	`, schemaForTable(db.defaultSchema, "bom_parts")).Scan(&legacyPartsExists); err != nil {
			log.Fatalf("检查旧部件表失败: %v", err)
		}
		if legacyPartsExists > 0 {
			if _, err := db.Exec("UPDATE bom_parts SET header_id = 1 WHERE header_id IS NULL"); err != nil {
				log.Fatalf("迁移旧版部件主外键编号失败: %v", err)
			}
		}
		if err := migratePartsIntoItems(db); err != nil {
			log.Fatalf("合并部件和物料表失败: %v", err)
		}
		if err := migrateDetailHeaderID(db); err != nil {
			log.Fatalf("迁移配色明细鞋款字段失败: %v", err)
		}
		for _, index := range []struct {
			table          string
			name           string
			columns        string
			duplicateQuery string
		}{
			{"bom_item", "uq_bom_item_id_header", "id, header_id", ""},
			{"bom_colorway", "uq_bom_colorway_id_header", "id, header_id", ""},
			{
				"bom_colorway", "uq_bom_colorway_header_code", "header_id, color_code",
				"SELECT COUNT(*) FROM (SELECT header_id, color_code FROM bom_colorway GROUP BY header_id, color_code HAVING COUNT(*) > 1) duplicate_rows",
			},
			{
				"bom_item_color_detail", "uq_bom_item_colorway", "bom_item_id, colorway_id",
				"SELECT COUNT(*) FROM (SELECT bom_item_id, colorway_id FROM bom_item_color_detail GROUP BY bom_item_id, colorway_id HAVING COUNT(*) > 1) duplicate_rows",
			},
		} {
			if err := ensureUniqueIndex(db, index.table, index.name, index.columns, index.duplicateQuery); err != nil {
				log.Fatalf("数据库唯一索引迁移失败 (%s.%s): %v", index.table, index.name, err)
			}
		}
		for _, index := range []struct {
			table, name, columns string
		}{
			{"bom_item", "idx_bom_item_header_order", "header_id, display_order, id"},
			{"bom_colorway", "idx_bom_colorway_header_order", "header_id, display_order, id"},
			{"bom_item_color_detail", "idx_bom_detail_colorway_item", "colorway_id, bom_item_id"},
		} {
			if err := ensureIndex(db, index.table, index.name, index.columns); err != nil {
				log.Fatalf("数据库查询索引迁移失败 (%s.%s): %v", index.table, index.name, err)
			}
		}
		for _, relation := range []struct {
			table, column, parentTable, constraint string
		}{
			{"bom_item_color_detail", "bom_item_id", "bom_item", "fk_bom_detail_item"},
			{"bom_item_color_detail", "colorway_id", "bom_colorway", "fk_bom_detail_colorway"},
		} {
			if err := ensureSingleColumnForeignKey(db, relation.table, relation.column, relation.parentTable, relation.constraint); err != nil {
				log.Fatalf("数据库主外键迁移失败 (%s.%s): %v", relation.table, relation.column, err)
			}
		}
		if err := migrateSharedBOMTablesByStyle(db); err != nil {
			log.Fatalf("按 Style No. 拆分 BOM 数据表失败: %v", err)
		}
	} else if err := validateStyleTables(db); err != nil {
		log.Fatalf("校验 Style No. 数据表失败: %v", err)
	}

	r := gin.Default()

	r.Static("/static", "./static")
	r.StaticFile("/", "./static/index.html")

	// 1. 获取头部基本信息接口
	r.GET("/api/bom/list", func(c *gin.Context) {
		type bomSummary struct {
			ID        int    `json:"id"`
			StyleNo   string `json:"style_no"`
			StyleName string `json:"style_name"`
		}
		type bomListResponse struct {
			Items         []bomSummary `json:"items"`
			HasMore       bool         `json:"has_more"`
			NextID        int          `json:"next_id,omitempty"`
			NextStyleNo   string       `json:"next_style_no,omitempty"`
			HighWatermark int          `json:"high_watermark,omitempty"`
		}

		limit := defaultBOMListLimit
		if rawLimit := c.Query("limit"); rawLimit != "" {
			parsedLimit, err := strconv.Atoi(rawLimit)
			if err != nil || parsedLimit <= 0 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "无效的列表分页大小"})
				return
			}
			limit = min(parsedLimit, maxBOMListLimit)
		}

		response := bomListResponse{Items: make([]bomSummary, 0, limit)}
		var rows *sql.Rows
		var err error
		if requestedID := c.Query("bom_id"); requestedID != "" {
			headerID, parseErr := strconv.Atoi(requestedID)
			if parseErr != nil || headerID <= 0 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "无效的 BOM 主记录编号"})
				return
			}
			rows, err = db.Query(`
				SELECT id, COALESCE(style_no, ''), COALESCE(style_name, '')
				FROM bom_header WHERE id = ?
			`, headerID)
		} else {
			if rawSinceID := c.Query("since_id"); rawSinceID != "" {
				sinceID, parseErr := strconv.Atoi(rawSinceID)
				if parseErr != nil || sinceID < 0 {
					c.JSON(http.StatusBadRequest, gin.H{"error": "无效的 Style No. 增量游标"})
					return
				}
				err = db.QueryRow("SELECT COALESCE(MAX(id), 0) FROM bom_header").Scan(&response.HighWatermark)
				if err == nil {
					rows, err = db.Query(`
						SELECT id, COALESCE(style_no, ''), COALESCE(style_name, '')
						FROM bom_header
						WHERE id > ? AND style_no IS NOT NULL AND style_no <> ''
						ORDER BY id
						LIMIT ?
					`, sinceID, limit+1)
				}
			} else if query := strings.TrimSpace(c.Query("q")); query != "" {
				if len(query) > 100 {
					c.JSON(http.StatusBadRequest, gin.H{"error": "Style No. 搜索内容不能超过 100 个字符"})
					return
				}
				rows, err = db.Query(`
					SELECT id, COALESCE(style_no, ''), COALESCE(style_name, '')
					FROM bom_header
					WHERE style_no LIKE ? ESCAPE '=' AND style_no > ?
					ORDER BY style_no
					LIMIT ?
				`, escapeLikePrefix(query), c.Query("cursor_style_no"), limit+1)
			} else {
				cursorID := int(^uint(0) >> 1)
				if rawCursor := c.Query("before_id"); rawCursor != "" {
					cursorID, err = strconv.Atoi(rawCursor)
					if err != nil || cursorID <= 0 {
						c.JSON(http.StatusBadRequest, gin.H{"error": "无效的 Style No. 分页游标"})
						return
					}
				}
				err = db.QueryRow("SELECT COALESCE(MAX(id), 0) FROM bom_header").Scan(&response.HighWatermark)
				if err == nil {
					rows, err = db.Query(`
						SELECT id, COALESCE(style_no, ''), COALESCE(style_name, '')
						FROM bom_header
						WHERE id < ? AND style_no IS NOT NULL AND style_no <> ''
						ORDER BY id DESC
						LIMIT ?
					`, cursorID, limit+1)
				}
			}
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "获取 BOM 列表失败: " + err.Error()})
			return
		}
		defer rows.Close()
		for rows.Next() {
			var bom bomSummary
			if err := rows.Scan(&bom.ID, &bom.StyleNo, &bom.StyleName); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "读取 BOM 列表失败: " + err.Error()})
				return
			}
			if len(response.Items) == limit {
				response.HasMore = true
				break
			}
			response.Items = append(response.Items, bom)
		}
		if err := rows.Err(); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "读取 BOM 列表失败: " + err.Error()})
			return
		}
		if len(response.Items) > 0 {
			last := response.Items[len(response.Items)-1]
			response.NextID = last.ID
			response.NextStyleNo = last.StyleNo
		}
		c.JSON(http.StatusOK, response)
	})

	r.GET("/api/bom/header", func(c *gin.Context) {
		headerID, err := resolveHeaderID(db, c.Query("bom_id"))
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "未找到指定的 BOM 主记录"})
			return
		}
		row := db.QueryRow(`
			SELECT COALESCE(company_name, ''), COALESCE(document_title, ''),
				COALESCE(sample_order_no, ''), COALESCE(sample_type, ''),
				COALESCE(customer, ''), COALESCE(season, ''), COALESCE(brand, ''),
				COALESCE(gender, ''), COALESCE(style_no, ''), COALESCE(style_name, ''),
				COALESCE(last_no, ''), COALESCE(bottom_no, ''), COALESCE(stage, ''),
				COALESCE(request_finish_date, ''), COALESCE(pattern_master, ''),
				COALESCE(tooling_master, ''), COALESCE(shoe_filler_no, ''),
				COALESCE(approved_by, ''), COALESCE(reviewed_by, ''), COALESCE(prepared_by, '')
			FROM bom_header WHERE id = ?
		`, headerID)
		var companyName, documentTitle, orderNo, sampleType, customer, season, brand, gender string
		var styleNo, styleName, lastNo, bottomNo, stage string
		var requestFinishDate, patternMaster, toolingMaster, shoeFillerNo string
		var approvedBy, reviewedBy, preparedBy string
		if err := row.Scan(&companyName, &documentTitle, &orderNo, &sampleType, &customer, &season, &brand, &gender,
			&styleNo, &styleName, &lastNo, &bottomNo, &stage, &requestFinishDate, &patternMaster, &toolingMaster,
			&shoeFillerNo, &approvedBy, &reviewedBy, &preparedBy); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"id":                  headerID,
			"company_name":        companyName,
			"document_title":      documentTitle,
			"sample_order_no":     orderNo,
			"sample_type":         sampleType,
			"customer":            customer,
			"season":              season,
			"brand":               brand,
			"gender":              gender,
			"style_no":            styleNo,
			"style_name":          styleName,
			"last_no":             lastNo,
			"bottom_no":           bottomNo,
			"stage":               stage,
			"shoetype":            stage,
			"request_finish_date": requestFinishDate,
			"pattern_master":      patternMaster,
			"tooling_master":      toolingMaster,
			"shoe_filler_no":      shoeFillerNo,
			"approved_by":         approvedBy,
			"reviewed_by":         reviewedBy,
			"prepared_by":         preparedBy,
		})
	})

	r.POST("/api/bom/update-header", func(c *gin.Context) {
		headerID, err := resolveHeaderID(db, c.Query("bom_id"))
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "未找到指定的 BOM 主记录"})
			return
		}
		var req struct {
			Field string `json:"field"`
			Value string `json:"value"`
		}
		if err := c.BindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		columns := map[string]string{
			"company_name": "company_name", "document_title": "document_title",
			"sample_order_no": "sample_order_no", "sample_type": "sample_type",
			"customer": "customer", "season": "season", "brand": "brand",
			"gender": "gender", "style_no": "style_no", "style_name": "style_name",
			"last_no": "last_no", "bottom_no": "bottom_no", "stage": "stage", "shoetype": "stage",
			"request_finish_date": "request_finish_date", "pattern_master": "pattern_master",
			"tooling_master": "tooling_master", "shoe_filler_no": "shoe_filler_no",
			"approved_by": "approved_by", "reviewed_by": "reviewed_by", "prepared_by": "prepared_by",
		}
		column, ok := columns[req.Field]
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "不支持修改的表头字段"})
			return
		}
		oldStyleNo := ""
		tablesMoved := false
		if req.Field == "style_no" {
			newStyleNo := strings.TrimSpace(req.Value)
			if err := validateStyleNo(newStyleNo); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			if err := db.QueryRow("SELECT COALESCE(style_no, '') FROM bom_header WHERE id = ?", headerID).Scan(&oldStyleNo); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "读取原 Style No. 失败: " + err.Error()})
				return
			}
			if styleTableName(oldStyleNo) != styleTableName(newStyleNo) {
				var duplicateID int
				err := db.QueryRow("SELECT id FROM bom_header WHERE LOWER(style_no) = LOWER(?) AND id <> ?", newStyleNo, headerID).Scan(&duplicateID)
				if err == nil {
					c.JSON(http.StatusConflict, gin.H{"error": "Style No. 已存在，请选择对应鞋款后再编辑"})
					return
				}
				if err != sql.ErrNoRows {
					c.JSON(http.StatusInternalServerError, gin.H{"error": "检查 Style No. 是否重复失败: " + err.Error()})
					return
				}
				if oldStyleNo == "" {
					if err := moveHeaderBetweenStyleTables(db, headerID, oldStyleNo, newStyleNo); err != nil {
						c.JSON(http.StatusConflict, gin.H{"error": "迁移 Style No. 数据表失败: " + err.Error()})
						return
					}
				} else if err := renameStyleTables(db, oldStyleNo, newStyleNo); err != nil {
					c.JSON(http.StatusConflict, gin.H{"error": "迁移 Style No. 数据表失败: " + err.Error()})
					return
				}
				tablesMoved = true
			}
		}
		if _, err := db.Exec("UPDATE bom_header SET "+column+" = ? WHERE id = ?", strings.TrimSpace(req.Value), headerID); err != nil {
			if tablesMoved {
				var rollbackErr error
				if oldStyleNo == "" {
					rollbackErr = moveHeaderBetweenStyleTables(db, headerID, strings.TrimSpace(req.Value), oldStyleNo)
				} else {
					rollbackErr = renameStyleTables(db, strings.TrimSpace(req.Value), oldStyleNo)
				}
				if rollbackErr != nil {
					c.JSON(http.StatusInternalServerError, gin.H{"error": "更新 Style No. 失败且回滚数据表迁移失败: " + rollbackErr.Error()})
					return
				}
			}
			if strings.Contains(strings.ToLower(err.Error()), "duplicate") {
				c.JSON(http.StatusConflict, gin.H{"error": "Style No. 已存在，请选择对应鞋款后再编辑"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "数据库表头更新失败: " + err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true})
	})

	// 2. 获取多配色 BOM 矩阵展开接口
	r.GET("/api/bom/matrix", func(c *gin.Context) {
		headerID, err := resolveHeaderID(db, c.Query("bom_id"))
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "未找到指定的 BOM 主记录"})
			return
		}
		styleDB, err := scopedDatabase(db, headerID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "解析 Style No. 数据表失败: " + err.Error()})
			return
		}
		colorRows, err := styleDB.Query("SELECT id, color_code, has_process FROM bom_colorway WHERE header_id = ? ORDER BY display_order, id", headerID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "获取配色失败: " + err.Error()})
			return
		}
		defer colorRows.Close()

		var colorCodes []string
		processColumns := make(map[string]bool)
		colorMap := make(map[int]string)
		for colorRows.Next() {
			var id int
			var code string
			var hasProcess bool
			if err := colorRows.Scan(&id, &code, &hasProcess); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "读取配色列失败: " + err.Error()})
				return
			}
			colorCodes = append(colorCodes, code)
			processColumns[code] = hasProcess
			colorMap[id] = code
		}
		if err := colorRows.Err(); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "读取配色列失败: " + err.Error()})
			return
		}

		itemRows, err := styleDB.Query(`
			SELECT i.id, COALESCE(i.part_name, ''), i.material_desc, i.unit
			FROM bom_item i
			WHERE i.header_id = ?
			ORDER BY i.display_order, i.id
		`, headerID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "获取物料失败: " + err.Error()})
			return
		}
		defer itemRows.Close()

		type ColorDetail struct {
			ColorDesc   string `json:"color_desc"`
			ProcessDesc string `json:"process_desc"`
		}

		type ItemRow struct {
			ItemID       int                    `json:"item_id"`
			PartName     string                 `json:"part_name"`
			MaterialDesc string                 `json:"material_desc"`
			Unit         string                 `json:"unit"`
			ColorDetails map[string]ColorDetail `json:"color_details"`
		}

		itemMap := make(map[int]*ItemRow)
		var itemList []*ItemRow

		for itemRows.Next() {
			var item ItemRow
			item.ColorDetails = make(map[string]ColorDetail)
			if err := itemRows.Scan(&item.ItemID, &item.PartName, &item.MaterialDesc, &item.Unit); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "读取物料数据失败: " + err.Error()})
				return
			}
			itemMap[item.ItemID] = &item
			itemList = append(itemList, &item)
		}
		if err := itemRows.Err(); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "读取物料数据失败: " + err.Error()})
			return
		}

		detailRows, err := styleDB.Query(`
			SELECT d.bom_item_id, d.colorway_id, COALESCE(d.material_color_spec, ''), COALESCE(d.process_desc, '')
			FROM bom_item_color_detail d
			JOIN bom_item i ON i.id = d.bom_item_id AND i.header_id = ?
			JOIN bom_colorway cw ON cw.id = d.colorway_id AND cw.header_id = ?
		`, headerID, headerID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "获取明细失败: " + err.Error()})
			return
		}
		defer detailRows.Close()

		for detailRows.Next() {
			var itemID, colorwayID int
			var colorDesc, processDesc string
			detailRows.Scan(&itemID, &colorwayID, &colorDesc, &processDesc)

			if item, exists := itemMap[itemID]; exists {
				if code, ok := colorMap[colorwayID]; ok {
					item.ColorDetails[code] = ColorDetail{
						ColorDesc:   colorDesc,
						ProcessDesc: processDesc,
					}
				}
			}
		}

		c.JSON(http.StatusOK, gin.H{
			"columns":         colorCodes,
			"process_columns": processColumns,
			"data":            itemList,
		})
	})

	// 3. 修改列名接口
	r.POST("/api/bom/update-column", func(c *gin.Context) {
		headerID, err := resolveHeaderID(db, c.Query("bom_id"))
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "未找到指定的 BOM 主记录"})
			return
		}
		styleDB, err := scopedDatabase(db, headerID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "解析 Style No. 数据表失败: " + err.Error()})
			return
		}
		var req struct {
			OldCode string `json:"old_code"`
			NewCode string `json:"new_code"`
		}
		if err := c.BindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		oldColorCode := strings.TrimSpace(req.OldCode)
		newColorCode := strings.TrimSpace(req.NewCode)
		if newColorCode == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "配色代码不能为空"})
			return
		}
		var existingID int
		if err := styleDB.QueryRow("SELECT id FROM bom_colorway WHERE header_id = ? AND color_code = ?", headerID, newColorCode).Scan(&existingID); err == nil && existingID > 0 && oldColorCode != newColorCode {
			c.JSON(http.StatusConflict, gin.H{"error": "配色代码已存在，不能重复命名"})
			return
		} else if err != sql.ErrNoRows {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "检查配色代码是否重复失败: " + err.Error()})
			return
		}
		_, err = styleDB.Exec("UPDATE bom_colorway SET color_code = ? WHERE header_id = ? AND color_code = ?", newColorCode, headerID, oldColorCode)
		if err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "duplicate") {
				c.JSON(http.StatusConflict, gin.H{"error": "配色代码已存在，不能重复命名"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "数据库更新失败: " + err.Error()})
			return
		}

		fmt.Printf("✓ 数据库同步成功: 配色列名从 [%s] 修改为 [%s]\n", req.OldCode, req.NewCode)
		c.JSON(http.StatusOK, gin.H{"success": true})
	})

	r.POST("/api/bom/update-row", func(c *gin.Context) {
		headerID, err := resolveHeaderID(db, c.Query("bom_id"))
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "未找到指定的 BOM 主记录"})
			return
		}
		styleDB, err := scopedDatabase(db, headerID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "解析 Style No. 数据表失败: " + err.Error()})
			return
		}
		var req struct {
			ItemID int    `json:"item_id"`
			Field  string `json:"field"`
			Value  string `json:"value"`
		}
		if err := c.BindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if req.ItemID <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "无效的物料编号"})
			return
		}

		var result sql.Result
		switch req.Field {
		case "part_name":
			result, err = styleDB.Exec("UPDATE bom_item SET part_name = ? WHERE id = ? AND header_id = ?", req.Value, req.ItemID, headerID)
		case "material_desc":
			result, err = styleDB.Exec("UPDATE bom_item SET material_desc = ? WHERE id = ? AND header_id = ?", req.Value, req.ItemID, headerID)
		case "unit":
			result, err = styleDB.Exec("UPDATE bom_item SET unit = ? WHERE id = ? AND header_id = ?", req.Value, req.ItemID, headerID)
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "不支持修改的物料字段"})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "数据库物料更新失败: " + err.Error()})
			return
		}
		rowsAffected, err := result.RowsAffected()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "读取更新结果失败: " + err.Error()})
			return
		}
		if rowsAffected == 0 {
			c.JSON(http.StatusNotFound, gin.H{"error": "未找到对应的物料记录"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"success": true})
	})

	r.POST("/api/bom/create", func(c *gin.Context) {
		var req struct {
			Header *struct {
				CompanyName       string `json:"company_name"`
				DocumentTitle     string `json:"document_title"`
				SampleOrderNo     string `json:"sample_order_no"`
				SampleType        string `json:"sample_type"`
				Customer          string `json:"customer"`
				Season            string `json:"season"`
				Brand             string `json:"brand"`
				Gender            string `json:"gender"`
				StyleNo           string `json:"style_no"`
				StyleName         string `json:"style_name"`
				LastNo            string `json:"last_no"`
				BottomNo          string `json:"bottom_no"`
				Stage             string `json:"stage"`
				RequestFinishDate string `json:"request_finish_date"`
				PatternMaster     string `json:"pattern_master"`
				ToolingMaster     string `json:"tooling_master"`
				ShoeFillerNo      string `json:"shoe_filler_no"`
				ApprovedBy        string `json:"approved_by"`
				ReviewedBy        string `json:"reviewed_by"`
				PreparedBy        string `json:"prepared_by"`
			} `json:"header"`
		}
		if err := c.BindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if req.Header == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "鞋款表头不能为空"})
			return
		}
		req.Header.StyleNo = strings.TrimSpace(req.Header.StyleNo)
		if err := validateStyleNo(req.Header.StyleNo); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		var duplicateID int
		err := db.QueryRow("SELECT id FROM bom_header WHERE LOWER(style_no) = LOWER(?)", req.Header.StyleNo).Scan(&duplicateID)
		if err == nil {
			c.JSON(http.StatusConflict, gin.H{"error": "Style No. 已存在"})
			return
		}
		if err != sql.ErrNoRows {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "检查 Style No. 是否重复失败: " + err.Error()})
			return
		}

		styleDB := db.forStyle(req.Header.StyleNo)
		anyTableExists := false
		for _, table := range []string{"bom_item", "bom_colorway", "bom_item_color_detail"} {
			exists, err := styleTableExists(db, styleDB, table)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "检查目标 Style No. 数据表失败: " + err.Error()})
				return
			}
			anyTableExists = anyTableExists || exists
		}
		if anyTableExists {
			c.JSON(http.StatusConflict, gin.H{"error": "目标 Style No. 数据表已存在，但没有对应主记录；为保护数据，拒绝覆盖"})
			return
		}
		templateTable, err := findStyleTableTemplate(db, styleDB.styleTable)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "查找 Style No. 数据表模板失败: " + err.Error()})
			return
		}
		styleDB, createdTables, err := ensureStyleTablesWithCreated(db, req.Header.StyleNo, templateTable)
		if err != nil {
			cleanupErr := dropCreatedStyleTables(db, styleDB, createdTables)
			message := "创建 Style No. 数据表失败: " + err.Error()
			if cleanupErr != nil {
				message += "; " + cleanupErr.Error()
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": message})
			return
		}

		tx, err := db.DB.Begin()
		if err != nil {
			cleanupErr := dropCreatedStyleTables(db, styleDB, createdTables)
			message := "开始新增鞋款事务失败: " + err.Error()
			if cleanupErr != nil {
				message += "; " + cleanupErr.Error()
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": message})
			return
		}
		result, err := tx.Exec(`
			INSERT INTO bom_header (
				company_name, document_title, sample_order_no, sample_type,
				customer, season, brand, gender, style_no, style_name,
				last_no, bottom_no, stage, request_finish_date, pattern_master,
				tooling_master, shoe_filler_no, approved_by, reviewed_by, prepared_by
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`,
			req.Header.CompanyName, req.Header.DocumentTitle, req.Header.SampleOrderNo, req.Header.SampleType,
			req.Header.Customer, req.Header.Season, req.Header.Brand, req.Header.Gender, req.Header.StyleNo,
			req.Header.StyleName, req.Header.LastNo, req.Header.BottomNo, req.Header.Stage,
			req.Header.RequestFinishDate, req.Header.PatternMaster, req.Header.ToolingMaster,
			req.Header.ShoeFillerNo, req.Header.ApprovedBy, req.Header.ReviewedBy, req.Header.PreparedBy,
		)
		if err != nil {
			rollbackErr := tx.Rollback()
			cleanupErr := dropCreatedStyleTables(db, styleDB, createdTables)
			if strings.Contains(strings.ToLower(err.Error()), "duplicate") {
				c.JSON(http.StatusConflict, gin.H{"error": "Style No. 已存在"})
				return
			}
			message := "新增鞋款主记录失败: " + err.Error()
			if rollbackErr != nil && rollbackErr != sql.ErrTxDone {
				message += "; 回滚主记录失败: " + rollbackErr.Error()
			}
			if cleanupErr != nil {
				message += "; " + cleanupErr.Error()
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": message})
			return
		}
		headerID64, err := result.LastInsertId()
		if err != nil {
			rollbackErr := tx.Rollback()
			cleanupErr := dropCreatedStyleTables(db, styleDB, createdTables)
			message := "读取新鞋款编号失败: " + err.Error()
			if rollbackErr != nil && rollbackErr != sql.ErrTxDone {
				message += "; 回滚主记录失败: " + rollbackErr.Error()
			}
			if cleanupErr != nil {
				message += "; " + cleanupErr.Error()
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": message})
			return
		}
		if err := tx.Commit(); err != nil {
			var committedID int
			verifyErr := db.QueryRow("SELECT id FROM bom_header WHERE style_no = ?", req.Header.StyleNo).Scan(&committedID)
			message := "提交新鞋款主记录失败: " + err.Error()
			if verifyErr == sql.ErrNoRows {
				if cleanupErr := dropCreatedStyleTables(db, styleDB, createdTables); cleanupErr != nil {
					message += "; " + cleanupErr.Error()
				}
			} else if verifyErr != nil {
				message += "; 无法确认主记录是否提交: " + verifyErr.Error()
			} else {
				message += "; 数据库中已存在主记录 ID " + strconv.Itoa(committedID) + "，请刷新确认"
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": message})
			return
		}

		c.JSON(http.StatusCreated, gin.H{"success": true, "bom_id": headerID64, "style_no": req.Header.StyleNo})
	})

	r.POST("/api/bom/delete", func(c *gin.Context) {
		requestedID := c.Query("bom_id")
		if strings.TrimSpace(requestedID) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "删除鞋款时必须指定 bom_id"})
			return
		}
		headerID, err := resolveHeaderID(db, requestedID)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "未找到要删除的 BOM 主记录"})
			return
		}
		var styleNo string
		if err := db.QueryRow("SELECT COALESCE(style_no, '') FROM bom_header WHERE id = ?", headerID).Scan(&styleNo); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "读取待删除 Style No. 失败: " + err.Error()})
			return
		}
		if strings.TrimSpace(styleNo) == "" {
			c.JSON(http.StatusConflict, gin.H{"error": "该主记录没有 Style No.，无法安全确定需要删除的数据表"})
			return
		}
		if err := validateStyleNo(styleNo); err != nil {
			c.JSON(http.StatusConflict, gin.H{"error": "该主记录的 Style No. 无效，已拒绝删除: " + err.Error()})
			return
		}
		styleDB := db.forStyle(styleNo)
		for _, table := range []string{"bom_item", "bom_colorway", "bom_item_color_detail"} {
			exists, err := styleTableExists(db, styleDB, table)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "检查待删除 Style No. 数据表失败: " + err.Error()})
				return
			}
			if !exists {
				c.JSON(http.StatusConflict, gin.H{"error": fmt.Sprintf("Style No. %s 缺少数据表 %s，拒绝不完整删除", styleNo, table)})
				return
			}
		}

		nonce := make([]byte, 8)
		if _, err := rand.Read(nonce); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "生成删除过程临时表名失败: " + err.Error()})
			return
		}
		token := hex.EncodeToString(nonce)
		temporaryNames := temporaryStyleTableNames(token)
		for _, table := range []string{"bom_item", "bom_colorway", "bom_item_color_detail"} {
			exists, err := tableExistsInSchema(db, styleDB.schemaForTable(table), temporaryNames[table])
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "检查删除临时表名失败: " + err.Error()})
				return
			}
			if exists {
				c.JSON(http.StatusConflict, gin.H{"error": "删除过程临时表名已被占用，请重试"})
				return
			}
		}
		originalNames := map[string]string{
			"bom_item":              styleDB.tableForTable("bom_item"),
			"bom_colorway":          styleDB.tableForTable("bom_colorway"),
			"bom_item_color_detail": styleDB.tableForTable("bom_item_color_detail"),
		}
		if err := renameStyleTablesWithinSchemas(db, styleDB, nil, temporaryNames); err != nil {
			c.JSON(http.StatusConflict, gin.H{"error": "暂存待删除数据表失败，主记录和原数据均未删除: " + err.Error()})
			return
		}

		tx, err := db.DB.Begin()
		if err != nil {
			restoreErr := renameStyleTablesWithinSchemas(db, styleDB, temporaryNames, originalNames)
			message := "开始删除主记录事务失败: " + err.Error()
			if restoreErr != nil {
				message += "; 恢复数据表失败: " + restoreErr.Error()
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": message})
			return
		}
		result, err := tx.Exec("DELETE FROM bom_header WHERE id = ?", headerID)
		if err == nil {
			var affected int64
			affected, err = result.RowsAffected()
			if err == nil && affected != 1 {
				err = fmt.Errorf("待删除主记录数量异常: %d", affected)
			}
		}
		if err != nil {
			if rollbackErr := tx.Rollback(); rollbackErr != nil && rollbackErr != sql.ErrTxDone {
				err = fmt.Errorf("%w; 回滚主记录失败: %v", err, rollbackErr)
			}
			restoreErr := renameStyleTablesWithinSchemas(db, styleDB, temporaryNames, originalNames)
			if restoreErr != nil {
				err = fmt.Errorf("%w; 恢复原数据表失败: %v", err, restoreErr)
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "删除 BOM 主记录失败: " + err.Error()})
			return
		}
		if commitErr := tx.Commit(); commitErr != nil {
			var remaining int
			verifyErr := db.QueryRow("SELECT COUNT(*) FROM bom_header WHERE id = ?", headerID).Scan(&remaining)
			if verifyErr != nil {
				c.JSON(http.StatusInternalServerError, gin.H{
					"error":          "提交删除结果无法确认，请联系管理员核实；相关数据表仍以临时表名保留: " + verifyErr.Error(),
					"cleanup_tables": temporaryNames,
				})
				return
			}
			if remaining > 0 {
				restoreErr := renameStyleTablesWithinSchemas(db, styleDB, temporaryNames, originalNames)
				message := "提交删除主记录失败: " + commitErr.Error()
				if restoreErr != nil {
					message += "; 恢复原数据表失败: " + restoreErr.Error()
				}
				c.JSON(http.StatusInternalServerError, gin.H{"error": message})
				return
			}
		}

		for index, table := range []string{"bom_item_color_detail", "bom_item", "bom_colorway"} {
			if _, err := db.DB.Exec(fmt.Sprintf("DROP TABLE `%s`.`%s`",
				styleDB.schemaForTable(table), temporaryNames[table])); err != nil {
				remainingTables := make(map[string]string)
				for _, remaining := range []string{"bom_item_color_detail", "bom_item", "bom_colorway"}[index:] {
					remainingTables[remaining] = temporaryNames[remaining]
				}
				c.JSON(http.StatusInternalServerError, gin.H{
					"error":          "主记录已删除，但清理暂存数据表失败，请联系管理员处理: " + err.Error(),
					"deleted":        true,
					"style_no":       styleNo,
					"bom_id":         headerID,
					"cleanup_tables": remainingTables,
				})
				return
			}
		}

		var fallbackID int
		err = db.QueryRow(`
			SELECT id FROM bom_header
			WHERE style_no IS NOT NULL AND style_no <> ''
			ORDER BY id DESC LIMIT 1
		`).Scan(&fallbackID)
		if err != nil && err != sql.ErrNoRows {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "鞋款已删除，但查找后续页面失败: " + err.Error(), "deleted": true})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"success": true, "deleted": true, "style_no": styleNo,
			"bom_id": headerID, "fallback_bom_id": fallbackID,
		})
	})

	r.POST("/api/bom/import", func(c *gin.Context) {
		var req struct {
			Header *struct {
				CompanyName       string `json:"company_name"`
				Customer          string `json:"customer"`
				Season            string `json:"season"`
				Brand             string `json:"brand"`
				Gender            string `json:"gender"`
				StyleNo           string `json:"style_no"`
				StyleName         string `json:"style_name"`
				LastNo            string `json:"last_no"`
				BottomNo          string `json:"bottom_no"`
				Stage             string `json:"stage"`
				Shoetype          string `json:"shoetype"`
				RequestFinishDate string `json:"request_finish_date"`
				PatternMaster     string `json:"pattern_master"`
				ToolingMaster     string `json:"tooling_master"`
				ShoeFillerNo      string `json:"shoe_filler_no"`
				ApprovedBy        string `json:"approved_by"`
				ReviewedBy        string `json:"reviewed_by"`
				PreparedBy        string `json:"prepared_by"`
			} `json:"header"`
			Columns        []string `json:"columns"`
			ProcessColumns []bool   `json:"process_columns"`
			Rows           []struct {
				PartName     string `json:"part_name"`
				MaterialDesc string `json:"material_desc"`
				Unit         string `json:"unit"`
				ColorDetails map[string]struct {
					ColorDesc   string `json:"color_desc"`
					ProcessDesc string `json:"process_desc"`
				} `json:"color_details"`
			} `json:"rows"`
		}
		if err := c.BindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if req.Header == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Excel 鞋款表头不能为空"})
			return
		}
		if len(req.Columns) == 0 || len(req.Columns) > 100 || len(req.Rows) == 0 || len(req.Rows) > 10000 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Excel 配色列或物料行数量超出允许范围"})
			return
		}
		if len(req.ProcessColumns) == 0 {
			req.ProcessColumns = make([]bool, len(req.Columns))
			for i := range req.ProcessColumns {
				req.ProcessColumns[i] = true
			}
		} else if len(req.ProcessColumns) != len(req.Columns) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Excel 配色列类型数量不匹配"})
			return
		}
		seenCodes := make(map[string]struct{}, len(req.Columns))
		for i, code := range req.Columns {
			code = strings.TrimSpace(code)
			if code == "" || len(code) > 100 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "Excel 中存在空配色代码或过长的配色代码"})
				return
			}
			normalizedCode := strings.ToLower(code)
			if _, exists := seenCodes[normalizedCode]; exists {
				c.JSON(http.StatusBadRequest, gin.H{"error": "Excel 中存在重复配色代码: " + code})
				return
			}
			seenCodes[normalizedCode] = struct{}{}
			req.Columns[i] = code
		}

		req.Header.StyleNo = strings.TrimSpace(req.Header.StyleNo)
		if err := validateStyleNo(req.Header.StyleNo); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		var existingStyleNo string
		err := db.QueryRow("SELECT style_no FROM bom_header WHERE LOWER(style_no) = LOWER(?)", req.Header.StyleNo).Scan(&existingStyleNo)
		if err == nil {
			req.Header.StyleNo = existingStyleNo
		} else if err != sql.ErrNoRows {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "检查 Style No. 主记录失败: " + err.Error()})
			return
		}
		styleDB, err := ensureStyleTablesForImport(db, req.Header.StyleNo)
		if err != nil {
			c.JSON(http.StatusConflict, gin.H{"error": "准备 Style No. 数据表失败: " + err.Error()})
			return
		}
		tx, err := styleDB.Begin()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "开始 Excel 导入事务失败: " + err.Error()})
			return
		}
		defer tx.Rollback()
		var headerID int
		err = tx.QueryRow("SELECT id FROM bom_header WHERE LOWER(style_no) = LOWER(?) FOR UPDATE", req.Header.StyleNo).Scan(&headerID)
		if err == sql.ErrNoRows {
			result, insertErr := tx.Exec("INSERT INTO bom_header (style_no) VALUES (?)", req.Header.StyleNo)
			if insertErr != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "创建鞋款主记录失败: " + insertErr.Error()})
				return
			}
			newHeaderID, insertErr := result.LastInsertId()
			if insertErr != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "读取鞋款主记录编号失败: " + insertErr.Error()})
				return
			}
			headerID = int(newHeaderID)
		} else if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "查询 Style No. 主记录失败: " + err.Error()})
			return
		}
		if req.Header.Stage == "" {
			req.Header.Stage = req.Header.Shoetype
		}
		if _, err := tx.Exec(`
				UPDATE bom_header SET company_name = ?,
					customer = ?, season = ?, brand = ?, gender = ?,
					style_no = ?, style_name = ?, last_no = ?, bottom_no = ?, stage = ?,
					request_finish_date = ?, pattern_master = ?, tooling_master = ?, shoe_filler_no = ?,
					approved_by = ?, reviewed_by = ?, prepared_by = ?
				WHERE id = ?
			`, req.Header.CompanyName,
			req.Header.Customer, req.Header.Season, req.Header.Brand, req.Header.Gender,
			req.Header.StyleNo, req.Header.StyleName, req.Header.LastNo, req.Header.BottomNo, req.Header.Stage,
			req.Header.RequestFinishDate, req.Header.PatternMaster, req.Header.ToolingMaster, req.Header.ShoeFillerNo,
			req.Header.ApprovedBy, req.Header.ReviewedBy, req.Header.PreparedBy, headerID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "导入鞋款表头失败: " + err.Error()})
			return
		}
		for _, statement := range []string{
			"DELETE d FROM bom_item_color_detail d JOIN bom_item i ON i.id = d.bom_item_id WHERE i.header_id = ?",
			"DELETE FROM bom_item WHERE header_id = ?",
			"DELETE FROM bom_colorway WHERE header_id = ?",
		} {
			if _, err := tx.Exec(statement, headerID); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "清理原 BOM 数据失败: " + err.Error()})
				return
			}
		}

		colorwayIDs := make(map[string]int64, len(req.Columns))
		for i, code := range req.Columns {
			result, err := tx.Exec(
				"INSERT INTO bom_colorway (header_id, color_code, display_order, has_process) VALUES (?, ?, ?, ?)",
				headerID, strings.TrimSpace(code), i+1, req.ProcessColumns[i],
			)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "导入配色列失败: " + err.Error()})
				return
			}
			colorwayID, err := result.LastInsertId()
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "读取导入配色编号失败: " + err.Error()})
				return
			}
			colorwayIDs[code] = colorwayID
		}

		for i, row := range req.Rows {
			result, err := tx.Exec(
				"INSERT INTO bom_item (header_id, part_name, item_seq, display_order, material_desc, unit) VALUES (?, ?, ?, ?, ?, ?)",
				headerID, row.PartName, i+1, i+1, row.MaterialDesc, row.Unit,
			)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "导入物料行失败: " + err.Error()})
				return
			}
			itemID, err := result.LastInsertId()
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "读取导入物料编号失败: " + err.Error()})
				return
			}

			for _, code := range req.Columns {
				detail := row.ColorDetails[code]
				if _, err := tx.Exec(
					"INSERT INTO bom_item_color_detail (bom_item_id, colorway_id, material_color_spec, process_desc) VALUES (?, ?, ?, ?)",
					itemID, colorwayIDs[code], detail.ColorDesc, detail.ProcessDesc,
				); err != nil {
					c.JSON(http.StatusInternalServerError, gin.H{"error": "导入配色明细失败: " + err.Error()})
					return
				}
			}
		}

		if err := tx.Commit(); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "提交 Excel 导入失败: " + err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"success": true, "rows": len(req.Rows), "columns": len(req.Columns),
			"bom_id": headerID,
		})
	})

	// 4. 新增列接口
	r.POST("/api/bom/add-column", func(c *gin.Context) {
		requestedHeaderID, err := resolveHeaderID(db, c.Query("bom_id"))
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "未找到指定的 BOM 主记录"})
			return
		}
		styleDB, err := scopedDatabase(db, requestedHeaderID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "解析 Style No. 数据表失败: " + err.Error()})
			return
		}
		var req struct {
			ColorCode       string `json:"color_code"`
			AnchorColorCode string `json:"anchor_color_code"`
			Position        string `json:"position"`
			HasProcess      *bool  `json:"has_process"`
		}
		if err := c.BindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		colorCode := strings.TrimSpace(req.ColorCode)
		anchorColorCode := strings.TrimSpace(req.AnchorColorCode)
		if colorCode == "" || (req.Position != "above" && req.Position != "below") {
			c.JSON(http.StatusBadRequest, gin.H{"error": "配色代码或插入位置无效"})
			return
		}
		var existingID int
		if err := styleDB.QueryRow("SELECT id FROM bom_colorway WHERE header_id = ? AND color_code = ?", requestedHeaderID, colorCode).Scan(&existingID); err == nil {
			c.JSON(http.StatusConflict, gin.H{"error": "配色代码已存在，不能重复新增"})
			return
		} else if err != sql.ErrNoRows {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "检查配色代码是否重复失败: " + err.Error()})
			return
		}

		tx, err := styleDB.Begin()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "新增配色失败: " + err.Error()})
			return
		}
		defer tx.Rollback()

		var insertOrder int
		if anchorColorCode == "" {
			var colorwayCount int
			if err := tx.QueryRow("SELECT COUNT(*) FROM bom_colorway WHERE header_id = ?", requestedHeaderID).Scan(&colorwayCount); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "读取配色列数量失败: " + err.Error()})
				return
			}
			if colorwayCount != 0 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "插入位置缺少配色列锚点"})
				return
			}
			insertOrder = 1
		} else {
			var anchorOrder int
			if err := tx.QueryRow("SELECT display_order FROM bom_colorway WHERE header_id = ? AND color_code = ?", requestedHeaderID, anchorColorCode).Scan(&anchorOrder); err != nil {
				c.JSON(http.StatusNotFound, gin.H{"error": "未找到要插入相邻位置的配色列"})
				return
			}
			insertOrder = anchorOrder
			if req.Position == "below" {
				insertOrder++
			}
		}
		if _, err := tx.Exec("UPDATE bom_colorway SET display_order = display_order + 1 WHERE header_id = ? AND display_order >= ?", requestedHeaderID, insertOrder); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "调整配色顺序失败: " + err.Error()})
			return
		}
		hasProcess := true
		if req.HasProcess != nil {
			hasProcess = *req.HasProcess
		}
		res, err := tx.Exec("INSERT INTO bom_colorway (header_id, color_code, display_order, has_process) VALUES (?, ?, ?, ?)", requestedHeaderID, colorCode, insertOrder, hasProcess)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "新增配色失败: " + err.Error()})
			return
		}

		colorwayID64, err := res.LastInsertId()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "读取新增配色编号失败: " + err.Error()})
			return
		}
		colorwayID := int(colorwayID64)

		if _, err := tx.Exec(`
			INSERT INTO bom_item_color_detail (bom_item_id, colorway_id, material_color_spec, process_desc)
			SELECT id, ?, '', '' FROM bom_item WHERE header_id = ?
		`, colorwayID, requestedHeaderID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "初始化新配色明细失败: " + err.Error()})
			return
		}
		if err := tx.Commit(); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "保存新配色失败: " + err.Error()})
			return
		}

		fmt.Printf("✓ 数据库同步成功: 新增配色列 [%s]\n", req.ColorCode)
		c.JSON(http.StatusOK, gin.H{"success": true, "colorway_id": colorwayID})
	})

	// 5. 删除列接口
	r.POST("/api/bom/delete-column", func(c *gin.Context) {
		headerID, err := resolveHeaderID(db, c.Query("bom_id"))
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "未找到指定的 BOM 主记录"})
			return
		}
		styleDB, err := scopedDatabase(db, headerID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "解析 Style No. 数据表失败: " + err.Error()})
			return
		}
		var req struct {
			ColorCode string `json:"color_code"`
		}
		if err := c.BindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		tx, err := styleDB.Begin()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "删除配色失败: " + err.Error()})
			return
		}
		defer tx.Rollback()
		var colorwayID int
		if err := tx.QueryRow("SELECT id FROM bom_colorway WHERE header_id = ? AND color_code = ?", headerID, req.ColorCode).Scan(&colorwayID); err != nil {
			if err == sql.ErrNoRows {
				c.JSON(http.StatusNotFound, gin.H{"error": "未找到要删除的配色列"})
			} else {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "查询配色列失败: " + err.Error()})
			}
			return
		}
		if _, err := tx.Exec("DELETE FROM bom_item_color_detail WHERE colorway_id = ?", colorwayID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "删除配色明细失败: " + err.Error()})
			return
		}
		if _, err := tx.Exec("DELETE FROM bom_colorway WHERE id = ?", colorwayID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "删除配色列失败: " + err.Error()})
			return
		}
		if err := tx.Commit(); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "保存配色删除失败: " + err.Error()})
			return
		}

		fmt.Printf("✓ 数据库同步成功: 删除配色列 [%s]\n", req.ColorCode)
		c.JSON(http.StatusOK, gin.H{"success": true})
	})

	// 6. 单元格实时修改接口
	r.POST("/api/bom/update-cell", func(c *gin.Context) {
		var req struct {
			ItemID    int    `json:"item_id"`
			ColorCode string `json:"color_code"`
			FieldType string `json:"field_type"` // "color" 或 "process"
			Value     string `json:"value"`
		}
		if err := c.BindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		headerID, err := resolveHeaderID(db, c.Query("bom_id"))
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "未找到指定的 BOM 主记录"})
			return
		}
		styleDB, err := scopedDatabase(db, headerID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "解析 Style No. 数据表失败: " + err.Error()})
			return
		}
		if req.FieldType != "color" && req.FieldType != "process" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "无效的配色明细类型"})
			return
		}
		if req.ItemID <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "无效的物料编号"})
			return
		}
		var colorwayID int
		err = styleDB.QueryRow("SELECT id FROM bom_colorway WHERE header_id = ? AND color_code = ?", headerID, req.ColorCode).Scan(&colorwayID)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "未找到对应的配色代码"})
			return
		}

		var query string
		if req.FieldType == "color" {
			query = `UPDATE bom_item_color_detail d
				JOIN bom_item i ON i.id = d.bom_item_id AND i.header_id = ?
				JOIN bom_colorway cw ON cw.id = d.colorway_id AND cw.header_id = i.header_id
				SET d.material_color_spec = ? WHERE d.bom_item_id = ? AND d.colorway_id = ?`
		} else {
			query = `UPDATE bom_item_color_detail d
				JOIN bom_item i ON i.id = d.bom_item_id AND i.header_id = ?
				JOIN bom_colorway cw ON cw.id = d.colorway_id AND cw.header_id = i.header_id
				SET d.process_desc = ? WHERE d.bom_item_id = ? AND d.colorway_id = ?`
		}

		result, err := styleDB.Exec(query, headerID, req.Value, req.ItemID, colorwayID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "数据库单元格更新失败: " + err.Error()})
			return
		}
		rowsAffected, err := result.RowsAffected()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "读取单元格更新结果失败: " + err.Error()})
			return
		}
		if rowsAffected == 0 {
			c.JSON(http.StatusNotFound, gin.H{"error": "未找到当前 BOM 对应的配色明细"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"success": true})
	})

	// 7. 新增行接口
	r.POST("/api/bom/add-row", func(c *gin.Context) {
		requestedHeaderID, err := resolveHeaderID(db, c.Query("bom_id"))
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "未找到指定的 BOM 主记录"})
			return
		}
		styleDB, err := scopedDatabase(db, requestedHeaderID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "解析 Style No. 数据表失败: " + err.Error()})
			return
		}
		var req struct {
			AnchorItemID int    `json:"anchor_item_id"`
			Position     string `json:"position"`
		}
		if err := c.BindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		if req.AnchorItemID < 0 || (req.Position != "above" && req.Position != "below") {
			c.JSON(http.StatusBadRequest, gin.H{"error": "物料行或插入位置无效"})
			return
		}
		tx, err := styleDB.Begin()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "新增行失败: " + err.Error()})
			return
		}
		defer tx.Rollback()

		headerID := requestedHeaderID
		if req.AnchorItemID > 0 {
			if err := tx.QueryRow("SELECT header_id FROM bom_item WHERE id = ? AND header_id = ?", req.AnchorItemID, requestedHeaderID).Scan(&headerID); err != nil {
				if err == sql.ErrNoRows {
					c.JSON(http.StatusNotFound, gin.H{"error": "未找到要插入相邻位置的物料行"})
				} else {
					c.JSON(http.StatusInternalServerError, gin.H{"error": "查询物料行失败: " + err.Error()})
				}
				return
			}
		}

		rows, err := tx.Query("SELECT id FROM bom_item WHERE header_id = ? ORDER BY display_order, id FOR UPDATE", headerID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "读取物料行顺序失败: " + err.Error()})
			return
		}
		itemIDs := make([]int, 0)
		anchorIndex := -1
		for rows.Next() {
			var itemID int
			if err := rows.Scan(&itemID); err != nil {
				rows.Close()
				c.JSON(http.StatusInternalServerError, gin.H{"error": "读取物料行顺序失败: " + err.Error()})
				return
			}
			if itemID == req.AnchorItemID {
				anchorIndex = len(itemIDs)
			}
			itemIDs = append(itemIDs, itemID)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "读取物料行顺序失败: " + err.Error()})
			return
		}
		if err := rows.Close(); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "关闭物料行顺序查询失败: " + err.Error()})
			return
		}
		if req.AnchorItemID == 0 && len(itemIDs) > 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "仅空 BOM 可以不指定物料行插入"})
			return
		}
		if req.AnchorItemID > 0 && anchorIndex < 0 {
			c.JSON(http.StatusNotFound, gin.H{"error": "未找到要插入相邻位置的物料行"})
			return
		}
		insertIndex := 0
		if req.AnchorItemID > 0 {
			insertIndex = anchorIndex
		}
		if req.AnchorItemID > 0 && req.Position == "below" {
			insertIndex++
		}

		var maxOrder int
		if err := tx.QueryRow("SELECT COALESCE(MAX(display_order), 0) FROM bom_item WHERE header_id = ?", headerID).Scan(&maxOrder); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "读取物料显示顺序失败: " + err.Error()})
			return
		}

		res, err := tx.Exec("INSERT INTO bom_item (header_id, part_name, item_seq, display_order, material_desc, unit) VALUES (?, '', 1, ?, '', '')", headerID, maxOrder+1)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "新增行失败: " + err.Error()})
			return
		}
		itemID64, err := res.LastInsertId()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "读取新增物料编号失败: " + err.Error()})
			return
		}
		itemID := int(itemID64)
		itemIDs = append(itemIDs, 0)
		copy(itemIDs[insertIndex+1:], itemIDs[insertIndex:])
		itemIDs[insertIndex] = itemID
		if err := setItemDisplayOrder(tx, itemIDs); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "保存物料行顺序失败: " + err.Error()})
			return
		}

		if _, err := tx.Exec(`
			INSERT INTO bom_item_color_detail (bom_item_id, colorway_id, material_color_spec, process_desc)
			SELECT ?, id, '', '' FROM bom_colorway WHERE header_id = ?
		`, itemID, headerID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "初始化新增行配色明细失败: " + err.Error()})
			return
		}
		if err := tx.Commit(); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "保存新增物料行失败: " + err.Error()})
			return
		}

		fmt.Printf("✓ 数据库同步成功: 新增物料行 ID=%d\n", itemID)
		c.JSON(http.StatusOK, gin.H{"success": true, "item_id": itemID})
	})

	// 8. 删除行接口
	r.POST("/api/bom/delete-row", func(c *gin.Context) {
		requestedHeaderID, err := resolveHeaderID(db, c.Query("bom_id"))
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "未找到指定的 BOM 主记录"})
			return
		}
		styleDB, err := scopedDatabase(db, requestedHeaderID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "解析 Style No. 数据表失败: " + err.Error()})
			return
		}
		var req struct {
			ItemID int `json:"item_id"`
		}
		if err := c.BindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		if req.ItemID <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "无效的物料编号"})
			return
		}
		tx, err := styleDB.Begin()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "删除物料行失败: " + err.Error()})
			return
		}
		defer tx.Rollback()
		var headerID int
		if err := tx.QueryRow("SELECT header_id FROM bom_item WHERE id = ? AND header_id = ?", req.ItemID, requestedHeaderID).Scan(&headerID); err != nil {
			if err == sql.ErrNoRows {
				c.JSON(http.StatusNotFound, gin.H{"error": "未找到对应的物料记录"})
			} else {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "查询物料行失败: " + err.Error()})
			}
			return
		}
		if _, err := tx.Exec("DELETE FROM bom_item_color_detail WHERE bom_item_id = ?", req.ItemID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "删除物料明细失败: " + err.Error()})
			return
		}
		result, err := tx.Exec("DELETE FROM bom_item WHERE id = ? AND header_id = ?", req.ItemID, headerID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "删除物料行失败: " + err.Error()})
			return
		}
		rowsAffected, err := result.RowsAffected()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "读取物料删除结果失败: " + err.Error()})
			return
		}
		if rowsAffected == 0 {
			c.JSON(http.StatusNotFound, gin.H{"error": "未找到对应的物料记录"})
			return
		}
		rows, err := tx.Query("SELECT id FROM bom_item WHERE header_id = ? ORDER BY display_order, id FOR UPDATE", headerID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "读取剩余物料顺序失败: " + err.Error()})
			return
		}
		remainingIDs := make([]int, 0)
		for rows.Next() {
			var itemID int
			if err := rows.Scan(&itemID); err != nil {
				rows.Close()
				c.JSON(http.StatusInternalServerError, gin.H{"error": "读取剩余物料顺序失败: " + err.Error()})
				return
			}
			remainingIDs = append(remainingIDs, itemID)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "读取剩余物料顺序失败: " + err.Error()})
			return
		}
		if err := rows.Close(); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "关闭物料顺序查询失败: " + err.Error()})
			return
		}
		if err := setItemDisplayOrder(tx, remainingIDs); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "保存剩余物料顺序失败: " + err.Error()})
			return
		}
		if err := tx.Commit(); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "保存物料删除失败: " + err.Error()})
			return
		}

		fmt.Printf("✓ 数据库同步成功: 删除物料行 ID=%d\n", req.ItemID)
		c.JSON(http.StatusOK, gin.H{"success": true})
	})

	fmt.Printf("=== Go 后端服务已启动，正在监听 %s ===\n", serverPort)
	r.Run(serverPort)
}
