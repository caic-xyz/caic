// Tests for SDK output generation methods.

package main

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type TestSDKEvent struct {
	Kind string `json:"kind"`
	Text string `json:"text,omitempty"`
}

func TestGenConfigGoTypeToDoc(t *testing.T) {
	t.Parallel()

	cfg := &genConfig{
		specialTypes: []specialType{
			{t: reflect.TypeFor[json.RawMessage](), docType: "JSONValue"},
			{t: reflect.TypeFor[any](), docType: "JSONValue"},
		},
	}
	for _, tc := range []struct {
		name string
		t    reflect.Type
		want string
	}{
		{name: "string boolean map", t: reflect.TypeFor[map[string]bool](), want: "Record<string, boolean>"},
		{name: "string value map", t: reflect.TypeFor[map[string]string](), want: "Record<string, string>"},
		{name: "raw JSON map", t: reflect.TypeFor[map[string]json.RawMessage](), want: "Record<string, JSONValue>"},
		{name: "any map", t: reflect.TypeFor[map[string]any](), want: "Record<string, JSONValue>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := cfg.goTypeToDoc(tc.t)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("goTypeToDoc(%s) = %q, want %q", tc.t, got, tc.want)
			}
		})
	}
}

func TestDocRegistryGenerateTSValidate(t *testing.T) {
	t.Parallel()

	t.Run("without SSE routes removes stale file", func(t *testing.T) {
		t.Parallel()

		outDir := t.TempDir()
		validatePath := filepath.Join(outDir, "validate.gen.ts")
		if err := os.WriteFile(validatePath, []byte("stale"), 0o600); err != nil {
			t.Fatal(err)
		}

		docs := &docRegistry{
			cfg: &genConfig{},
		}
		if err := docs.generateTSValidate(outDir); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(validatePath); !os.IsNotExist(err) {
			t.Fatalf("validate.gen.ts exists after generation without SSE routes: %v", err)
		}
	})

	t.Run("with SSE routes writes validator", func(t *testing.T) {
		t.Parallel()

		outDir := t.TempDir()
		eventType := reflect.TypeFor[TestSDKEvent]()
		docs := &docRegistry{
			cfg: &genConfig{
				routes: []routeDef{{Name: "events", Resp: eventType, IsSSE: true}},
				sdkPackagePaths: map[string]struct{}{
					eventType.PkgPath(): {},
				},
				sseSeeds: []reflect.Type{eventType},
			},
			aliasNames: map[string]struct{}{},
		}

		if err := docs.generateTSValidate(outDir); err != nil {
			t.Fatal(err)
		}
		content, err := fs.ReadFile(os.DirFS(outDir), "validate.gen.ts")
		if err != nil {
			t.Fatal(err)
		}
		text := string(content)
		for _, want := range []string{
			"type ValidatorInput = unknown;",
			"export function validateTestSDKEvent(raw: ValidatorInput): TestSDKEvent",
		} {
			if !strings.Contains(text, want) {
				t.Fatalf("validate.gen.ts does not contain %q:\n%s", want, text)
			}
		}
	})
}
