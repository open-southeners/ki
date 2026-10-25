package compose

import (
	"reflect"
	"strings"
	"testing"
)

func TestExposures(t *testing.T) {
	model := `{"services":{
	  "web":{"labels":{"ki.expose.port":"8080","ki.expose.domains":"A.example.com, www.example.com","ki.expose.path":"/api"}},
	  "api":{"labels":["ki.expose.port=3000","ki.expose.domains=api.example.com","other=x"]},
	  "db":{"labels":{"foo":"bar"}},
	  "bare":{}
	}}`
	got, err := Exposures([]byte(model))
	if err != nil {
		t.Fatal(err)
	}
	want := []ExposureLabel{
		{Service: "api", Port: 3000, Domains: []string{"api.example.com"}},
		{Service: "web", Port: 8080, Domains: []string{"A.example.com", "www.example.com"}, Path: "/api"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
	none, err := Exposures([]byte(`{"services":{"a":{}}}`))
	if err != nil || len(none) != 0 {
		t.Fatalf("none = %v, %v", none, err)
	}
}

func TestExposuresErrors(t *testing.T) {
	for name, model := range map[string]string{
		"no domains": `{"services":{"web":{"labels":{"ki.expose.port":"80"}}}}`,
		"no port":    `{"services":{"web":{"labels":{"ki.expose.domains":"a.example.com"}}}}`,
		"bad port":   `{"services":{"web":{"labels":{"ki.expose.port":"x","ki.expose.domains":"a.example.com"}}}}`,
		"path only":  `{"services":{"web":{"labels":{"ki.expose.path":"/x"}}}}`,
		"not json":   `nope`,
	} {
		_, err := Exposures([]byte(model))
		if err == nil {
			t.Errorf("%s: no error", name)
		} else if name != "not json" && !strings.Contains(err.Error(), `"web"`) {
			t.Errorf("%s: error %q does not name the service", name, err)
		}
	}
}
