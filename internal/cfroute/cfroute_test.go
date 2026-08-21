package cfroute

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := New("test-token", "zone-1", "ziga-email-ingest")
	c.SetBaseURL(srv.URL)
	return c
}

func TestCreateAddressRuleShape(t *testing.T) {
	var gotPath, gotAuth string
	var body map[string]any

	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"success":true,"errors":[],"result":{"id":"id-1","tag":"tag-1","enabled":true}}`)
	})

	id, err := c.CreateAddressRule(context.Background(), "Lead-ABC@In.Example.com")
	if err != nil {
		t.Fatal(err)
	}
	// The rules API addresses a rule by its tag, so that is what we must store
	// — an id we cannot delete with is a leaked rule.
	if id != "tag-1" {
		t.Errorf("rule id = %q, want the tag", id)
	}
	if gotPath != "/zones/zone-1/email/routing/rules" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer test-token" {
		t.Errorf("auth header = %q", gotAuth)
	}

	matchers, _ := body["matchers"].([]any)
	if len(matchers) != 1 {
		t.Fatalf("want exactly one matcher, got %v", body["matchers"])
	}
	m := matchers[0].(map[string]any)
	if m["type"] != "literal" || m["field"] != "to" {
		t.Errorf("matcher = %v, want a literal match on the recipient", m)
	}
	// Recipients arrive lowercased; a mixed-case rule value would never match.
	if m["value"] != "lead-abc@in.example.com" {
		t.Errorf("matcher value = %v, want it lowercased", m["value"])
	}

	actions, _ := body["actions"].([]any)
	act := actions[0].(map[string]any)
	if act["type"] != "worker" {
		t.Errorf("action type = %v, want the mail to reach our worker", act["type"])
	}
	if vals, _ := act["value"].([]any); len(vals) != 1 || vals[0] != "ziga-email-ingest" {
		t.Errorf("action value = %v, want the configured worker name", act["value"])
	}
	if body["enabled"] != true {
		t.Error("a rule created disabled would silently drop every lead")
	}
}

func TestCreateAddressRuleReportsAPIFailure(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"success":false,"errors":[{"code":1004,"message":"rule limit exceeded"}],"result":null}`)
	})

	_, err := c.CreateAddressRule(context.Background(), "lead-x@in.example.com")
	if err == nil {
		t.Fatal("want an error when the provider rejects the rule")
	}
	// The operator has to be able to tell "we ran out of rules" from any other
	// failure without opening the provider's dashboard.
	if !strings.Contains(err.Error(), "rule limit exceeded") {
		t.Errorf("error must carry the provider's message, got: %v", err)
	}
}

func TestCreateAddressRuleRejectsAnIdentifierlessResponse(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"success":true,"errors":[],"result":{"enabled":true}}`)
	})
	// A rule we cannot address later is a rule we can never release, and rules
	// are the capped resource that bounds the whole feature.
	if _, err := c.CreateAddressRule(context.Background(), "lead-x@in.example.com"); err == nil {
		t.Fatal("want an error when the provider returns no rule identifier")
	}
}

func TestCreateAddressRuleHandlesNonJSONResponse(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		io.WriteString(w, "<html>gateway error</html>")
	})
	_, err := c.CreateAddressRule(context.Background(), "lead-x@in.example.com")
	if err == nil {
		t.Fatal("want an error for a non-JSON failure page")
	}
	if !strings.Contains(err.Error(), "502") {
		t.Errorf("error should report the status, got: %v", err)
	}
}

func TestDeleteRuleTreatsMissingAsSuccess(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"success":false,"errors":[{"code":1002,"message":"rule not found"}]}`)
	})
	// The goal is that the rule no longer exists. One that is already gone has
	// met it, and erroring here would stall the release sweep forever on a
	// rule someone deleted by hand.
	if err := c.DeleteRule(context.Background(), "tag-gone"); err != nil {
		t.Fatalf("deleting an absent rule must succeed, got %v", err)
	}
}

func TestDeleteRuleReportsRealFailures(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `{"success":false,"errors":[{"code":10000,"message":"internal"}]}`)
	})
	var notFound *NotFoundError
	err := c.DeleteRule(context.Background(), "tag-1")
	if err == nil {
		t.Fatal("a server error must not be swallowed — the rule is still there")
	}
	if errors.As(err, &notFound) {
		t.Error("a 500 must not be classified as not-found")
	}
}

func TestListRulesExtractsRecipients(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"success":true,"errors":[],"result":[
			{"id":"i1","tag":"t1","enabled":true,
			 "matchers":[{"type":"literal","field":"to","value":"Lead-One@In.Example.com"}],
			 "actions":[{"type":"worker","value":["ziga-email-ingest"]}]},
			{"id":"i2","tag":"t2","enabled":true,
			 "matchers":[{"type":"all"}],
			 "actions":[{"type":"forward","value":["someone@example.com"]}]}
		]}`)
	})

	rules, err := c.ListRules(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 2 {
		t.Fatalf("want 2 rules, got %d", len(rules))
	}
	if rules[0].Address != "lead-one@in.example.com" {
		t.Errorf("address = %q, want it lowercased for comparison against stored addresses", rules[0].Address)
	}
	// A catch-all or forward rule has no literal recipient; reconciliation
	// must not invent one for it.
	if rules[1].Address != "" {
		t.Errorf("non-literal rule reported address %q", rules[1].Address)
	}
}
