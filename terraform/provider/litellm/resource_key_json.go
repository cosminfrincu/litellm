package litellm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hashicorp/go-cty/cty"
	ctyjson "github.com/hashicorp/go-cty/cty/json"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
)

func parseKeyJSONObject(raw string) (map[string]interface{}, error) {
	if raw == "" {
		return map[string]interface{}{}, nil
	}
	var result map[string]interface{}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&result); err != nil || result == nil || !json.Valid([]byte(raw)) {
		return nil, fmt.Errorf("expected a JSON object")
	}
	return result, nil
}

func validateKeyJSONObject(v interface{}, k string) ([]string, []error) {
	if _, err := parseKeyJSONObject(v.(string)); err != nil || v.(string) == "" {
		return nil, []error{fmt.Errorf("%s must be a JSON object", k)}
	}
	return nil, nil
}

func validateKeyMetadataJSON(v interface{}, k string) ([]string, []error) {
	warnings, errors := validateKeyJSONObject(v, k)
	if len(errors) > 0 {
		return warnings, errors
	}
	metadata, _ := parseKeyJSONObject(v.(string))
	if value, present := metadata["allowed_passthrough_routes"]; present {
		routes, ok := value.([]interface{})
		if !ok {
			return nil, []error{fmt.Errorf("%s.allowed_passthrough_routes must be an array of strings", k)}
		}
		for _, route := range routes {
			if _, ok := route.(string); !ok {
				return nil, []error{fmt.Errorf("%s.allowed_passthrough_routes must be an array of strings", k)}
			}
		}
	}
	for _, field := range keyFieldsStoredInMetadata {
		if _, present := metadata[field]; present && field != "allowed_passthrough_routes" {
			return nil, []error{fmt.Errorf("use the %s attribute instead of metadata_json.%s", field, field)}
		}
	}
	return nil, nil
}

func suppressEquivalentKeyJSON(_ string, old, new string, _ *schema.ResourceData) bool {
	if old == "" || new == "" {
		return old == new
	}
	oldObject, oldErr := parseKeyJSONObject(old)
	newObject, newErr := parseKeyJSONObject(new)
	return oldErr == nil && newErr == nil && equalKeyJSON(oldObject, newObject)
}

func equalKeyJSON(left, right interface{}) bool {
	leftBytes, leftErr := json.Marshal(left)
	rightBytes, rightErr := json.Marshal(right)
	if leftErr != nil || rightErr != nil {
		return false
	}
	var leftValue, rightValue ctyjson.SimpleJSONValue
	if json.Unmarshal(leftBytes, &leftValue) != nil || json.Unmarshal(rightBytes, &rightValue) != nil {
		return false
	}
	return leftValue.Value.RawEquals(rightValue.Value)
}

func validateKeyJSONOwnership(_ context.Context, d *schema.ResourceDiff, _ interface{}) error {
	if !d.NewValueKnown("metadata_json") {
		return nil
	}
	metadata, err := parseKeyJSONObject(d.Get("metadata_json").(string))
	if err != nil {
		return err
	}
	if _, present := metadata["allowed_passthrough_routes"]; !present {
		return nil
	}
	raw, diags := d.GetRawConfigAt(cty.GetAttrPath("allowed_passthrough_routes"))
	if !diags.HasError() && !raw.IsNull() {
		return fmt.Errorf("allowed_passthrough_routes must be managed in either metadata_json or its own attribute, not both")
	}
	return nil
}

func configureKeyJSON(d *schema.ResourceData, key *Key) error {
	if raw := d.Get("metadata_json").(string); raw != "" {
		metadata, err := parseKeyJSONObject(raw)
		if err != nil {
			return fmt.Errorf("metadata_json: %w", err)
		}
		key.Metadata = metadata
	}
	if raw := d.Get("router_settings").(string); raw != "" {
		settings, err := parseKeyJSONObject(raw)
		if err != nil {
			return fmt.Errorf("router_settings: %w", err)
		}
		key.RouterSettings = settings
	}
	return nil
}

func readKeyJSON(d *schema.ResourceData, key *Key) error {
	for name, server := range map[string]map[string]interface{}{
		"metadata_json": key.Metadata, "router_settings": key.RouterSettings,
	} {
		raw := d.Get(name).(string)
		if raw == "" {
			continue
		}
		declared, err := parseKeyJSONObject(raw)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		selected := declaredKeyMetadata(server, declared)
		if selected == nil {
			selected = map[string]interface{}{}
		}
		encoded, err := json.Marshal(selected)
		if err != nil {
			return fmt.Errorf("encoding %s: %w", name, err)
		}
		if err := d.Set(name, string(encoded)); err != nil {
			return err
		}
	}
	return nil
}

func plannedKeySettings(c *Client, d *schema.ResourceData, key *Key) error {
	key.Metadata = nil
	key.RouterSettings = nil
	metadataChanged := d.HasChange("metadata") || d.HasChange("metadata_json")
	if !metadataChanged && !d.HasChange("router_settings") {
		return nil
	}
	current, err := c.GetKey(d.Id())
	if err != nil {
		return err
	}
	if current == nil {
		return fmt.Errorf("key %s %w", d.Id(), errKeyGone)
	}
	if metadataChanged {
		oldLegacy, newLegacy := d.GetChange("metadata")
		oldRaw, newRaw := d.GetChange("metadata_json")
		oldJSON, err := parseKeyJSONObject(oldRaw.(string))
		if err != nil {
			return err
		}
		newJSON, err := parseKeyJSONObject(newRaw.(string))
		if err != nil {
			return err
		}
		oldDeclared := mergeKeyMetadata(oldLegacy.(map[string]interface{}), nil, oldJSON)
		newDeclared := mergeKeyMetadata(newLegacy.(map[string]interface{}), nil, newJSON)
		key.Metadata = mergeKeyMetadata(current.Metadata, oldDeclared, newDeclared)
	}
	if d.HasChange("router_settings") {
		key.RouterSettings, err = mergeKeyJSONChange(d, "router_settings", current.RouterSettings)
	}
	return err
}

func mergeKeyJSONChange(d *schema.ResourceData, name string, server map[string]interface{}) (map[string]interface{}, error) {
	old, new := d.GetChange(name)
	oldDeclared, err := parseKeyJSONObject(old.(string))
	if err != nil {
		return nil, fmt.Errorf("%s in prior state: %w", name, err)
	}
	newDeclared, err := parseKeyJSONObject(new.(string))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return mergeKeyMetadata(server, oldDeclared, newDeclared), nil
}

func verifyKeyJSONUpdate(d *schema.ResourceData, desired, stored *Key) error {
	if stored == nil {
		return fmt.Errorf("key %s %w after update", d.Id(), errKeyGone)
	}
	for name, values := range map[string][2]map[string]interface{}{
		"metadata_json":   {desired.Metadata, stored.Metadata},
		"router_settings": {desired.RouterSettings, stored.RouterSettings},
	} {
		if !d.HasChange(name) {
			continue
		}
		old, new := d.GetChange(name)
		for _, raw := range []string{old.(string), new.(string)} {
			declared, err := parseKeyJSONObject(raw)
			if err != nil {
				return err
			}
			for field := range declared {
				want, wantPresent := values[0][field]
				got, gotPresent := values[1][field]
				if wantPresent != gotPresent || !equalKeyJSON(want, got) {
					return fmt.Errorf("%s.%s readback differs from the requested update", name, field)
				}
			}
		}
	}
	return nil
}

func preserveKeyJSONFields(body []byte, result map[string]interface{}) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return err
	}
	if info, ok := result["info"].(map[string]interface{}); ok {
		return preserveKeyJSONFields(raw["info"], info)
	}
	hoistKeyFieldsStoredInMetadata(result)
	for _, name := range []string{"metadata", "router_settings"} {
		value, present := raw[name]
		if !present || string(value) == "null" {
			continue
		}
		if name == "router_settings" && len(value) > 0 && value[0] == '"' {
			var serialized string
			if err := json.Unmarshal(value, &serialized); err != nil {
				return err
			}
			value = []byte(serialized)
		}
		parsed, err := parseKeyJSONObject(string(value))
		if err != nil {
			return fmt.Errorf("invalid %s in key response: %w", name, err)
		}
		result[name] = parsed
	}
	return nil
}
