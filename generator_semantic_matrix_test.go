package goja

import (
	"encoding/json"
	"os"
	"testing"
)

func TestGeneratorSemanticMatrix(t *testing.T) {
	var cases []struct{ Name, Source, Expected string }
	buf, err := os.ReadFile("testdata/generator_semantic_cases.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(buf, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			rt := New()
			v, err := rt.RunString(c.Source)
			if err != nil {
				t.Fatal(err)
			}
			if got := v.String(); got != c.Expected {
				t.Fatalf("got=%s expected=%s", got, c.Expected)
			}
			assertGeneratorBoundaryClean(t, rt)
		})
	}
}
