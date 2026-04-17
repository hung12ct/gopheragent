package builtin

import (
	"strings"
	"testing"
)

func sampleSchema() Schema {
	return Schema{
		Tables: []TableSchema{
			{
				Name:        "customers",
				Description: "Paying customers.",
				Columns: []ColumnSchema{
					{Name: "id", Type: "INT", Description: "Primary key"},
					{Name: "email", Type: "VARCHAR(255)", Examples: []string{"a@x.com", "b@x.com"}},
					{Name: "status", Type: "VARCHAR(16)", Nullable: true, Examples: []string{"ACTIVE", "CHURNED"}},
				},
				PrimaryKey: []string{"id"},
			},
			{
				Name: "orders",
				Columns: []ColumnSchema{
					{Name: "id", Type: "INT"},
					{Name: "customer_id", Type: "INT"},
					{Name: "total_cents", Type: "BIGINT"},
				},
				ForeignKeys: []ForeignKey{
					{Column: "customer_id", RefTable: "customers", RefColumn: "id"},
				},
			},
		},
	}
}

func TestSchema_String_ContainsTablesAndColumns(t *testing.T) {
	out := sampleSchema().String()
	for _, want := range []string{"customers", "orders", "total_cents", "**Primary key:** id", "customer_id → customers.id"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected %q in rendered schema, got:\n%s", want, out)
		}
	}
}

func TestSchema_String_IncludesExamplesAndNullability(t *testing.T) {
	out := sampleSchema().String()
	if !strings.Contains(out, "`ACTIVE`, `CHURNED`") {
		t.Fatalf("examples missing in output:\n%s", out)
	}
	if !strings.Contains(out, "| status | VARCHAR(16) | YES |") {
		t.Fatalf("nullability not rendered:\n%s", out)
	}
}

func TestSchema_String_EmptyIsEmptyString(t *testing.T) {
	if got := (Schema{}).String(); got != "" {
		t.Fatalf("empty schema should stringify to empty, got %q", got)
	}
}

func TestSchema_String_SkipsTablesWithNoColumns(t *testing.T) {
	s := Schema{Tables: []TableSchema{{Name: "empty"}, {Name: "t", Columns: []ColumnSchema{{Name: "a", Type: "INT"}}}}}
	out := s.String()
	if strings.Contains(out, "Table: empty") {
		t.Fatalf("empty table should be skipped, got:\n%s", out)
	}
	if !strings.Contains(out, "Table: t") {
		t.Fatalf("non-empty table missing, got:\n%s", out)
	}
}

func TestSchema_Filter_KeepsOnlyRequested(t *testing.T) {
	got := sampleSchema().Filter("orders")
	if len(got.Tables) != 1 || got.Tables[0].Name != "orders" {
		t.Fatalf("expected only 'orders', got %+v", got.TableNames())
	}
}

func TestSchema_Filter_CaseInsensitiveAndDedupes(t *testing.T) {
	got := sampleSchema().Filter("CUSTOMERS", "customers", "ORDERS")
	if names := got.TableNames(); len(names) != 2 || names[0] != "customers" || names[1] != "orders" {
		t.Fatalf("unexpected filter result: %+v", names)
	}
}

func TestSchema_Filter_UnknownTablesSilentlySkipped(t *testing.T) {
	got := sampleSchema().Filter("does_not_exist", "orders")
	if names := got.TableNames(); len(names) != 1 || names[0] != "orders" {
		t.Fatalf("unknown tables should be skipped, got %+v", names)
	}
}

func TestSchema_Filter_EmptyInputsReturnEmpty(t *testing.T) {
	if got := sampleSchema().Filter(); len(got.Tables) != 0 {
		t.Fatal("no-arg filter should return empty")
	}
	if got := (Schema{}).Filter("customers"); len(got.Tables) != 0 {
		t.Fatal("filter on empty schema should return empty")
	}
}

func TestSchema_Summary_Format(t *testing.T) {
	out := sampleSchema().Summary()
	// Sorted alphabetically: customers before orders.
	customersIdx := strings.Index(out, "- customers")
	ordersIdx := strings.Index(out, "- orders")
	if customersIdx < 0 || ordersIdx < 0 {
		t.Fatalf("missing entries:\n%s", out)
	}
	if customersIdx >= ordersIdx {
		t.Fatalf("summary should be alphabetical:\n%s", out)
	}
	if !strings.Contains(out, "Paying customers.") {
		t.Fatalf("description missing in summary:\n%s", out)
	}
}
