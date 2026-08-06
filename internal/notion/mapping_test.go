package notion

import (
	"reflect"
	"strings"
	"testing"
)

var zigaFields = []string{"date", "name", "contact", "source", "need", "notes", "flags"}

func ds(props ...Property) *DataSource {
	d := &DataSource{ID: "ds-1", Title: "Leads", Properties: props}
	sortProperties(d.Properties)
	return d
}

// A database built for Ziga (or created by it) maps cleanly by name.
func TestAutoMapByName(t *testing.T) {
	got := AutoMap(zigaFields, ds(
		Property{Name: "Name", Type: TypeTitle},
		Property{Name: "Contact", Type: TypeEmail},
		Property{Name: "Source", Type: TypeSelect},
		Property{Name: "Need", Type: TypeRichText},
		Property{Name: "Date", Type: TypeDate},
		Property{Name: "Notes", Type: TypeRichText},
		Property{Name: "Flags", Type: TypeRichText},
	))
	want := Mapping{
		"name":    {Name: "Name", Type: TypeTitle},
		"contact": {Name: "Contact", Type: TypeEmail},
		"source":  {Name: "Source", Type: TypeSelect},
		"need":    {Name: "Need", Type: TypeRichText},
		"date":    {Name: "Date", Type: TypeDate},
		"notes":   {Name: "Notes", Type: TypeRichText},
		"flags":   {Name: "Flags", Type: TypeRichText},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mapping =\n%+v\nwant\n%+v", got, want)
	}
}

// Notion property names are case-sensitive: "Name" and "name" are different
// properties. An exact match must win over a case-insensitive one, and the
// stored name must keep the database's own casing.
func TestAutoMapPrefersExactCase(t *testing.T) {
	got := AutoMap([]string{"name", "need"}, ds(
		Property{Name: "NAME", Type: TypeRichText},
		Property{Name: "name", Type: TypeTitle},
		Property{Name: "Need", Type: TypeRichText},
	))
	if got["name"].Name != "name" {
		t.Fatalf("name mapped to %q, want the exactly-matching \"name\"", got["name"].Name)
	}
	// The other-cased property keeps its own casing when it is used.
	if got["need"].Name != "Need" {
		t.Fatalf("need mapped to %q, want \"Need\" with the database's casing", got["need"].Name)
	}
}

// A database that was not built for Ziga still gets a sensible mapping from
// property types alone.
func TestAutoMapByTypeAffinity(t *testing.T) {
	got := AutoMap(zigaFields, ds(
		Property{Name: "Person", Type: TypeTitle},
		Property{Name: "Email address", Type: TypeEmail},
		Property{Name: "Channel", Type: TypeSelect},
		Property{Name: "When", Type: TypeDate},
		Property{Name: "Summary", Type: TypeRichText},
	))
	if got["name"].Name != "Person" {
		t.Fatalf("name -> %q, want the title property", got["name"].Name)
	}
	if got["contact"].Name != "Email address" {
		t.Fatalf("contact -> %q, want the email property", got["contact"].Name)
	}
	if got["source"].Name != "Channel" {
		t.Fatalf("source -> %q, want the select property", got["source"].Name)
	}
	if got["date"].Name != "When" {
		t.Fatalf("date -> %q, want the date property", got["date"].Name)
	}
	// Only one rich_text property exists; a field claims it and the rest are
	// left unmapped rather than doubling up.
	seen := map[string]int{}
	for _, target := range got {
		seen[target.Name]++
	}
	for name, n := range seen {
		if n > 1 {
			t.Fatalf("property %q claimed by %d fields", name, n)
		}
	}
}

// The title property is the row's visible identity, so a non-name field must
// not consume it by type affinity while "name" is still unmapped.
func TestAutoMapReservesTitleForName(t *testing.T) {
	got := AutoMap([]string{"date", "name"}, ds(
		Property{Name: "Untitled", Type: TypeTitle},
	))
	if got["name"].Name != "Untitled" {
		t.Fatalf("title went to %+v, want it reserved for name", got)
	}
	if _, ok := got["date"]; ok {
		t.Fatal("date must not consume the title property")
	}
}

// status cannot take new options through the API, so it is never auto-mapped.
func TestAutoMapSkipsUnwritableTypes(t *testing.T) {
	got := AutoMap([]string{"source"}, ds(
		Property{Name: "Source", Type: TypeStatus},
	))
	if _, ok := got["source"]; ok {
		t.Fatalf("source mapped to a status property: %+v", got)
	}
	if Mappable(TypeStatus) {
		t.Fatal("status must not be reported as writable")
	}
}

func TestValidateRejectsDrift(t *testing.T) {
	schema := ds(
		Property{Name: "Name", Type: TypeTitle},
		Property{Name: "Notes", Type: TypeRichText},
		Property{Name: "Stage", Type: TypeStatus},
	)
	for _, tc := range []struct {
		name    string
		mapping Mapping
		wantErr string
	}{
		{"missing property", Mapping{"name": {Name: "Gone", Type: TypeTitle}}, "does not exist"},
		{"wrong case", Mapping{"name": {Name: "name", Type: TypeTitle}}, "does not exist"},
		{"type changed", Mapping{"name": {Name: "Name", Type: TypeRichText}}, "not a"},
		{"unwritable type", Mapping{"source": {Name: "Stage", Type: TypeStatus}}, "cannot write"},
		{"duplicate target", Mapping{
			"need":  {Name: "Notes", Type: TypeRichText},
			"notes": {Name: "Notes", Type: TypeRichText},
		}, "mapped to both"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.mapping.Validate(schema)
			if err == nil {
				t.Fatal("want an error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q must mention %q", err, tc.wantErr)
			}
		})
	}

	valid := Mapping{
		"name":  {Name: "Name", Type: TypeTitle},
		"notes": {Name: "Notes", Type: TypeRichText},
	}
	if err := valid.Validate(schema); err != nil {
		t.Fatalf("valid mapping rejected: %v", err)
	}
}

// buildValue must respect the target property's type, and refuse a value it
// cannot represent rather than writing something wrong.
func TestBuildValueRespectsPropertyType(t *testing.T) {
	for _, tc := range []struct {
		name     string
		propType string
		value    string
		wantOK   bool
	}{
		{"email accepts an address", TypeEmail, "ada@lumen.studio", true},
		{"email refuses a phone number", TypeEmail, "+221 77 555 0100", false},
		{"email refuses a handle", TypeEmail, "@adaokafor", false},
		{"phone takes anything", TypePhoneNumber, "+221 77 555 0100", true},
		{"date accepts ISO", TypeDate, "2026-07-15", true},
		{"date refuses prose", TypeDate, "last Tuesday", false},
		{"select accepts a value", TypeSelect, "X DM", true},
		{"select refuses a comma", TypeSelect, "X DM, referral", false},
		{"number accepts a number", TypeNumber, "1200", true},
		{"number refuses text", TypeNumber, "about $1200", false},
		{"rich text takes anything", TypeRichText, "anything at all", true},
		{"title takes anything", TypeTitle, "Ada Okafor", true},
		{"empty is never written", TypeRichText, "   ", false},
		{"unknown type is refused", TypeStatus, "Lead", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, ok := buildValue(tc.propType, tc.value)
			if ok != tc.wantOK {
				t.Fatalf("buildValue(%s, %q) ok = %v, want %v", tc.propType, tc.value, ok, tc.wantOK)
			}
		})
	}
}

// The auto-created schema and its mapping must agree, or the database the app
// owns would drop fields on its very first lead.
func TestLeadsDatabaseSchemaMatchesItsMapping(t *testing.T) {
	schema := LeadsDatabaseSchema()
	mapping := LeadsDatabaseMapping()

	byName := map[string]Property{}
	for _, p := range schema {
		byName[p.Name] = p
	}
	for field, target := range mapping {
		p, ok := byName[target.Name]
		if !ok {
			t.Fatalf("field %q maps to %q, which the created schema does not define", field, target.Name)
		}
		if p.Type != target.Type {
			t.Fatalf("field %q expects %s but the schema defines %s", field, target.Type, p.Type)
		}
	}
	for _, field := range zigaFields {
		if _, ok := mapping[field]; !ok {
			t.Fatalf("the app-created database drops %q; auto-create must cover every field", field)
		}
	}
}
