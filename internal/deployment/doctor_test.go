package deployment

import "testing"

// Compose v2.21 changed `ps --format json` from a JSON array to one object per
// line. Both shapes have to read the same way.
func TestHealthyServicesReadsBothComposeOutputShapes(t *testing.T) {
	lines := `{"Service":"core","State":"running","Health":"healthy"}
{"Service":"control","State":"running","Health":"healthy"}
{"Service":"edge","State":"running","Health":""}
`
	array := `[{"Service":"core","State":"running","Health":"healthy"},
{"Service":"control","State":"running","Health":"healthy"},
{"Service":"edge","State":"running","Health":""}]`

	for name, data := range map[string]string{"objects per line": lines, "array": array} {
		detail, err := healthyServices([]byte(data))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if detail != "core, control, and edge are running" {
			t.Fatalf("%s: unexpected detail %q", name, detail)
		}
	}
}

func TestHealthyServicesReportsUnhealthyAndMissingServices(t *testing.T) {
	for name, data := range map[string]string{
		"stopped": `{"Service":"core","State":"exited","Health":""}
{"Service":"control","State":"running","Health":"healthy"}
{"Service":"edge","State":"running","Health":""}`,
		"unhealthy": `{"Service":"core","State":"running","Health":"starting"}
{"Service":"control","State":"running","Health":"healthy"}
{"Service":"edge","State":"running","Health":""}`,
		"missing edge": `{"Service":"core","State":"running","Health":"healthy"}
{"Service":"control","State":"running","Health":"healthy"}`,
		"nothing running": ``,
	} {
		if _, err := healthyServices([]byte(data)); err == nil {
			t.Fatalf("%s: expected an error", name)
		}
	}
}

func TestHealthyServicesRejectsUnreadableOutput(t *testing.T) {
	if _, err := healthyServices([]byte("not json")); err == nil {
		t.Fatal("expected an error for output that is not JSON")
	}
}
