package notion

import (
	"fmt"
	"net/mail"
	"sort"
	"strconv"
	"strings"
	"time"
)

// A Google Sheet is rows and columns: every schema field has a cell, always.
// A Notion database has typed properties, and a user's existing database was
// not built for Ziga — it may name things differently, type them differently,
// or lack a home for a field entirely. This file is where that mismatch is
// resolved: automatically as a starting point, then the user adjusts it, and
// the result is stored on the destination.

// Mapping maps a Ziga schema field name to a target Notion property.
type Mapping map[string]MappedProperty

// MappedProperty is one field's target property: the exact (case-sensitive)
// Notion property name and its type.
type MappedProperty struct {
	Name string
	Type string
}

// preferredTypes lists, per Ziga field, the Notion property types that suit it
// best, in descending order of preference. A field falls back to rich_text
// when none of its preferred types is available, because any value can be
// rendered as text without losing information.
var preferredTypes = map[string][]string{
	"name":    {TypeTitle},
	"contact": {TypeEmail, TypePhoneNumber, TypeRichText},
	"source":  {TypeSelect, TypeRichText},
	"need":    {TypeRichText, TypeTitle},
	"date":    {TypeDate, TypeRichText},
	"notes":   {TypeRichText},
	"flags":   {TypeRichText},
}

// mappableTypes are the property types a value can actually be written to. A
// type outside this set (status, relation, formula, rollup, people, files…) is
// either not writable by this integration or cannot accept an arbitrary lead
// value, so it is never auto-mapped and is rejected if chosen manually.
var mappableTypes = map[string]bool{
	TypeTitle: true, TypeRichText: true, TypeEmail: true, TypePhoneNumber: true,
	TypeURL: true, TypeDate: true, TypeSelect: true, TypeMultiSelect: true,
	TypeNumber: true,
}

// Mappable reports whether a property type can be written to.
func Mappable(propType string) bool { return mappableTypes[propType] }

// AutoMap proposes a mapping from Ziga schema fields onto a data source's
// properties, best-effort. It is a starting point the user reviews and adjusts,
// never the final word.
//
// Resolution order per field: an exact name match, then a case-insensitive
// name match, then the first free property of a preferred type. Every property
// is claimed at most once, so two fields never collide on one column.
//
// The title property is special: Notion gives every database exactly one, it
// is required, and it is the row's visible identity — so it is reserved for a
// name-ish field rather than being consumed by whichever field asks first.
func AutoMap(fields []string, ds *DataSource) Mapping {
	mapping := Mapping{}
	claimed := map[string]bool{}

	claim := func(field string, p Property) {
		mapping[field] = MappedProperty{Name: p.Name, Type: p.Type}
		claimed[p.Name] = true
	}

	// Pass 1: exact, then case-insensitive, name matches.
	for _, matchExact := range []bool{true, false} {
		for _, field := range fields {
			if _, done := mapping[field]; done {
				continue
			}
			for _, p := range ds.Properties {
				if claimed[p.Name] || !Mappable(p.Type) {
					continue
				}
				hit := p.Name == field
				if !matchExact {
					hit = strings.EqualFold(p.Name, field)
				}
				if hit {
					claim(field, p)
					break
				}
			}
		}
	}

	// Pass 2: type affinity, in field order so earlier schema fields get first
	// choice of a scarce type.
	for _, field := range fields {
		if _, done := mapping[field]; done {
			continue
		}
		for _, want := range preferredTypes[field] {
			var found bool
			for _, p := range ds.Properties {
				if claimed[p.Name] || p.Type != want {
					continue
				}
				// Don't let a non-name field consume the title property by
				// affinity; the title carries the row's identity.
				if p.Type == TypeTitle && field != "name" {
					continue
				}
				claim(field, p)
				found = true
				break
			}
			if found {
				break
			}
		}
	}

	// The title property is mandatory in every Notion database and cannot be
	// left empty in a useful row. If nothing claimed it, give it to the first
	// field that has any value to offer.
	if title, ok := titleProperty(ds); ok && !claimed[title.Name] {
		for _, field := range fields {
			if _, done := mapping[field]; !done {
				claim(field, title)
				break
			}
		}
	}
	return mapping
}

func titleProperty(ds *DataSource) (Property, bool) {
	for _, p := range ds.Properties {
		if p.Type == TypeTitle {
			return p, true
		}
	}
	return Property{}, false
}

// Validate checks a user-adjusted mapping against the live schema: every
// target property must still exist, with the type the mapping recorded, and be
// writable. Re-validating at save time means a database edited between the
// mapping screen and the save cannot produce a destination that fails on the
// first lead.
func (m Mapping) Validate(ds *DataSource) error {
	seen := map[string]string{}
	for field, target := range m {
		p, ok := ds.Property(target.Name)
		if !ok {
			return fmt.Errorf("property %q does not exist in this database (property names are case-sensitive)", target.Name)
		}
		if p.Type != target.Type {
			return fmt.Errorf("property %q is a %s, not a %s", target.Name, p.Type, target.Type)
		}
		if !Mappable(p.Type) {
			return fmt.Errorf("property %q is a %s, which Ziga cannot write to", target.Name, p.Type)
		}
		if other, dup := seen[target.Name]; dup {
			return fmt.Errorf("property %q is mapped to both %q and %q", target.Name, other, field)
		}
		seen[target.Name] = field
	}
	return nil
}

// LeadsDatabaseSchema is the schema used when the app creates a database for
// the user. Auto-create is the safe default precisely because this schema is
// known-correct: every Ziga field has a home of the right type, so nothing is
// ever dropped.
func LeadsDatabaseSchema() []Property {
	return []Property{
		{Name: "Name", Type: TypeTitle},
		{Name: "Contact", Type: TypeEmail},
		{Name: "Source", Type: TypeSelect},
		{Name: "Need", Type: TypeRichText},
		{Name: "Date", Type: TypeDate},
		{Name: "Notes", Type: TypeRichText},
		{Name: "Flags", Type: TypeRichText},
	}
}

// LeadsDatabaseMapping is the mapping for a database created by
// LeadsDatabaseSchema.
func LeadsDatabaseMapping() Mapping {
	return Mapping{
		"name":    {Name: "Name", Type: TypeTitle},
		"contact": {Name: "Contact", Type: TypeEmail},
		"source":  {Name: "Source", Type: TypeSelect},
		"need":    {Name: "Need", Type: TypeRichText},
		"date":    {Name: "Date", Type: TypeDate},
		"notes":   {Name: "Notes", Type: TypeRichText},
		"flags":   {Name: "Flags", Type: TypeRichText},
	}
}

// buildValue renders a value into the Notion payload for a property of the
// given type. ok is false when the value cannot be represented in that type —
// a malformed date into a date property, say. Such a value is reported as
// dropped rather than silently coerced into something wrong.
//
// An empty value is never written: Notion treats an omitted property as empty,
// and writing an explicit empty select would be an error.
func buildValue(propType, value string) (any, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, false
	}
	switch propType {
	case TypeTitle:
		return map[string]any{"title": newRichText(value)}, true
	case TypeRichText:
		return map[string]any{"rich_text": newRichText(value)}, true
	case TypeEmail:
		// The contact field may hold a phone number or a social handle, which
		// an email property would reject outright.
		if _, err := mail.ParseAddress(value); err != nil {
			return nil, false
		}
		return map[string]any{"email": value}, true
	case TypePhoneNumber:
		return map[string]any{"phone_number": value}, true
	case TypeURL:
		return map[string]any{"url": value}, true
	case TypeDate:
		if !validNotionDate(value) {
			return nil, false
		}
		return map[string]any{"date": map[string]any{"start": value}}, true
	case TypeSelect:
		// Notion rejects a select option containing a comma.
		if strings.Contains(value, ",") {
			return nil, false
		}
		return map[string]any{"select": map[string]any{"name": value}}, true
	case TypeMultiSelect:
		var opts []map[string]string
		for _, part := range strings.Split(value, ",") {
			if p := strings.TrimSpace(part); p != "" {
				opts = append(opts, map[string]string{"name": p})
			}
		}
		if len(opts) == 0 {
			return nil, false
		}
		return map[string]any{"multi_select": opts}, true
	case TypeNumber:
		n, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return nil, false
		}
		return map[string]any{"number": n}, true
	}
	return nil, false
}

// validNotionDate accepts the ISO forms Notion's date property takes. Ziga's
// date field is YYYY-MM-DD, but a user edit could be anything.
func validNotionDate(v string) bool {
	for _, layout := range []string{"2006-01-02", time.RFC3339, "2006-01-02T15:04:05"} {
		if _, err := time.Parse(layout, v); err == nil {
			return true
		}
	}
	return false
}

// sortProperties orders a schema deterministically: the title first (it is the
// row's identity), then by name. Response property order is a map iteration
// otherwise, which would shuffle the mapping UI between loads.
func sortProperties(props []Property) {
	sort.SliceStable(props, func(i, j int) bool {
		if (props[i].Type == TypeTitle) != (props[j].Type == TypeTitle) {
			return props[i].Type == TypeTitle
		}
		return props[i].Name < props[j].Name
	})
}

func joinComma(parts []string) string { return strings.Join(parts, ", ") }

// trimFloat renders a number without a trailing ".0" for whole values.
func trimFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}
