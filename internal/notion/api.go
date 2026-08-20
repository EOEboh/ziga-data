package notion

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// Property types this package understands. Notion has more; anything not
// listed here is still surfaced to the user in the mapping UI but is only ever
// written as text when its type permits.
const (
	TypeTitle       = "title"
	TypeRichText    = "rich_text"
	TypeEmail       = "email"
	TypePhoneNumber = "phone_number"
	TypeURL         = "url"
	TypeDate        = "date"
	TypeSelect      = "select"
	TypeMultiSelect = "multi_select"
	TypeNumber      = "number"
	// TypeStatus is deliberately never auto-mapped: unlike select, Notion does
	// not allow adding new status options through the API, so a lead with an
	// unseen value could not be written.
	TypeStatus = "status"
)

// Property is one property in a data source's schema. Name is the property's
// exact name as Notion returned it — Notion property names are case-sensitive
// ("Name" and "name" are different properties), so this string is never
// normalized.
type Property struct {
	ID   string
	Name string
	Type string
	// SelectOptions are the existing options of a select property, used to
	// decide whether writing a value needs a schema update first.
	SelectOptions []string
}

// DataSource is a data source's identity and schema.
type DataSource struct {
	ID         string
	Title      string
	Properties []Property
}

// Property returns the property with an exact (case-sensitive) name.
func (d *DataSource) Property(name string) (Property, bool) {
	for _, p := range d.Properties {
		if p.Name == name {
			return p, true
		}
	}
	return Property{}, false
}

// Resource is a database or page the integration was granted, as returned by
// search. Databases are destination candidates; pages are potential parents
// for an auto-created database.
type Resource struct {
	ID    string
	Title string
	// DataSourceID is set for databases: the data source to read the schema
	// from and write pages into.
	DataSourceID string
}

// rawProperty is the wire shape of one schema property.
type rawProperty struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Type   string `json:"type"`
	Select *struct {
		Options []struct {
			Name string `json:"name"`
		} `json:"options"`
	} `json:"select"`
	MultiSelect *struct {
		Options []struct {
			Name string `json:"name"`
		} `json:"options"`
	} `json:"multi_select"`
}

func (r rawProperty) toProperty(name string) Property {
	p := Property{ID: r.ID, Name: name, Type: r.Type}
	// The map key is authoritative for the name; the body may omit it.
	if r.Name != "" {
		p.Name = r.Name
	}
	switch {
	case r.Select != nil:
		for _, o := range r.Select.Options {
			p.SelectOptions = append(p.SelectOptions, o.Name)
		}
	case r.MultiSelect != nil:
		for _, o := range r.MultiSelect.Options {
			p.SelectOptions = append(p.SelectOptions, o.Name)
		}
	}
	return p
}

// richText is Notion's array-of-spans text representation, used for both
// titles and rich_text values.
type richText struct {
	Text struct {
		Content string `json:"content"`
	} `json:"text"`
	PlainText string `json:"plain_text,omitempty"`
}

func newRichText(s string) []richText {
	var rt richText
	rt.Text.Content = s
	return []richText{rt}
}

func plainText(spans []richText) string {
	out := ""
	for _, s := range spans {
		if s.PlainText != "" {
			out += s.PlainText
			continue
		}
		out += s.Text.Content
	}
	return out
}

// ResolveDataSource follows a database to the data source that holds its
// schema.
//
// Since 2025-09-03 a database parents one or more data sources. v1 writes to a
// single data source, so the first one is used; the id is stored on the
// destination so later writes skip this hop.
func (c *Client) ResolveDataSource(ctx context.Context, databaseID string) (string, error) {
	var resp struct {
		DataSources []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"data_sources"`
	}
	if err := c.do(ctx, http.MethodGet, "/databases/"+databaseID, nil, &resp); err != nil {
		return "", err
	}
	if len(resp.DataSources) == 0 {
		return "", fmt.Errorf("notion: database %s has no data sources", databaseID)
	}
	return resp.DataSources[0].ID, nil
}

// GetDataSource reads a data source's property schema.
func (c *Client) GetDataSource(ctx context.Context, dataSourceID string) (*DataSource, error) {
	var resp struct {
		ID         string                 `json:"id"`
		Title      []richText             `json:"title"`
		Properties map[string]rawProperty `json:"properties"`
	}
	if err := c.do(ctx, http.MethodGet, "/data_sources/"+dataSourceID, nil, &resp); err != nil {
		return nil, err
	}
	ds := &DataSource{ID: resp.ID, Title: plainText(resp.Title)}
	if ds.ID == "" {
		ds.ID = dataSourceID
	}
	for name, raw := range resp.Properties {
		ds.Properties = append(ds.Properties, raw.toProperty(name))
	}
	sortProperties(ds.Properties)
	return ds, nil
}

// GrantedResources lists the databases and pages the user granted during
// consent. Databases are resolved to their data source so the caller can read
// a schema without a second round trip per database.
//
// Notion's search returns results the integration can see, which is exactly
// the set the user picked — the app never sees the rest of the workspace.
func (c *Client) GrantedResources(ctx context.Context) (databases, pages []Resource, err error) {
	databases, err = c.search(ctx, "data_source")
	if err != nil {
		return nil, nil, err
	}
	pages, err = c.search(ctx, "page")
	if err != nil {
		return nil, nil, err
	}
	return databases, pages, nil
}

func (c *Client) search(ctx context.Context, objectType string) ([]Resource, error) {
	body := map[string]any{
		"filter":    map[string]string{"property": "object", "value": objectType},
		"page_size": 100,
	}
	var resp struct {
		Results []struct {
			ID     string     `json:"id"`
			Object string     `json:"object"`
			Title  []richText `json:"title"`
			Parent struct {
				Type       string `json:"type"`
				DatabaseID string `json:"database_id"`
			} `json:"parent"`
			// `title` is decoded lazily: on a page result it is an array of
			// rich-text spans (the page's name), but on a data_source result
			// the same key is the property *schema*, where it is an empty
			// object. Decoding it as []richText unconditionally fails the
			// whole response.
			Properties map[string]struct {
				Type  string          `json:"type"`
				Title json.RawMessage `json:"title"`
			} `json:"properties"`
		} `json:"results"`
	}
	if err := c.do(ctx, http.MethodPost, "/search", body, &resp); err != nil {
		return nil, err
	}

	out := make([]Resource, 0, len(resp.Results))
	for _, r := range resp.Results {
		res := Resource{ID: r.ID, Title: plainText(r.Title)}
		if res.Title == "" {
			// A page's name lives in its title property rather than a
			// top-level title array.
			for _, p := range r.Properties {
				if p.Type != TypeTitle {
					continue
				}
				// Only the array form carries a name; the schema's object
				// form has none, which leaves the "(untitled)" fallback.
				if len(p.Title) > 0 && p.Title[0] == '[' {
					var spans []richText
					if err := json.Unmarshal(p.Title, &spans); err == nil {
						res.Title = plainText(spans)
					}
				}
				break
			}
		}
		if res.Title == "" {
			res.Title = "(untitled)"
		}
		if r.Object == "data_source" {
			// For a data source, the user-facing identity is its parent
			// database; the data source id is what writes address.
			res.DataSourceID = r.ID
			if r.Parent.DatabaseID != "" {
				res.ID = r.Parent.DatabaseID
			}
		}
		out = append(out, res)
	}
	return out, nil
}

// CreateDatabase creates a database under a parent page with the given
// property schema, and returns the new database and data source ids.
//
// Since 2025-09-03 the schema is nested under initial_data_source, separating
// database-level attributes (title) from data-source-level schema.
func (c *Client) CreateDatabase(ctx context.Context, parentPageID, title string, props []Property) (databaseID, dataSourceID string, err error) {
	schema := map[string]any{}
	for _, p := range props {
		spec := map[string]any{p.Type: map[string]any{}}
		if p.Type == TypeSelect && len(p.SelectOptions) > 0 {
			opts := make([]map[string]string, 0, len(p.SelectOptions))
			for _, o := range p.SelectOptions {
				opts = append(opts, map[string]string{"name": o})
			}
			spec[p.Type] = map[string]any{"options": opts}
		}
		schema[p.Name] = spec
	}

	body := map[string]any{
		"parent": map[string]any{"type": "page_id", "page_id": parentPageID},
		"title":  newRichText(title),
		"initial_data_source": map[string]any{
			"properties": schema,
		},
	}
	var resp struct {
		ID          string `json:"id"`
		DataSources []struct {
			ID string `json:"id"`
		} `json:"data_sources"`
	}
	if err := c.do(ctx, http.MethodPost, "/databases", body, &resp); err != nil {
		return "", "", err
	}
	if len(resp.DataSources) == 0 {
		// The response should always carry the initial data source; resolve it
		// explicitly rather than storing a destination we cannot write to.
		dsID, rerr := c.ResolveDataSource(ctx, resp.ID)
		if rerr != nil {
			return "", "", rerr
		}
		return resp.ID, dsID, nil
	}
	return resp.ID, resp.DataSources[0].ID, nil
}

// CreatePage adds one page (a row) to a data source. properties is the
// already-built Notion property payload. It returns the new page's URL.
func (c *Client) CreatePage(ctx context.Context, dataSourceID string, properties map[string]any) (string, error) {
	body := map[string]any{
		"parent":     map[string]any{"type": "data_source_id", "data_source_id": dataSourceID},
		"properties": properties,
	}
	var resp struct {
		URL string `json:"url"`
	}
	if err := c.do(ctx, http.MethodPost, "/pages", body, &resp); err != nil {
		return "", err
	}
	return resp.URL, nil
}

// AddSelectOption appends an option to a select property's schema.
//
// Notion's documentation is not explicit about whether creating a page with an
// unseen select value creates the option implicitly. Rather than depend on
// that, the writer attempts the write and calls this only if Notion rejects
// the value — correct either way, and it costs nothing on the common path.
func (c *Client) AddSelectOption(ctx context.Context, dataSourceID string, prop Property, option string) error {
	opts := make([]map[string]string, 0, len(prop.SelectOptions)+1)
	for _, o := range prop.SelectOptions {
		opts = append(opts, map[string]string{"name": o})
	}
	opts = append(opts, map[string]string{"name": option})

	body := map[string]any{
		"properties": map[string]any{
			prop.Name: map[string]any{
				prop.Type: map[string]any{"options": opts},
			},
		},
	}
	return c.do(ctx, http.MethodPatch, "/data_sources/"+dataSourceID, body, nil)
}

// QueryRecent returns up to n most recently created pages of a data source,
// as a map of property name to display string, for the preview strip.
func (c *Client) QueryRecent(ctx context.Context, dataSourceID string, n int) ([]map[string]string, error) {
	body := map[string]any{
		"page_size": n,
		"sorts": []map[string]string{
			{"timestamp": "created_time", "direction": "descending"},
		},
	}
	var resp struct {
		Results []struct {
			Properties map[string]propertyValue `json:"properties"`
		} `json:"results"`
	}
	if err := c.do(ctx, http.MethodPost, "/data_sources/"+dataSourceID+"/query", body, &resp); err != nil {
		return nil, err
	}
	out := make([]map[string]string, 0, len(resp.Results))
	for _, page := range resp.Results {
		row := map[string]string{}
		for name, val := range page.Properties {
			row[name] = val.display()
		}
		out = append(out, row)
	}
	return out, nil
}

// propertyValue is a page's value for one property, across the types this
// package writes.
type propertyValue struct {
	Type     string     `json:"type"`
	Title    []richText `json:"title"`
	RichText []richText `json:"rich_text"`
	Email    *string    `json:"email"`
	Phone    *string    `json:"phone_number"`
	URL      *string    `json:"url"`
	Number   *float64   `json:"number"`
	Select   *struct {
		Name string `json:"name"`
	} `json:"select"`
	MultiSelect []struct {
		Name string `json:"name"`
	} `json:"multi_select"`
	Date *struct {
		Start string `json:"start"`
	} `json:"date"`
}

// display renders a property value as the string the preview strip shows.
func (v propertyValue) display() string {
	switch v.Type {
	case TypeTitle:
		return plainText(v.Title)
	case TypeRichText:
		return plainText(v.RichText)
	case TypeEmail:
		return derefString(v.Email)
	case TypePhoneNumber:
		return derefString(v.Phone)
	case TypeURL:
		return derefString(v.URL)
	case TypeSelect:
		if v.Select != nil {
			return v.Select.Name
		}
	case TypeMultiSelect:
		names := make([]string, 0, len(v.MultiSelect))
		for _, o := range v.MultiSelect {
			names = append(names, o.Name)
		}
		return joinComma(names)
	case TypeDate:
		if v.Date != nil {
			return v.Date.Start
		}
	case TypeNumber:
		if v.Number != nil {
			return trimFloat(*v.Number)
		}
	}
	return ""
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
