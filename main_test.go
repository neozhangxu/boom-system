package main

import (
	"database/sql"
	"strings"
	"testing"
)

func TestQualifySQLUsesSeparateSchemas(t *testing.T) {
	query := `
		SELECT i.id, i.part_name, d.bom_item_id
		FROM bom_item i
		JOIN bom_item_color_detail d ON d.bom_item_id = i.id
		JOIN bom_colorway cw ON cw.id = d.colorway_id
		JOIN bom_header h ON h.id = i.header_id
		WHERE i.header_id = ?
	`
	got := qualifySQL(query)
	for _, expected := range []string{
		"`bom_item`.`bom_item` i",
		"`bom_item_color_detail`.`bom_item_color_detail` d",
		"`bom_colorway`.`bom_colorway` cw",
		"bom_header h",
		"d.bom_item_id",
		"i.part_name",
	} {
		if !strings.Contains(got, expected) {
			t.Errorf("qualified SQL does not contain %q:\n%s", expected, got)
		}
	}
	legacyMigrationQuery := qualifySQL("UPDATE bom_parts SET part_name = ? WHERE id = ?")
	if !strings.Contains(legacyMigrationQuery, "`bom_parts`.`bom_parts`") {
		t.Errorf("legacy migration query does not qualify bom_parts:\n%s", legacyMigrationQuery)
	}
}

func TestQualifySQLForStyle(t *testing.T) {
	query := "SELECT * FROM bom_item i JOIN bom_colorway cw ON cw.id = i.id " +
		"JOIN bom_item_color_detail d ON d.bom_item_id = i.id JOIN bom_header h ON h.id = i.header_id"
	got := qualifySQLForStyle(query, "STYLE-01")
	for _, expected := range []string{
		"`bom_item`.`STYLE-01` i",
		"`bom_colorway`.`STYLE-01` cw",
		"`bom_item_color_detail`.`STYLE-01` d",
		"bom_header h",
	} {
		if !strings.Contains(got, expected) {
			t.Errorf("style-qualified SQL does not contain %q:\n%s", expected, got)
		}
	}
}

func TestStyleTableNameAndValidation(t *testing.T) {
	if got := styleTableName("  AbC_12-X "); got != "AbC_12-X" {
		t.Fatalf("styleTableName() = %q, want %q", got, "AbC_12-X")
	}
	if got := styleTableKey("  AbC_12-X "); got != "abc_12-x" {
		t.Fatalf("styleTableKey() = %q, want %q", got, "abc_12-x")
	}
	if got := styleTableName(""); got != unassignedStyleTable {
		t.Fatalf("styleTableName(empty) = %q, want %q", got, unassignedStyleTable)
	}
	for _, styleNo := range []string{"", "with space", "超过64位", strings.Repeat("a", 65), "bom_item", "_UNASSIGNED_"} {
		if err := validateStyleNo(styleNo); err == nil {
			t.Errorf("validateStyleNo(%q) unexpectedly succeeded", styleNo)
		}
	}
	for _, styleNo := range []string{"A", "STYLE-01", "style_02", strings.Repeat("a", 64)} {
		if err := validateStyleNo(styleNo); err != nil {
			t.Errorf("validateStyleNo(%q) failed: %v", styleNo, err)
		}
	}
}

func TestStyleTableRouting(t *testing.T) {
	db := (&database{defaultSchema: "bom_header"}).forStyle("STYLE-01")
	for logical, wantSchema := range map[string]string{
		"bom_item":              "bom_item",
		"bom_colorway":          "bom_colorway",
		"bom_item_color_detail": "bom_item_color_detail",
	} {
		if got := db.schemaForTable(logical); got != wantSchema {
			t.Errorf("schemaForTable(%q) = %q, want %q", logical, got, wantSchema)
		}
		if got := db.tableForTable(logical); got != "STYLE-01" {
			t.Errorf("tableForTable(%q) = %q, want %q", logical, got, "STYLE-01")
		}
	}
}

func TestTemporaryStyleTableNames(t *testing.T) {
	names := temporaryStyleTableNames("0123456789abcdef")
	want := map[string]string{
		"bom_item":              "__deleted_0123456789abcdef_item",
		"bom_colorway":          "__deleted_0123456789abcdef_colorway",
		"bom_item_color_detail": "__deleted_0123456789abcdef_detail",
	}
	for table, expected := range want {
		if got := names[table]; got != expected {
			t.Errorf("temporaryStyleTableNames()[%q] = %q, want %q", table, got, expected)
		}
		if len(names[table]) > 64 {
			t.Errorf("temporary table name %q exceeds MariaDB's 64-character limit", names[table])
		}
	}
}

func TestSchemaForTable(t *testing.T) {
	for _, table := range childTableNames {
		if got := schemaForTable("bom_header", table); got != table {
			t.Errorf("schemaForTable(%q) = %q, want %q", table, got, table)
		}
	}
	if got := schemaForTable("bom_header", "bom_header"); got != "bom_header" {
		t.Errorf("schemaForTable(bom_header) = %q, want bom_header", got)
	}
}

func TestEscapeLikePrefix(t *testing.T) {
	for input, want := range map[string]string{
		"ABC":   "ABC%",
		"A_B":   "A=_B%",
		"A%B":   "A=%B%",
		"A=B":   "A==B%",
		`A\B`:   `A\B%`,
		`A\_%B`: `A\=_=%B%`,
	} {
		if got := escapeLikePrefix(input); got != want {
			t.Errorf("escapeLikePrefix(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestBuildDisplayOrderUpdate(t *testing.T) {
	query, args := buildDisplayOrderUpdate([]int{7, 12}, []int{1, 4})

	wantQuery := "UPDATE bom_item SET display_order = CASE id WHEN ? THEN ? WHEN ? THEN ? ELSE display_order END WHERE id IN (?, ?)"
	if query != wantQuery {
		t.Fatalf("buildDisplayOrderUpdate() query = %q, want %q", query, wantQuery)
	}
	wantArgs := []any{7, 1, 12, 4, 7, 12}
	if len(args) != len(wantArgs) {
		t.Fatalf("buildDisplayOrderUpdate() args = %v, want %v", args, wantArgs)
	}
	for i := range wantArgs {
		if args[i] != wantArgs[i] {
			t.Fatalf("buildDisplayOrderUpdate() args = %v, want %v", args, wantArgs)
		}
	}
}

func TestIndexHasColumns(t *testing.T) {
	fullIndex := []indexColumn{
		{name: "header_id"},
		{name: "display_order"},
		{name: "id"},
	}
	tests := []struct {
		name          string
		actual        []indexColumn
		wanted        []string
		requireUnique bool
		actualUnique  bool
		exact         bool
		want          bool
	}{
		{"non-unique prefix", fullIndex, []string{"header_id", "display_order"}, false, false, false, true},
		{"unique requires unique index", fullIndex, []string{"header_id", "display_order", "id"}, true, false, true, false},
		{"unique exact match", fullIndex, []string{"header_id", "display_order", "id"}, true, true, true, true},
		{"unique rejects extra columns", fullIndex, []string{"header_id", "display_order"}, true, true, true, false},
		{"prefix index rejected", []indexColumn{{name: "style_no", prefixSize: sql.NullInt64{Int64: 32, Valid: true}}}, []string{"style_no"}, false, false, false, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := indexHasColumns(test.actual, test.wanted, test.requireUnique, test.actualUnique, test.exact); got != test.want {
				t.Fatalf("indexHasColumns() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestSplitIndexColumns(t *testing.T) {
	got := splitIndexColumns("header_id, display_order, id")
	want := []string{"header_id", "display_order", "id"}
	if len(got) != len(want) {
		t.Fatalf("splitIndexColumns() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("splitIndexColumns() = %v, want %v", got[i], want[i])
		}
	}
}
