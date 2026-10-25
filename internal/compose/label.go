package compose

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/open-southeners/ki-agent/internal/runtime"
)

// Label adds the ki.* labels to a normalized compose model: ki.managed,
// ki.project and ki.service on every service, and ki.managed and ki.project on
// every non-external top-level network and volume. Existing labels are kept
// (list-form labels are turned into a map, as in normalized output) except
// that ki.* keys are overwritten. Every other field is left as it was; only the
// key order of the JSON changes.
func Label(model []byte, slug string) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(model))
	dec.UseNumber() // keep numbers exactly as Compose printed them
	var root map[string]any
	if err := dec.Decode(&root); err != nil {
		return nil, fmt.Errorf("compose: parse model: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("compose: parse model: trailing data")
	}
	if root == nil {
		return nil, fmt.Errorf("compose: parse model: not an object")
	}

	services, err := section(root, "services")
	if err != nil {
		return nil, err
	}
	for name, v := range services {
		svc, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("compose: service %q is not an object", name)
		}
		if err := setLabels(svc, slug, name); err != nil {
			return nil, fmt.Errorf("compose: service %q: %w", name, err)
		}
	}
	for _, key := range []string{"networks", "volumes"} {
		defs, err := section(root, key)
		if err != nil {
			return nil, err
		}
		for name, v := range defs {
			def, ok := v.(map[string]any)
			if v == nil {
				def, ok = map[string]any{}, true
			}
			if !ok {
				return nil, fmt.Errorf("compose: %s %q is not an object", key, name)
			}
			if isExternal(def["external"]) {
				continue
			}
			if err := setLabels(def, slug, ""); err != nil {
				return nil, fmt.Errorf("compose: %s %q: %w", key, name, err)
			}
			defs[name] = def
		}
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(root); err != nil {
		return nil, fmt.Errorf("compose: encode model: %w", err)
	}
	return buf.Bytes(), nil
}

// section returns root[key] as an object (nil map when absent or null).
func section(root map[string]any, key string) (map[string]any, error) {
	v, ok := root[key]
	if !ok || v == nil {
		return nil, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("compose: %q is not an object", key)
	}
	return m, nil
}

// isExternal reports whether an `external` value marks the resource external:
// true, or (older models) an object such as {"name": "x"}.
func isExternal(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case map[string]any:
		return true
	case string:
		return strings.EqualFold(t, "true")
	}
	return false
}

func setLabels(obj map[string]any, slug, service string) error {
	labels := map[string]any{}
	switch t := obj["labels"].(type) {
	case nil:
	case map[string]any:
		labels = t
	case []any:
		for _, e := range t {
			s, ok := e.(string)
			if !ok {
				return fmt.Errorf("labels entry %v is not a string", e)
			}
			k, v, _ := strings.Cut(s, "=")
			labels[k] = v
		}
	default:
		return fmt.Errorf("labels is neither a map nor a list")
	}
	labels[runtime.LabelManaged] = "true"
	labels[runtime.LabelProject] = slug
	if service != "" {
		labels[runtime.LabelService] = service
	} else {
		delete(labels, runtime.LabelService)
	}
	obj["labels"] = labels
	return nil
}
