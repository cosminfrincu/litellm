package litellm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/hashicorp/go-cty/cty"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
)

func keyJSONRawConfig(t *testing.T, config map[string]interface{}) cty.Value {
	t.Helper()
	raw := map[string]cty.Value{}
	for _, field := range []string{"allowed_routes", "allowed_passthrough_routes"} {
		if list, present := config[field].([]interface{}); present {
			if len(list) == 0 {
				raw[field] = cty.ListValEmpty(cty.String)
			} else {
				raw[field] = keyRawConfigRoutes(t, expandStringList(list))
			}
		} else {
			raw[field] = cty.NullVal(cty.List(cty.String))
		}
	}
	return cty.ObjectVal(raw)
}

type jsonKeyProxy struct {
	info   map[string]interface{}
	writes []map[string]interface{}
	ignore string
}

func (p *jsonKeyProxy) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/key/generate", "/key/update":
			var body map[string]interface{}
			decoder := json.NewDecoder(r.Body)
			decoder.UseNumber()
			if err := decoder.Decode(&body); err != nil {
				t.Errorf("decode request: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if _, present := body["allowed_passthrough_routes"]; present {
				w.WriteHeader(http.StatusForbidden)
				w.Write([]byte(`{"error":"top-level passthrough routes require Enterprise"}`))
				return
			}
			p.writes = append(p.writes, body)
			for field, value := range body {
				if field != p.ignore {
					p.info[field] = value
				}
			}
			json.NewEncoder(w).Encode(map[string]interface{}{"token_id": "hash-1", "key": "sk-test-only"})
		case "/key/info":
			json.NewEncoder(w).Encode(map[string]interface{}{"key": "hash-1", "info": p.info})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}
}

func TestKeyJSONCreateAndRefresh(t *testing.T) {
	proxy := &jsonKeyProxy{info: map[string]interface{}{}}
	srv := httptest.NewServer(proxy.handler(t))
	defer srv.Close()
	client := NewClient(srv.URL, "test-key", true)
	config := map[string]interface{}{
		"key_alias": "json-key",
		"metadata_json": `{
			"allowed_passthrough_routes":["/direct/openai/v1/batches"],
			"nested":{"enabled":false,"count":9007199254740993,"value":null},
			"timestamp":"2026-01-01T00:00:00Z"
		}`,
		"router_settings": `{"fallbacks":[],"context_window_fallbacks":[]}`,
	}
	r := resourceKey()
	prior := &terraform.InstanceState{RawConfig: keyJSONRawConfig(t, config)}
	diff, err := r.Diff(context.Background(), prior, terraform.NewResourceConfigRaw(config), client)
	if err != nil {
		t.Fatal(err)
	}
	diff.RawConfig = prior.RawConfig
	state, diags := r.Apply(context.Background(), nil, diff, client)
	if diags.HasError() {
		t.Fatal(diags)
	}
	if len(proxy.writes) != 1 {
		t.Fatalf("writes = %d, want one create", len(proxy.writes))
	}
	metadata, ok := proxy.info["metadata"].(map[string]interface{})
	if !ok {
		t.Fatalf("create did not send JSON metadata: %v", proxy.info)
	}
	nested := metadata["nested"].(map[string]interface{})
	if nested["count"] != json.Number("9007199254740993") || nested["enabled"] != false || nested["value"] != nil {
		t.Errorf("metadata changed JSON types or precision: %v", nested)
	}
	if _, present := proxy.writes[0]["metadata_json"]; present {
		t.Error("Terraform-only field sent to the API")
	}
	if state.ID != "hash-1" || state.Attributes["key"] != "" {
		t.Errorf("state must contain only the token ID, got ID=%q key=%q", state.ID, state.Attributes["key"])
	}
	state.RawConfig = prior.RawConfig
	next, err := r.Diff(context.Background(), state, terraform.NewResourceConfigRaw(config), client)
	if err != nil {
		t.Fatal(err)
	}
	if next != nil && !next.Empty() {
		t.Fatalf("unchanged config drifted after create: %v", next)
	}
	metadata["allowed_passthrough_routes"] = []interface{}{"/unexpected"}
	refreshed, diags := r.RefreshWithoutUpgrade(context.Background(), state, client)
	if diags.HasError() {
		t.Fatal(diags)
	}
	refreshed.RawConfig = prior.RawConfig
	next, err = r.Diff(context.Background(), refreshed, terraform.NewResourceConfigRaw(config), client)
	if err != nil || next == nil || next.Attributes["metadata_json"] == nil {
		t.Fatalf("out-of-band route change did not appear in the plan: diff=%v err=%v", next, err)
	}
}

func TestKeyJSONUpdatesPreserveUnmanagedSettings(t *testing.T) {
	for _, removal := range []string{"replace", "empty-object", "remove-attribute"} {
		t.Run(removal, func(t *testing.T) {
			proxy := &jsonKeyProxy{info: map[string]interface{}{
				"key_alias": "alias-1",
				"blocked":   true,
				"metadata": map[string]interface{}{
					"allowed_passthrough_routes": []interface{}{"/old"},
					"outside": map[string]interface{}{
						"count": json.Number("9007199254740993"), "empty": []interface{}{}, "false": false, "null": nil,
					},
				},
				"router_settings": map[string]interface{}{
					"fallbacks": []interface{}{}, "context_window_fallbacks": []interface{}{}, "num_retries": 3,
				},
			}}
			srv := httptest.NewServer(proxy.handler(t))
			defer srv.Close()
			config := map[string]interface{}{"key_alias": "alias-1", "blocked": true}
			switch removal {
			case "replace":
				config["metadata_json"] = `{"allowed_passthrough_routes":["/new"]}`
				config["router_settings"] = `{"fallbacks":null,"context_window_fallbacks":[]}`
			case "empty-object":
				config["metadata_json"] = `{}`
				config["router_settings"] = `{}`
			}
			state := applyKeyUpdate(t, NewClient(srv.URL, "test-key", true), map[string]string{
				"key_alias": "alias-1", "blocked": "true",
				"metadata_json":                `{"allowed_passthrough_routes":["/old"]}`,
				"router_settings":              `{"fallbacks":[],"context_window_fallbacks":[]}`,
				"allowed_passthrough_routes.#": "1", "allowed_passthrough_routes.0": "/old",
			}, config)
			if len(proxy.writes) != 1 || proxy.info["blocked"] != true {
				t.Fatalf("expected one update preserving the block: writes=%v info=%v", proxy.writes, proxy.info)
			}
			metadata := proxy.info["metadata"].(map[string]interface{})
			outside := metadata["outside"].(map[string]interface{})
			if outside["count"] != json.Number("9007199254740993") || outside["false"] != false || outside["null"] != nil {
				t.Errorf("unmanaged metadata was altered: %v", outside)
			}
			router := proxy.info["router_settings"].(map[string]interface{})
			if router["num_retries"] != json.Number("3") {
				t.Errorf("unmanaged router setting was altered: %v", router)
			}
			if removal == "replace" {
				if !reflect.DeepEqual(metadata["allowed_passthrough_routes"], []interface{}{"/new"}) {
					t.Errorf("route update was lost: %v", metadata)
				}
				if value, present := router["fallbacks"]; !present || value != nil {
					t.Errorf("explicit null lost: %v", router)
				}
				if !reflect.DeepEqual(router["context_window_fallbacks"], []interface{}{}) {
					t.Errorf("explicit empty fallback list lost: %v", router)
				}
			} else {
				if _, present := metadata["allowed_passthrough_routes"]; present {
					t.Errorf("removed route grant remains: %v", metadata)
				}
				if _, present := router["fallbacks"]; present {
					t.Errorf("removed fallback setting remains: %v", router)
				}
			}
			state.RawConfig = keyJSONRawConfig(t, config)
			diff, err := resourceKey().Diff(context.Background(), state, terraform.NewResourceConfigRaw(config), NewClient(srv.URL, "test-key", true))
			if err != nil || (diff != nil && !diff.Empty()) {
				t.Errorf("update did not converge: diff=%v err=%v", diff, err)
			}
		})
	}
}

func TestKeyJSONUnchangedSettingsAreNotWritten(t *testing.T) {
	proxy := &jsonKeyProxy{info: map[string]interface{}{
		"key_alias":       "alias-1",
		"metadata":        map[string]interface{}{"allowed_passthrough_routes": []interface{}{"/keep"}},
		"router_settings": map[string]interface{}{"fallbacks": []interface{}{}},
	}}
	srv := httptest.NewServer(proxy.handler(t))
	defer srv.Close()
	applyKeyUpdate(t, NewClient(srv.URL, "test-key", true),
		map[string]string{
			"key_alias": "alias-1", "models.#": "1", "models.0": "old-model",
			"metadata_json": `{"allowed_passthrough_routes":["/keep"]}`, "router_settings": `{"fallbacks":[]}`,
			"allowed_passthrough_routes.#": "1", "allowed_passthrough_routes.0": "/keep",
		},
		map[string]interface{}{
			"key_alias": "alias-1", "models": []interface{}{"new-model"},
			"metadata_json": `{"allowed_passthrough_routes":["/keep"]}`, "router_settings": `{"fallbacks":[]}`,
		},
	)
	for _, field := range []string{"metadata", "router_settings", "allowed_passthrough_routes"} {
		if _, present := proxy.writes[0][field]; present {
			t.Errorf("unchanged field %q was written", field)
		}
	}
}

func TestKeyJSONFailsWhenProxyDoesNotStoreUpdate(t *testing.T) {
	for _, field := range []string{"metadata_json", "router_settings"} {
		t.Run(field, func(t *testing.T) {
			apiField := field
			if field == "metadata_json" {
				apiField = "metadata"
			}
			proxy := &jsonKeyProxy{
				info:   map[string]interface{}{apiField: map[string]interface{}{"owned": "old"}},
				ignore: apiField,
			}
			srv := httptest.NewServer(proxy.handler(t))
			defer srv.Close()
			config := map[string]interface{}{field: `{"owned":"new"}`}
			prior := &terraform.InstanceState{
				ID: "hash-1", Attributes: map[string]string{field: `{"owned":"old"}`},
				RawConfig: keyJSONRawConfig(t, config),
			}
			client := NewClient(srv.URL, "test-key", true)
			r := resourceKey()
			diff, err := r.Diff(context.Background(), prior, terraform.NewResourceConfigRaw(config), client)
			if err != nil {
				t.Fatal(err)
			}
			diff.RawConfig = prior.RawConfig
			_, diags := r.Apply(context.Background(), prior, diff, client)
			if !diags.HasError() {
				t.Fatal("apply succeeded even though the proxy ignored the JSON update")
			}
		})
	}
}

func TestKeyJSONMetadataMigrationPreservesBothForms(t *testing.T) {
	for _, toJSON := range []bool{true, false} {
		t.Run(map[bool]string{true: "to-json", false: "to-map"}[toJSON], func(t *testing.T) {
			proxy := &jsonKeyProxy{info: map[string]interface{}{
				"metadata": map[string]interface{}{"owned": "old", "outside": false},
			}}
			srv := httptest.NewServer(proxy.handler(t))
			defer srv.Close()
			prior := map[string]string{"metadata_json": `{"owned":"old"}`}
			config := map[string]interface{}{"metadata": map[string]interface{}{"owned": "new"}}
			if toJSON {
				prior = map[string]string{"metadata.%": "1", "metadata.owned": "old"}
				config = map[string]interface{}{"metadata_json": `{"owned":"new"}`}
			}
			applyKeyUpdate(t, NewClient(srv.URL, "test-key", true), prior, config)
			want := map[string]interface{}{"owned": "new", "outside": false}
			if !reflect.DeepEqual(proxy.info["metadata"], want) {
				t.Errorf("migration lost metadata: %v", proxy.info["metadata"])
			}
		})
	}
}

func TestKeyJSONRejectsInvalidObjectsAndConflictingOwnership(t *testing.T) {
	for _, field := range []string{"metadata_json", "router_settings"} {
		for _, raw := range []string{"null", "[]", `"string"`, `{"a":1} {}`, `{"a":`} {
			t.Run(field+"/"+raw, func(t *testing.T) {
				diags := resourceKey().Validate(terraform.NewResourceConfigRaw(map[string]interface{}{field: raw}))
				if !diags.HasError() {
					t.Fatalf("invalid JSON object accepted for %s", field)
				}
			})
		}
	}
	for _, raw := range []string{
		`{"allowed_passthrough_routes":null}`, `{"allowed_passthrough_routes":"/route"}`,
		`{"allowed_passthrough_routes":[1]}`, `{"tags":["tag"]}`,
	} {
		diags := resourceKey().Validate(terraform.NewResourceConfigRaw(map[string]interface{}{"metadata_json": raw}))
		if !diags.HasError() {
			t.Errorf("invalid or separately managed metadata accepted: %s", raw)
		}
	}
	diags := resourceKey().Validate(terraform.NewResourceConfigRaw(map[string]interface{}{
		"metadata_json": `{"owner":"one"}`, "metadata": map[string]interface{}{"owner": "two"},
	}))
	if !diags.HasError() {
		t.Error("both metadata forms accepted")
	}
	conflicting := map[string]interface{}{
		"metadata_json": `{"allowed_passthrough_routes":["/one"]}`, "allowed_passthrough_routes": []interface{}{"/two"},
	}
	prior := &terraform.InstanceState{RawConfig: keyJSONRawConfig(t, conflicting)}
	_, err := resourceKey().Diff(context.Background(), prior, terraform.NewResourceConfigRaw(conflicting), nil)
	if err == nil {
		t.Error("conflicting top-level and JSON passthrough grants accepted")
	}
}

func TestKeyJSONCreateReadbackFailureRetainsIdentity(t *testing.T) {
	proxy := &jsonKeyProxy{info: map[string]interface{}{}, ignore: "router_settings"}
	srv := httptest.NewServer(proxy.handler(t))
	defer srv.Close()
	client := NewClient(srv.URL, "test-key", true)
	r := resourceKey()
	config := map[string]interface{}{"router_settings": `{"fallbacks":[]}`}
	prior := &terraform.InstanceState{RawConfig: keyJSONRawConfig(t, config)}
	diff, err := r.Diff(context.Background(), prior, terraform.NewResourceConfigRaw(config), client)
	if err != nil {
		t.Fatal(err)
	}
	diff.RawConfig = prior.RawConfig
	state, diags := r.Apply(context.Background(), nil, diff, client)
	if !diags.HasError() {
		t.Fatal("create succeeded despite ignored router settings")
	}
	if state == nil || state.ID != "hash-1" || len(proxy.writes) != 1 {
		t.Fatalf("failed create lost the generated key identity: state=%v writes=%d", state, len(proxy.writes))
	}
}

func TestKeyJSONReadsSerializedRouterSettings(t *testing.T) {
	proxy := &jsonKeyProxy{info: map[string]interface{}{
		"router_settings": `{"fallbacks":[],"timeout":0,"context_window_fallbacks":null}`,
	}}
	srv := httptest.NewServer(proxy.handler(t))
	defer srv.Close()
	d := newKeyResourceData(t, map[string]interface{}{
		"router_settings": `{"fallbacks":[],"timeout":10,"context_window_fallbacks":[]}`,
	})
	d.SetId("hash-1")
	if diags := resourceKeyRead(context.Background(), d, NewClient(srv.URL, "test-key", true)); diags.HasError() {
		t.Fatal(diags)
	}
	got, err := parseKeyJSONObject(d.Get("router_settings").(string))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got["fallbacks"], []interface{}{}) || got["timeout"] != json.Number("0") {
		t.Errorf("serialized router settings were not read correctly: %v", got)
	}
	if value, present := got["context_window_fallbacks"]; !present || value != nil {
		t.Errorf("JSON null did not survive refresh: %v", got)
	}
}

func TestKeyJSONNumberComparisonPreservesPrecision(t *testing.T) {
	for _, tc := range []struct {
		name string
		old  string
		new  string
		same bool
	}{
		{"formatting", `{"timeout":1}`, `{"timeout":1.0}`, true},
		{"exponent", `{"nested":[1000]}`, `{"nested":[1e3]}`, true},
		{"large-integers", `{"count":9007199254740992}`, `{"count":9007199254740993}`, false},
		{"null-is-not-empty", `{"fallbacks":null}`, `{"fallbacks":[]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := suppressEquivalentKeyJSON("", tc.old, tc.new, nil); got != tc.same {
				t.Errorf("equivalence=%v, want %v", got, tc.same)
			}
		})
	}
}

func TestKeyUpdateOwnerAssignment(t *testing.T) {
	for _, owner := range []string{"current-owner", "new-owner"} {
		t.Run(owner, func(t *testing.T) {
			proxy := &jsonKeyProxy{info: map[string]interface{}{
				"user_id": "current-owner", "models": []interface{}{"old-model"},
			}}
			srv := httptest.NewServer(proxy.handler(t))
			defer srv.Close()
			state := applyKeyUpdate(t, NewClient(srv.URL, "test-key", true),
				map[string]string{"user_id": "current-owner", "models.#": "1", "models.0": "old-model"},
				map[string]interface{}{"user_id": owner, "models": []interface{}{"new-model"}},
			)
			if state.Attributes["user_id"] != owner {
				t.Errorf("owner = %q, want %q", state.Attributes["user_id"], owner)
			}
			_, sent := proxy.writes[0]["user_id"]
			if sent != (owner != "current-owner") {
				t.Errorf("user_id sent=%v, want only explicit owner changes", sent)
			}
		})
	}
}

func TestKeyPassthroughExplicitEmptyClearsStoredGrant(t *testing.T) {
	proxy := &fakeKeyProxy{metadata: map[string]interface{}{}}
	srv := httptest.NewServer(proxy.handler())
	defer srv.Close()
	state := applyKeyUpdate(t, NewClient(srv.URL, "test-key", true),
		map[string]string{"allowed_passthrough_routes.#": "1", "allowed_passthrough_routes.0": "/old"},
		map[string]interface{}{"allowed_passthrough_routes": []interface{}{}},
	)
	if got, present := proxy.updates[0]["allowed_passthrough_routes"]; !present || !reflect.DeepEqual(got, []interface{}{}) {
		t.Fatalf("explicit empty grant was not sent: %v", proxy.updates[0])
	}
	if got := state.Attributes["allowed_passthrough_routes.#"]; got != "0" {
		t.Errorf("removed grant remains in state: %v", state.Attributes)
	}
}
