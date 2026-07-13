package contracts

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestPublicOpenAPIParsesAndAllLocalReferencesResolve(t *testing.T) {
	var document map[string]any
	if err := yaml.Unmarshal(PublicOpenAPI(), &document); err != nil {
		t.Fatalf("parse public OpenAPI: %v", err)
	}
	if document["openapi"] != "3.1.0" {
		t.Fatalf("unexpected OpenAPI version: %v", document["openapi"])
	}
	var walk func(any)
	walk = func(value any) {
		switch value := value.(type) {
		case map[string]any:
			if reference, ok := value["$ref"].(string); ok {
				if _, err := resolveLocalReference(document, reference); err != nil {
					t.Error(err)
				}
			}
			for _, child := range value {
				walk(child)
			}
		case []any:
			for _, child := range value {
				walk(child)
			}
		}
	}
	walk(document)
}

func TestPrivateOpenAPIParsesAndHasLiveFLXRoutes(t *testing.T) {
	data, err := os.ReadFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatalf("parse private OpenAPI: %v", err)
	}
	paths, ok := document["paths"].(map[string]any)
	if !ok {
		t.Fatal("private OpenAPI paths are missing")
	}
	for _, path := range []string{"/internal/v1/schema/read", "/internal/v1/flx/rules/read", "/internal/v1/flx/rules/plan", "/internal/v1/flx/rules/apply", "/internal/v1/flx/rules/test"} {
		if _, ok := paths[path]; !ok {
			t.Errorf("private OpenAPI is missing %s", path)
		}
	}
}

func TestPublicServiceKeySchemaCannotExposeDigest(t *testing.T) {
	var document map[string]any
	if err := yaml.Unmarshal(PublicOpenAPI(), &document); err != nil {
		t.Fatal(err)
	}
	value, err := resolveLocalReference(document, "#/components/schemas/ServiceKey/properties")
	if err != nil {
		t.Fatal(err)
	}
	properties, ok := value.(map[string]any)
	if !ok {
		t.Fatal("ServiceKey properties are not an object")
	}
	if _, exists := properties["digest"]; exists {
		t.Fatal("public ServiceKey schema exposes the stored digest")
	}
	if _, exists := properties["secret"]; exists {
		t.Fatal("public ServiceKey metadata exposes a raw secret")
	}
}

func resolveLocalReference(document map[string]any, reference string) (any, error) {
	if !strings.HasPrefix(reference, "#/") {
		return nil, fmt.Errorf("OpenAPI reference must be local: %s", reference)
	}
	var current any = document
	for _, token := range strings.Split(strings.TrimPrefix(reference, "#/"), "/") {
		token = strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
		object, ok := current.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("OpenAPI reference is not an object at %q: %s", token, reference)
		}
		current, ok = object[token]
		if !ok {
			return nil, fmt.Errorf("unresolved OpenAPI reference: %s", reference)
		}
	}
	return current, nil
}
