package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/EOEboh/ziga-data/internal/destination"
	"github.com/EOEboh/ziga-data/internal/notion"
	"github.com/EOEboh/ziga-data/internal/store"
)

// notionLeadsDatabaseTitle is the name of a database the app creates.
const notionLeadsDatabaseTitle = "Ziga Leads"

// notionClient builds an API client for the user's connected workspace.
func (s *Server) notionClient(ctx context.Context, uid int64) (*notion.Client, error) {
	token, err := s.notionAccessToken(ctx, uid)
	if err != nil {
		return nil, err
	}
	acct, err := s.store.GetOAuthAccount(ctx, uid, notionProvider)
	if err != nil {
		return nil, err
	}
	// The bot id keys the rate limiter: Notion's budget is per integration
	// install, so each connected workspace gets its own allowance.
	return notion.New(token, acct.ProviderSub, s.cfg.NotionVersion, s.notionBaseURL)
}

// notionError maps a Notion API error to a response. Revoked access and
// ungranted resources become a reconnect prompt rather than a generic failure;
// everything else is a bad-gateway with the detail kept in the log.
func (s *Server) notionError(w http.ResponseWriter, r *http.Request, what string, err error) {
	uid := userID(r)
	switch {
	case errors.Is(err, errReconnect):
		httpError(w, http.StatusConflict, "Connect your Notion workspace")
	case notion.NeedsReconnect(err):
		s.log.Error(what, "err", err, "notion_access", "revoked_or_ungranted")
		s.markNotionBroken(r.Context(), uid)
		httpError(w, http.StatusConflict,
			"Ziga has lost access to that Notion page. Reconnect Notion and grant it again")
	default:
		s.log.Error(what, "err", err)
		httpError(w, http.StatusBadGateway, "Notion did not accept that request")
	}
}

// handleNotionResources lists what the user granted during consent: databases
// they could use as a destination, and pages a new database could be created
// under.
func (s *Server) handleNotionResources(w http.ResponseWriter, r *http.Request) {
	if !s.requireNotion(w) {
		return
	}
	ctx := r.Context()
	client, err := s.notionClient(ctx, userID(r))
	if err != nil {
		s.notionError(w, r, "notion client", err)
		return
	}
	databases, pages, err := client.GrantedResources(ctx)
	if err != nil {
		s.notionError(w, r, "notion search", err)
		return
	}

	toJSON := func(list []notion.Resource) []map[string]any {
		out := make([]map[string]any, 0, len(list))
		for _, res := range list {
			out = append(out, map[string]any{
				"id": res.ID, "title": res.Title, "data_source_id": res.DataSourceID,
			})
		}
		return out
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"databases": toJSON(databases),
		"pages":     toJSON(pages),
		// Creating a database needs a parent page. If the user granted only
		// databases there is nowhere to put one, and the UI has to say so
		// rather than let them hit an opaque failure.
		"can_create": len(pages) > 0,
	})
}

// handleNotionMapping returns the proposed field→property mapping for a
// database, along with every property the user could choose instead. The
// mapping is only a starting point: the user reviews and adjusts it before it
// is saved.
func (s *Server) handleNotionMapping(w http.ResponseWriter, r *http.Request) {
	if !s.requireNotion(w) {
		return
	}
	databaseID := r.PathValue("id")
	if databaseID == "" {
		httpError(w, http.StatusBadRequest, "database id is required")
		return
	}
	ctx := r.Context()
	client, err := s.notionClient(ctx, userID(r))
	if err != nil {
		s.notionError(w, r, "notion client", err)
		return
	}
	ds, _, err := s.resolveDataSource(ctx, client, databaseID)
	if err != nil {
		s.notionError(w, r, "notion schema fetch", err)
		return
	}

	fields := s.cfg.Schema.Columns
	mapping := notion.AutoMap(fields, ds)

	properties := make([]map[string]any, 0, len(ds.Properties))
	for _, p := range ds.Properties {
		properties = append(properties, map[string]any{
			"name": p.Name, "type": p.Type, "writable": notion.Mappable(p.Type),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"database_id":    databaseID,
		"data_source_id": ds.ID,
		"database_title": ds.Title,
		"fields":         fields,
		"properties":     properties,
		"mapping":        mappingJSON(mapping),
		"unmapped":       unmappedFields(fields, mapping),
	})
}

// resolveDataSource follows a database to its data source and reads the schema.
func (s *Server) resolveDataSource(ctx context.Context, client *notion.Client, databaseID string) (*notion.DataSource, string, error) {
	dataSourceID, err := client.ResolveDataSource(ctx, databaseID)
	if err != nil {
		return nil, "", err
	}
	ds, err := client.GetDataSource(ctx, dataSourceID)
	if err != nil {
		return nil, "", err
	}
	return ds, dataSourceID, nil
}

// handleNotionCreateDatabase creates a "Ziga Leads" database under a granted
// page and makes it the user's destination.
//
// This is the safe default offered in the UI: the app owns the schema, so
// every field has a home of the right type and nothing is ever dropped.
func (s *Server) handleNotionCreateDatabase(w http.ResponseWriter, r *http.Request) {
	if !s.requireNotion(w) {
		return
	}
	var req struct {
		ParentPageID string `json:"parent_page_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ParentPageID == "" {
		httpError(w, http.StatusBadRequest, "parent_page_id is required")
		return
	}
	ctx := r.Context()
	uid := userID(r)
	client, err := s.notionClient(ctx, uid)
	if err != nil {
		s.notionError(w, r, "notion client", err)
		return
	}
	databaseID, dataSourceID, err := client.CreateDatabase(
		ctx, req.ParentPageID, notionLeadsDatabaseTitle, notion.LeadsDatabaseSchema())
	if err != nil {
		s.notionError(w, r, "notion create database", err)
		return
	}

	cfg := &store.NotionConfig{
		DatabaseID:    databaseID,
		DataSourceID:  dataSourceID,
		DatabaseTitle: notionLeadsDatabaseTitle,
		CreatedByApp:  true,
		Mapping:       storeMapping(notion.LeadsDatabaseMapping()),
	}
	if err := s.saveNotionDestination(ctx, uid, cfg); err != nil {
		s.log.Error("save notion destination", "err", err)
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"database_id": databaseID, "database_title": notionLeadsDatabaseTitle,
		"created_by_app": true, "mapping": cfg.Mapping,
	})
}

// handleNotionSetDestination saves an existing database, with the user's
// reviewed mapping, as the destination.
//
// The mapping is re-validated against the live schema here rather than trusted
// from the client: the database could have been edited between the mapping
// screen and the save, and a mapping that no longer fits would fail on the
// user's first real lead.
func (s *Server) handleNotionSetDestination(w http.ResponseWriter, r *http.Request) {
	if !s.requireNotion(w) {
		return
	}
	var req struct {
		DatabaseID string                          `json:"database_id"`
		Mapping    map[string]store.MappedProperty `json:"mapping"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.DatabaseID == "" {
		httpError(w, http.StatusBadRequest, "database_id is required")
		return
	}
	ctx := r.Context()
	uid := userID(r)
	client, err := s.notionClient(ctx, uid)
	if err != nil {
		s.notionError(w, r, "notion client", err)
		return
	}
	ds, dataSourceID, err := s.resolveDataSource(ctx, client, req.DatabaseID)
	if err != nil {
		s.notionError(w, r, "notion schema fetch", err)
		return
	}

	// An empty mapping means "use the suggestion".
	mapping := notionMapping(req.Mapping)
	if len(mapping) == 0 {
		mapping = notion.AutoMap(s.cfg.Schema.Columns, ds)
	}
	// Unknown field names would silently never be written.
	known := map[string]bool{}
	for _, f := range s.cfg.Schema.Columns {
		known[f] = true
	}
	for field := range mapping {
		if !known[field] {
			httpError(w, http.StatusBadRequest, "unknown field "+field)
			return
		}
	}
	if err := mapping.Validate(ds); err != nil {
		httpError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	cfg := &store.NotionConfig{
		DatabaseID:    req.DatabaseID,
		DataSourceID:  dataSourceID,
		DatabaseTitle: ds.Title,
		CreatedByApp:  false,
		Mapping:       storeMapping(mapping),
	}
	if err := s.saveNotionDestination(ctx, uid, cfg); err != nil {
		s.log.Error("save notion destination", "err", err)
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"database_id": req.DatabaseID, "database_title": ds.Title,
		"created_by_app": false,
		"mapping":        cfg.Mapping,
		"unmapped":       unmappedFields(s.cfg.Schema.Columns, mapping),
	})
}

// saveNotionDestination makes Notion the user's active destination, replacing
// whatever was there (one destination at a time).
func (s *Server) saveNotionDestination(ctx context.Context, uid int64, cfg *store.NotionConfig) error {
	// Carry the workspace identity from the stored link so the destination is
	// self-describing in the UI.
	if acct, err := s.store.GetOAuthAccount(ctx, uid, notionProvider); err == nil {
		cfg.WorkspaceID = acct.ProviderSub
	}
	blob, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return s.store.SetDestination(ctx, &store.Destination{
		UserID: uid, Type: string(destination.TypeNotion), Config: blob,
	})
}

// notionWriter builds the per-user Notion writer for the confirm path.
func (s *Server) notionWriter(ctx context.Context, uid int64, dest *store.Destination) (destination.Writer, error) {
	cfg, err := dest.NotionConfig()
	if err != nil {
		return nil, err
	}
	client, err := s.notionClient(ctx, uid)
	if err != nil {
		return nil, err
	}
	return notion.NewWriter(client, cfg.DataSourceID, notionMapping(cfg.Mapping), s.cfg.Schema.Columns), nil
}

// --- conversions between the store's JSON shape and the notion package ---

func storeMapping(m notion.Mapping) map[string]store.MappedProperty {
	out := make(map[string]store.MappedProperty, len(m))
	for field, target := range m {
		out[field] = store.MappedProperty{Name: target.Name, Type: target.Type}
	}
	return out
}

func notionMapping(m map[string]store.MappedProperty) notion.Mapping {
	out := make(notion.Mapping, len(m))
	for field, target := range m {
		out[field] = notion.MappedProperty{Name: target.Name, Type: target.Type}
	}
	return out
}

func mappingJSON(m notion.Mapping) map[string]store.MappedProperty { return storeMapping(m) }

// unmappedFields names the schema fields with no target property, so the UI
// can show up front which fields this database will not receive.
func unmappedFields(fields []string, m notion.Mapping) []string {
	out := []string{}
	for _, f := range fields {
		if _, ok := m[f]; !ok {
			out = append(out, f)
		}
	}
	return out
}
