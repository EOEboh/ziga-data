// Package cfroute provisions Cloudflare Email Routing rules, one per user
// capture address.
//
// Why one rule per address rather than a single catch-all: Cloudflare supports
// catch-all only at a zone apex, never on a subdomain. Capture addresses live
// on a subdomain precisely so enabling mail routing does not touch the apex's
// MX records and break the domain's real mail. The cost of that choice is that
// each address needs its own literal-match rule, and rules are capped per
// domain — see MaxRulesPerDomain.
package cfroute

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// MaxRulesPerDomain is Cloudflare's documented cap on email routing rules for
// one domain. It is the hard ceiling on how many users can have a capture
// address, which is why provisioning checks a configured budget below this
// number and fails with our own message rather than letting Cloudflare reject
// the request opaquely. Cloudflare raises the limit on request.
const MaxRulesPerDomain = 200

const defaultBaseURL = "https://api.cloudflare.com/client/v4"

// Client talks to the Cloudflare Email Routing rules API for one zone.
type Client struct {
	token      string
	zoneID     string
	workerName string
	baseURL    string
	hc         *http.Client
}

// New returns a client for one zone, routing mail to the named worker.
func New(token, zoneID, workerName string) *Client {
	return &Client{
		token:      token,
		zoneID:     zoneID,
		workerName: workerName,
		baseURL:    defaultBaseURL,
		hc:         &http.Client{Timeout: 15 * time.Second},
	}
}

// SetBaseURL points the client at a different API root. Tests use it to stand
// up a fake; production never calls it.
func (c *Client) SetBaseURL(u string) { c.baseURL = strings.TrimRight(u, "/") }

// Rule is one routing rule as Cloudflare reports it.
type Rule struct {
	ID      string
	Tag     string
	Enabled bool
	// Address is the literal recipient this rule matches, empty for rules with
	// a different matcher shape.
	Address string
}

type apiResponse struct {
	Success bool            `json:"success"`
	Errors  []apiError      `json:"errors"`
	Result  json.RawMessage `json:"result"`
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type apiRule struct {
	ID       string `json:"id"`
	Tag      string `json:"tag"`
	Name     string `json:"name"`
	Enabled  bool   `json:"enabled"`
	Matchers []struct {
		Type  string `json:"type"`
		Field string `json:"field"`
		Value string `json:"value"`
	} `json:"matchers"`
	Actions []struct {
		Type  string   `json:"type"`
		Value []string `json:"value"`
	} `json:"actions"`
}

func (r apiRule) toRule() Rule {
	out := Rule{ID: r.ID, Tag: r.Tag, Enabled: r.Enabled}
	for _, m := range r.Matchers {
		if m.Type == "literal" && m.Field == "to" {
			out.Address = strings.ToLower(m.Value)
			break
		}
	}
	return out
}

// CreateAddressRule makes one address deliverable by pointing a literal
// recipient match at the ingest worker. It returns the rule's id, which the
// caller stores so the rule can be released when the address is retired —
// leaked rules consume a budget that caps the whole feature.
func (c *Client) CreateAddressRule(ctx context.Context, address string) (string, error) {
	body := map[string]any{
		"name":    "ziga inbound " + address,
		"enabled": true,
		"matchers": []map[string]string{
			{"type": "literal", "field": "to", "value": strings.ToLower(address)},
		},
		"actions": []map[string]any{
			{"type": "worker", "value": []string{c.workerName}},
		},
	}
	raw, err := c.do(ctx, http.MethodPost, "/zones/"+c.zoneID+"/email/routing/rules", body)
	if err != nil {
		return "", err
	}
	var rule apiRule
	if err := json.Unmarshal(raw, &rule); err != nil {
		return "", fmt.Errorf("decode created rule: %w", err)
	}
	if rule.ID == "" && rule.Tag == "" {
		return "", fmt.Errorf("cloudflare accepted the rule but returned no identifier")
	}
	// Prefer id: the delete path is /rules/{id}, which wrangler and the API
	// reference agree on. The response also carries a tag, and picking that
	// instead is a trap — the two can differ, DeleteRule would then 404, and
	// because an absent rule is (correctly) treated as already-deleted, the
	// real rule would be leaked silently. Rules are capped per domain, so a
	// leak is capacity lost for good.
	if rule.ID != "" {
		return rule.ID, nil
	}
	return rule.Tag, nil
}

// DeleteRule releases a routing rule. A rule that is already gone is treated
// as success: the goal is that it no longer exists.
func (c *Client) DeleteRule(ctx context.Context, ruleID string) error {
	_, err := c.do(ctx, http.MethodDelete, "/zones/"+c.zoneID+"/email/routing/rules/"+ruleID, nil)
	var notFound *NotFoundError
	if errors.As(err, &notFound) {
		return nil
	}
	return err
}

// ListRules returns every routing rule on the zone, for reconciling stored
// addresses against what actually routes.
func (c *Client) ListRules(ctx context.Context) ([]Rule, error) {
	raw, err := c.do(ctx, http.MethodGet, "/zones/"+c.zoneID+"/email/routing/rules?per_page=200", nil)
	if err != nil {
		return nil, err
	}
	var rules []apiRule
	if err := json.Unmarshal(raw, &rules); err != nil {
		return nil, fmt.Errorf("decode rule list: %w", err)
	}
	out := make([]Rule, 0, len(rules))
	for _, r := range rules {
		out = append(out, r.toRule())
	}
	return out, nil
}

// NotFoundError reports that the addressed rule does not exist.
type NotFoundError struct{ Message string }

func (e *NotFoundError) Error() string { return "cloudflare: not found: " + e.Message }

func (c *Client) do(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	var buf io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		buf = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cloudflare %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}

	var out apiResponse
	// A non-JSON body (a proxy error page, say) is still a failure worth
	// reporting clearly rather than as a decode error.
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("cloudflare %s %s: status %d: %s", method, path, resp.StatusCode, snippet(raw))
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, &NotFoundError{Message: apiErrorText(out.Errors)}
	}
	if !out.Success || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("cloudflare %s %s: status %d: %s", method, path, resp.StatusCode, apiErrorText(out.Errors))
	}
	return out.Result, nil
}

func apiErrorText(errs []apiError) string {
	if len(errs) == 0 {
		return "no error detail returned"
	}
	parts := make([]string, 0, len(errs))
	for _, e := range errs {
		parts = append(parts, fmt.Sprintf("%d %s", e.Code, e.Message))
	}
	return strings.Join(parts, "; ")
}

func snippet(b []byte) string {
	const max = 200
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
