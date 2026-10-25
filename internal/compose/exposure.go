package compose

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Exposure label keys on a service.
const (
	LabelExposePort    = "ki.expose.port"
	LabelExposeDomains = "ki.expose.domains"
	LabelExposePath    = "ki.expose.path"
)

// ExposureLabel is the exposure a service declares through its ki.expose.*
// labels. Values are as written (domains trimmed, not validated).
type ExposureLabel struct {
	Service string
	Port    int
	Domains []string
	Path    string
}

// Exposures reads the ki.expose.* labels of every service of a normalized
// compose model (labels as a map or as a list), sorted by service. Services
// without any such label are skipped. A service with a port but no domains, or
// domains but no port, or an unparsable port is an error naming the service.
func Exposures(model []byte) ([]ExposureLabel, error) {
	dec := json.NewDecoder(bytes.NewReader(model))
	dec.UseNumber()
	var root map[string]any
	if err := dec.Decode(&root); err != nil {
		return nil, fmt.Errorf("compose: parse model: %w", err)
	}
	services, err := section(root, "services")
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []ExposureLabel
	for _, name := range names {
		svc, ok := services[name].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("compose: service %q is not an object", name)
		}
		labels, err := stringLabels(svc["labels"])
		if err != nil {
			return nil, fmt.Errorf("compose: service %q: %w", name, err)
		}
		portS, hasPort := labels[LabelExposePort]
		domS, hasDom := labels[LabelExposeDomains]
		_, hasPath := labels[LabelExposePath]
		if !hasPort && !hasDom && !hasPath {
			continue
		}
		if strings.TrimSpace(portS) == "" {
			return nil, fmt.Errorf("compose: service %q: %s is required when exposing", name, LabelExposePort)
		}
		var domains []string
		for d := range strings.SplitSeq(domS, ",") {
			if d = strings.TrimSpace(d); d != "" {
				domains = append(domains, d)
			}
		}
		if len(domains) == 0 {
			return nil, fmt.Errorf("compose: service %q: %s is required when exposing", name, LabelExposeDomains)
		}
		port, err := strconv.Atoi(strings.TrimSpace(portS))
		if err != nil {
			return nil, fmt.Errorf("compose: service %q: %s %q is not a number", name, LabelExposePort, portS)
		}
		out = append(out, ExposureLabel{
			Service: name, Port: port, Domains: domains,
			Path: strings.TrimSpace(labels[LabelExposePath]),
		})
	}
	return out, nil
}

// stringLabels flattens a labels value (map or list form) to strings.
func stringLabels(v any) (map[string]string, error) {
	out := map[string]string{}
	switch t := v.(type) {
	case nil:
	case map[string]any:
		for k, val := range t {
			switch x := val.(type) {
			case nil:
				out[k] = ""
			case string:
				out[k] = x
			case json.Number:
				out[k] = x.String()
			case bool:
				out[k] = strconv.FormatBool(x)
			default:
				return nil, fmt.Errorf("label %q is not a scalar", k)
			}
		}
	case []any:
		for _, e := range t {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("labels entry %v is not a string", e)
			}
			k, val, _ := strings.Cut(s, "=")
			out[k] = val
		}
	default:
		return nil, fmt.Errorf("labels is neither a map nor a list")
	}
	return out, nil
}
